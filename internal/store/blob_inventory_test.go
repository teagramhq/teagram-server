package store_test

import (
	"context"
	"reflect"
	"strconv"
	"testing"

	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestBlobInventoryReferencesIncludesOnlyStoredFilesAndLivePartKeys(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	u, err := s.CreateUser(ctx, "+155590002426")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	stored, err := s.AllocateFile(ctx, u.ID, 6, "text/plain", "stored.txt", 1<<20)
	if err != nil {
		t.Fatalf("allocate stored file: %v", err)
	}
	if err := s.MarkFileStored(ctx, stored.ID); err != nil {
		t.Fatalf("mark file stored: %v", err)
	}
	pending, err := s.AllocateFile(ctx, u.ID, 7, "text/plain", "pending.txt", 1<<20)
	if err != nil {
		t.Fatalf("allocate pending file: %v", err)
	}
	partKey := "parts/01/0123456789abcdef0123456789abcd"
	if err := store.InsertUploadPartWithKey(ctx, s, u.ID, 17, 0, 4, partKey); err != nil {
		t.Fatalf("insert live part: %v", err)
	}
	if err := store.InsertUploadPartWithoutKey(ctx, s, u.ID, 18, 0, 5); err != nil {
		t.Fatalf("insert part without key: %v", err)
	}

	got, err := s.BlobInventoryReferences(ctx)
	if err != nil {
		t.Fatalf("list inventory references: %v", err)
	}
	want := []store.BlobInventoryReference{
		{Key: blob.Key(stored.ID), Kind: "file", ID: strconv.FormatInt(stored.ID, 10), LogicalBytes: 6},
		{Key: partKey, Kind: "part", ID: partKey, LogicalBytes: 4},
	}
	if len(got) != len(want) {
		t.Fatalf("references = %+v, want only stored file %d and live part", got, stored.ID)
	}
	byKey := make(map[string]store.BlobInventoryReference, len(got))
	for _, ref := range got {
		byKey[ref.Key] = ref
	}
	if byKey[blob.Key(stored.ID)].ID != want[0].ID || byKey[blob.Key(stored.ID)].LogicalBytes != want[0].LogicalBytes {
		t.Fatalf("stored file reference = %+v, want %+v", byKey[blob.Key(stored.ID)], want[0])
	}
	if byKey[partKey].Kind != want[1].Kind || byKey[partKey].ID != want[1].ID || byKey[partKey].LogicalBytes != want[1].LogicalBytes {
		t.Fatalf("part reference = %+v, want %+v", byKey[partKey], want[1])
	}
	if _, exists := byKey[blob.Key(pending.ID)]; exists {
		t.Fatalf("unstored file %d was counted as an object reference", pending.ID)
	}
	if _, exists := byKey[""]; exists {
		t.Fatal("part row with no blob key was counted as an object reference")
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("references = %+v, want ordered values %+v", got, want)
	}
}
