package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestRunCommandMaintenanceInitializesMessageSearchPostings(t *testing.T) {
	dsn := pgtest.DSN(t)
	t.Setenv("TG_POSTGRES_DSN", dsn)
	t.Setenv("TG_AUTHKEY_ENC_KEY", strings.Repeat("2a", 32))
	t.Setenv("TG_AUTHKEY_ENC_KEY_FILE", "")
	t.Setenv("TG_REGISTRATION", "closed")
	t.Setenv("TG_RSA_KEY_PATH", filepath.Join(t.TempDir(), "must-not-be-created.pem"))
	t.Setenv("TG_BLOB_DIR", filepath.Join(t.TempDir(), "must-not-be-created"))

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

	owner, err := s.CreateUsernameUser(ctx, "searchowner", "Search", "Owner")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	firstPeer, err := s.CreateUsernameUser(ctx, "searchpeerone", "Search", "Peer One")
	if err != nil {
		t.Fatalf("create first peer: %v", err)
	}
	secondPeer, err := s.CreateUsernameUser(ctx, "searchpeertwo", "Search", "Peer Two")
	if err != nil {
		t.Fatalf("create second peer: %v", err)
	}
	first, _, _, _, err := s.SendMessage(ctx, owner.ID, firstPeer.ID, "amber fox", 30001, 0, 0) //nolint:dogsled // only the stored message is needed here
	if err != nil {
		t.Fatalf("send first message: %v", err)
	}
	second, _, _, _, err := s.SendMessage(ctx, owner.ID, firstPeer.ID, "amber bird", 30002, 0, 0) //nolint:dogsled // only the stored message is needed here
	if err != nil {
		t.Fatalf("send second message: %v", err)
	}
	secondDialog, _, _, _, err := s.SendMessage(ctx, owner.ID, secondPeer.ID, "amber fox elsewhere", 30003, 0, 0) //nolint:dogsled // only the stored message is needed here
	if err != nil {
		t.Fatalf("send other-dialog message: %v", err)
	}
	thirdPeer, err := s.CreateUsernameUser(ctx, "searchpeerthree", "Search", "Peer Three")
	if err != nil {
		t.Fatalf("create third peer: %v", err)
	}
	if _, _, _, _, err := s.SendMessage(ctx, owner.ID, thirdPeer.ID, "amber fox again", 30004, 0, 0); err != nil {
		t.Fatalf("send third-dialog message: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		DELETE FROM message_search_postings
		WHERE owner_id = $1 AND peer_type = $2 AND peer_id IN ($3, $4, $5)
	`, owner.ID, int16(store.PeerTypeUser), firstPeer.ID, secondPeer.ID, thirdPeer.ID); err != nil {
		t.Fatalf("remove simulated historical postings: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO message_search_unready_dialogs (owner_id, peer_type, peer_id)
		VALUES ($1, $2, $3), ($1, $2, $4), ($1, $2, $5)
	`, owner.ID, int16(store.PeerTypeUser), firstPeer.ID, secondPeer.ID, thirdPeer.ID); err != nil {
		t.Fatalf("mark simulated historical dialogs unready: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE FUNCTION public.test_reject_message_search_ready() RETURNS trigger
		LANGUAGE plpgsql AS $function$
		BEGIN
			IF OLD.peer_id = %d THEN
				RAISE EXCEPTION 'intentional message search initializer interruption';
			END IF;
			RETURN OLD;
		END;
		$function$;
		CREATE TRIGGER test_reject_message_search_ready
		BEFORE DELETE ON message_search_unready_dialogs
		FOR EACH ROW EXECUTE FUNCTION public.test_reject_message_search_ready();
	`, secondPeer.ID)); err != nil {
		t.Fatalf("install initializer interruption fixture: %v", err)
	}
	defer func() {
		if _, err := pool.Exec(context.Background(), `
			DROP TRIGGER IF EXISTS test_reject_message_search_ready ON message_search_unready_dialogs;
			DROP FUNCTION IF EXISTS public.test_reject_message_search_ready();
		`); err != nil {
			t.Errorf("remove initializer interruption fixture: %v", err)
		}
	}()

	var stdout, stderr bytes.Buffer
	if err := runCommand([]string{"maintenance", "initialize-message-search-postings", "--batch-size", "1"}, slog.New(slog.DiscardHandler), &stdout, &stderr); err == nil || !strings.Contains(err.Error(), "intentional message search initializer interruption") {
		t.Fatalf("interrupted initialization error = %v, want injected failure", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	var remainingAfterInterruption int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM message_search_unready_dialogs
		WHERE owner_id = $1 AND peer_type = $2 AND peer_id IN ($3, $4, $5)
	`, owner.ID, int16(store.PeerTypeUser), firstPeer.ID, secondPeer.ID, thirdPeer.ID).Scan(&remainingAfterInterruption); err != nil {
		t.Fatalf("count dialogs after interrupted initialization: %v", err)
	}
	if remainingAfterInterruption != 2 {
		t.Fatalf("unready dialogs after interrupted initialization = %d, want 2", remainingAfterInterruption)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM message_search_postings
		WHERE owner_id = $1 AND peer_type = $2 AND peer_id = $3
	`, owner.ID, int16(store.PeerTypeUser), secondPeer.ID).Scan(&remainingAfterInterruption); err != nil {
		t.Fatalf("count committed postings: %v", err)
	}
	if remainingAfterInterruption != 3 {
		t.Fatalf("committed postings before interruption = %d, want 3", remainingAfterInterruption)
	}
	var savedCursor int64
	if err := pool.QueryRow(ctx, `
		SELECT last_local_id FROM message_search_unready_dialogs
		WHERE owner_id = $1 AND peer_type = $2 AND peer_id = $3
	`, owner.ID, int16(store.PeerTypeUser), secondPeer.ID).Scan(&savedCursor); err != nil {
		t.Fatalf("read progress cursor after interruption: %v", err)
	}
	if savedCursor != secondDialog.LocalID {
		t.Fatalf("saved progress cursor = %d, want %d", savedCursor, secondDialog.LocalID)
	}
	if _, err := pool.Exec(ctx, `
		DROP TRIGGER test_reject_message_search_ready ON message_search_unready_dialogs;
		DROP FUNCTION public.test_reject_message_search_ready();
	`); err != nil {
		t.Fatalf("remove initializer interruption fixture: %v", err)
	}

	stderr.Reset()
	if err := runCommand([]string{"maintenance", "initialize-message-search-postings", "--batch-size", "1"}, slog.New(slog.DiscardHandler), &stdout, &stderr); err != nil {
		t.Fatalf("resume search postings command: %v", err)
	}
	if stderr.String() != "Message search postings initialized: 2 dialogs\n" {
		t.Fatalf("resume stderr = %q, want remaining initialization count", stderr.String())
	}

	var remaining int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM message_search_unready_dialogs
		WHERE owner_id = $1 AND peer_type = $2 AND peer_id IN ($3, $4, $5)
	`, owner.ID, int16(store.PeerTypeUser), firstPeer.ID, secondPeer.ID, thirdPeer.ID).Scan(&remaining); err != nil {
		t.Fatalf("count unready dialogs: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("unready dialogs remaining = %d, want 0", remaining)
	}

	got, err := s.SearchMessages(ctx, owner.ID, store.PeerTypeUser, firstPeer.ID, "amber fox", 0, 10)
	if err != nil {
		t.Fatalf("search initialized dialog: %v", err)
	}
	gotRows := make([][2]any, len(got))
	for i, message := range got {
		gotRows[i] = [2]any{message.LocalID, message.Text}
	}
	rows, err := pool.Query(ctx, `
		SELECT local_id, message
		FROM messages
		WHERE owner_id = $1 AND peer_type = $2 AND peer_id = $3
		  AND deleted = false
		  AND (expires_at IS NULL OR expires_at > statement_timestamp())
		  AND message_tsv @@ plainto_tsquery('simple', $4)
		ORDER BY local_id DESC
		LIMIT 10
	`, owner.ID, int16(store.PeerTypeUser), firstPeer.ID, "amber fox")
	if err != nil {
		t.Fatalf("run legacy search: %v", err)
	}
	var wantRows [][2]any
	for rows.Next() {
		var localID int64
		var text string
		if err := rows.Scan(&localID, &text); err != nil {
			rows.Close()
			t.Fatalf("scan legacy search result: %v", err)
		}
		wantRows = append(wantRows, [2]any{localID, text})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatalf("read legacy search results: %v", err)
	}
	rows.Close()
	if !reflect.DeepEqual(gotRows, wantRows) || len(gotRows) != 1 || gotRows[0][0] != first.LocalID {
		t.Fatalf("initialized search = %v, legacy search = %v; expected message %d", gotRows, wantRows, first.LocalID)
	}
	if second.LocalID == first.LocalID {
		t.Fatal("fixture messages share a local ID")
	}

	stderr.Reset()
	if err := runCommand([]string{"maintenance", "initialize-message-search-postings"}, slog.New(slog.DiscardHandler), &stdout, &stderr); err != nil {
		t.Fatalf("repeat initialization command: %v", err)
	}
	if stderr.String() != "Message search postings initialized: 0 dialogs\n" {
		t.Fatalf("repeat stderr = %q, want no-op confirmation", stderr.String())
	}
}

func TestRunCommandMaintenanceInitializeMessageSearchPostingsValidatesBatchSize(t *testing.T) {
	for _, args := range [][]string{
		{"maintenance", "initialize-message-search-postings", "--batch-size", "0"},
		{"maintenance", "initialize-message-search-postings", "--batch-size", "-1"},
		{"maintenance", "initialize-message-search-postings", "--batch-size", "1001"},
		{"maintenance", "initialize-message-search-postings", "--batch-size", "not-a-number"},
		{"maintenance", "initialize-message-search-postings", "--batch-size"},
		{"maintenance", "initialize-message-search-postings", "--batch-size", "1", "extra"},
	} {
		if err := runCommand(args, slog.New(slog.DiscardHandler), &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
			t.Fatalf("runCommand(%v) succeeded, want usage error", args)
		}
	}
}

func TestRunCommandMaintenanceInitializesPostingsDuringMessageMutations(t *testing.T) {
	dsn := pgtest.DSN(t)
	t.Setenv("TG_POSTGRES_DSN", dsn)
	t.Setenv("TG_AUTHKEY_ENC_KEY", strings.Repeat("2a", 32))
	t.Setenv("TG_AUTHKEY_ENC_KEY_FILE", "")
	t.Setenv("TG_REGISTRATION", "closed")
	t.Setenv("TG_RSA_KEY_PATH", filepath.Join(t.TempDir(), "must-not-be-created.pem"))
	t.Setenv("TG_BLOB_DIR", filepath.Join(t.TempDir(), "must-not-be-created"))

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

	firstOwner, err := s.CreateUsernameUser(ctx, "searchraceone", "Search", "Race One")
	if err != nil {
		t.Fatalf("create first owner: %v", err)
	}
	firstPeer, err := s.CreateUsernameUser(ctx, "searchracepeerone", "Search", "Race Peer One")
	if err != nil {
		t.Fatalf("create first peer: %v", err)
	}
	secondOwner, err := s.CreateUsernameUser(ctx, "searchracetwo", "Search", "Race Two")
	if err != nil {
		t.Fatalf("create second owner: %v", err)
	}
	secondPeer, err := s.CreateUsernameUser(ctx, "searchracepeertwo", "Search", "Race Peer Two")
	if err != nil {
		t.Fatalf("create second peer: %v", err)
	}
	first, _, _, _, err := s.SendMessage(ctx, firstOwner.ID, firstPeer.ID, "legacyterm", 30101, 0, 0) //nolint:dogsled // only the stored message is needed here
	if err != nil {
		t.Fatalf("send first message: %v", err)
	}
	second, _, _, _, err := s.SendMessage(ctx, secondOwner.ID, secondPeer.ID, "deleteme", 30102, 0, 0) //nolint:dogsled // only the stored message is needed here
	if err != nil {
		t.Fatalf("send second message: %v", err)
	}
	for _, owner := range []struct {
		id     int64
		peerID int64
	}{{firstOwner.ID, firstPeer.ID}, {secondOwner.ID, secondPeer.ID}} {
		if _, err := pool.Exec(ctx, `
			DELETE FROM message_search_postings
			WHERE owner_id = $1 AND peer_type = $2 AND peer_id = $3
		`, owner.id, int16(store.PeerTypeUser), owner.peerID); err != nil {
			t.Fatalf("remove historical postings for owner %d: %v", owner.id, err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO message_search_unready_dialogs (owner_id, peer_type, peer_id)
			VALUES ($1, $2, $3)
		`, owner.id, int16(store.PeerTypeUser), owner.peerID); err != nil {
			t.Fatalf("mark dialog unready for owner %d: %v", owner.id, err)
		}
	}
	if _, err := pool.Exec(ctx, `
		CREATE FUNCTION public.test_pause_message_search_posting_insert() RETURNS trigger
		LANGUAGE plpgsql AS $function$
		BEGIN
			IF NEW.lexeme = 'legacyterm' THEN
				PERFORM pg_sleep(2);
			END IF;
			RETURN NEW;
		END;
		$function$;
		CREATE TRIGGER test_pause_message_search_posting_insert
		BEFORE INSERT ON message_search_postings
		FOR EACH ROW EXECUTE FUNCTION public.test_pause_message_search_posting_insert();
	`); err != nil {
		t.Fatalf("install posting pause fixture: %v", err)
	}
	defer func() {
		if _, err := pool.Exec(context.Background(), `
			DROP TRIGGER IF EXISTS test_pause_message_search_posting_insert ON message_search_postings;
			DROP FUNCTION IF EXISTS public.test_pause_message_search_posting_insert();
		`); err != nil {
			t.Errorf("remove posting pause fixture: %v", err)
		}
	}()

	type commandResult struct {
		stderr string
		err    error
	}
	commandDone := make(chan commandResult, 1)
	go func() {
		var stdout, stderr bytes.Buffer
		err := runCommand([]string{"maintenance", "initialize-message-search-postings", "--batch-size", "10"}, slog.New(slog.DiscardHandler), &stdout, &stderr)
		commandDone <- commandResult{stderr: stderr.String(), err: err}
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		var paused bool
		if err := pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity
				WHERE pid <> pg_backend_pid()
				  AND state = 'active'
				  AND wait_event = 'PgSleep'
				  AND query ILIKE '%WITH source_messages AS MATERIALIZED%'
			)
		`).Scan(&paused); err != nil {
			t.Fatalf("check initializer pause: %v", err)
		}
		if paused {
			break
		}
		if time.Now().After(deadline) {
			select {
			case result := <-commandDone:
				t.Fatalf("initializer returned before backfill pause: %v", result.err)
			default:
				t.Fatal("initializer did not enter the backfill pause")
			}
		}
		select {
		case result := <-commandDone:
			t.Fatalf("initializer returned before backfill pause: %v", result.err)
		case <-time.After(10 * time.Millisecond):
		}
	}

	newMessageDone := make(chan error, 1)
	go func() {
		_, _, _, _, err := s.SendMessage(ctx, firstOwner.ID, firstPeer.ID, "brandnewterm", 30103, 0, 0)
		newMessageDone <- err
	}()
	select {
	case err := <-newMessageDone:
		if err != nil {
			t.Fatalf("insert message during initialization: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("new message insert did not complete during initialization")
	}

	editDone := make(chan error, 1)
	go func() {
		_, _, err := s.EditMessage(ctx, firstOwner.ID, first.LocalID, "freshterm")
		editDone <- err
	}()
	select {
	case err := <-editDone:
		t.Fatalf("edit completed while initializer was backfilling: %v", err)
	case result := <-commandDone:
		t.Fatalf("initializer ended before concurrent edit blocked: %v", result.err)
	case <-time.After(100 * time.Millisecond):
	}

	deleteDone := make(chan error, 1)
	go func() {
		_, err := s.DeleteMessages(ctx, secondOwner.ID, []int64{second.LocalID}, false)
		deleteDone <- err
	}()

	result := <-commandDone
	if result.err != nil {
		t.Fatalf("initialize search postings: %v", result.err)
	}
	if result.stderr != "Message search postings initialized: 2 dialogs\n" {
		t.Fatalf("initializer stderr = %q, want both dialogs initialized", result.stderr)
	}
	if err := <-editDone; err != nil {
		t.Fatalf("edit after initialization: %v", err)
	}
	if err := <-deleteDone; err != nil {
		t.Fatalf("delete during initialization: %v", err)
	}

	for _, tc := range []struct {
		ownerID int64
		peerID  int64
		query   string
		wantID  int64
	}{
		{firstOwner.ID, firstPeer.ID, "legacyterm", 0},
		{firstOwner.ID, firstPeer.ID, "freshterm", first.LocalID},
		{firstOwner.ID, firstPeer.ID, "brandnewterm", first.LocalID + 1},
		{secondOwner.ID, secondPeer.ID, "deleteme", 0},
	} {
		got, err := s.SearchMessages(ctx, tc.ownerID, store.PeerTypeUser, tc.peerID, tc.query, 0, 10)
		if err != nil {
			t.Fatalf("search %q after concurrent initialization: %v", tc.query, err)
		}
		if tc.wantID == 0 && len(got) == 0 {
			continue
		}
		if len(got) != 1 || got[0].LocalID != tc.wantID {
			t.Fatalf("search %q returned %+v, want local ID %d", tc.query, got, tc.wantID)
		}
	}
}

func TestRunCommandMaintenanceHandlesLongConcurrentBackfillWithoutStalePostings(t *testing.T) {
	dsn := pgtest.DSN(t)
	t.Setenv("TG_POSTGRES_DSN", dsn)
	t.Setenv("TG_AUTHKEY_ENC_KEY", strings.Repeat("2a", 32))
	t.Setenv("TG_AUTHKEY_ENC_KEY_FILE", "")
	t.Setenv("TG_STATEMENT_TIMEOUT", "50ms")
	t.Setenv("TG_REGISTRATION", "closed")
	t.Setenv("TG_RSA_KEY_PATH", filepath.Join(t.TempDir(), "must-not-be-created.pem"))
	t.Setenv("TG_BLOB_DIR", filepath.Join(t.TempDir(), "must-not-be-created"))

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

	owner, err := s.CreateUsernameUser(ctx, "searchinsertowner", "Search", "Insert Owner")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	peer, err := s.CreateUsernameUser(ctx, "searchinsertpeer", "Search", "Insert Peer")
	if err != nil {
		t.Fatalf("create peer: %v", err)
	}
	seed, _, _, _, err := s.SendMessage(ctx, owner.ID, peer.ID, "seedterm", 30201, 0, 0) //nolint:dogsled // only the stored message is needed here
	if err != nil {
		t.Fatalf("send seed message: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO message_search_unready_dialogs (owner_id, peer_type, peer_id)
		VALUES ($1, $2, $3)
	`, owner.ID, int16(store.PeerTypeUser), peer.ID); err != nil {
		t.Fatalf("mark dialog unready: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		CREATE FUNCTION public.test_pause_message_search_posting_clear() RETURNS trigger
		LANGUAGE plpgsql AS $function$
		BEGIN
			IF OLD.lexeme = 'seedterm' THEN
				PERFORM pg_sleep(2);
			END IF;
			RETURN OLD;
		END;
		$function$;
		CREATE TRIGGER test_pause_message_search_posting_clear
		AFTER DELETE ON message_search_postings
		FOR EACH ROW EXECUTE FUNCTION public.test_pause_message_search_posting_clear();
	`); err != nil {
		t.Fatalf("install posting insert pause fixture: %v", err)
	}
	defer func() {
		if _, err := pool.Exec(context.Background(), `
			DROP TRIGGER IF EXISTS test_pause_message_search_posting_clear ON message_search_postings;
			DROP FUNCTION IF EXISTS public.test_pause_message_search_posting_clear();
		`); err != nil {
			t.Errorf("remove posting clear pause fixture: %v", err)
		}
	}()

	type commandResult struct {
		err error
	}
	commandDone := make(chan commandResult, 1)
	go func() {
		var stdout, stderr bytes.Buffer
		commandDone <- commandResult{err: runCommand([]string{"maintenance", "initialize-message-search-postings", "--batch-size", "10"}, slog.New(slog.DiscardHandler), &stdout, &stderr)}
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		var paused bool
		if err := pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity
				WHERE pid <> pg_backend_pid()
				  AND state = 'active'
				  AND wait_event = 'PgSleep'
				  AND query ILIKE '%WITH source_messages AS MATERIALIZED%'
			)
		`).Scan(&paused); err != nil {
			t.Fatalf("check initializer clear pause: %v", err)
		}
		if paused {
			break
		}
		if time.Now().After(deadline) {
			select {
			case result := <-commandDone:
				t.Fatalf("initializer returned before clear pause: %v", result.err)
			default:
				t.Fatal("initializer did not enter the clear pause")
			}
		}
		select {
		case result := <-commandDone:
			t.Fatalf("initializer returned before clear pause: %v", result.err)
		case <-time.After(10 * time.Millisecond):
		}
	}

	inserted, _, _, _, err := s.SendMessage(ctx, owner.ID, peer.ID, "legacyterm", 30202, 0, 0) //nolint:dogsled // only the stored message is needed here
	if err != nil {
		t.Fatalf("insert message during clear: %v", err)
	}
	startEdit := func(localID int64, text string) <-chan error {
		done := make(chan error, 1)
		go func() {
			tx, err := pool.Begin(ctx)
			if err != nil {
				done <- fmt.Errorf("begin concurrent edit: %w", err)
				return
			}
			if _, err := tx.Exec(ctx, `
				UPDATE messages SET message = $3, edit_date = now()
				WHERE owner_id = $1 AND local_id = $2
			`, owner.ID, localID, text); err != nil {
				if rollbackErr := tx.Rollback(context.Background()); rollbackErr != nil {
					done <- fmt.Errorf("edit message: %w; roll back edit: %w", err, rollbackErr)
					return
				}
				done <- fmt.Errorf("edit message: %w", err)
				return
			}
			done <- tx.Commit(ctx)
		}()
		return done
	}
	seedEditDone := startEdit(seed.LocalID, "newseedterm")
	insertedEditDone := startEdit(inserted.LocalID, "insertedfreshterm")
	select {
	case err := <-insertedEditDone:
		if err != nil {
			t.Fatalf("edit message inserted after the backfill snapshot: %v", err)
		}
	case result := <-commandDone:
		t.Fatalf("initializer ended before the post-snapshot edit completed: %v", result.err)
	case <-time.After(time.Second):
		t.Fatal("edit of a post-snapshot insert waited for the whole backfill")
	}

	deadline = time.Now().Add(5 * time.Second)
	for {
		var waitingEdits int
		if err := pool.QueryRow(ctx, `
			SELECT count(*)
			FROM pg_stat_activity
			WHERE pid <> pg_backend_pid()
			  AND state = 'active'
			  AND wait_event_type = 'Lock'
			  AND query ILIKE '%UPDATE messages%'
		`).Scan(&waitingEdits); err != nil {
			t.Fatalf("check concurrent edit waits: %v", err)
		}
		if waitingEdits >= 1 {
			break
		}
		if time.Now().After(deadline) {
			select {
			case result := <-commandDone:
				t.Fatalf("initializer returned before concurrent edits waited: %v", result.err)
			case err := <-seedEditDone:
				t.Fatalf("seed edit completed before initializer released its locks: %v", err)
			default:
				t.Fatalf("only %d concurrent edits waited for the initializer", waitingEdits)
			}
		}
		select {
		case result := <-commandDone:
			t.Fatalf("initializer returned before concurrent edits waited: %v", result.err)
		case err := <-seedEditDone:
			t.Fatalf("seed edit completed before initializer released its locks: %v", err)
		case <-time.After(10 * time.Millisecond):
		}
	}

	result := <-commandDone
	if result.err != nil {
		t.Fatalf("initialize search postings: %v", result.err)
	}
	if err := <-seedEditDone; err != nil {
		t.Fatalf("commit seed edit: %v", err)
	}

	var oldPostingCount, freshPostingCount int64
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE lexeme = 'legacyterm'),
		       count(*) FILTER (WHERE lexeme = 'insertedfreshterm')
		FROM message_search_postings
		WHERE owner_id = $1 AND peer_type = $2 AND peer_id = $3 AND local_id = $4
	`, owner.ID, int16(store.PeerTypeUser), peer.ID, inserted.LocalID).Scan(&oldPostingCount, &freshPostingCount); err != nil {
		t.Fatalf("count message postings: %v", err)
	}
	if oldPostingCount != 0 || freshPostingCount != 1 {
		t.Fatalf("post-snapshot message postings = old %d, fresh %d; want old 0, fresh 1", oldPostingCount, freshPostingCount)
	}
	var oldLexemeCount, freshLexemeCount int64
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(max(posting_count) FILTER (WHERE lexeme = 'legacyterm'), 0),
		       COALESCE(max(posting_count) FILTER (WHERE lexeme = 'insertedfreshterm'), 0)
		FROM message_search_posting_counts
		WHERE owner_id = $1 AND peer_type = $2 AND peer_id = $3
	`, owner.ID, int16(store.PeerTypeUser), peer.ID).Scan(&oldLexemeCount, &freshLexemeCount); err != nil {
		t.Fatalf("count dialog lexeme postings: %v", err)
	}
	if oldLexemeCount != 0 || freshLexemeCount != 1 {
		t.Fatalf("dialog lexeme counts = old %d, fresh %d; want old 0, fresh 1", oldLexemeCount, freshLexemeCount)
	}
	var ready bool
	if err := pool.QueryRow(ctx, `
		SELECT NOT EXISTS (
			SELECT 1 FROM message_search_unready_dialogs
			WHERE owner_id = $1 AND peer_type = $2 AND peer_id = $3
		)
	`, owner.ID, int16(store.PeerTypeUser), peer.ID).Scan(&ready); err != nil {
		t.Fatalf("check dialog readiness: %v", err)
	}
	if !ready {
		t.Fatal("dialog remained unready after initialization")
	}
}

func TestRunCommandMaintenanceBoundsWritersForLargeDialog(t *testing.T) {
	dsn := pgtest.DSN(t)
	t.Setenv("TG_POSTGRES_DSN", dsn)
	t.Setenv("TG_AUTHKEY_ENC_KEY", strings.Repeat("2a", 32))
	t.Setenv("TG_AUTHKEY_ENC_KEY_FILE", "")
	t.Setenv("TG_STATEMENT_TIMEOUT", "3s")
	t.Setenv("TG_REGISTRATION", "closed")
	t.Setenv("TG_RSA_KEY_PATH", filepath.Join(t.TempDir(), "must-not-be-created.pem"))
	t.Setenv("TG_BLOB_DIR", filepath.Join(t.TempDir(), "must-not-be-created"))

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

	owner, err := s.CreateUsernameUser(ctx, "searchlargeowner", "Search", "Large Owner")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	peer, err := s.CreateUsernameUser(ctx, "searchlargepeer", "Search", "Large Peer")
	if err != nil {
		t.Fatalf("create peer: %v", err)
	}
	otherPeer, err := s.CreateUsernameUser(ctx, "searchlargeother", "Search", "Large Other")
	if err != nil {
		t.Fatalf("create other peer: %v", err)
	}

	const historySize = 1000
	const chunkSize = 25
	fixtureTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin large history fixture: %v", err)
	}
	var firstLocalID int64
	if err := fixtureTx.QueryRow(ctx, `SELECT next_local_id FROM update_state WHERE user_id = $1`, owner.ID).Scan(&firstLocalID); err != nil {
		if rollbackErr := fixtureTx.Rollback(ctx); rollbackErr != nil {
			t.Errorf("rollback large history fixture: %v", rollbackErr)
		}
		t.Fatalf("read owner's next local ID: %v", err)
	}
	if _, err := fixtureTx.Exec(ctx, `
		INSERT INTO messages (owner_id, local_id, peer_type, peer_id, from_id, message, out)
		SELECT $1, $2 + sample.n, $3, $4, $1, 'commonterm archive', true
		FROM generate_series(0, $5::bigint - 1) AS sample(n)
	`, owner.ID, firstLocalID, int16(store.PeerTypeUser), peer.ID, historySize); err != nil {
		if rollbackErr := fixtureTx.Rollback(ctx); rollbackErr != nil {
			t.Errorf("rollback large history fixture: %v", rollbackErr)
		}
		t.Fatalf("insert large message history: %v", err)
	}
	if _, err := fixtureTx.Exec(ctx, `
		UPDATE update_state SET next_local_id = $2 WHERE user_id = $1
	`, owner.ID, firstLocalID+historySize); err != nil {
		if rollbackErr := fixtureTx.Rollback(ctx); rollbackErr != nil {
			t.Errorf("rollback large history fixture: %v", rollbackErr)
		}
		t.Fatalf("advance owner's local ID: %v", err)
	}
	if err := fixtureTx.Commit(ctx); err != nil {
		t.Fatalf("commit large history fixture: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		DELETE FROM message_search_postings
		WHERE owner_id = $1 AND peer_type = $2 AND peer_id = $3
	`, owner.ID, int16(store.PeerTypeUser), peer.ID); err != nil {
		t.Fatalf("remove legacy postings: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO message_search_unready_dialogs (owner_id, peer_type, peer_id)
		VALUES ($1, $2, $3)
	`, owner.ID, int16(store.PeerTypeUser), peer.ID); err != nil {
		t.Fatalf("mark dialog unready: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE FUNCTION public.test_pause_large_message_search_posting_insert() RETURNS trigger
		LANGUAGE plpgsql AS $function$
		BEGIN
			IF NEW.owner_id = %d AND NEW.peer_id = %d AND NEW.lexeme = 'commonterm' AND (NEW.local_id - %d) %% %d = 0 THEN
				IF NEW.local_id = %d THEN
					PERFORM pg_sleep(0.5);
				ELSE
					PERFORM pg_sleep(0.1);
				END IF;
			END IF;
			RETURN NEW;
		END;
		$function$;
		CREATE TRIGGER test_pause_large_message_search_posting_insert
		AFTER INSERT ON message_search_postings
		FOR EACH ROW EXECUTE FUNCTION public.test_pause_large_message_search_posting_insert();
	`, owner.ID, peer.ID, firstLocalID, chunkSize, firstLocalID)); err != nil {
		t.Fatalf("install posting clear pause fixture: %v", err)
	}

	type commandResult struct {
		err error
	}
	commandDone := make(chan commandResult, 1)
	commandFinished := false
	go func() {
		var stdout, stderr bytes.Buffer
		commandDone <- commandResult{err: runCommand([]string{"maintenance", "initialize-message-search-postings", "--batch-size", strconv.Itoa(chunkSize)}, slog.New(slog.DiscardHandler), &stdout, &stderr)}
	}()
	defer func() {
		if !commandFinished {
			select {
			case <-commandDone:
			case <-time.After(10 * time.Second):
				t.Error("initializer did not stop during cleanup")
			}
		}
		if _, err := pool.Exec(context.Background(), `
			DROP TRIGGER IF EXISTS test_pause_large_message_search_posting_insert ON message_search_postings;
			DROP FUNCTION IF EXISTS public.test_pause_large_message_search_posting_insert();
		`); err != nil {
			t.Errorf("remove posting clear pause fixture: %v", err)
		}
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		var paused bool
		if err := pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity
				WHERE pid <> pg_backend_pid()
				  AND state = 'active'
				  AND wait_event = 'PgSleep'
				  AND query ILIKE '%WITH source_messages AS MATERIALIZED%'
			)
		`).Scan(&paused); err != nil {
			t.Fatalf("check initializer pause: %v", err)
		}
		if paused {
			break
		}
		if time.Now().After(deadline) {
			select {
			case result := <-commandDone:
				commandFinished = true
				t.Fatalf("initializer returned before backfill pause: %v", result.err)
			default:
				t.Fatal("initializer did not enter the backfill pause")
			}
		}
		select {
		case result := <-commandDone:
			commandFinished = true
			t.Fatalf("initializer returned before backfill pause: %v", result.err)
		case <-time.After(10 * time.Millisecond):
		}
	}

	type sendResult struct {
		localID int64
		err     error
	}
	sameDialogSendDone := make(chan sendResult, 1)
	go func() {
		message, _, _, _, err := s.SendMessage(ctx, owner.ID, peer.ID, "commonterm live send", 30301, 0, 0)
		sameDialogSendDone <- sendResult{localID: message.LocalID, err: err}
	}()
	deadline = time.Now().Add(5 * time.Second)
	for {
		var waiting bool
		if err := pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity
				WHERE pid <> pg_backend_pid()
				  AND state = 'active'
				  AND wait_event_type = 'Lock'
				  AND query ILIKE '%INSERT INTO messages%'
			)
		`).Scan(&waiting); err != nil {
			t.Fatalf("check concurrent send wait: %v", err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("send into the rebuilding dialog did not wait on its posting count")
		}
		select {
		case result := <-sameDialogSendDone:
			t.Fatalf("send completed before the posting count was locked: %v", result.err)
		case result := <-commandDone:
			commandFinished = true
			t.Fatalf("initializer finished before the send waited: %v", result.err)
		case <-time.After(10 * time.Millisecond):
		}
	}

	otherDialogSendDone := make(chan error, 1)
	go func() {
		_, _, _, _, err := s.SendMessage(ctx, owner.ID, otherPeer.ID, "separate dialog", 30302, 0, 0)
		otherDialogSendDone <- err
	}()
	editDone := make(chan error, 1)
	go func() {
		_, _, err := s.EditMessage(ctx, owner.ID, firstLocalID, "editedterm")
		editDone <- err
	}()
	deleteDone := make(chan error, 1)
	go func() {
		_, err := s.DeleteMessages(ctx, owner.ID, []int64{firstLocalID + 1}, false)
		deleteDone <- err
	}()

	select {
	case result := <-sameDialogSendDone:
		if result.err != nil {
			t.Fatalf("send to the rebuilding dialog: %v", result.err)
		}
		if result.localID <= firstLocalID+historySize-1 {
			t.Fatalf("new message local ID = %d, want above backfill history", result.localID)
		}
	case result := <-commandDone:
		commandFinished = true
		t.Fatalf("initializer completed before the first bounded writer was released: %v", result.err)
	case <-time.After(time.Second):
		t.Fatal("send into the large dialog waited for the whole history rebuild")
	}
	for _, writer := range []struct {
		name string
		done <-chan error
	}{{"other-dialog send", otherDialogSendDone}, {"edit", editDone}, {"delete", deleteDone}} {
		select {
		case err := <-writer.done:
			if err != nil {
				t.Fatalf("%s after one backfill chunk: %v", writer.name, err)
			}
		case result := <-commandDone:
			commandFinished = true
			t.Fatalf("initializer completed before %s was released by a chunk boundary: %v", writer.name, result.err)
		case <-time.After(time.Second):
			t.Fatalf("%s waited for the whole history rebuild", writer.name)
		}
	}
	select {
	case result := <-commandDone:
		commandFinished = true
		t.Fatalf("initializer completed before finishing the large-history contention assertions: %v", result.err)
	default:
	}

	select {
	case result := <-commandDone:
		commandFinished = true
		if result.err != nil {
			t.Fatalf("initialize large dialog: %v", result.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("initializer did not finish the large dialog")
	}

	var commonCount, editedCount int64
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE lexeme = 'commonterm'),
		       count(*) FILTER (WHERE lexeme = 'editedterm')
		FROM message_search_postings
		WHERE owner_id = $1 AND peer_type = $2 AND peer_id = $3
	`, owner.ID, int16(store.PeerTypeUser), peer.ID).Scan(&commonCount, &editedCount); err != nil {
		t.Fatalf("count final postings: %v", err)
	}
	if commonCount != historySize-2+1 || editedCount != 1 {
		t.Fatalf("final lexeme counts = common %d, edited %d; want %d and 1", commonCount, editedCount, historySize-1)
	}
	results, err := s.SearchMessages(ctx, owner.ID, store.PeerTypeUser, peer.ID, "commonterm", 0, 10)
	if err != nil {
		t.Fatalf("search initialized large dialog: %v", err)
	}
	if len(results) != 10 {
		t.Fatalf("large dialog search returned %d results, want 10", len(results))
	}
	if results[0].LocalID != firstLocalID+historySize {
		t.Fatalf("large dialog search first local ID = %d, want newest %d", results[0].LocalID, firstLocalID+historySize)
	}
}

func TestRunCommandMaintenanceUsesConsistentPostingCountLockOrder(t *testing.T) {
	dsn := pgtest.DSN(t)
	t.Setenv("TG_POSTGRES_DSN", dsn)
	t.Setenv("TG_AUTHKEY_ENC_KEY", strings.Repeat("2a", 32))
	t.Setenv("TG_AUTHKEY_ENC_KEY_FILE", "")
	t.Setenv("TG_REGISTRATION", "closed")
	t.Setenv("TG_RSA_KEY_PATH", filepath.Join(t.TempDir(), "must-not-be-created.pem"))
	t.Setenv("TG_BLOB_DIR", filepath.Join(t.TempDir(), "must-not-be-created"))

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

	owner, err := s.CreateUsernameUser(ctx, "searchorderowner", "Search", "Order Owner")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	peer, err := s.CreateUsernameUser(ctx, "searchorderpeer", "Search", "Order Peer")
	if err != nil {
		t.Fatalf("create peer: %v", err)
	}
	first, _, _, _, err := s.SendMessage(ctx, owner.ID, peer.ID, "z", 30401, 0, 0) //nolint:dogsled // only the stored message is needed here
	if err != nil {
		t.Fatalf("send first message: %v", err)
	}
	second, _, _, _, err := s.SendMessage(ctx, owner.ID, peer.ID, "a", 30402, 0, 0) //nolint:dogsled // only the stored message is needed here
	if err != nil {
		t.Fatalf("send second message: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		DELETE FROM message_search_postings
		WHERE owner_id = $1 AND peer_type = $2 AND peer_id = $3
	`, owner.ID, int16(store.PeerTypeUser), peer.ID); err != nil {
		t.Fatalf("remove simulated historical postings: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO message_search_unready_dialogs (owner_id, peer_type, peer_id)
		VALUES ($1, $2, $3)
	`, owner.ID, int16(store.PeerTypeUser), peer.ID); err != nil {
		t.Fatalf("mark dialog unready: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE FUNCTION public.test_pause_ordered_message_search_posting_insert() RETURNS trigger
		LANGUAGE plpgsql AS $function$
		BEGIN
			IF NEW.owner_id = %d AND NEW.peer_id = %d AND NEW.local_id = %d AND NEW.lexeme = 'z' THEN
				PERFORM pg_sleep(2);
			END IF;
			RETURN NEW;
		END;
		$function$;
		CREATE TRIGGER zzz_test_pause_ordered_message_search_posting_insert
		AFTER INSERT ON message_search_postings
		FOR EACH ROW EXECUTE FUNCTION public.test_pause_ordered_message_search_posting_insert();
	`, owner.ID, peer.ID, first.LocalID)); err != nil {
		t.Fatalf("install posting count lock-order pause fixture: %v", err)
	}

	type commandResult struct{ err error }
	commandDone := make(chan commandResult, 1)
	commandFinished := false
	go func() {
		var stdout, stderr bytes.Buffer
		commandDone <- commandResult{err: runCommand([]string{"maintenance", "initialize-message-search-postings", "--batch-size", "2"}, slog.New(slog.DiscardHandler), &stdout, &stderr)}
	}()
	defer func() {
		if !commandFinished {
			select {
			case <-commandDone:
			case <-time.After(10 * time.Second):
				t.Error("initializer did not stop during cleanup")
			}
		}
		if _, err := pool.Exec(context.Background(), `
			DROP TRIGGER IF EXISTS zzz_test_pause_ordered_message_search_posting_insert ON message_search_postings;
			DROP FUNCTION IF EXISTS public.test_pause_ordered_message_search_posting_insert();
		`); err != nil {
			t.Errorf("remove posting lock-order pause fixture: %v", err)
		}
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		var paused bool
		if err := pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity
				WHERE pid <> pg_backend_pid()
				  AND state = 'active'
				  AND wait_event = 'PgSleep'
				  AND query ILIKE '%WITH source_messages AS MATERIALIZED%'
			)
		`).Scan(&paused); err != nil {
			t.Fatalf("check initializer pause: %v", err)
		}
		if paused {
			break
		}
		if time.Now().After(deadline) {
			select {
			case result := <-commandDone:
				commandFinished = true
				t.Fatalf("initializer returned before posting pause: %v", result.err)
			default:
				t.Fatal("initializer did not enter the posting pause")
			}
		}
		select {
		case result := <-commandDone:
			commandFinished = true
			t.Fatalf("initializer returned before posting pause: %v", result.err)
		case <-time.After(10 * time.Millisecond):
		}
	}

	type sendResult struct{ err error }
	sendDone := make(chan sendResult, 1)
	go func() {
		_, _, _, _, err := s.SendMessage(ctx, owner.ID, peer.ID, "a z", 30403, 0, 0)
		sendDone <- sendResult{err: err}
	}()
	deadline = time.Now().Add(5 * time.Second)
	for {
		var waiting bool
		if err := pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity
				WHERE pid <> pg_backend_pid()
				  AND state = 'active'
				  AND wait_event_type = 'Lock'
				  AND query ILIKE '%INSERT INTO messages%'
			)
		`).Scan(&waiting); err != nil {
			t.Fatalf("check concurrent send lock wait: %v", err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			select {
			case result := <-sendDone:
				t.Fatalf("send completed before initializer released posting locks: %v", result.err)
			default:
				t.Fatal("send did not wait for the initializer's posting locks")
			}
		}
		select {
		case result := <-sendDone:
			t.Fatalf("send completed before initializer released posting locks: %v", result.err)
		case result := <-commandDone:
			commandFinished = true
			t.Fatalf("initializer returned before send waited: %v", result.err)
		case <-time.After(10 * time.Millisecond):
		}
	}

	select {
	case result := <-commandDone:
		commandFinished = true
		if result.err != nil {
			t.Fatalf("initialize dialog with concurrent send: %v", result.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("initializer and concurrent send deadlocked")
	}
	select {
	case result := <-sendDone:
		if result.err != nil {
			t.Fatalf("send with concurrent initializer: %v", result.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("send did not finish after initializer committed")
	}

	var aCount, zCount int64
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE lexeme = 'a'), count(*) FILTER (WHERE lexeme = 'z')
		FROM message_search_postings
		WHERE owner_id = $1 AND peer_type = $2 AND peer_id = $3
	`, owner.ID, int16(store.PeerTypeUser), peer.ID).Scan(&aCount, &zCount); err != nil {
		t.Fatalf("count final postings: %v", err)
	}
	if aCount != 2 || zCount != 2 {
		t.Fatalf("final posting counts = a %d, z %d; want 2 each (messages %d and %d)", aCount, zCount, second.LocalID, first.LocalID)
	}
}
