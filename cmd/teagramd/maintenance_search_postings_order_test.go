package main

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestInitializerLocksChunkRowsBeforePostingBucket(t *testing.T) {
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

	owner, err := s.CreateUsernameUser(ctx, "chunklockowner", "Chunk", "Lock Owner")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	peer, err := s.CreateUsernameUser(ctx, "chunklockpeer", "Chunk", "Lock Peer")
	if err != nil {
		t.Fatalf("create peer: %v", err)
	}
	var thirdLocalID int64
	for i, text := range []string{"first chunk one", "first chunk two", "next chunk three", "next chunk four"} {
		message, _, _, _, err := s.SendMessage(ctx, owner.ID, peer.ID, text, int64(30501+i), 0, 0)
		if err != nil {
			t.Fatalf("send initializer fixture %d: %v", i+1, err)
		}
		if i == 2 {
			thirdLocalID = message.LocalID
		}
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO message_search_unready_dialogs (owner_id, peer_type, peer_id)
		VALUES ($1, $2, $3)
	`, owner.ID, int16(store.PeerTypeUser), peer.ID); err != nil {
		t.Fatalf("mark dialog unready: %v", err)
	}

	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin row-lock holder: %v", err)
	}
	defer func() { _ = holder.Rollback(context.Background()) }() //nolint:errcheck // no-op after commit
	var holderPID int32
	if err := holder.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holderPID); err != nil {
		t.Fatalf("read row-lock holder pid: %v", err)
	}
	var lockedLocalID int64
	if err := holder.QueryRow(ctx, `
		SELECT local_id
		FROM messages
		WHERE owner_id = $1 AND peer_type = $2 AND peer_id = $3 AND local_id = $4
		FOR UPDATE
	`, owner.ID, int16(store.PeerTypeUser), peer.ID, thirdLocalID).Scan(&lockedLocalID); err != nil {
		t.Fatalf("lock first row of next initializer chunk: %v", err)
	}

	initCtx, cancelInit := context.WithTimeout(ctx, 10*time.Second)
	defer cancelInit()
	initDone := make(chan error, 1)
	go func() {
		_, err := s.InitializeMessageSearchPostings(initCtx, 2)
		initDone <- err
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		var blockedOnHolder bool
		if err := pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1
				FROM pg_stat_activity AS activity
				WHERE activity.pid <> pg_backend_pid()
				  AND activity.state = 'active'
				  AND activity.wait_event_type = 'Lock'
				  AND activity.query ILIKE '%WITH source_messages AS MATERIALIZED%'
				  AND $1 = ANY(pg_blocking_pids(activity.pid))
			)
		`, holderPID).Scan(&blockedOnHolder); err != nil {
			t.Fatalf("check initializer wait on next-chunk row: %v", err)
		}
		if blockedOnHolder {
			break
		}
		if time.Now().After(deadline) {
			select {
			case err := <-initDone:
				t.Fatalf("initializer returned before waiting on the next-chunk row: %v", err)
			default:
				t.Fatal("initializer did not block on the held next-chunk row")
			}
		}
		select {
		case err := <-initDone:
			t.Fatalf("initializer returned before waiting on the next-chunk row: %v", err)
		case <-time.After(10 * time.Millisecond):
		}
	}

	bucketCtx, cancelBucket := context.WithTimeout(ctx, 5*time.Second)
	var bucketLocked bool
	err = holder.QueryRow(bucketCtx, `
		SELECT public.lock_message_search_posting_dialogs(
			ARRAY[$1::bigint], ARRAY[$2::smallint], ARRAY[$3::bigint]
		)
	`, owner.ID, int16(store.PeerTypeUser), peer.ID).Scan(&bucketLocked)
	cancelBucket()
	if err != nil {
		if rollbackErr := holder.Rollback(context.Background()); rollbackErr != nil {
			t.Errorf("roll back row-lock holder after bucket lock error: %v", rollbackErr)
		}
		select {
		case <-initDone:
		case <-time.After(5 * time.Second):
			t.Error("initializer did not finish after the row-lock holder rolled back")
		}
		t.Fatalf("take posting bucket after locking only the next chunk row: %v", err)
	}
	if !bucketLocked {
		t.Fatal("posting bucket lock function returned false")
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("commit row-lock holder: %v", err)
	}
	select {
	case err := <-initDone:
		if err != nil {
			t.Fatalf("initialize dialog after holder committed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("initializer did not finish after the holder released its locks")
	}
}
