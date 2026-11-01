package blobmigration

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math"

	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/blobinventory"
)

const rotationProgressSchema = "teagram.blob-key-rotation-progress/v1"

// RotationOptions controls a resumable in-place media key rotation.
type RotationOptions struct {
	ProgressPath string
	Rewrite      bool
}

// RotationSummary reports objects processed by the pass. Objects already
// under the target version are authenticated and counted as verified without
// another write.
type RotationSummary struct {
	Mode            string `json:"mode"`
	KeyVersion      uint32 `json:"key_version"`
	Objects         int    `json:"objects"`
	Rotated         int    `json:"rotated"`
	AlreadyVerified int    `json:"already_verified"`
	LogicalBytes    int64  `json:"logical_bytes"`
	ProgressSHA256  string `json:"progress_sha256"`
}

// RotateKeyVersion rewraps every referenced envelope to the ring's current
// version. Rewrite mode also gives each object a fresh DEK. The journal is
// synced before replacement and after authenticated read-back, so an
// interrupted pass can identify completed objects and safely continue.
func RotateKeyVersion(
	ctx context.Context,
	backend blob.Store,
	ring *blob.KeyRing,
	references []blobinventory.Reference,
	options RotationOptions,
) (summary RotationSummary, retErr error) {
	refs, err := normalizedReferences(references)
	if err != nil {
		return summary, err
	}
	if backend == nil || ring == nil || ring.Current() == 0 {
		return summary, errors.New("media key rotation requires a blob store and current media key")
	}
	if options.ProgressPath == "" {
		return summary, errors.New("media key rotation requires a progress journal path")
	}
	mode := "rewrap"
	if options.Rewrite {
		mode = "rewrite"
	}
	tree, ok := backend.(encryptionTree)
	if !ok {
		return summary, errors.New("media key rotation requires full-tree enumeration")
	}
	entries, err := collectMigrationEntries(ctx, tree)
	if err != nil {
		return summary, fmt.Errorf("media key rotation: enumerate blob store: %w", err)
	}
	initial, err := blobinventory.Scan(ctx, backend, ring, refs)
	if err != nil {
		return summary, err
	}
	if initial.Classes.Missing.Count != 0 || initial.Classes.NoValidEnvelope.Count != 0 {
		return summary, fmt.Errorf("media key rotation: referenced inventory is incomplete: %s", initial.Summary)
	}
	journal, err := openProgress(options.ProgressPath, progressHeader{
		Type: "header", Schema: rotationProgressSchema, Mode: mode,
		KeyVersion: ring.Current(),
	}, false)
	if err != nil {
		return summary, err
	}
	defer func() {
		if closeErr := journal.Close(); closeErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close rotation progress journal: %w", closeErr))
		}
	}()

	sealed, err := blob.NewEncrypted(backend, ring, blob.DefaultChunkSize)
	if err != nil {
		return summary, err
	}
	summary.Mode, summary.KeyVersion = mode, ring.Current()
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		entry, exists := entries[ref.Key]
		if !exists || !entry.Regular {
			return summary, fmt.Errorf("media key rotation: referenced object %q is unavailable", ref.ID)
		}
		info, err := blob.InspectEnvelopeHeader(ctx, backend, ring, ref.Key, entry.Size)
		if err != nil {
			return summary, fmt.Errorf("media key rotation: inspect %s %q: %w", ref.Kind, ref.ID, err)
		}
		if info.LogicalBytes != ref.LogicalBytes {
			return summary, fmt.Errorf("media key rotation: %s %q has %d logical bytes, database reference says %d",
				ref.Kind, ref.ID, info.LogicalBytes, ref.LogicalBytes)
		}
		if ref.LogicalBytes > math.MaxInt64-summary.LogicalBytes {
			return summary, errors.New("media key rotation: logical byte total overflows int64")
		}
		summary.LogicalBytes += ref.LogicalBytes
		summary.Objects++
		currentSalt := hex.EncodeToString(info.WrapSalt[:])
		progress, hasProgress := journal.objects[ref.Key]
		sameReference := hasProgress && progress.Kind == ref.Kind && progress.ID == ref.ID && progress.LogicalBytes == ref.LogicalBytes
		wasWritten := info.KeyVersion == ring.Current()
		resumeCompletedRewrite := options.Rewrite && sameReference && progress.TargetSalt != "" &&
			(progress.State == "prepared" || progress.State == "verified") && progress.TargetSalt == currentSalt && wasWritten
		if wasWritten && (!options.Rewrite || resumeCompletedRewrite) {
			if err := verifyRotatedObject(ctx, backend, sealed, ring, ref, entry.Size, currentSalt); err != nil {
				return summary, fmt.Errorf("media key rotation: verify current %s %q: %w", ref.Kind, ref.ID, err)
			}
			if !sameReference || progress.TargetSalt != currentSalt || progress.State != "verified" {
				if err := journal.recordRotation(ref, currentSalt, "verified"); err != nil {
					return summary, err
				}
			}
			summary.AlreadyVerified++
			continue
		}

		// A prepared salt is reused only when its resulting header is already
		// present. Otherwise a fresh salt is journaled before the next attempt,
		// preventing nonce reuse if an earlier write was superseded.
		plan, err := sealed.PrepareRotation(ref.Key, options.Rewrite)
		if err != nil {
			return summary, fmt.Errorf("media key rotation: prepare %s %q: %w", ref.Kind, ref.ID, err)
		}
		if err := journal.recordRotation(ref, plan.SaltID(), "prepared"); err != nil {
			return summary, err
		}
		if err := sealed.Rotate(ctx, plan, entry.Size); err != nil {
			return summary, fmt.Errorf("media key rotation: rotate %s %q: %w", ref.Kind, ref.ID, err)
		}
		if err := verifyRotatedObject(ctx, backend, sealed, ring, ref, entry.Size, plan.SaltID()); err != nil {
			return summary, fmt.Errorf("media key rotation: verify rotated %s %q: %w", ref.Kind, ref.ID, err)
		}
		if err := journal.recordRotation(ref, plan.SaltID(), "verified"); err != nil {
			return summary, err
		}
		summary.Rotated++
	}
	summary.ProgressSHA256, err = journal.digest()
	if err != nil {
		return summary, err
	}
	return summary, nil
}

func verifyRotatedObject(
	ctx context.Context,
	backend blob.Store,
	sealed blob.Store,
	ring *blob.KeyRing,
	ref blobinventory.Reference,
	physicalBytes int64,
	expectedSalt string,
) error {
	info, err := blob.InspectEnvelopeHeader(ctx, backend, ring, ref.Key, physicalBytes)
	if err != nil {
		return err
	}
	if info.KeyVersion != ring.Current() || info.LogicalBytes != ref.LogicalBytes ||
		hex.EncodeToString(info.WrapSalt[:]) != expectedSalt {
		return errors.New("authenticated envelope does not match the verified rotation")
	}
	if ref.LogicalBytes == 0 {
		plain, err := sealed.ReadAt(ctx, ref.Key, 0, 1)
		clear(plain)
		if err != nil {
			return err
		}
		if len(plain) != 0 {
			return errors.New("empty reference has non-empty plaintext")
		}
		return nil
	}
	for offset := int64(0); offset < ref.LogicalBytes; {
		if err := ctx.Err(); err != nil {
			return err
		}
		want := min(int64(readChunkSize), ref.LogicalBytes-offset)
		plain, err := sealed.ReadAt(ctx, ref.Key, offset, want)
		if err != nil {
			return err
		}
		if int64(len(plain)) != want {
			clear(plain)
			return fmt.Errorf("authenticated read returned %d bytes at offset %d, want %d", len(plain), offset, want)
		}
		clear(plain)
		offset += want
	}
	return nil
}
