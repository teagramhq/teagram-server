//go:build performance

package store

import (
	"context"
	"reflect"
	"testing"

	"github.com/teagramhq/teagram-server/internal/store/db"
)

func TestDialogsPageProfilePreservesFirstAndCursorRows(t *testing.T) {
	ctx := context.Background()
	s := openDialogPinsStore(t)
	owner, err := s.CreateUser(ctx, "+15551369991")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	firstPeer, err := s.CreateUser(ctx, "+15551369992")
	if err != nil {
		t.Fatalf("create first peer: %v", err)
	}
	secondPeer, err := s.CreateUser(ctx, "+15551369993")
	if err != nil {
		t.Fatalf("create second peer: %v", err)
	}
	for _, peer := range []int64{firstPeer.ID, secondPeer.ID} {
		if err := s.q.UpsertDialog(ctx, db.UpsertDialogParams{
			OwnerID: owner.ID, PeerType: int16(PeerTypeUser), PeerID: peer,
			TopMessage: 10,
		}); err != nil {
			t.Fatalf("insert dialog for peer %d: %v", peer, err)
		}
	}
	t.Setenv("TG_PERF_PROFILE_GETDIALOGS", "1")

	for _, params := range []dialogsPageParams{
		{ownerID: owner.ID, limit: 1},
		{ownerID: owner.ID, offsetID: 10, offsetPeerType: PeerTypeUser, offsetPeerID: firstPeer.ID, limit: 1},
	} {
		got, profile, err := s.dialogsPageWithPins(ctx, params)
		if err != nil {
			t.Fatalf("profile dialog page: %v", err)
		}
		if !profile.enabled {
			t.Fatal("profile result was not enabled")
		}
		var want []db.DialogsCursorPageWithPinsRow
		if params.offsetID == 0 && params.offsetPeerType == 0 {
			first, err := s.q.DialogsFirstPageWithPins(ctx, db.DialogsFirstPageWithPinsParams{
				OwnerID: params.ownerID, ExcludePinned: params.excludePinned, Lim: 1,
			})
			if err != nil {
				t.Fatalf("read generated first page: %v", err)
			}
			want = make([]db.DialogsCursorPageWithPinsRow, len(first))
			for i, row := range first {
				want[i] = db.DialogsCursorPageWithPinsRow(row)
			}
		} else {
			want, err = s.q.DialogsCursorPageWithPins(ctx, db.DialogsCursorPageWithPinsParams{
				OwnerID: params.ownerID, OffsetID: params.offsetID,
				OffsetPeerType: int16(params.offsetPeerType), OffsetPeerID: params.offsetPeerID,
				ExcludePinned: params.excludePinned, Lim: 1,
			})
			if err != nil {
				t.Fatalf("read generated cursor page: %v", err)
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("profile page = %+v, generated page = %+v", got, want)
		}
	}
}
