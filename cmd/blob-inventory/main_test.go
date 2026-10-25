package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/blobinventory"
	"github.com/teagramhq/teagram-server/internal/blobmode"
)

func TestWriteReportEmitsMachineReadableJSONAndHumanSummary(t *testing.T) {
	t.Parallel()
	wantSummary := "media inventory: valid envelope=2"
	report := blobinventory.Report{
		Summary: wantSummary,
		Classes: blobinventory.Classes{
			ValidEnvelope: blobinventory.Class{Count: 2, LogicalBytes: 10, PhysicalBytes: 246},
		},
		KeyVersions: []blobinventory.KeyVersion{{
			Version:           1,
			Objects:           2,
			ReferencedObjects: 2,
			Retirable:         false,
		}},
	}
	var stdout, stderr bytes.Buffer
	if err := writeReport(&stdout, &stderr, report); err != nil {
		t.Fatalf("write report: %v", err)
	}
	var decoded blobinventory.Report
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("stdout is not JSON: %v", err)
	}
	if decoded.Classes.ValidEnvelope.Count != 2 || decoded.KeyVersions[0].ReferencedObjects != 2 {
		t.Fatalf("decoded report = %+v, want counts in JSON", decoded)
	}
	if !strings.Contains(stderr.String(), wantSummary) {
		t.Fatalf("stderr = %q, want human summary", stderr.String())
	}
}

func TestInventoryStoreUsesMigratedEncryptedNamespace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backend, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("create backend: %v", err)
	}
	ring, err := blob.NewKeyRing(2, map[uint32][]byte{2: bytes.Repeat([]byte{0x32}, blob.MediaKeyLen)})
	if err != nil {
		t.Fatalf("create key ring: %v", err)
	}
	prefixed, err := blob.NewPrefixedStore(backend, blob.EncryptedObjectsPrefix)
	if err != nil {
		t.Fatalf("create prefixed store: %v", err)
	}
	sealed, err := blob.NewEncrypted(prefixed, ring, blob.DefaultChunkSize)
	if err != nil {
		t.Fatalf("create encrypted store: %v", err)
	}
	if _, err := sealed.Put(ctx, blob.Key(2), bytes.NewReader([]byte("media"))); err != nil {
		t.Fatalf("put encrypted object: %v", err)
	}
	store, err := inventoryStoreWithAuthority(backend, blobmode.Authority{Present: true, Outcome: blobmode.OutcomeEncrypted})
	if err != nil {
		t.Fatalf("select inventory namespace: %v", err)
	}
	report, err := blobinventory.Scan(ctx, store, ring, []blobinventory.Reference{{
		Key: blob.Key(2), Kind: "file", ID: "2", LogicalBytes: 5,
	}})
	if err != nil {
		t.Fatalf("scan migrated reference: %v", err)
	}
	if report.Classes.ValidEnvelope.Count != 1 || report.Classes.NoValidEnvelope.Count != 0 || report.Classes.Missing.Count != 0 {
		t.Fatalf("inventory classes = %+v, want one valid migrated reference", report.Classes)
	}
}
