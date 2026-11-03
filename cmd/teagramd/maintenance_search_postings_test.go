package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"reflect"
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
	if _, _, _, _, err := s.SendMessage(ctx, owner.ID, secondPeer.ID, "amber fox elsewhere", 30003, 0, 0); err != nil {
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
		t.Fatalf("count rolled-back postings: %v", err)
	}
	if remainingAfterInterruption != 0 {
		t.Fatalf("postings from failed batch = %d, want 0", remainingAfterInterruption)
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
				  AND query ILIKE '%INSERT INTO message_search_postings%'
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
