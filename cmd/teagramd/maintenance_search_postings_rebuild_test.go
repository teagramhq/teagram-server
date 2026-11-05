package main

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestInitializerRebuildsTriggerWrittenPostings(t *testing.T) {
	dsn := pgtest.DSN(t)
	ctx := context.Background()
	blobs, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("create fixture blob store: %v", err)
	}
	s, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithBlobStore(blobs))
	if err != nil {
		t.Fatalf("open fixture store: %v", err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Errorf("close fixture store: %v", err)
		}
	}()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open fixture database pool: %v", err)
	}
	defer pool.Close()

	owner, err := s.CreateUsernameUser(ctx, "rebuildowner", "Rebuild", "Owner")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	peer, err := s.CreateUsernameUser(ctx, "rebuildpeer", "Rebuild", "Peer")
	if err != nil {
		t.Fatalf("create peer: %v", err)
	}
	first, _, _, _, err := s.SendMessage(ctx, owner.ID, peer.ID, "amber uniquealpha", 30601, 0, 0) //nolint:dogsled // only the stored message is needed here
	if err != nil {
		t.Fatalf("send first message: %v", err)
	}
	second, _, _, _, err := s.SendMessage(ctx, owner.ID, peer.ID, "violet uniquebeta", 30602, 0, 0) //nolint:dogsled // only the stored message is needed here
	if err != nil {
		t.Fatalf("send second message: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO message_search_unready_dialogs (owner_id, peer_type, peer_id)
		VALUES ($1, $2, $3)
	`, owner.ID, int16(store.PeerTypeUser), peer.ID); err != nil {
		t.Fatalf("mark dialog unready without removing trigger-written postings: %v", err)
	}

	for _, localID := range []int64{first.LocalID, second.LocalID} {
		assertMessageSearchPostingMatchesTSVector(t, ctx, pool, owner.ID, store.PeerTypeUser, peer.ID, localID)
	}
	initialized, err := s.InitializeMessageSearchPostings(ctx, 10)
	if err != nil {
		t.Fatalf("initialize dialog with existing postings: %v", err)
	}
	if initialized != 1 {
		t.Fatalf("initialized dialogs = %d, want 1", initialized)
	}

	for _, localID := range []int64{first.LocalID, second.LocalID} {
		assertMessageSearchPostingMatchesTSVector(t, ctx, pool, owner.ID, store.PeerTypeUser, peer.ID, localID)
	}
	for _, tc := range []struct {
		query   string
		localID int64
	}{
		{query: "uniquealpha", localID: first.LocalID},
		{query: "uniquebeta", localID: second.LocalID},
	} {
		got, err := s.SearchMessages(ctx, owner.ID, store.PeerTypeUser, peer.ID, tc.query, 0, 10)
		if err != nil {
			t.Fatalf("search %q after rebuilding existing postings: %v", tc.query, err)
		}
		if len(got) != 1 || got[0].LocalID != tc.localID {
			t.Fatalf("search %q results = %+v, want local_id %d", tc.query, got, tc.localID)
		}
	}
}

func assertMessageSearchPostingMatchesTSVector(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ownerID int64, peerType store.PeerType, peerID, localID int64) {
	t.Helper()
	var expected, actual []string
	err := pool.QueryRow(ctx, `
		SELECT tsvector_to_array(message.message_tsv),
		       ARRAY(
			       SELECT posting.lexeme
			       FROM message_search_postings AS posting
			       WHERE posting.owner_id = message.owner_id
			         AND posting.local_id = message.local_id
			       ORDER BY posting.lexeme
		       )
		FROM messages AS message
		WHERE message.owner_id = $1 AND message.peer_type = $2
		  AND message.peer_id = $3 AND message.local_id = $4
	`, ownerID, int16(peerType), peerID, localID).Scan(&expected, &actual)
	if err != nil {
		t.Fatalf("load expected and actual postings for message %d: %v", localID, err)
	}
	slices.Sort(expected)
	if !slices.Equal(actual, expected) {
		t.Fatalf("postings for message %d = %v, want tsvector terms %v", localID, actual, expected)
	}
}
