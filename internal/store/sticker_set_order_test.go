package store_test

import (
	"context"
	"slices"
	"testing"

	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

func openStickerOrderStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), pgtest.DSN(t), pgtest.EncKey(), store.WithoutBlobStore())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return s
}

func installStickerOrderSets(t *testing.T, s *store.Store, userID int64, ids ...int64) {
	t.Helper()
	for _, id := range ids {
		if _, err := s.InstallStickerSet(context.Background(), userID, id); err != nil {
			t.Fatalf("install set %d: %v", id, err)
		}
	}
}

func installedStickerOrder(t *testing.T, s *store.Store, userID int64) []int64 {
	t.Helper()
	installed, err := s.InstalledStickerSets(context.Background(), userID)
	if err != nil {
		t.Fatalf("list installed sets: %v", err)
	}
	ids := make([]int64, 0, len(installed))
	for _, set := range installed {
		ids = append(ids, set.ID)
	}
	return ids
}

func reorderedStickerSetIDs(sets []store.ReorderedStickerSet) []int64 {
	ids := make([]int64, 0, len(sets))
	for _, set := range sets {
		ids = append(ids, set.ID)
	}
	return ids
}

func TestReorderInstalledStickerSetsPlacesListedSetsFirst(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStickerOrderStore(t)
	owner, err := s.CreateUser(ctx, "+15551830001")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	installStickerOrderSets(t, s, owner.ID, -40, -30, -20, -10)
	if got, want := installedStickerOrder(t, s, owner.ID), []int64{-40, -30, -20, -10}; !slices.Equal(got, want) {
		t.Fatalf("default order = %v, want set_id order %v", got, want)
	}

	// Ids the caller has not installed are ignored, and a repeated id is placed once.
	order, err := s.ReorderInstalledStickerSets(ctx, owner.ID, []int64{-10, -99, -20, -10, -40})
	if err != nil {
		t.Fatalf("reorder: %v", err)
	}
	want := []int64{-10, -20, -40, -30}
	if got := reorderedStickerSetIDs(order); !slices.Equal(got, want) {
		t.Fatalf("reorder returned %v, want %v", got, want)
	}
	if got := installedStickerOrder(t, s, owner.ID); !slices.Equal(got, want) {
		t.Fatalf("installed order after reorder = %v, want %v", got, want)
	}
}

func TestReorderInstalledStickerSetsIsPerOwner(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStickerOrderStore(t)
	owner, err := s.CreateUser(ctx, "+15551830002")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	other, err := s.CreateUser(ctx, "+15551830003")
	if err != nil {
		t.Fatalf("create other account: %v", err)
	}
	installStickerOrderSets(t, s, owner.ID, -20, -10)
	installStickerOrderSets(t, s, other.ID, -20, -10)

	if _, err := s.ReorderInstalledStickerSets(ctx, owner.ID, []int64{-10, -20}); err != nil {
		t.Fatalf("reorder owner: %v", err)
	}
	if got, want := installedStickerOrder(t, s, other.ID), []int64{-20, -10}; !slices.Equal(got, want) {
		t.Fatalf("other account order = %v, want unchanged %v", got, want)
	}
}

func TestInstallAndUninstallAfterReorderKeepRelativeOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStickerOrderStore(t)
	owner, err := s.CreateUser(ctx, "+15551830004")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	installStickerOrderSets(t, s, owner.ID, -30, -20, -10)
	if _, err := s.ReorderInstalledStickerSets(ctx, owner.ID, []int64{-10, -30}); err != nil {
		t.Fatalf("reorder: %v", err)
	}

	// A set installed after a reorder has no position yet, so it reads after the ordered sets.
	installStickerOrderSets(t, s, owner.ID, -5)
	if got, want := installedStickerOrder(t, s, owner.ID), []int64{-10, -30, -20, -5}; !slices.Equal(got, want) {
		t.Fatalf("order after install = %v, want %v", got, want)
	}
	if _, err := s.UninstallStickerSet(ctx, owner.ID, -30); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if got, want := installedStickerOrder(t, s, owner.ID), []int64{-10, -20, -5}; !slices.Equal(got, want) {
		t.Fatalf("order after uninstall = %v, want %v", got, want)
	}
}

func TestReorderInstalledStickerSetsWithoutInstalledSetsIsEmpty(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStickerOrderStore(t)
	owner, err := s.CreateUser(ctx, "+15551830005")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	order, err := s.ReorderInstalledStickerSets(ctx, owner.ID, []int64{-10})
	if err != nil {
		t.Fatalf("reorder without installs: %v", err)
	}
	if len(order) != 0 {
		t.Fatalf("reorder without installs returned %v, want empty", order)
	}
}
