package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/blobinventory"
	"github.com/teagramhq/teagram-server/internal/blobmode"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestRequireEncryptedAuthorityRejectsMissingAndPlaintextModes(t *testing.T) {
	t.Parallel()
	for _, authority := range []blobmode.Authority{
		{},
		{Present: true, Outcome: blobmode.OutcomeS3Accepted},
	} {
		if err := requireEncryptedAuthority(authority); err == nil || !strings.Contains(err.Error(), "encrypted blob-mode authority") {
			t.Fatalf("authority %+v error = %v, want encrypted authority requirement", authority, err)
		}
	}
	if err := requireEncryptedAuthority(blobmode.Authority{Present: true, Outcome: blobmode.OutcomeEncrypted}); err != nil {
		t.Fatalf("encrypted authority rejected: %v", err)
	}
}

func TestRequireCurrentTargetRejectsVersionMismatch(t *testing.T) {
	t.Parallel()
	if err := requireCurrentTarget(2, 1); err == nil || !strings.Contains(err.Error(), "must match current media key version 2") {
		t.Fatalf("target mismatch error = %v, want current version requirement", err)
	}
	if err := requireCurrentTarget(2, 2); err != nil {
		t.Fatalf("current target rejected: %v", err)
	}
}

func TestScanFinalInventoryRejectsVersionOnePartCommittedDuringScan(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	local, err := blob.OpenLocal(t.TempDir())
	if err != nil {
		t.Fatalf("open local blob store: %v", err)
	}
	t.Cleanup(func() {
		if err := local.Close(); err != nil {
			t.Errorf("close local blob store: %v", err)
		}
	})

	versionOneKey := bytes.Repeat([]byte{1}, blob.MediaKeyLen)
	versionTwoKey := bytes.Repeat([]byte{2}, blob.MediaKeyLen)
	oldRing, err := blob.NewKeyRing(1, map[uint32][]byte{1: versionOneKey})
	if err != nil {
		t.Fatalf("create version 1 key ring: %v", err)
	}
	currentRing, err := blob.NewKeyRing(2, map[uint32][]byte{1: versionOneKey, 2: versionTwoKey})
	if err != nil {
		t.Fatalf("create current key ring: %v", err)
	}
	oldWriter, err := blob.NewEncrypted(local, oldRing, blob.MinChunkSize)
	if err != nil {
		t.Fatalf("create version 1 writer: %v", err)
	}
	partKey, err := blob.NewPartKey()
	if err != nil {
		t.Fatalf("create part key: %v", err)
	}
	payload := []byte("part committed while final inventory scans")
	database := &changingInventoryReferenceReader{}
	tree := &finalInventoryWalkHookStore{
		Local: local,
		afterWalk: func() error {
			written, err := oldWriter.Put(ctx, partKey, bytes.NewReader(payload))
			if err != nil {
				return err
			}
			if written != int64(len(payload)) {
				return fmt.Errorf("write version 1 part = %d bytes, want %d", written, len(payload))
			}
			database.references = []store.BlobInventoryReference{{
				Key: partKey, Kind: "part", ID: partKey, LogicalBytes: int64(len(payload)),
			}}
			return nil
		},
	}

	_, err = scanFinalInventory(ctx, database, tree, currentRing)
	if err == nil || !strings.Contains(err.Error(), "references changed during final inventory") {
		t.Fatalf("final inventory error = %v, want fail-closed reference drift error", err)
	}
	if database.reads != 2 || len(database.references) != 1 {
		t.Fatalf("reference reads = %d, final references = %d, want before/after reads around committed part", database.reads, len(database.references))
	}

	var partInfo blob.EnvelopeHeaderInfo
	foundPart := false
	if err := local.Walk(ctx, func(entry blob.Entry) error {
		if entry.Key != partKey {
			return nil
		}
		foundPart = true
		var inspectErr error
		partInfo, inspectErr = blob.InspectEnvelopeHeader(ctx, local, currentRing, partKey, entry.Size)
		return inspectErr
	}); err != nil {
		t.Fatalf("inspect concurrently committed part: %v", err)
	}
	if !foundPart || !partInfo.HasKeyVersion || partInfo.KeyVersion != 1 {
		t.Fatalf("concurrent part header = %+v (found=%t), want an authenticated version 1 part", partInfo, foundPart)
	}
}

type changingInventoryReferenceReader struct {
	references []store.BlobInventoryReference
	reads      int
}

func (r *changingInventoryReferenceReader) BlobInventoryReferences(ctx context.Context) ([]store.BlobInventoryReference, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.reads++
	return append([]store.BlobInventoryReference(nil), r.references...), nil
}

type finalInventoryWalkHookStore struct {
	*blob.Local

	afterWalk func() error
}

func (s *finalInventoryWalkHookStore) Walk(ctx context.Context, fn func(blob.Entry) error) error {
	if err := s.Local.Walk(ctx, fn); err != nil {
		return err
	}
	if s.afterWalk != nil {
		afterWalk := s.afterWalk
		s.afterWalk = nil
		return afterWalk()
	}
	return nil
}

func TestCommandResultEncodesRotationAndInventoryForOperators(t *testing.T) {
	t.Parallel()
	result := commandResult{
		Mode: "rewrap", KeyVersion: 2, Objects: 4, Rotated: 4, LogicalBytes: 91,
		Inventory: blobinventory.Report{Summary: "key versions 1=retirable, 2=not retirable"},
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("encode command result: %v", err)
	}
	var got struct {
		Mode       string               `json:"mode"`
		KeyVersion uint32               `json:"key_version"`
		Objects    int                  `json:"objects"`
		Inventory  blobinventory.Report `json:"inventory"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode command result: %v", err)
	}
	if got.Mode != "rewrap" || got.KeyVersion != 2 || got.Objects != 4 || got.Inventory.Summary == "" {
		t.Fatalf("encoded result = %+v, want rotation counts and inventory summary", got)
	}
}
