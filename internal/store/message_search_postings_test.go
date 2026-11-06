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

func TestUnreadyDisjointSendsWithReversedPostingBucketCollisions(t *testing.T) {
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
		INSERT INTO message_search_unready_dialogs (owner_id, peer_type, peer_id)
		VALUES ($1, $5, $2), ($2, $5, $1), ($3, $5, $4), ($4, $5, $3)
	`, first.fromID, first.toID, second.fromID, second.toID, int16(store.PeerTypeUser)); err != nil {
		t.Fatalf("mark direct-send dialogs unready: %v", err)
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
				  AND datname = current_database()
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
			  AND datname = current_database()
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

func TestReadyDisjointChatFanoutsOverlapWithReversedPostingBucketCollisions(t *testing.T) {
	testDisjointChatFanoutsWithReversedPostingBucketCollisions(t, false)
}

func TestUnreadyDisjointChatFanoutsWithReversedPostingBucketCollisions(t *testing.T) {
	testDisjointChatFanoutsWithReversedPostingBucketCollisions(t, true)
}

func testDisjointChatFanoutsWithReversedPostingBucketCollisions(t *testing.T, unready bool) {
	t.Helper()
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	pool := store.StorePool(s)

	if _, err := pool.Exec(ctx, `
		UPDATE server_administration SET election_closed = true WHERE singleton_id = 1;
		INSERT INTO users (id, phone, first_name)
		SELECT n, '+group-posting-' || n, 'Group Posting'
		FROM generate_series(1, 2002) AS n;
	`); err != nil {
		t.Fatalf("seed group posting fixture: %v", err)
	}
	chatA, err := s.CreateChat(ctx, 2001, "posting buckets A", nil)
	if err != nil {
		t.Fatalf("create first posting chat: %v", err)
	}
	chatB, err := s.CreateChat(ctx, 2002, "posting buckets B", nil)
	if err != nil {
		t.Fatalf("create second posting chat: %v", err)
	}

	// Find two disjoint two-member prefixes whose message-copy bucket order is
	// reversed. Each chat creator has a larger id and is inserted after these
	// copies, so both chat sends and forwards exercise the same collision.
	var ownerAFirst, ownerALater, ownerBFirst, ownerBLater int64
	if err := pool.QueryRow(ctx, `
		WITH a_first AS (
			SELECT n AS owner_id,
			       ((hashtextextended('message-search-posting-dialog/v1:' || n::text || ':2:' || $1::bigint::text, 73194627) % 1024 + 1024) % 1024)::int AS bucket
			FROM generate_series(1, 500) AS n
		), a_later AS (
			SELECT n AS owner_id,
			       ((hashtextextended('message-search-posting-dialog/v1:' || n::text || ':2:' || $1::bigint::text, 73194627) % 1024 + 1024) % 1024)::int AS bucket
			FROM generate_series(501, 1000) AS n
		), b_first AS (
			SELECT n AS owner_id,
			       ((hashtextextended('message-search-posting-dialog/v1:' || n::text || ':2:' || $2::bigint::text, 73194627) % 1024 + 1024) % 1024)::int AS bucket
			FROM generate_series(1001, 1500) AS n
		), b_later AS (
			SELECT n AS owner_id,
			       ((hashtextextended('message-search-posting-dialog/v1:' || n::text || ':2:' || $2::bigint::text, 73194627) % 1024 + 1024) % 1024)::int AS bucket
			FROM generate_series(1501, 2000) AS n
		)
		SELECT a_first.owner_id, a_later.owner_id, b_first.owner_id, b_later.owner_id
		FROM a_first
		JOIN b_later ON b_later.bucket = a_first.bucket
		JOIN a_later ON a_later.bucket <> a_first.bucket
		JOIN b_first ON b_first.bucket = a_later.bucket
		LIMIT 1
	`, chatA.ID, chatB.ID).Scan(&ownerAFirst, &ownerALater, &ownerBFirst, &ownerBLater); err != nil {
		t.Fatalf("find reversed group posting buckets: %v", err)
	}
	for _, participant := range []struct {
		chatID, ownerID, inviterID int64
	}{
		{chatA.ID, ownerAFirst, 2001},
		{chatA.ID, ownerALater, 2001},
		{chatB.ID, ownerBFirst, 2002},
		{chatB.ID, ownerBLater, 2002},
	} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO chat_participants (chat_id, user_id, inviter_id)
			VALUES ($1, $2, $3)
		`, participant.chatID, participant.ownerID, participant.inviterID); err != nil {
			t.Fatalf("add group posting participant %d: %v", participant.ownerID, err)
		}
	}
	if unready {
		if _, err := pool.Exec(ctx, `
			INSERT INTO message_search_unready_dialogs (owner_id, peer_type, peer_id)
			VALUES ($1, $5, $3), ($2, $5, $3), ($4, $5, $6), ($7, $5, $6)
		`, ownerAFirst, ownerALater, chatA.ID, ownerBFirst, int16(store.PeerTypeChat), chatB.ID, ownerBLater); err != nil {
			t.Fatalf("mark chat dialogs unready: %v", err)
		}
	}
	var unreadyDialogs int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		FROM message_search_unready_dialogs
		WHERE peer_type = $1 AND peer_id = ANY($2::bigint[])
	`, int16(store.PeerTypeChat), []int64{chatA.ID, chatB.ID}).Scan(&unreadyDialogs); err != nil {
		t.Fatalf("check chat search readiness: %v", err)
	}
	wantUnready := 0
	if unready {
		wantUnready = 4
	}
	if unreadyDialogs != wantUnready {
		t.Fatalf("test chats have %d unready dialogs, want %d", unreadyDialogs, wantUnready)
	}

	sourceA, _, _, _, err := s.SendMessage(ctx, ownerAFirst, ownerALater, "groupforwardalpha", 720001, 0, 0) //nolint:dogsled // only the stored source is needed for forwarding.
	if err != nil {
		t.Fatalf("send first forward source: %v", err)
	}
	sourceB, _, _, _, err := s.SendMessage(ctx, ownerBFirst, ownerBLater, "groupforwardbravo", 720002, 0, 0) //nolint:dogsled // only the stored source is needed for forwarding.
	if err != nil {
		t.Fatalf("send second forward source: %v", err)
	}

	if _, err := pool.Exec(ctx, `
		CREATE FUNCTION public.test_pause_group_search_posting_sender_copy() RETURNS trigger
		LANGUAGE plpgsql AS $function$
		BEGIN
			IF NEW.lexeme IN ('groupfanoutalpha', 'groupfanoutbravo', 'groupforwardalpha', 'groupforwardbravo') AND EXISTS (
				SELECT 1 FROM public.messages
				WHERE owner_id = NEW.owner_id AND local_id = NEW.local_id AND out
			) THEN
				PERFORM pg_sleep(2);
			END IF;
			RETURN NEW;
		END;
		$function$;
		CREATE TRIGGER zzz_test_pause_group_search_posting_sender_copy
		AFTER INSERT ON public.message_search_postings
		FOR EACH ROW EXECUTE FUNCTION public.test_pause_group_search_posting_sender_copy();
	`); err != nil {
		t.Fatalf("install group sender-copy pause fixture: %v", err)
	}
	defer func() {
		if _, err := pool.Exec(context.Background(), `
			DROP TRIGGER IF EXISTS zzz_test_pause_group_search_posting_sender_copy ON public.message_search_postings;
			DROP FUNCTION IF EXISTS public.test_pause_group_search_posting_sender_copy();
		`); err != nil {
			t.Errorf("remove group sender-copy pause fixture: %v", err)
		}
	}()

	type fanoutResult struct {
		name string
		err  error
	}
	runConcurrentFanouts := func(name string, first, second func(context.Context) error) {
		t.Helper()
		results := make(chan fanoutResult, 2)
		fanoutCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
		defer cancel()
		go func() { results <- fanoutResult{name: "first", err: first(fanoutCtx)} }()

		deadline := time.Now().Add(5 * time.Second)
		for {
			var paused bool
			if err := pool.QueryRow(ctx, `
				SELECT EXISTS (
					SELECT 1 FROM pg_stat_activity
					WHERE pid <> pg_backend_pid()
					  AND datname = current_database()
					  AND state = 'active'
					  AND wait_event = 'PgSleep'
					  AND query ILIKE '%INSERT INTO messages%'
				)
			`).Scan(&paused); err != nil {
				t.Fatalf("check %s first fanout pause: %v", name, err)
			}
			if paused {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s first fanout did not pause after taking its posting bucket", name)
			}
			select {
			case result := <-results:
				t.Fatalf("%s first fanout returned before its posting pause: %v", name, result.err)
			case <-time.After(10 * time.Millisecond):
			}
		}
		go func() { results <- fanoutResult{name: "second", err: second(fanoutCtx)} }()

		deadline = time.Now().Add(5 * time.Second)
		for {
			var sleepingFanouts, waitingFanouts int
			if err := pool.QueryRow(ctx, `
				SELECT count(*) FILTER (WHERE wait_event = 'PgSleep'),
				       count(*) FILTER (WHERE wait_event_type = 'Lock')
				FROM pg_stat_activity
				WHERE pid <> pg_backend_pid()
				  AND datname = current_database()
				  AND state = 'active'
				  AND (query ILIKE '%INSERT INTO messages%' OR query ILIKE '%lock_message_search_posting_dialogs%')
			`).Scan(&sleepingFanouts, &waitingFanouts); err != nil {
				t.Fatalf("check %s overlapping fanouts: %v", name, err)
			}
			if (!unready && sleepingFanouts >= 2) || (unready && sleepingFanouts >= 1 && waitingFanouts >= 1) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s fanouts did not overlap as expected: unready=%t sleeping=%d waiting=%d", name, unready, sleepingFanouts, waitingFanouts)
			}
			select {
			case result := <-results:
				t.Fatalf("%s fanout %s returned before both fanouts overlapped: %v", name, result.name, result.err)
			case <-time.After(10 * time.Millisecond):
			}
		}

		var failures []error
		for range 2 {
			select {
			case result := <-results:
				if result.err != nil {
					failures = append(failures, fmt.Errorf("%s: %w", result.name, result.err))
				}
			case <-time.After(12 * time.Second):
				t.Fatalf("%s disjoint fanouts did not complete", name)
			}
		}
		if len(failures) != 0 {
			t.Fatalf("%s disjoint fanouts with reversed posting bucket collisions failed: %v", name, failures)
		}
	}

	runConcurrentFanouts("chat sends", func(ctx context.Context) error {
		_, _, _, err := s.SendChatMessage(ctx, store.FanOut{ChatID: chatA.ID, FromID: ownerAFirst, Text: "groupfanoutalpha", RandomID: 720011})
		return err
	}, func(ctx context.Context) error {
		_, _, _, err := s.SendChatMessage(ctx, store.FanOut{ChatID: chatB.ID, FromID: ownerBFirst, Text: "groupfanoutbravo", RandomID: 720012})
		return err
	})

	runConcurrentFanouts("chat forwards", func(ctx context.Context) error {
		_, _, err := s.ForwardMessages(ctx, ownerAFirst, store.PeerTypeChat, chatA.ID, []store.ForwardSource{{FromID: sourceA.FromID, Date: sourceA.Date, Text: sourceA.Text}}, []int64{720013})
		return err
	}, func(ctx context.Context) error {
		_, _, err := s.ForwardMessages(ctx, ownerBFirst, store.PeerTypeChat, chatB.ID, []store.ForwardSource{{FromID: sourceB.FromID, Date: sourceB.Date, Text: sourceB.Text}}, []int64{720014})
		return err
	})
}

func TestUnreadyDisjointRevokesWithReversedPostingBucketCollisions(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()

	const userCount = 100
	users := make([]store.User, userCount)
	userIDs := make([]int64, userCount)
	for i := range users {
		users[i] = mustUser(t, s, fmt.Sprintf("+155514%05d", i))
		userIDs[i] = users[i].ID
	}

	type revokePair struct {
		fromID, toID         int64
		fromBucket, toBucket int
	}
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
		t.Fatalf("find revoke posting buckets: %v", err)
	}
	candidatesByBuckets := make(map[[2]int][]revokePair)
	var candidates []revokePair
	for rows.Next() {
		var candidate revokePair
		if err := rows.Scan(&candidate.fromID, &candidate.toID, &candidate.fromBucket, &candidate.toBucket); err != nil {
			rows.Close()
			t.Fatalf("scan revoke posting buckets: %v", err)
		}
		candidates = append(candidates, candidate)
		key := [2]int{candidate.fromBucket, candidate.toBucket}
		candidatesByBuckets[key] = append(candidatesByBuckets[key], candidate)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatalf("read revoke posting buckets: %v", err)
	}
	rows.Close()

	var first, second revokePair
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
		t.Fatalf("no disjoint revoke pairs with reversed bucket collisions among %d users", len(users))
	}

	firstMessage, _, _, _, err := s.SendMessage(ctx, first.fromID, first.toID, "revokefirstterm", 710001, 0, 0) //nolint:dogsled // only the stored message is needed here.
	if err != nil {
		t.Fatalf("send first revoke fixture: %v", err)
	}
	secondMessage, _, _, _, err := s.SendMessage(ctx, second.fromID, second.toID, "revokesecondterm", 710002, 0, 0) //nolint:dogsled // only the stored message is needed here.
	if err != nil {
		t.Fatalf("send second revoke fixture: %v", err)
	}
	if _, err := store.StorePool(s).Exec(ctx, `
		INSERT INTO message_search_unready_dialogs (owner_id, peer_type, peer_id)
		VALUES ($1, $5, $2), ($2, $5, $1), ($3, $5, $4), ($4, $5, $3)
	`, first.fromID, first.toID, second.fromID, second.toID, int16(store.PeerTypeUser)); err != nil {
		t.Fatalf("mark revoke dialogs unready: %v", err)
	}
	if _, err := store.StorePool(s).Exec(ctx, `
		CREATE FUNCTION public.test_pause_revoke_search_posting_delete() RETURNS trigger
		LANGUAGE plpgsql AS $function$
		BEGIN
			IF OLD.lexeme IN ('revokefirstterm', 'revokesecondterm') AND EXISTS (
				SELECT 1 FROM public.messages
				WHERE owner_id = OLD.owner_id AND local_id = OLD.local_id AND out
			) THEN
				PERFORM pg_sleep(2);
			END IF;
			RETURN OLD;
		END;
		$function$;
		CREATE TRIGGER zzz_test_pause_revoke_search_posting_delete
		AFTER DELETE ON public.message_search_postings
		FOR EACH ROW EXECUTE FUNCTION public.test_pause_revoke_search_posting_delete();
	`); err != nil {
		t.Fatalf("install revoke pause fixture: %v", err)
	}
	defer func() {
		if _, err := store.StorePool(s).Exec(context.Background(), `
			DROP TRIGGER IF EXISTS zzz_test_pause_revoke_search_posting_delete ON public.message_search_postings;
			DROP FUNCTION IF EXISTS public.test_pause_revoke_search_posting_delete();
		`); err != nil {
			t.Errorf("remove revoke pause fixture: %v", err)
		}
	}()

	type revokeResult struct {
		name string
		err  error
	}
	results := make(chan revokeResult, 2)
	revokeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	revoke := func(name string, pair revokePair, localID int64) {
		_, err := s.DeleteMessages(revokeCtx, pair.fromID, []int64{localID}, true)
		results <- revokeResult{name: name, err: err}
	}
	go revoke("first", first, firstMessage.LocalID)

	deadline := time.Now().Add(5 * time.Second)
	for {
		var paused bool
		if err := store.StorePool(s).QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity
				WHERE pid <> pg_backend_pid()
				  AND datname = current_database()
				  AND state = 'active'
				  AND wait_event = 'PgSleep'
				  AND query ILIKE '%UPDATE messages SET deleted%'
			)
		`).Scan(&paused); err != nil {
			t.Fatalf("check first revoke pause: %v", err)
		}
		if paused {
			break
		}
		if time.Now().After(deadline) {
			select {
			case result := <-results:
				t.Fatalf("first revoke returned before its posting pause: %v", result.err)
			default:
				t.Fatal("first revoke did not pause after locking its first posting bucket")
			}
		}
		select {
		case result := <-results:
			t.Fatalf("first revoke returned before its posting pause: %v", result.err)
		case <-time.After(10 * time.Millisecond):
		}
	}
	go revoke("second", second, secondMessage.LocalID)

	deadline = time.Now().Add(5 * time.Second)
	for {
		var activeRevokes int
		if err := store.StorePool(s).QueryRow(ctx, `
			SELECT count(*)
			FROM pg_stat_activity
			WHERE pid <> pg_backend_pid()
			  AND datname = current_database()
			  AND state = 'active'
			  AND (query ILIKE '%UPDATE messages SET deleted%'
			       OR query ILIKE '%lock_message_search_posting_dialogs%')
			  AND (wait_event = 'PgSleep' OR wait_event_type = 'Lock')
		`).Scan(&activeRevokes); err != nil {
			t.Fatalf("check overlapping revokes: %v", err)
		}
		if activeRevokes >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second revoke did not overlap the first revoke's posting lock")
		}
		select {
		case result := <-results:
			t.Fatalf("revoke %s returned before both revokes overlapped: %v", result.name, result.err)
		case <-time.After(10 * time.Millisecond):
		}
	}

	for range 2 {
		select {
		case result := <-results:
			if result.err != nil {
				t.Errorf("%s revoke failed: %v", result.name, result.err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("disjoint revokes did not complete")
		}
	}
}

func TestBatchDeleteLocksAllRowsBeforePostingBuckets(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	owner := mustUser(t, s, "+15551500001")
	peer := mustUser(t, s, "+15551500002")

	chunkMessage, _, _, _, err := s.SendMessage(ctx, owner.ID, peer.ID, "chunklockterm", 720001, 0, 0) //nolint:dogsled // only the stored message is needed here.
	if err != nil {
		t.Fatalf("send initializer chunk fixture: %v", err)
	}
	outsideMessage, _, _, _, err := s.SendMessage(ctx, owner.ID, peer.ID, "outsidelockterm", 720002, 0, 0) //nolint:dogsled // only the stored message is needed here.
	if err != nil {
		t.Fatalf("send outside chunk fixture: %v", err)
	}
	pool := store.StorePool(s)
	if _, err := pool.Exec(ctx, `
		INSERT INTO message_search_unready_dialogs (owner_id, peer_type, peer_id)
		VALUES ($1, $2, $3)
	`, owner.ID, int16(store.PeerTypeUser), peer.ID); err != nil {
		t.Fatalf("mark batch-delete dialog unready: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		CREATE FUNCTION public.test_pause_outside_chunk_posting_delete() RETURNS trigger
		LANGUAGE plpgsql AS $function$
		BEGIN
			IF OLD.lexeme = 'outsidelockterm' THEN
				PERFORM pg_sleep(2);
			END IF;
			RETURN OLD;
		END;
		$function$;
		CREATE TRIGGER zzz_test_pause_outside_chunk_posting_delete
		AFTER DELETE ON public.message_search_postings
		FOR EACH ROW EXECUTE FUNCTION public.test_pause_outside_chunk_posting_delete();
	`); err != nil {
		t.Fatalf("install outside chunk pause fixture: %v", err)
	}
	defer func() {
		if _, err := pool.Exec(context.Background(), `
			DROP TRIGGER IF EXISTS zzz_test_pause_outside_chunk_posting_delete ON public.message_search_postings;
			DROP FUNCTION IF EXISTS public.test_pause_outside_chunk_posting_delete();
		`); err != nil {
			t.Errorf("remove outside chunk pause fixture: %v", err)
		}
	}()

	initTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin initializer chunk transaction: %v", err)
	}
	defer func() { _ = initTx.Rollback(context.Background()) }() //nolint:errcheck // no-op after commit
	var lockedLocalID int64
	if err := initTx.QueryRow(ctx, `
		SELECT local_id
		FROM messages
		WHERE owner_id = $1 AND peer_type = $2 AND peer_id = $3
		ORDER BY local_id
		LIMIT 1
		FOR SHARE
	`, owner.ID, int16(store.PeerTypeUser), peer.ID).Scan(&lockedLocalID); err != nil {
		t.Fatalf("lock first initializer chunk row: %v", err)
	}
	if lockedLocalID != chunkMessage.LocalID {
		t.Fatalf("initializer locked local_id %d, want first chunk row %d", lockedLocalID, chunkMessage.LocalID)
	}

	deleteCtx, cancelDelete := context.WithTimeout(ctx, 8*time.Second)
	defer cancelDelete()
	deleteDone := make(chan error, 1)
	go func() {
		_, err := s.DeleteMessages(deleteCtx, owner.ID,
			[]int64{outsideMessage.LocalID, chunkMessage.LocalID}, false)
		deleteDone <- err
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		var writerWaiting bool
		if err := pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity
				WHERE pid <> pg_backend_pid()
				  AND datname = current_database()
				  AND state = 'active'
				  AND wait_event_type = 'Lock'
				  AND query ILIKE '%messages%'
			)
		`).Scan(&writerWaiting); err != nil {
			t.Fatalf("check delete wait on chunk row: %v", err)
		}
		if writerWaiting {
			break
		}
		if time.Now().After(deadline) {
			select {
			case err := <-deleteDone:
				t.Fatalf("batch delete returned before row-lock interleaving: %v", err)
			default:
				t.Fatal("batch delete did not block on the initializer's row lock")
			}
		}
		select {
		case err := <-deleteDone:
			t.Fatalf("batch delete returned before row-lock interleaving: %v", err)
		case <-time.After(10 * time.Millisecond):
		}
	}

	initLockDone := make(chan error, 1)
	go func() {
		_, err := initTx.Exec(ctx, `
			SELECT public.lock_message_search_posting_dialogs(
				ARRAY[$1::bigint], ARRAY[$2::smallint], ARRAY[$3::bigint]
			)
		`, owner.ID, int16(store.PeerTypeUser), peer.ID)
		initLockDone <- err
	}()
	select {
	case err := <-initLockDone:
		if err != nil {
			t.Fatalf("initializer lock bucket after row locks: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("initializer could not acquire posting bucket after the writer's row-lock wait")
	}
	if err := initTx.Commit(ctx); err != nil {
		t.Fatalf("commit initializer chunk lock fixture: %v", err)
	}
	select {
	case err := <-deleteDone:
		if err != nil {
			t.Fatalf("batch delete after initializer chunk: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("batch delete did not finish after initializer released its chunk row")
	}
}

func TestDeleteChatLocksAllMessageRowsBeforePostingBuckets(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	owner := mustUser(t, s, "+15551510001")
	member := mustUser(t, s, "+15551510002")
	chat := chatWith(t, s, owner, member)

	var first store.Message
	for i, text := range []string{"deletechat one", "deletechat two", "deletechat three", "deletechat four"} {
		message, _ := sendChat(t, s, store.FanOut{
			ChatID: chat.ID, FromID: owner.ID, Text: text, RandomID: int64(730001 + i),
		})
		if i == 0 {
			first = message
		}
	}
	pool := store.StorePool(s)
	if _, err := pool.Exec(ctx, `
		INSERT INTO message_search_unready_dialogs (owner_id, peer_type, peer_id, last_local_id)
		VALUES ($1, $2, $3, 2)
	`, owner.ID, int16(store.PeerTypeChat), chat.ID); err != nil {
		t.Fatalf("mark chat dialog partially initialized: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE FUNCTION public.test_pause_delete_chat_first_copy() RETURNS trigger
		LANGUAGE plpgsql AS $function$
		BEGIN
			IF OLD.owner_id = %d AND OLD.peer_type = %d AND OLD.peer_id = %d AND OLD.local_id = %d THEN
				PERFORM pg_sleep(3);
			END IF;
			RETURN NEW;
		END;
		$function$;
		CREATE TRIGGER zzz_test_pause_delete_chat_first_copy
		AFTER UPDATE OF deleted ON public.messages
		FOR EACH ROW
		WHEN (OLD.deleted = false AND NEW.deleted = true)
		EXECUTE FUNCTION public.test_pause_delete_chat_first_copy();
	`, owner.ID, int16(store.PeerTypeChat), chat.ID, first.LocalID)); err != nil {
		t.Fatalf("install DeleteChat pause fixture: %v", err)
	}
	defer func() {
		if _, err := pool.Exec(context.Background(), `
			DROP TRIGGER IF EXISTS zzz_test_pause_delete_chat_first_copy ON public.messages;
			DROP FUNCTION IF EXISTS public.test_pause_delete_chat_first_copy();
		`); err != nil {
			t.Errorf("remove DeleteChat pause fixture: %v", err)
		}
	}()

	type deleteResult struct{ err error }
	deleteCtx, cancelDelete := context.WithTimeout(ctx, 12*time.Second)
	defer cancelDelete()
	deleteDone := make(chan deleteResult, 1)
	go func() {
		_, err := s.DeleteChat(deleteCtx, chat.ID, owner.ID)
		deleteDone <- deleteResult{err: err}
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		var paused bool
		if err := pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1
				FROM pg_stat_activity
				WHERE pid <> pg_backend_pid()
				  AND state = 'active'
				  AND wait_event = 'PgSleep'
				  AND query ILIKE '%UPDATE messages SET deleted%'
			)
		`).Scan(&paused); err != nil {
			t.Fatalf("check DeleteChat first-copy pause: %v", err)
		}
		if paused {
			break
		}
		if time.Now().After(deadline) {
			select {
			case result := <-deleteDone:
				t.Fatalf("DeleteChat returned before its first-copy pause: %v", result.err)
			default:
				t.Fatal("DeleteChat did not pause after deleting its first copy")
			}
		}
		select {
		case result := <-deleteDone:
			t.Fatalf("DeleteChat returned before its first-copy pause: %v", result.err)
		case <-time.After(10 * time.Millisecond):
		}
	}

	initCtx, cancelInit := context.WithTimeout(ctx, 12*time.Second)
	defer cancelInit()
	type initResult struct {
		initialized int64
		err         error
	}
	initDone := make(chan initResult, 1)
	go func() {
		initialized, err := s.InitializeMessageSearchPostings(initCtx, 2)
		initDone <- initResult{initialized: initialized, err: err}
	}()

	deadline = time.Now().Add(5 * time.Second)
	for {
		var initializerWaiting bool
		if err := pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1
				FROM pg_stat_activity
				WHERE pid <> pg_backend_pid()
				  AND state = 'active'
				  AND wait_event_type = 'Lock'
				  AND query ILIKE '%WITH source_messages AS MATERIALIZED%'
			)
		`).Scan(&initializerWaiting); err != nil {
			t.Fatalf("check initializer wait during DeleteChat: %v", err)
		}
		if initializerWaiting {
			break
		}
		if time.Now().After(deadline) {
			select {
			case result := <-initDone:
				t.Fatalf("initializer returned before overlapping DeleteChat: initialized=%d err=%v", result.initialized, result.err)
			default:
				t.Fatal("initializer did not wait on DeleteChat's lock set")
			}
		}
		select {
		case result := <-initDone:
			t.Fatalf("initializer returned before overlapping DeleteChat: initialized=%d err=%v", result.initialized, result.err)
		case result := <-deleteDone:
			t.Fatalf("DeleteChat returned before initializer waited: %v", result.err)
		case <-time.After(10 * time.Millisecond):
		}
	}

	select {
	case result := <-deleteDone:
		if result.err != nil {
			t.Fatalf("DeleteChat while initializer waits on a chunk: %v", result.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("DeleteChat did not finish after its pause")
	}
	select {
	case result := <-initDone:
		if result.err != nil {
			t.Fatalf("initialize after DeleteChat released its locks: %v", result.err)
		}
		if result.initialized != 1 {
			t.Fatalf("initialized dialogs = %d, want 1", result.initialized)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("initializer did not finish after DeleteChat released its locks")
	}
}
