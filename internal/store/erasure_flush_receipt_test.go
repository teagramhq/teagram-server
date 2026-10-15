package store_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/store/db"
)

// The receipt snapshots include every column, so accidental changes to saved
// results, identity or local_no_change rows are visible as well as state changes.
func erasureFlushReceiptSnapshot(t *testing.T, ctx context.Context, conn db.DBTX, f messageDeleteReceiptFixture) []byte {
	t.Helper()
	var row []byte
	if err := conn.QueryRow(ctx, `SELECT row_to_json(r)::text FROM message_delete_receipts AS r
		WHERE owner_id=$1 AND auth_key_id=$2 AND session_id=$3 AND msg_id=$4`,
		f.ownerID, f.authKeyID, f.sessionID, f.msgID).Scan(&row); err != nil {
		t.Fatalf("snapshot receipt: %v", err)
	}
	return row
}

func erasureFlushReceiptFixture(t *testing.T, owner int64, s erasureFlushStream) messageDeleteReceiptFixture {
	t.Helper()
	f := messageDeletePendingFixture(t, owner)
	position := int64(1)
	f.operationKey, f.epoch, f.streamID, f.position = s.keys[0], &s.epoch, s.stream, &position
	return f
}

func TestErasureFlushConfirmsOnlyMatchingPendingReceipt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn := profilePhotoConn(t, ctx)
	q := db.New(conn)
	s := erasureFlushOutbox(t, ctx, q, 1, 1)
	owner := messageDeleteReceiptOwner(t, ctx, conn, "+15551861001")
	f := erasureFlushReceiptFixture(t, owner, s)
	if err := messageDeleteReceiptRawInsert(ctx, conn, f); err != nil {
		t.Fatal(err)
	}
	untouched := make([]messageDeleteReceiptFixture, 0, 5)
	for i, field := range []string{"key", "epoch", "stream", "position", "local_no_change"} {
		other := erasureFlushReceiptFixture(t, owner, s)
		other.msgID += int64(4 * (i + 1))
		switch field {
		case "key":
			key := erasureLedgerOpKey(t)
			other.operationKey = key[:]
		case "epoch":
			epoch := int64(2)
			other.epoch = &epoch
		case "stream":
			stream := erasureLedgerStream(t)
			other.streamID = stream[:]
		case "position":
			position := int64(2)
			other.position = &position
		case "local_no_change":
			anchor := time.Now().UTC().Add(-time.Hour)
			other = messageDeleteNoChangeRetentionFixture(t, owner, int64(4*(i+1)), anchor, anchor.Add(messageDeleteReceiptRetentionFloor))
		}
		if err := messageDeleteReceiptRawInsert(ctx, conn, other); err != nil {
			t.Fatal(err)
		}
		untouched = append(untouched, other)
	}
	before := make([][]byte, len(untouched))
	for i, other := range untouched {
		before[i] = erasureFlushReceiptSnapshot(t, ctx, conn, other)
	}
	generation := erasureFlushMustGrant(t, ctx, q, s, erasureFlushHolder(t), 1)
	// Use a transaction clock established before confirmation, rather than a
	// statement clock, and verify that both persisted timestamps use that anchor.
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Error(err)
		}
	}()
	var anchor time.Time
	if err := tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&anchor); err != nil {
		t.Fatal(err)
	}
	erasureFlushWantConfirm(t, ctx, db.New(tx), s, 1, generation, erasureFlushMatchingProof(s, 1, 1), 1)
	row, err := db.New(tx).MessageDeleteReceiptByOwnerIdentity(ctx, db.MessageDeleteReceiptByOwnerIdentityParams{
		OwnerID: f.ownerID, AuthKeyID: f.authKeyID, SessionID: f.sessionID, MsgID: f.msgID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if row.State != "confirmed" || !row.ConfirmedAt.Valid || !row.ConfirmedAt.Time.Equal(anchor) ||
		!row.RetainUntil.Valid || row.RetainUntil.Time.Sub(row.ConfirmedAt.Time) < 97*24*time.Hour {
		t.Fatalf("confirmation did not atomically anchor receipt retention: %+v, transaction clock %v", row, anchor)
	}
	for i, other := range untouched {
		if after := erasureFlushReceiptSnapshot(t, ctx, tx, other); !bytes.Equal(after, before[i]) {
			t.Fatalf("unrelated receipt changed: before %s, after %s", before[i], after)
		}
	}
	// Rollback must restore both sides of the coupling together.
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if got := erasureFlushConfirmations(t, ctx, q, s); len(got) != 0 {
		t.Fatalf("confirmation survived rollback: %+v", got)
	}
	var state string
	if err := conn.QueryRow(ctx, `SELECT state FROM message_delete_receipts WHERE receipt_key=$1`, f.receiptKey).Scan(&state); err != nil || state != "pending" {
		t.Fatalf("receipt survived rollback: state %s, err %v", state, err)
	}
}

func TestErasureFlushRefusedAndRepeatedConfirmationLeaveReceiptsUnchanged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn := profilePhotoConn(t, ctx)
	q := db.New(conn)
	s := erasureFlushOutbox(t, ctx, q, 1, 1)
	owner := messageDeleteReceiptOwner(t, ctx, conn, "+15551861002")
	f := erasureFlushReceiptFixture(t, owner, s)
	if err := messageDeleteReceiptRawInsert(ctx, conn, f); err != nil {
		t.Fatal(err)
	}
	generation := erasureFlushMustGrant(t, ctx, q, s, erasureFlushHolder(t), 1)
	proof := erasureFlushMatchingProof(s, 1, 1)
	before := erasureFlushReceiptSnapshot(t, ctx, conn, f)
	erasureFlushWantConfirm(t, ctx, q, s, 1, generation+1, proof, 0)
	if after := erasureFlushReceiptSnapshot(t, ctx, conn, f); !bytes.Equal(before, after) {
		t.Fatalf("refusal changed receipt: %s -> %s", before, after)
	}
	erasureFlushWantConfirm(t, ctx, q, s, 1, generation, proof, 1)
	before = erasureFlushReceiptSnapshot(t, ctx, conn, f)
	// A late pending reference must not be updated by an already-confirmed
	// insert. This catches coupling based on stored evidence instead of RETURNING.
	late := erasureFlushReceiptFixture(t, owner, s)
	late.msgID += 4
	if err := messageDeleteReceiptRawInsert(ctx, conn, late); err != nil {
		t.Fatal(err)
	}
	lateBefore := erasureFlushReceiptSnapshot(t, ctx, conn, late)
	erasureFlushWantConfirm(t, ctx, q, s, 1, generation, proof, 0)
	if after := erasureFlushReceiptSnapshot(t, ctx, conn, f); !bytes.Equal(before, after) {
		t.Fatalf("repeat changed confirmed receipt: %s -> %s", before, after)
	}
	if after := erasureFlushReceiptSnapshot(t, ctx, conn, late); !bytes.Equal(lateBefore, after) {
		t.Fatalf("repeat changed pending receipt: %s -> %s", lateBefore, after)
	}
}
