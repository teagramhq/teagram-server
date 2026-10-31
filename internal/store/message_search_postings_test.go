package store_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/teagramhq/teagram-server/internal/store"
)

func messageSearchTerms(t *testing.T, s *store.Store, ownerID, localID int64) []string {
	t.Helper()
	rows, err := store.StorePool(s).Query(context.Background(), `
		SELECT lexeme
		FROM message_search_postings
		WHERE owner_id = $1 AND local_id = $2
		ORDER BY lexeme`, ownerID, localID)
	if err != nil {
		t.Fatalf("load search postings for message %d:%d: %v", ownerID, localID, err)
	}
	defer rows.Close()

	var terms []string
	for rows.Next() {
		var term string
		if err := rows.Scan(&term); err != nil {
			t.Fatalf("scan search posting: %v", err)
		}
		terms = append(terms, term)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read search postings: %v", err)
	}
	return terms
}

func assertMessageSearchTerms(t *testing.T, s *store.Store, ownerID, localID int64, want []string) {
	t.Helper()
	if got := messageSearchTerms(t, s, ownerID, localID); !slices.Equal(got, want) {
		t.Fatalf("search postings for message %d:%d = %v, want %v", ownerID, localID, got, want)
	}
}

func TestMessageSearchPostingsTrackMessageLifecycle(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a := mustUser(t, s, "+15551260001")
	b := mustUser(t, s, "+15551260002")

	message := send(t, s, a, b, "amber old", 6001)
	assertMessageSearchTerms(t, s, a.ID, message.LocalID, []string{"amber", "old"})
	assertMessageSearchTerms(t, s, b.ID, message.PeerLocalID, []string{"amber", "old"})

	if _, _, err := s.EditMessage(ctx, a.ID, message.LocalID, "violet new"); err != nil {
		t.Fatalf("edit message: %v", err)
	}
	assertMessageSearchTerms(t, s, a.ID, message.LocalID, []string{"new", "violet"})
	assertMessageSearchTerms(t, s, b.ID, message.PeerLocalID, []string{"new", "violet"})

	if _, err := s.DeleteMessages(ctx, a.ID, []int64{message.LocalID}, false); err != nil {
		t.Fatalf("soft-delete owner copy: %v", err)
	}
	assertMessageSearchTerms(t, s, a.ID, message.LocalID, nil)
	assertMessageSearchTerms(t, s, b.ID, message.PeerLocalID, []string{"new", "violet"})

	if _, err := store.StorePool(s).Exec(ctx,
		`DELETE FROM messages WHERE owner_id = $1 AND local_id = $2`, b.ID, message.PeerLocalID); err != nil {
		t.Fatalf("hard-delete peer copy: %v", err)
	}
	assertMessageSearchTerms(t, s, b.ID, message.PeerLocalID, nil)
}

type searchedMessage struct {
	localID int64
	text    string
}

func legacySearchMessages(t *testing.T, s *store.Store, ownerID int64, peerType store.PeerType, peerID int64, query string, offsetID int64, limit int) []searchedMessage {
	t.Helper()
	rows, err := store.StorePool(s).Query(context.Background(), `
		SELECT local_id, message
		FROM messages
		WHERE owner_id = $1
		  AND peer_type = $2
		  AND peer_id = $3
		  AND deleted = false
		  AND (expires_at IS NULL OR expires_at > statement_timestamp())
		  AND message_tsv @@ plainto_tsquery('simple', $4)
		  AND ($5::bigint = 0 OR local_id < $5::bigint)
		ORDER BY local_id DESC
		LIMIT $6`, ownerID, int16(peerType), peerID, query, offsetID, limit)
	if err != nil {
		t.Fatalf("run legacy message search: %v", err)
	}
	defer rows.Close()

	var found []searchedMessage
	for rows.Next() {
		var message searchedMessage
		if err := rows.Scan(&message.localID, &message.text); err != nil {
			t.Fatalf("scan legacy message search: %v", err)
		}
		found = append(found, message)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read legacy message search: %v", err)
	}
	return found
}

func TestSearchMessagesMatchesLegacyForReadyDialogs(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a := mustUser(t, s, "+15551260011")
	b := mustUser(t, s, "+15551260012")

	first := send(t, s, a, b, "red fox crossing", 6011)
	second := send(t, s, a, b, "red fox sleeping", 6012)
	third := send(t, s, a, b, "red bird crossing", 6013)
	expired := send(t, s, a, b, "red fox expired", 6014)
	deleted := send(t, s, a, b, "red fox deleted", 6015)
	if _, err := store.StorePool(s).Exec(ctx,
		`UPDATE messages SET expires_at = statement_timestamp() - interval '1 second' WHERE owner_id = $1 AND local_id = $2`, a.ID, expired.LocalID); err != nil {
		t.Fatalf("expire message: %v", err)
	}
	if _, err := s.DeleteMessages(ctx, a.ID, []int64{deleted.LocalID}, false); err != nil {
		t.Fatalf("soft-delete message: %v", err)
	}

	for _, tc := range []struct {
		name    string
		query   string
		offset  int
		limit   int
		wantIDs []int64
	}{
		{name: "single word", query: "crossing", limit: 10, wantIDs: []int64{third.LocalID, first.LocalID}},
		{name: "multiple words", query: "red fox", limit: 10, wantIDs: []int64{second.LocalID, first.LocalID}},
		{name: "offset page", query: "red fox", offset: int(second.LocalID), limit: 10, wantIDs: []int64{first.LocalID}},
		{name: "limited page", query: "red fox", limit: 1, wantIDs: []int64{second.LocalID}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.SearchMessages(ctx, a.ID, store.PeerTypeUser, b.ID, tc.query, tc.offset, tc.limit)
			if err != nil {
				t.Fatalf("search messages: %v", err)
			}
			gotRows := make([]searchedMessage, len(got))
			for i, message := range got {
				gotRows[i] = searchedMessage{localID: message.LocalID, text: message.Text}
			}
			wantRows := legacySearchMessages(t, s, a.ID, store.PeerTypeUser, b.ID, tc.query, int64(tc.offset), tc.limit)
			if !slices.Equal(gotRows, wantRows) {
				t.Fatalf("search rows = %+v, legacy rows = %+v", gotRows, wantRows)
			}
			gotIDs := make([]int64, len(got))
			for i, message := range got {
				gotIDs[i] = message.LocalID
			}
			if !slices.Equal(gotIDs, tc.wantIDs) {
				t.Fatalf("search ids = %v, want %v", gotIDs, tc.wantIDs)
			}
		})
	}
}

func TestSearchMessagesFallsBackForNotReadyDialog(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a := mustUser(t, s, "+15551260021")
	b := mustUser(t, s, "+15551260022")
	emptyOwner := mustUser(t, s, "+15551260023")
	emptyPeer := mustUser(t, s, "+15551260024")

	message := send(t, s, a, b, "legacy amber", 6021)
	if _, err := store.StorePool(s).Exec(ctx, `
		DROP TRIGGER messages_search_postings_after_update ON messages;
		DROP TRIGGER messages_search_postings_after_insert ON messages;
		DROP FUNCTION public.sync_message_search_postings();
		DROP TABLE public.message_search_unready_dialogs;
		DROP TABLE public.message_search_postings;`); err != nil {
		t.Fatalf("restore pre-migration search schema: %v", err)
	}
	migration, err := os.ReadFile(filepath.Join("..", "..", "migrations", "20261030000004_message_search_postings.sql"))
	if err != nil {
		t.Fatalf("read message-search migration: %v", err)
	}
	if _, err := store.StorePool(s).Exec(ctx, string(migration)); err != nil {
		t.Fatalf("apply message-search migration to existing messages: %v", err)
	}
	var existingUnready, emptyUnready bool
	if err := store.StorePool(s).QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM message_search_unready_dialogs
			WHERE owner_id = $1 AND peer_type = $2 AND peer_id = $3
		), EXISTS (
			SELECT 1 FROM message_search_unready_dialogs
			WHERE owner_id = $4 AND peer_type = $2 AND peer_id = $5
		)`, a.ID, int16(store.PeerTypeUser), b.ID, emptyOwner.ID, emptyPeer.ID).Scan(&existingUnready, &emptyUnready); err != nil {
		t.Fatalf("check migration readiness markers: %v", err)
	}
	if !existingUnready || emptyUnready {
		t.Fatalf("readiness markers existing/empty = %v/%v, want true/false", existingUnready, emptyUnready)
	}
	var postings int
	if err := store.StorePool(s).QueryRow(ctx, `
		SELECT count(*) FROM message_search_postings
		WHERE owner_id = $1 AND local_id = $2`, a.ID, message.LocalID).Scan(&postings); err != nil {
		t.Fatalf("check migration did not backfill postings: %v", err)
	}
	if postings != 0 {
		t.Fatalf("migration created %d postings for existing message, want none", postings)
	}

	got, err := s.SearchMessages(ctx, a.ID, store.PeerTypeUser, b.ID, "amber", 0, 10)
	if err != nil {
		t.Fatalf("search not-ready dialog: %v", err)
	}
	if len(got) != 1 || got[0].LocalID != message.LocalID || got[0].Text != message.Text {
		t.Fatalf("search not-ready dialog = %+v, want message %d %q", got, message.LocalID, message.Text)
	}
}
