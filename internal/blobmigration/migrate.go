// Package blobmigration copies media between the local and S3-backed stores
// and verifies every transferred object before a backend switch.
package blobmigration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"sort"
	"strings"

	"github.com/teagramhq/teagram-server/internal/blob"
)

const readChunkSize = 64 << 10

type tree interface {
	Walk(context.Context, func(blob.Entry) error) error
}

type fullTreeStore interface {
	blob.Store
	tree
}

// Summary describes the copied and verified source namespace.
type Summary struct {
	Objects                   int    `json:"objects"`
	Bytes                     int64  `json:"bytes"`
	SourceManifestSHA256      string `json:"source_manifest_sha256"`
	DestinationManifestSHA256 string `json:"destination_manifest_sha256"`
}

type objectReport struct {
	Type              string `json:"type"`
	Key               string `json:"key"`
	Bytes             int64  `json:"bytes"`
	SourceSHA256      string `json:"source_sha256"`
	DestinationSHA256 string `json:"destination_sha256"`
}

type summaryReport struct {
	Summary

	Type string `json:"type"`
}

// Migrate streams each regular source object to destination, checks its bytes
// against a second read from the destination, and verifies the complete final
// key set. The source is never modified. Existing destination objects are
// overwritten only when their key also exists in the source, making an
// interrupted copy safe to rerun.
func Migrate(ctx context.Context, source *blob.Local, destination blob.Store, report io.Writer) (Summary, error) {
	if source == nil || destination == nil || report == nil {
		return Summary{}, errors.New("blob migration requires source, destination, and report writer")
	}
	destinationTree, ok := destination.(tree)
	if !ok {
		return Summary{}, errors.New("blob migration destination does not support full-tree enumeration")
	}
	return copyBlobs(ctx, source, destination, destinationTree, report, true)
}

// Restore copies every object from source into the retained local destination.
// Destination-only keys are preserved: they may be older media that was
// removed from S3 after cutover, while keys present in both stores are replaced
// only with checksum-verified source bytes.
func Restore(ctx context.Context, source fullTreeStore, destination blob.Store, report io.Writer) (Summary, error) {
	if source == nil || destination == nil || report == nil {
		return Summary{}, errors.New("blob restore requires source, destination, and report writer")
	}
	destinationTree, ok := destination.(tree)
	if !ok {
		return Summary{}, errors.New("blob restore destination does not support full-tree enumeration")
	}
	return copyBlobs(ctx, source, destination, destinationTree, report, false)
}

func copyBlobs(ctx context.Context, source fullTreeStore, destination blob.Store, destinationTree tree, report io.Writer, exactDestination bool) (Summary, error) {
	entries, sourceEntries, err := collectEntries(ctx, source, "walk source blob store")
	if err != nil {
		return Summary{}, err
	}
	if exactDestination {
		if err := verifyDestinationKeys(ctx, destinationTree, sourceEntries, false); err != nil {
			return Summary{}, err
		}
	}

	sourceManifest := sha256.New()
	destinationManifest := sha256.New()
	encoder := json.NewEncoder(report)
	var summary Summary
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return Summary{}, err
		}
		body, err := migrationReader(ctx, source, entry)
		if err != nil {
			return Summary{}, err
		}
		sourceDigest, err := digestStore(ctx, source, entry)
		if err != nil {
			return Summary{}, err
		}
		written, err := destination.Put(ctx, entry.Key, body)
		if err != nil {
			return Summary{}, fmt.Errorf("copy blob %q to destination: %w", entry.Key, err)
		}
		if written != entry.Size {
			return Summary{}, fmt.Errorf("copy blob %q wrote %d bytes, source has %d", entry.Key, written, entry.Size)
		}
		destinationDigest, err := digestRemote(ctx, destination, entry.Key, entry.Size)
		if err != nil {
			return Summary{}, err
		}
		if sourceDigest != destinationDigest {
			return Summary{}, fmt.Errorf("blob %q checksum mismatch: source %s destination %s", entry.Key, sourceDigest, destinationDigest)
		}
		if err := writeManifestLeaf(sourceManifest, entry.Key, entry.Size, sourceDigest); err != nil {
			return Summary{}, fmt.Errorf("checksum source manifest: %w", err)
		}
		if err := writeManifestLeaf(destinationManifest, entry.Key, entry.Size, destinationDigest); err != nil {
			return Summary{}, fmt.Errorf("checksum destination manifest: %w", err)
		}
		if entry.Size > math.MaxInt64-summary.Bytes {
			return Summary{}, errors.New("blob migration byte total overflows int64")
		}
		summary.Objects++
		summary.Bytes += entry.Size
		if err := encoder.Encode(objectReport{
			Type: "object", Key: entry.Key, Bytes: entry.Size,
			SourceSHA256: sourceDigest, DestinationSHA256: destinationDigest,
		}); err != nil {
			return Summary{}, fmt.Errorf("write migration object report: %w", err)
		}
	}
	if exactDestination {
		if err := verifyDestinationKeys(ctx, destinationTree, sourceEntries, true); err != nil {
			return Summary{}, err
		}
	} else if err := verifyDestinationIncludes(ctx, destinationTree, sourceEntries); err != nil {
		return Summary{}, err
	}
	summary.SourceManifestSHA256 = hex.EncodeToString(sourceManifest.Sum(nil))
	summary.DestinationManifestSHA256 = hex.EncodeToString(destinationManifest.Sum(nil))
	if summary.SourceManifestSHA256 != summary.DestinationManifestSHA256 {
		return Summary{}, errors.New("blob migration manifest checksums do not match")
	}
	if err := encoder.Encode(summaryReport{Type: "summary", Summary: summary}); err != nil {
		return Summary{}, fmt.Errorf("write migration summary: %w", err)
	}
	return summary, nil
}

func collectEntries(ctx context.Context, source tree, operation string) ([]blob.Entry, map[string]blob.Entry, error) {
	entries := make([]blob.Entry, 0)
	if err := source.Walk(ctx, func(entry blob.Entry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Dir {
			return nil
		}
		if !entry.Regular || entry.Size < 0 {
			return fmt.Errorf("source contains a non-regular blob entry %q", entry.Key)
		}
		if err := blob.ValidateKey(entry.Key); err != nil {
			return fmt.Errorf("source blob key %q is invalid: %w", entry.Key, err)
		}
		entries = append(entries, entry)
		return nil
	}); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", operation, err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
	sourceEntries := make(map[string]blob.Entry, len(entries))
	for _, entry := range entries {
		if _, duplicate := sourceEntries[entry.Key]; duplicate {
			return nil, nil, fmt.Errorf("source listed blob %q more than once", entry.Key)
		}
		sourceEntries[entry.Key] = entry
	}
	return entries, sourceEntries, nil
}

func migrationReader(ctx context.Context, source blob.Store, entry blob.Entry) (io.ReadSeeker, error) {
	reader := &storeReader{ctx: ctx, source: source, key: entry.Key, size: entry.Size}
	if !strings.HasPrefix(entry.Key, blob.PartsPrefix) {
		return reader, nil
	}
	if entry.Size > blob.MaxPartBytes {
		return nil, fmt.Errorf("source part blob %q has %d bytes, exceeds maximum %d", entry.Key, entry.Size, blob.MaxPartBytes)
	}
	payload, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("read source part blob %q: %w", entry.Key, err)
	}
	if int64(len(payload)) != entry.Size {
		return nil, fmt.Errorf("source part blob %q changed size while migrating: read %d bytes, expected %d", entry.Key, len(payload), entry.Size)
	}
	return bytes.NewReader(payload), nil
}

func verifyDestinationKeys(ctx context.Context, destination tree, source map[string]blob.Entry, requireComplete bool) error {
	seen := make(map[string]struct{}, len(source))
	if err := destination.Walk(ctx, func(entry blob.Entry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Dir {
			return nil
		}
		if !entry.Regular {
			return fmt.Errorf("destination contains a non-regular blob entry %q", entry.Key)
		}
		expected, ok := source[entry.Key]
		if !ok {
			return fmt.Errorf("destination contains unexpected blob %q", entry.Key)
		}
		if entry.Size != expected.Size {
			return fmt.Errorf("destination blob %q has %d bytes, source has %d", entry.Key, entry.Size, expected.Size)
		}
		if _, duplicate := seen[entry.Key]; duplicate {
			return fmt.Errorf("destination listed blob %q more than once", entry.Key)
		}
		seen[entry.Key] = struct{}{}
		return nil
	}); err != nil {
		return fmt.Errorf("verify destination blob key set: %w", err)
	}
	if requireComplete && len(seen) != len(source) {
		return fmt.Errorf("destination has %d of %d source blobs", len(seen), len(source))
	}
	return nil
}

func verifyDestinationIncludes(ctx context.Context, destination tree, source map[string]blob.Entry) error {
	seen := make(map[string]struct{}, len(source))
	if err := destination.Walk(ctx, func(entry blob.Entry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Dir {
			return nil
		}
		if !entry.Regular || entry.Size < 0 {
			return fmt.Errorf("destination contains a non-regular blob entry %q", entry.Key)
		}
		if err := blob.ValidateKey(entry.Key); err != nil {
			return fmt.Errorf("destination blob key %q is invalid: %w", entry.Key, err)
		}
		expected, ok := source[entry.Key]
		if !ok {
			return nil
		}
		if entry.Size != expected.Size {
			return fmt.Errorf("destination blob %q has %d bytes, source has %d", entry.Key, entry.Size, expected.Size)
		}
		if _, duplicate := seen[entry.Key]; duplicate {
			return fmt.Errorf("destination listed blob %q more than once", entry.Key)
		}
		seen[entry.Key] = struct{}{}
		return nil
	}); err != nil {
		return fmt.Errorf("verify restored blob key set: %w", err)
	}
	if len(seen) != len(source) {
		return fmt.Errorf("destination has %d of %d source blobs", len(seen), len(source))
	}
	return nil
}

func digestStore(ctx context.Context, source blob.Store, entry blob.Entry) (string, error) {
	h := sha256.New()
	n, err := io.CopyBuffer(h, &storeReader{ctx: ctx, source: source, key: entry.Key, size: entry.Size}, make([]byte, readChunkSize))
	if err != nil {
		return "", fmt.Errorf("checksum source blob %q: %w", entry.Key, err)
	}
	if n != entry.Size {
		return "", fmt.Errorf("source blob %q changed size while migrating: read %d bytes, expected %d", entry.Key, n, entry.Size)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func digestRemote(ctx context.Context, destination blob.Store, key string, size int64) (string, error) {
	h := sha256.New()
	for offset := int64(0); offset < size; {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		limit := int64(readChunkSize)
		if remaining := size - offset; remaining < limit {
			limit = remaining
		}
		chunk, err := destination.ReadAt(ctx, key, offset, limit)
		if err != nil {
			return "", fmt.Errorf("verify destination blob %q at offset %d: %w", key, offset, err)
		}
		if int64(len(chunk)) != limit {
			return "", fmt.Errorf("verify destination blob %q at offset %d: read %d bytes, expected %d", key, offset, len(chunk), limit)
		}
		if _, err := h.Write(chunk); err != nil {
			return "", fmt.Errorf("checksum destination blob %q: %w", key, err)
		}
		offset += limit
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func writeManifestLeaf(h hash.Hash, key string, size int64, checksum string) error {
	if _, err := fmt.Fprintf(h, "%s\x00%d\x00%s\n", key, size, checksum); err != nil {
		return err
	}
	return nil
}

type storeReader struct {
	ctx    context.Context
	source blob.Store
	key    string
	size   int64
	offset int64
}

func (r *storeReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.offset >= r.size {
		return 0, io.EOF
	}
	limit := int64(len(p))
	if remaining := r.size - r.offset; remaining < limit {
		limit = remaining
	}
	chunk, err := r.source.ReadAt(r.ctx, r.key, r.offset, limit)
	if err != nil {
		return 0, err
	}
	if len(chunk) == 0 {
		return 0, io.ErrUnexpectedEOF
	}
	if int64(len(chunk)) > limit {
		return 0, fmt.Errorf("blob %q read %d bytes for a %d byte range", r.key, len(chunk), limit)
	}
	n := copy(p, chunk)
	r.offset += int64(n)
	return n, nil
}

func (r *storeReader) Seek(offset int64, whence int) (int64, error) {
	var base int64
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		base = r.offset
	case io.SeekEnd:
		base = r.size
	default:
		return 0, errors.New("invalid seek origin")
	}
	if (offset > 0 && base > math.MaxInt64-offset) || (offset < 0 && base < math.MinInt64-offset) {
		return 0, errors.New("seek offset overflows")
	}
	next := base + offset
	if next < 0 {
		return 0, errors.New("negative seek offset")
	}
	r.offset = next
	return next, nil
}

var _ io.ReadSeeker = (*storeReader)(nil)
