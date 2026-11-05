package store_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

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

func assertMessageSearchLexemeCount(t *testing.T, s *store.Store, ownerID int64, peerType store.PeerType, peerID int64, lexeme string, want int64) {
	t.Helper()
	var got int64
	if err := store.StorePool(s).QueryRow(context.Background(), `
		SELECT COALESCE((
			SELECT posting_count
			FROM message_search_posting_counts
			WHERE owner_id = $1 AND peer_type = $2 AND peer_id = $3 AND lexeme = $4
		), 0)`, ownerID, int16(peerType), peerID, lexeme).Scan(&got); err != nil {
		t.Fatalf("read message search lexeme count for %q: %v", lexeme, err)
	}
	if got != want {
		t.Fatalf("message search count for %d/%d/%d %q = %d, want %d", ownerID, peerType, peerID, lexeme, got, want)
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
	assertMessageSearchLexemeCount(t, s, a.ID, store.PeerTypeUser, b.ID, "amber", 1)
	assertMessageSearchLexemeCount(t, s, b.ID, store.PeerTypeUser, a.ID, "old", 1)

	if _, _, err := s.EditMessage(ctx, a.ID, message.LocalID, "violet new"); err != nil {
		t.Fatalf("edit message: %v", err)
	}
	assertMessageSearchTerms(t, s, a.ID, message.LocalID, []string{"new", "violet"})
	assertMessageSearchTerms(t, s, b.ID, message.PeerLocalID, []string{"new", "violet"})
	assertMessageSearchLexemeCount(t, s, a.ID, store.PeerTypeUser, b.ID, "amber", 0)
	assertMessageSearchLexemeCount(t, s, b.ID, store.PeerTypeUser, a.ID, "violet", 1)

	if _, err := s.DeleteMessages(ctx, a.ID, []int64{message.LocalID}, false); err != nil {
		t.Fatalf("soft-delete owner copy: %v", err)
	}
	assertMessageSearchTerms(t, s, a.ID, message.LocalID, nil)
	assertMessageSearchTerms(t, s, b.ID, message.PeerLocalID, []string{"new", "violet"})
	assertMessageSearchLexemeCount(t, s, a.ID, store.PeerTypeUser, b.ID, "new", 0)
	assertMessageSearchLexemeCount(t, s, b.ID, store.PeerTypeUser, a.ID, "new", 1)

	if _, err := store.StorePool(s).Exec(ctx,
		`DELETE FROM messages WHERE owner_id = $1 AND local_id = $2`, b.ID, message.PeerLocalID); err != nil {
		t.Fatalf("hard-delete peer copy: %v", err)
	}
	assertMessageSearchTerms(t, s, b.ID, message.PeerLocalID, nil)
	assertMessageSearchLexemeCount(t, s, b.ID, store.PeerTypeUser, a.ID, "new", 0)
}

func TestMessageSearchPostingDialogLocksStayBoundedForBulkWrites(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	const dialogCount = 20_000
	pool := store.StorePool(s)
	for _, sql := range []string{
		`UPDATE server_administration SET election_closed = true WHERE singleton_id = 1`,
		`INSERT INTO users (id, phone, first_name)
		 SELECT n, '+posting-lock-' || n, 'Posting Lock' FROM generate_series(1, 20000) AS n`,
		`INSERT INTO messages (owner_id, local_id, peer_type, peer_id, from_id, message, out)
		 SELECT n, 1, 1, n + 20000, n, 'alpha beta', true
		 FROM generate_series(1, 20000) AS n`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("seed bounded posting-lock fixture: %v", err)
		}
	}

	var postingCount int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM message_search_postings`).Scan(&postingCount); err != nil {
		t.Fatalf("count bulk message postings: %v", err)
	}
	if want := int64(dialogCount * 2); postingCount != want {
		t.Fatalf("bulk message postings = %d, want %d", postingCount, want)
	}
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
	c := mustUser(t, s, "+15551260013")
	var sameIDChat store.Chat
	for id := int64(1); id <= b.ID; id++ {
		chat, err := s.CreateChat(ctx, a.ID, fmt.Sprintf("Search peer collision %d", id), []int64{b.ID})
		if err != nil {
			t.Fatalf("create cross-type search fixture chat: %v", err)
		}
		if chat.ID == b.ID {
			sameIDChat = chat
		}
	}
	if sameIDChat.ID != b.ID {
		t.Fatalf("cross-type fixture chat id = %d, want user peer id %d", sameIDChat.ID, b.ID)
	}

	first := send(t, s, a, b, "red fox crossing", 6011)
	second := send(t, s, a, b, "red fox sleeping", 6012)
	third := send(t, s, a, b, "red bird crossing", 6013)
	expired := send(t, s, a, b, "red fox expired", 6014)
	deleted := send(t, s, a, b, "red fox deleted", 6015)
	send(t, s, a, b, "fox only", 6016)
	send(t, s, a, c, "red fox other dialog", 6017)
	send(t, s, c, b, "red fox other owner", 6018)
	chatMessage, _ := sendChat(t, s, store.FanOut{
		ChatID: sameIDChat.ID, FromID: a.ID, Text: "red fox other peer type", RandomID: 6019,
	})
	if chatMessage.PeerType != store.PeerTypeChat || chatMessage.PeerID != b.ID {
		t.Fatalf("cross-type fixture peer = %d/%d, want %d/%d", chatMessage.PeerType, chatMessage.PeerID, store.PeerTypeChat, b.ID)
	}
	if _, err := store.StorePool(s).Exec(ctx,
		`UPDATE messages SET expires_at = statement_timestamp() - interval '1 second' WHERE owner_id = $1 AND local_id = $2`, a.ID, expired.LocalID); err != nil {
		t.Fatalf("expire message: %v", err)
	}
	if _, err := s.DeleteMessages(ctx, a.ID, []int64{deleted.LocalID}, false); err != nil {
		t.Fatalf("soft-delete message: %v", err)
	}
	assertMessageSearchLexemeCount(t, s, a.ID, store.PeerTypeUser, b.ID, "fox", 4)
	assertMessageSearchLexemeCount(t, s, a.ID, store.PeerTypeUser, b.ID, "red", 4)

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

	if _, err := store.StorePool(s).Exec(ctx, `
		DELETE FROM message_search_postings
		WHERE owner_id = $1 AND peer_type = $2 AND peer_id = $3 AND local_id = $4 AND lexeme = 'fox'`,
		a.ID, int16(store.PeerTypeUser), b.ID, first.LocalID); err != nil {
		t.Fatalf("remove matched message's fox search posting: %v", err)
	}
	got, err := s.SearchMessages(ctx, a.ID, store.PeerTypeUser, b.ID, "red fox", 0, 10)
	if err != nil {
		t.Fatalf("search messages after removing matched postings: %v", err)
	}
	gotIDs := make([]int64, len(got))
	for i, message := range got {
		gotIDs[i] = message.LocalID
	}
	if !slices.Equal(gotIDs, []int64{second.LocalID}) {
		t.Fatalf("search ids after removing matched postings = %v, want [%d]", gotIDs, second.LocalID)
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
	newOwner := mustUser(t, s, "+15551260025")
	newPeer := mustUser(t, s, "+15551260026")

	message := send(t, s, a, b, "legacy amber", 6021)
	if _, err := store.StorePool(s).Exec(ctx, `
		DROP TRIGGER IF EXISTS messages_search_postings_after_delete ON messages;
		DROP TRIGGER IF EXISTS messages_search_postings_after_update ON messages;
		DROP TRIGGER IF EXISTS messages_search_postings_after_insert ON messages;
		DROP TRIGGER IF EXISTS message_search_posting_counts_after_delete ON message_search_postings;
		DROP TRIGGER IF EXISTS message_search_posting_counts_after_insert ON message_search_postings;
		DROP FUNCTION IF EXISTS public.track_message_search_posting_count();
		DROP FUNCTION IF EXISTS public.sync_message_search_postings();
		DROP TABLE IF EXISTS public.message_search_unready_dialogs;
		DROP TABLE IF EXISTS public.message_search_posting_counts;
		DROP TABLE IF EXISTS public.message_search_postings;`); err != nil {
		t.Fatalf("restore pre-migration search schema: %v", err)
	}
	setupMigration, err := os.ReadFile(filepath.Join("..", "..", "migrations", "20261102000001_message_search_postings.sql"))
	if err != nil {
		t.Fatalf("read message-search setup migration: %v", err)
	}
	if _, err := store.StorePool(s).Exec(ctx, string(setupMigration)); err != nil {
		t.Fatalf("apply message-search setup migration to existing messages: %v", err)
	}
	newMessage := send(t, s, newOwner, newPeer, "fresh lavender", 6022)
	readinessMigration, err := os.ReadFile(filepath.Join("..", "..", "migrations", "20261102000002_message_search_postings_readiness.sql"))
	if err != nil {
		t.Fatalf("read message-search readiness migration: %v", err)
	}
	if _, err := store.StorePool(s).Exec(ctx, string(readinessMigration)); err != nil {
		t.Fatalf("apply message-search readiness migration: %v", err)
	}
	var existingUnready, emptyUnready, newUnready bool
	if err := store.StorePool(s).QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM message_search_unready_dialogs
			WHERE owner_id = $1 AND peer_type = $2 AND peer_id = $3
		), EXISTS (
			SELECT 1 FROM message_search_unready_dialogs
			WHERE owner_id = $4 AND peer_type = $2 AND peer_id = $5
		), EXISTS (
			SELECT 1 FROM message_search_unready_dialogs
			WHERE owner_id = $6 AND peer_type = $2 AND peer_id = $7
		)`, a.ID, int16(store.PeerTypeUser), b.ID,
		emptyOwner.ID, emptyPeer.ID, newOwner.ID, newPeer.ID,
	).Scan(&existingUnready, &emptyUnready, &newUnready); err != nil {
		t.Fatalf("check migration readiness markers: %v", err)
	}
	if !existingUnready || emptyUnready || newUnready {
		t.Fatalf("readiness markers existing/empty/new = %v/%v/%v, want true/false/false", existingUnready, emptyUnready, newUnready)
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
	assertMessageSearchTerms(t, s, newOwner.ID, newMessage.LocalID, []string{"fresh", "lavender"})
	assertMessageSearchLexemeCount(t, s, newOwner.ID, store.PeerTypeUser, newPeer.ID, "fresh", 1)

	got, err := s.SearchMessages(ctx, a.ID, store.PeerTypeUser, b.ID, "amber", 0, 10)
	if err != nil {
		t.Fatalf("search not-ready dialog: %v", err)
	}
	if len(got) != 1 || got[0].LocalID != message.LocalID || got[0].Text != message.Text {
		t.Fatalf("search not-ready dialog = %+v, want message %d %q", got, message.LocalID, message.Text)
	}
}

func TestConcurrentDisjointSendsWithReversedPostingBucketCollisions(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()

	const userCount = 100
	users := make([]store.User, userCount)
	userIDs := make([]int64, userCount)
	for i := range users {
		users[i] = mustUser(t, s, fmt.Sprintf("+155513%05d", i))
		userIDs[i] = users[i].ID
	}

	type sendPair struct {
		fromID, toID         int64
		fromBucket, toBucket int
	}
	// Find actual IDs whose bucket pairs reverse to reproduce the lock cycle.
	rows, err := store.StorePool(s).Query(ctx, `
		SELECT sender.id,
		       recipient.id,
		       ((hashtextextended(
		           'message-search-posting-dialog/v1:' || sender.id::text || ':' || $2::smallint::text || ':' || recipient.id::text,
		           73194627
		       ) % 1024 + 1024) % 1024)::int AS sender_bucket,
		       ((hashtextextended(
		           'message-search-posting-dialog/v1:' || recipient.id::text || ':' || $2::smallint::text || ':' || sender.id::text,
		           73194627
		       ) % 1024 + 1024) % 1024)::int AS recipient_bucket
		FROM unnest($1::bigint[]) AS sender(id)
		CROSS JOIN unnest($1::bigint[]) AS recipient(id)
		WHERE sender.id <> recipient.id
	`, userIDs, int16(store.PeerTypeUser))
	if err != nil {
		t.Fatalf("find direct-message posting buckets: %v", err)
	}
	candidatesByBuckets := make(map[[2]int][]sendPair)
	var candidates []sendPair
	for rows.Next() {
		var candidate sendPair
		if err := rows.Scan(&candidate.fromID, &candidate.toID, &candidate.fromBucket, &candidate.toBucket); err != nil {
			rows.Close()
			t.Fatalf("scan direct-message posting buckets: %v", err)
		}
		candidates = append(candidates, candidate)
		key := [2]int{candidate.fromBucket, candidate.toBucket}
		candidatesByBuckets[key] = append(candidatesByBuckets[key], candidate)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatalf("read direct-message posting buckets: %v", err)
	}
	rows.Close()

	var first, second sendPair
	found := false
	for _, candidate := range candidates {
		if candidate.fromBucket == candidate.toBucket {
			continue
		}
		for _, reversed := range candidatesByBuckets[[2]int{candidate.toBucket, candidate.fromBucket}] {
			if candidate.fromID == reversed.fromID || candidate.fromID == reversed.toID ||
				candidate.toID == reversed.fromID || candidate.toID == reversed.toID {
				continue
			}
			first, second, found = candidate, reversed, true
			break
		}
		if found {
			break
		}
	}
	if !found {
		t.Fatalf("no disjoint send pairs with reversed bucket collisions among %d users", len(users))
	}

	if _, err := store.StorePool(s).Exec(ctx, `
		CREATE FUNCTION public.test_pause_search_posting_sender_copy() RETURNS trigger
		LANGUAGE plpgsql AS $function$
		BEGIN
			IF NEW.lexeme IN ('deadlockalpha', 'deadlockbravo') AND EXISTS (
				SELECT 1 FROM public.messages
				WHERE owner_id = NEW.owner_id AND local_id = NEW.local_id AND out
			) THEN
				PERFORM pg_sleep(2);
			END IF;
			RETURN NEW;
		END;
		$function$;
		CREATE TRIGGER zzz_test_pause_search_posting_sender_copy
		AFTER INSERT ON public.message_search_postings
		FOR EACH ROW EXECUTE FUNCTION public.test_pause_search_posting_sender_copy();
	`); err != nil {
		t.Fatalf("install sender-copy pause fixture: %v", err)
	}
	defer func() {
		if _, err := store.StorePool(s).Exec(context.Background(), `
			DROP TRIGGER IF EXISTS zzz_test_pause_search_posting_sender_copy ON public.message_search_postings;
			DROP FUNCTION IF EXISTS public.test_pause_search_posting_sender_copy();
		`); err != nil {
			t.Errorf("remove sender-copy pause fixture: %v", err)
		}
	}()

	type sendResult struct {
		name string
		err  error
	}
	results := make(chan sendResult, 2)
	sendCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	send := func(name string, pair sendPair, text string, randomID int64) {
		_, _, _, _, err := s.SendMessage(sendCtx, pair.fromID, pair.toID, text, randomID, 0, 0)
		results <- sendResult{name: name, err: err}
	}
	go send("first", first, "deadlockalpha", 700001)

	deadline := time.Now().Add(5 * time.Second)
	for {
		var paused bool
		if err := store.StorePool(s).QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity
				WHERE pid <> pg_backend_pid()
				  AND state = 'active'
				  AND wait_event = 'PgSleep'
				  AND query ILIKE '%INSERT INTO messages%'
			)
		`).Scan(&paused); err != nil {
			t.Fatalf("check first send pause: %v", err)
		}
		if paused {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first send did not pause after taking its posting bucket")
		}
		select {
		case result := <-results:
			t.Fatalf("first send returned before its posting pause: %v", result.err)
		case <-time.After(10 * time.Millisecond):
		}
	}
	go send("second", second, "deadlockbravo", 700002)

	deadline = time.Now().Add(5 * time.Second)
	for {
		var activeSends int
		if err := store.StorePool(s).QueryRow(ctx, `
			SELECT count(*)
			FROM pg_stat_activity
			WHERE pid <> pg_backend_pid()
			  AND state = 'active'
			  AND query ILIKE '%INSERT INTO messages%'
			  AND (wait_event = 'PgSleep' OR wait_event_type = 'Lock')
		`).Scan(&activeSends); err != nil {
			t.Fatalf("check overlapping send transactions: %v", err)
		}
		if activeSends >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second send did not overlap the first send's posting lock")
		}
		select {
		case result := <-results:
			t.Fatalf("send %s returned before both sends overlapped: %v", result.name, result.err)
		case <-time.After(10 * time.Millisecond):
		}
	}

	var sendErrors []error
	for range 2 {
		select {
		case result := <-results:
			if result.err != nil {
				sendErrors = append(sendErrors, fmt.Errorf("%s send: %w", result.name, result.err))
			}
		case <-time.After(10 * time.Second):
			t.Fatal("disjoint sends did not complete")
		}
	}
	if len(sendErrors) != 0 {
		t.Fatalf("disjoint sends with reversed posting bucket collisions failed: %v", sendErrors)
	}
}
