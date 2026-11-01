package blobmigration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/blobinventory"
)

const (
	encryptionProgressSchema = "teagram.blob-encryption-progress/v1"
	maxProgressBytes         = 256 << 20
)

// EncryptionOptions records the operator's write-quiescence choice and names
// the durable journal used to resume the pass.
type EncryptionOptions struct {
	ProgressPath string
	Writes       string
}

// EncryptionSummary describes a fully verified encrypted copy of every
// referenced object. Unaccounted contains unreferenced objects only; accepted
// plaintext source copies are expected to remain until CleanupPlaintext.
type EncryptionSummary struct {
	Objects         int
	Bytes           int64
	AlreadyVerified int
	Writes          string
	InventorySHA256 string
	InventoryReport []byte
	ProgressSHA256  string
	Unaccounted     []blobinventory.ObjectID
}

// CleanupSummary reports only the plaintext copies removed by an explicit
// cleanup pass. The encrypted copies remain under EncryptedObjectsPrefix.
type CleanupSummary struct {
	ObjectsRemoved int
	BytesRemoved   int64
}

type encryptionTree interface {
	Walk(context.Context, func(blob.Entry) error) error
}

type plaintextOnlyStore struct {
	blob.Store

	tree encryptionTree
}

func (s plaintextOnlyStore) Walk(ctx context.Context, fn func(blob.Entry) error) error {
	return s.tree.Walk(ctx, func(entry blob.Entry) error {
		if entry.Key == blob.EncryptedObjectsPrefix || strings.HasPrefix(entry.Key, blob.EncryptedObjectsPrefix+"/") {
			return nil
		}
		return fn(entry)
	})
}

type progressHeader struct {
	Type          string `json:"type"`
	Schema        string `json:"schema"`
	Mode          string `json:"mode,omitempty"`
	Writes        string `json:"writes"`
	KeyVersion    uint32 `json:"key_version"`
	ReferencesSHA string `json:"references_sha256"`
}

type progressEvent struct {
	Type         string `json:"type"`
	Key          string `json:"key,omitempty"`
	Kind         string `json:"kind,omitempty"`
	ID           string `json:"id,omitempty"`
	LogicalBytes int64  `json:"logical_bytes,omitempty"`
	SourceSHA256 string `json:"source_sha256,omitempty"`
	TargetSalt   string `json:"target_salt,omitempty"`
	State        string `json:"state,omitempty"`
}

type progressObject struct {
	Kind         string
	ID           string
	LogicalBytes int64
	SourceSHA256 string
	TargetSalt   string
	State        string
}

type progressJournal struct {
	file    *os.File
	header  progressHeader
	objects map[string]progressObject
}

// EncryptReferenced writes a verified envelope copy for every database
// reference under blob.EncryptedObjectsPrefix. Plaintext keys are never
// overwritten or removed. The progress journal is fsynced before each write
// and after each authenticated read-back, so a later invocation can verify and
// reuse a completed object without rewriting it.
func EncryptReferenced(
	ctx context.Context,
	backend blob.Store,
	ring *blob.KeyRing,
	references []blobinventory.Reference,
	options EncryptionOptions,
) (summary EncryptionSummary, retErr error) {
	refs, err := normalizedReferences(references)
	if err != nil {
		return summary, err
	}
	if backend == nil || ring == nil {
		return summary, errors.New("media encryption migration requires a blob store and media key ring")
	}
	if ring.Current() == 0 {
		return summary, errors.New("media encryption migration requires a current media key")
	}
	if options.Writes != "stopped" && options.Writes != "drained" {
		return summary, errors.New("media encryption migration requires --writes stopped or --writes drained")
	}
	if options.ProgressPath == "" {
		return summary, errors.New("media encryption migration requires a progress journal path")
	}
	tree, ok := backend.(encryptionTree)
	if !ok {
		return summary, errors.New("media encryption migration requires full-tree enumeration")
	}
	refsHash, err := referencesDigest(refs)
	if err != nil {
		return summary, err
	}
	journal, err := openProgress(options.ProgressPath, progressHeader{
		Type: "header", Schema: encryptionProgressSchema, Writes: options.Writes,
		KeyVersion: ring.Current(), ReferencesSHA: refsHash,
	}, false)
	if err != nil {
		return summary, err
	}
	defer func() {
		if closeErr := journal.Close(); closeErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close progress journal: %w", closeErr))
		}
	}()
	if err := validateProgressReferences(journal, refs); err != nil {
		return summary, err
	}
	entries, err := collectMigrationEntries(ctx, tree)
	if err != nil {
		return summary, err
	}
	prefixed, err := blob.NewPrefixedStore(backend, blob.EncryptedObjectsPrefix)
	if err != nil {
		return summary, err
	}
	sealed, err := blob.NewEncrypted(prefixed, ring, blob.DefaultChunkSize)
	if err != nil {
		return summary, err
	}
	refsByKey := make(map[string]blobinventory.Reference, len(refs))
	for _, ref := range refs {
		refsByKey[ref.Key] = ref
	}

	summary.Writes = options.Writes
	summary.Unaccounted = make([]blobinventory.ObjectID, 0)
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		sourceEntry, sourceExists := entries[ref.Key]
		if sourceExists && (!sourceEntry.Regular || sourceEntry.Dir) {
			return summary, fmt.Errorf("media encryption migration: referenced source %q is not a regular object", ref.Key)
		}
		if sourceExists && sourceEntry.Size != ref.LogicalBytes {
			return summary, fmt.Errorf("media encryption migration: source %q has %d bytes, database reference says %d", ref.ID, sourceEntry.Size, ref.LogicalBytes)
		}
		targetKey, err := blob.EncryptedObjectKey(ref.Key)
		if err != nil {
			return summary, err
		}
		targetEntry, targetExists := entries[targetKey]
		if targetExists && (!targetEntry.Regular || targetEntry.Dir) {
			return summary, fmt.Errorf("media encryption migration: encrypted target for %q is not a regular object", ref.ID)
		}
		progress, hasProgress := journal.objects[ref.Key]
		var sourceDigest string
		if sourceExists {
			sourceDigest, err = digestStore(ctx, backend, sourceEntry)
			if err != nil {
				return summary, fmt.Errorf("media encryption migration: checksum plaintext object %q: %w", ref.ID, err)
			}
		} else if hasProgress {
			sourceDigest = progress.SourceSHA256
		}

		if targetExists {
			targetSize := targetEntry.Size
			targetDigest, verifyErr := verifyEncryptedObject(ctx, prefixed, sealed, ring, ref, targetSize)
			if verifyErr == nil && sourceDigest != "" && targetDigest == sourceDigest {
				summary.AlreadyVerified++
				if err := journal.record(ref, sourceDigest, "verified"); err != nil {
					return summary, err
				}
				continue
			}
			if verifyErr == nil && !sourceExists && !hasProgress {
				// New references written after cutover have no legacy source or
				// migration event. Their authenticated readback is enough to include
				// them in a later inventory without rewriting them.
				summary.AlreadyVerified++
				if err := journal.record(ref, targetDigest, "verified"); err != nil {
					return summary, err
				}
				continue
			}
			if !hasProgress || sourceDigest == "" {
				if verifyErr != nil {
					return summary, fmt.Errorf("media encryption migration: encrypted target for %q is not a resumable verified copy: %w", ref.ID, verifyErr)
				}
				return summary, fmt.Errorf("media encryption migration: encrypted target for %q does not match its plaintext source", ref.ID)
			}
		} else if !sourceExists {
			return summary, fmt.Errorf("media encryption migration: reference %q has neither a plaintext source nor an encrypted copy", ref.ID)
		}
		if !sourceExists {
			return summary, fmt.Errorf("media encryption migration: cannot repair encrypted target for %q without its plaintext source", ref.ID)
		}

		if err := journal.record(ref, sourceDigest, "started"); err != nil {
			return summary, err
		}
		reader := &rangeReader{ctx: ctx, store: backend, key: ref.Key, size: ref.LogicalBytes}
		written, err := sealed.Put(ctx, ref.Key, reader)
		if err != nil {
			return summary, fmt.Errorf("media encryption migration: write envelope for %q: %w", ref.ID, err)
		}
		if written != ref.LogicalBytes {
			return summary, fmt.Errorf("media encryption migration: envelope for %q contains %d logical bytes, want %d", ref.ID, written, ref.LogicalBytes)
		}
		physicalSize, err := blob.EnvelopePhysicalSize(ref.LogicalBytes, blob.DefaultChunkSize)
		if err != nil {
			return summary, err
		}
		verifiedDigest, err := verifyEncryptedObject(ctx, prefixed, sealed, ring, ref, physicalSize)
		if err != nil {
			return summary, fmt.Errorf("media encryption migration: verify envelope for %q: %w", ref.ID, err)
		}
		if verifiedDigest != sourceDigest {
			return summary, fmt.Errorf("media encryption migration: envelope for %q differs from its plaintext source", ref.ID)
		}
		if err := journal.record(ref, sourceDigest, "verified"); err != nil {
			return summary, err
		}
	}
	for _, ref := range refs {
		sourceEntry, sourceExists := entries[ref.Key]
		if !sourceExists {
			continue
		}
		progress, ok := journal.objects[ref.Key]
		if !ok || progress.State != "verified" {
			return summary, fmt.Errorf("media encryption migration: plaintext source %q has no verified progress record", ref.ID)
		}
		currentDigest, err := digestStore(ctx, backend, sourceEntry)
		if err != nil {
			return summary, fmt.Errorf("media encryption migration: recheck plaintext source %q: %w", ref.ID, err)
		}
		if currentDigest != progress.SourceSHA256 {
			return summary, fmt.Errorf("media encryption migration: plaintext source changed during migration for %q", ref.ID)
		}
	}

	report, err := blobinventory.Scan(ctx, prefixed, ring, refs)
	if err != nil {
		return summary, fmt.Errorf("media encryption migration: encrypted inventory: %w", err)
	}
	if report.Classes.NoValidEnvelope.Count != 0 || report.Classes.Missing.Count != 0 {
		remaining := append(append([]blobinventory.ObjectID(nil), report.Classes.NoValidEnvelope.IDs...), report.Classes.Missing.IDs...)
		return summary, fmt.Errorf("media encryption migration: inventory found references without a valid envelope: %s", formatObjectIDs(remaining))
	}
	plaintextReport, err := blobinventory.Scan(ctx, plaintextOnlyStore{Store: backend, tree: tree}, ring, refs)
	if err != nil {
		return summary, fmt.Errorf("media encryption migration: plaintext inventory: %w", err)
	}
	unaccounted := make(map[string]blobinventory.ObjectID)
	for _, object := range report.Classes.Unaccounted.IDs {
		unaccounted[object.Key] = object
	}
	for _, object := range plaintextReport.Classes.Unaccounted.IDs {
		if _, isPlaintextCopy := refsByKey[object.Key]; !isPlaintextCopy {
			unaccounted[object.Key] = object
		}
	}
	for _, object := range unaccounted {
		summary.Unaccounted = append(summary.Unaccounted, object)
	}
	sort.Slice(summary.Unaccounted, func(i, j int) bool {
		return summary.Unaccounted[i].Key < summary.Unaccounted[j].Key
	})
	for _, ref := range refs {
		if ref.LogicalBytes > math.MaxInt64-summary.Bytes {
			return summary, errors.New("media encryption migration: logical byte total overflows int64")
		}
		summary.Bytes += ref.LogicalBytes
	}
	summary.Objects = len(refs)
	encodedReport, err := json.Marshal(struct {
		Schema    string               `json:"schema"`
		Encrypted blobinventory.Report `json:"encrypted"`
		Plaintext blobinventory.Report `json:"plaintext"`
	}{Schema: "teagram.blob-encryption-inventory/v1", Encrypted: report, Plaintext: plaintextReport})
	if err != nil {
		return summary, fmt.Errorf("media encryption migration: encode final inventory: %w", err)
	}
	summary.InventorySHA256 = digestBytes(encodedReport)
	summary.InventoryReport = encodedReport
	summary.ProgressSHA256, err = journal.digest()
	if err != nil {
		return summary, err
	}
	return summary, nil
}

// CleanupPlaintext removes verified plaintext copies for current references
// and journaled objects erased after cutover. Current references require a
// matching authenticated envelope; erased objects require a matching journal
// digest. Unjournaled objects are left untouched. It is intentionally separate
// from [EncryptReferenced].
func CleanupPlaintext(
	ctx context.Context,
	backend blob.Store,
	ring *blob.KeyRing,
	references []blobinventory.Reference,
	options EncryptionOptions,
) (summary CleanupSummary, retErr error) {
	refs, err := normalizedReferences(references)
	if err != nil {
		return summary, err
	}
	if backend == nil || ring == nil || options.ProgressPath == "" {
		return summary, errors.New("plaintext cleanup requires a blob store, media key ring, and progress journal")
	}
	if options.Writes != "stopped" && options.Writes != "drained" {
		return summary, errors.New("plaintext cleanup requires --writes stopped or --writes drained")
	}
	tree, ok := backend.(encryptionTree)
	if !ok {
		return summary, errors.New("plaintext cleanup requires full-tree enumeration")
	}
	refsHash, err := referencesDigest(refs)
	if err != nil {
		return summary, err
	}
	journal, err := openProgress(options.ProgressPath, progressHeader{
		Type: "header", Schema: encryptionProgressSchema, Writes: options.Writes,
		KeyVersion: ring.Current(), ReferencesSHA: refsHash,
	}, false)
	if err != nil {
		return summary, err
	}
	defer func() {
		if closeErr := journal.Close(); closeErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close progress journal: %w", closeErr))
		}
	}()
	if err := validateProgressReferences(journal, refs); err != nil {
		return summary, err
	}
	entries, err := collectMigrationEntries(ctx, tree)
	if err != nil {
		return summary, err
	}
	prefixed, err := blob.NewPrefixedStore(backend, blob.EncryptedObjectsPrefix)
	if err != nil {
		return summary, err
	}
	sealed, err := blob.NewEncrypted(prefixed, ring, blob.DefaultChunkSize)
	if err != nil {
		return summary, err
	}
	referenced := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		referenced[ref.Key] = struct{}{}
		progress, hasProgress := journal.objects[ref.Key]
		targetKey, err := blob.EncryptedObjectKey(ref.Key)
		if err != nil {
			return summary, err
		}
		target, exists := entries[targetKey]
		if !exists || !target.Regular {
			return summary, fmt.Errorf("plaintext cleanup: verified encrypted copy for %q is missing", ref.ID)
		}
		digest, err := verifyEncryptedObject(ctx, prefixed, sealed, ring, ref, target.Size)
		if err != nil {
			return summary, fmt.Errorf("plaintext cleanup: encrypted copy for %q failed verification: %w", ref.ID, err)
		}
		if hasProgress && digest != progress.SourceSHA256 {
			return summary, fmt.Errorf("plaintext cleanup: encrypted copy for %q differs from its verified source", ref.ID)
		}
		source, exists := entries[ref.Key]
		if exists {
			if !source.Regular || source.Size != ref.LogicalBytes {
				return summary, fmt.Errorf("plaintext cleanup: source for %q is not the expected regular object", ref.ID)
			}
			if hasProgress && progress.State != "verified" && progress.State != "cleaned" {
				return summary, fmt.Errorf("plaintext cleanup: object %q has no completed migration verification", ref.ID)
			}
			sourceDigest, err := digestStore(ctx, backend, source)
			if err != nil {
				return summary, fmt.Errorf("plaintext cleanup: checksum source for %q: %w", ref.ID, err)
			}
			if sourceDigest != digest {
				return summary, fmt.Errorf("plaintext cleanup: source for %q differs from its verified envelope", ref.ID)
			}
			if ref.LogicalBytes > math.MaxInt64-summary.BytesRemoved {
				return summary, errors.New("plaintext cleanup: removed byte total overflows int64")
			}
			if err := backend.Remove(ctx, ref.Key); err != nil {
				return summary, fmt.Errorf("plaintext cleanup: remove source for %q: %w", ref.ID, err)
			}
			summary.ObjectsRemoved++
			summary.BytesRemoved += ref.LogicalBytes
		}
		if err := journal.record(ref, digest, "cleaned"); err != nil {
			return summary, err
		}
	}
	staleKeys := make([]string, 0, len(journal.objects))
	for key, progress := range journal.objects {
		if progress.State != "verified" {
			continue
		}
		if _, isReferenced := referenced[key]; isReferenced {
			continue
		}
		staleKeys = append(staleKeys, key)
	}
	sort.Strings(staleKeys)
	for _, key := range staleKeys {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		progress := journal.objects[key]
		source, exists := entries[key]
		if !exists {
			continue
		}
		if !source.Regular || source.Dir || source.Size != progress.LogicalBytes {
			return summary, fmt.Errorf("plaintext cleanup: erased source %q is not the expected regular object", progress.ID)
		}
		sourceDigest, err := digestStore(ctx, backend, source)
		if err != nil {
			return summary, fmt.Errorf("plaintext cleanup: checksum erased source for %q: %w", progress.ID, err)
		}
		if sourceDigest != progress.SourceSHA256 {
			return summary, fmt.Errorf("plaintext cleanup: erased source %q differs from its verified migration digest", progress.ID)
		}
		if progress.LogicalBytes > math.MaxInt64-summary.BytesRemoved {
			return summary, errors.New("plaintext cleanup: removed byte total overflows int64")
		}
		if err := backend.Remove(ctx, key); err != nil {
			return summary, fmt.Errorf("plaintext cleanup: remove erased source for %q: %w", progress.ID, err)
		}
		summary.ObjectsRemoved++
		summary.BytesRemoved += progress.LogicalBytes
		ref := blobinventory.Reference{
			Key: key, Kind: progress.Kind, ID: progress.ID, LogicalBytes: progress.LogicalBytes,
		}
		if err := journal.record(ref, progress.SourceSHA256, "cleaned"); err != nil {
			return summary, err
		}
	}
	return summary, nil
}

func normalizedReferences(references []blobinventory.Reference) ([]blobinventory.Reference, error) {
	refs := append([]blobinventory.Reference(nil), references...)
	for _, ref := range refs {
		if err := blob.ValidateKey(ref.Key); err != nil {
			return nil, fmt.Errorf("media encryption migration: invalid reference key: %w", err)
		}
		if (ref.Kind != "file" && ref.Kind != "part") || ref.ID == "" || ref.LogicalBytes < 0 {
			return nil, fmt.Errorf("media encryption migration: invalid reference for key %q", ref.Key)
		}
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Key < refs[j].Key })
	for i := 1; i < len(refs); i++ {
		if refs[i-1].Key == refs[i].Key {
			return nil, fmt.Errorf("media encryption migration: duplicate reference key %q", refs[i].Key)
		}
	}
	return refs, nil
}

func referencesDigest(refs []blobinventory.Reference) (string, error) {
	h := sha256.New()
	for _, ref := range refs {
		if _, err := fmt.Fprintf(h, "%s\t%s\t%s\t%d\n", ref.Kind, ref.ID, ref.Key, ref.LogicalBytes); err != nil {
			return "", fmt.Errorf("hash reference inventory: %w", err)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func collectMigrationEntries(ctx context.Context, tree encryptionTree) (map[string]blob.Entry, error) {
	entries := make(map[string]blob.Entry)
	if err := tree.Walk(ctx, func(entry blob.Entry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, exists := entries[entry.Key]; exists {
			return fmt.Errorf("media encryption migration: store listed key %q more than once", entry.Key)
		}
		entries[entry.Key] = entry
		return nil
	}); err != nil {
		return nil, fmt.Errorf("media encryption migration: enumerate blob store: %w", err)
	}
	return entries, nil
}

func validateProgressReferences(journal *progressJournal, refs []blobinventory.Reference) error {
	byKey := make(map[string]blobinventory.Reference, len(refs))
	for _, ref := range refs {
		byKey[ref.Key] = ref
	}
	for key, event := range journal.objects {
		ref, ok := byKey[key]
		if ok && (event.Kind != ref.Kind || event.ID != ref.ID || event.LogicalBytes != ref.LogicalBytes) {
			return fmt.Errorf("media encryption migration: progress journal reference %q no longer matches the database inventory", key)
		}
	}
	return nil
}

func verifyEncryptedObject(
	ctx context.Context,
	backend blob.Store,
	sealed blob.Store,
	ring *blob.KeyRing,
	ref blobinventory.Reference,
	physicalBytes int64,
) (string, error) {
	info, err := blob.InspectEnvelopeHeader(ctx, backend, ring, ref.Key, physicalBytes)
	if err != nil {
		return "", err
	}
	if info.KeyVersion != ring.Current() {
		return "", fmt.Errorf("envelope key version %d, want current version %d", info.KeyVersion, ring.Current())
	}
	if info.LogicalBytes != ref.LogicalBytes {
		return "", fmt.Errorf("envelope has %d logical bytes, database reference says %d", info.LogicalBytes, ref.LogicalBytes)
	}
	h := sha256.New()
	if ref.LogicalBytes == 0 {
		plain, err := sealed.ReadAt(ctx, ref.Key, 0, 1)
		if err != nil {
			return "", err
		}
		if len(plain) != 0 {
			return "", errors.New("envelope contains bytes beyond the empty database reference")
		}
		return hex.EncodeToString(h.Sum(nil)), nil
	}
	for offset := int64(0); offset < ref.LogicalBytes; {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		limit := min(int64(readChunkSize), ref.LogicalBytes-offset)
		plain, err := sealed.ReadAt(ctx, ref.Key, offset, limit)
		if err != nil {
			return "", err
		}
		if int64(len(plain)) != limit {
			return "", fmt.Errorf("envelope read returned %d bytes at offset %d, want %d", len(plain), offset, limit)
		}
		if _, err := h.Write(plain); err != nil {
			return "", fmt.Errorf("hash authenticated plaintext: %w", err)
		}
		offset += limit
	}
	if extra, err := sealed.ReadAt(ctx, ref.Key, ref.LogicalBytes, 1); err != nil {
		return "", err
	} else if len(extra) != 0 {
		return "", errors.New("envelope contains bytes beyond the database reference")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func formatObjectIDs(ids []blobinventory.ObjectID) string {
	if len(ids) == 0 {
		return "unknown ids"
	}
	sort.Slice(ids, func(i, j int) bool {
		if ids[i].Kind != ids[j].Kind {
			return ids[i].Kind < ids[j].Kind
		}
		return ids[i].ID < ids[j].ID
	})
	values := make([]string, len(ids))
	for i, id := range ids {
		values[i] = id.Kind + ":" + id.ID
	}
	return strings.Join(values, ",")
}

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

type rangeReader struct {
	ctx    context.Context
	store  blob.Store
	key    string
	size   int64
	offset int64
}

func (r *rangeReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if r.offset >= r.size {
		return 0, io.EOF
	}
	want := min(int64(len(p)), r.size-r.offset)
	data, err := r.store.ReadAt(r.ctx, r.key, r.offset, want)
	if err != nil {
		return 0, fmt.Errorf("read migration source range: %w", err)
	}
	if int64(len(data)) != want {
		return 0, fmt.Errorf("read migration source returned %d bytes at offset %d, want %d", len(data), r.offset, want)
	}
	copy(p, data)
	r.offset += want
	return int(want), nil
}

func (r *rangeReader) Seek(offset int64, whence int) (int64, error) {
	var target int64
	if offset == math.MinInt64 {
		return 0, errors.New("migration source seek overflows")
	}
	switch whence {
	case io.SeekStart:
		target = offset
	case io.SeekCurrent:
		if offset > 0 && r.offset > math.MaxInt64-offset || offset < 0 && r.offset < -offset {
			return 0, errors.New("migration source seek overflows")
		}
		target = r.offset + offset
	case io.SeekEnd:
		if offset > 0 && r.size > math.MaxInt64-offset || offset < 0 && r.size < -offset {
			return 0, errors.New("migration source seek overflows")
		}
		target = r.size + offset
	default:
		return 0, errors.New("migration source seek has invalid whence")
	}
	if target < 0 || target > r.size {
		return 0, errors.New("migration source seek is outside the object")
	}
	r.offset = target
	return target, nil
}

func (r *rangeReader) Size() int64 { return r.size }

func openProgress(path string, expected progressHeader, requireExisting bool) (*progressJournal, error) {
	if path == "" {
		return nil, errors.New("media encryption migration requires a progress journal path")
	}
	parent := filepath.Dir(path)
	parentInfo, err := os.Lstat(parent)
	if err != nil {
		return nil, fmt.Errorf("open progress journal directory: %w", err)
	}
	if !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("progress journal parent must be a real directory")
	}
	flags := unix.O_RDWR | unix.O_CLOEXEC | unix.O_NOFOLLOW
	if !requireExisting {
		flags |= unix.O_CREAT
	}
	fd, err := unix.Open(path, flags, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open progress journal: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		if closeErr := file.Close(); closeErr != nil {
			return nil, errors.Join(fmt.Errorf("inspect progress journal: %w", err), fmt.Errorf("close progress journal: %w", closeErr))
		}
		return nil, fmt.Errorf("inspect progress journal: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0o077 != 0 {
		if closeErr := file.Close(); closeErr != nil {
			return nil, errors.Join(errors.New("progress journal must be a private regular file"), fmt.Errorf("close progress journal: %w", closeErr))
		}
		return nil, errors.New("progress journal must be a private regular file")
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if closeErr := file.Close(); closeErr != nil {
			return nil, errors.Join(fmt.Errorf("lock progress journal: %w", err), fmt.Errorf("close progress journal: %w", closeErr))
		}
		return nil, fmt.Errorf("lock progress journal: %w", err)
	}
	journal := &progressJournal{file: file, header: expected, objects: make(map[string]progressObject)}
	if stat.Size == 0 {
		if requireExisting {
			return closeProgressError(journal, errors.New("progress journal is empty"))
		}
		if err := journal.writeHeader(); err != nil {
			return closeProgressError(journal, err)
		}
		return journal, nil
	}
	if stat.Size > maxProgressBytes {
		return closeProgressError(journal, errors.New("progress journal exceeds the size limit"))
	}
	data, err := io.ReadAll(io.LimitReader(file, maxProgressBytes+1))
	if err != nil {
		return closeProgressError(journal, fmt.Errorf("read progress journal: %w", err))
	}
	if len(data) > maxProgressBytes {
		return closeProgressError(journal, errors.New("progress journal exceeds the size limit"))
	}
	lastNewline := bytes.LastIndexByte(data, '\n')
	if lastNewline != len(data)-1 {
		validBytes := int64(lastNewline + 1)
		if err := file.Truncate(validBytes); err != nil {
			return closeProgressError(journal, fmt.Errorf("truncate incomplete progress record: %w", err))
		}
		if err := file.Sync(); err != nil {
			return closeProgressError(journal, fmt.Errorf("sync truncated progress journal: %w", err))
		}
		data = data[:validBytes]
	}
	if len(data) == 0 {
		if requireExisting {
			return closeProgressError(journal, errors.New("progress journal has no complete header"))
		}
		if err := journal.writeHeader(); err != nil {
			return closeProgressError(journal, err)
		}
		return journal, nil
	}
	lines := bytes.Split(data[:len(data)-1], []byte{'\n'})
	var header progressHeader
	if err := json.Unmarshal(lines[0], &header); err != nil || header.Type != "header" {
		return closeProgressError(journal, errors.New("progress journal header is invalid"))
	}
	if header.Type != expected.Type || header.Schema != expected.Schema || header.Mode != expected.Mode || header.Writes != expected.Writes ||
		header.KeyVersion != expected.KeyVersion ||
		(expected.ReferencesSHA == "" && header.ReferencesSHA != "") ||
		(expected.ReferencesSHA != "" && !validDigest(header.ReferencesSHA)) {
		return closeProgressError(journal, errors.New("progress journal does not match the current write mode or media key"))
	}
	journal.header = header
	for _, line := range lines[1:] {
		var event progressEvent
		if err := json.Unmarshal(line, &event); err != nil || event.Type != "object" {
			return closeProgressError(journal, errors.New("progress journal contains an invalid object record"))
		}
		if err := validateProgressEvent(event); err != nil {
			return closeProgressError(journal, err)
		}
		journal.objects[event.Key] = progressObject{
			Kind: event.Kind, ID: event.ID, LogicalBytes: event.LogicalBytes,
			SourceSHA256: event.SourceSHA256, TargetSalt: event.TargetSalt, State: event.State,
		}
	}
	if _, err := file.Seek(0, io.SeekEnd); err != nil {
		return closeProgressError(journal, fmt.Errorf("seek progress journal: %w", err))
	}
	return journal, nil
}

func closeProgressError(journal *progressJournal, err error) (*progressJournal, error) {
	if closeErr := journal.Close(); closeErr != nil {
		return nil, errors.Join(err, fmt.Errorf("close progress journal: %w", closeErr))
	}
	return nil, err
}

func validateProgressEvent(event progressEvent) error {
	if err := blob.ValidateKey(event.Key); err != nil ||
		(event.Kind != "file" && event.Kind != "part") || event.ID == "" ||
		event.LogicalBytes < 0 {
		return errors.New("progress journal contains an invalid object record")
	}
	if event.TargetSalt != "" {
		if event.SourceSHA256 != "" || !validSaltID(event.TargetSalt) ||
			(event.State != "prepared" && event.State != "verified") {
			return errors.New("progress journal contains an invalid object record")
		}
		return nil
	}
	if !validDigest(event.SourceSHA256) ||
		(event.State != "started" && event.State != "verified" && event.State != "cleaned") {
		return errors.New("progress journal contains an invalid object record")
	}
	return nil
}

func validDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && strings.ToLower(value) == value
}

func validSaltID(value string) bool {
	if len(value) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && hex.EncodeToString(decoded) == value
}

func (j *progressJournal) writeHeader() error {
	data, err := json.Marshal(j.header)
	if err != nil {
		return fmt.Errorf("encode progress journal header: %w", err)
	}
	return j.writeLine(data)
}

func (j *progressJournal) record(ref blobinventory.Reference, sourceDigest, state string) error {
	event := progressEvent{
		Type: "object", Key: ref.Key, Kind: ref.Kind, ID: ref.ID,
		LogicalBytes: ref.LogicalBytes, SourceSHA256: sourceDigest, State: state,
	}
	if err := validateProgressEvent(event); err != nil {
		return err
	}
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode progress event: %w", err)
	}
	if err := j.writeLine(data); err != nil {
		return err
	}
	j.objects[ref.Key] = progressObject{
		Kind: ref.Kind, ID: ref.ID, LogicalBytes: ref.LogicalBytes,
		SourceSHA256: sourceDigest, State: state,
	}
	return nil
}

func (j *progressJournal) recordRotation(ref blobinventory.Reference, targetSalt, state string) error {
	event := progressEvent{
		Type: "object", Key: ref.Key, Kind: ref.Kind, ID: ref.ID,
		LogicalBytes: ref.LogicalBytes, TargetSalt: targetSalt, State: state,
	}
	if err := validateProgressEvent(event); err != nil {
		return err
	}
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode rotation progress event: %w", err)
	}
	if err := j.writeLine(data); err != nil {
		return err
	}
	j.objects[ref.Key] = progressObject{
		Kind: ref.Kind, ID: ref.ID, LogicalBytes: ref.LogicalBytes,
		TargetSalt: targetSalt, State: state,
	}
	return nil
}

func (j *progressJournal) writeLine(data []byte) error {
	if _, err := j.file.Seek(0, io.SeekEnd); err != nil {
		return fmt.Errorf("seek progress journal: %w", err)
	}
	data = append(data, '\n')
	n, err := j.file.Write(data)
	if err != nil {
		return fmt.Errorf("write progress journal: %w", err)
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
	if err := j.file.Sync(); err != nil {
		return fmt.Errorf("sync progress journal: %w", err)
	}
	return nil
}

func (j *progressJournal) digest() (string, error) {
	if err := j.file.Sync(); err != nil {
		return "", fmt.Errorf("sync progress journal before digest: %w", err)
	}
	if _, err := j.file.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("rewind progress journal: %w", err)
	}
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(j.file, maxProgressBytes+1)); err != nil {
		return "", fmt.Errorf("checksum progress journal: %w", err)
	}
	if _, err := j.file.Seek(0, io.SeekEnd); err != nil {
		return "", fmt.Errorf("seek progress journal: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (j *progressJournal) Close() error {
	var closeErr error
	if err := unix.Flock(int(j.file.Fd()), unix.LOCK_UN); err != nil {
		closeErr = fmt.Errorf("unlock progress journal: %w", err)
	}
	if err := j.file.Close(); err != nil {
		closeErr = errors.Join(closeErr, fmt.Errorf("close progress journal file: %w", err))
	}
	return closeErr
}

var _ io.ReadSeeker = (*rangeReader)(nil)
var _ interface{ Size() int64 } = (*rangeReader)(nil)
