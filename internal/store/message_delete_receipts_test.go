package store_test

import (
	"bytes"
	"context"
	"errors"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/teagramhq/teagram-server/internal/store"
	"github.com/teagramhq/teagram-server/internal/store/db"
)

const messageDeleteReceiptMigration = "20261013000080_message_delete_receipts.sql"
const messageDeleteReceiptNoChangeMigration = "20261013000081_message_delete_no_change_immutable.sql"

var messageDeleteReceiptTables = []string{"message_delete_receipts", "message_delete_capacity_refusals"}

type messageDeleteReceiptFixture struct {
	ownerID          int64
	authKeyID        []byte
	sessionID        int64
	msgID            int64
	method           string
	idSpace          string
	revoke           bool
	targets          []int32
	pts              int32
	ptsCount         int32
	receiptKey       []byte
	state            string
	operationKey     []byte
	epoch            *int64
	streamID         []byte
	position         *int64
	confirmedAt      *time.Time
	localCompletedAt *time.Time
	retainUntil      *time.Time
}

func messageDeletePendingFixture(t *testing.T, ownerID int64) messageDeleteReceiptFixture {
	t.Helper()
	key, operation := erasureLedgerOpKey(t), erasureLedgerOpKey(t)
	stream := erasureLedgerStream(t)
	epoch, position := int64(3), int64(11)
	return messageDeleteReceiptFixture{
		ownerID: ownerID, authKeyID: []byte{0xff, 0, 1, 2, 3, 4, 5, 0x80},
		sessionID: math.MinInt64, msgID: 1700000000000,
		method: "messages.deleteMessages", idSpace: "owner_message", revoke: true,
		targets: []int32{4, 9}, pts: 7, ptsCount: 2, receiptKey: key[:], state: "pending",
		operationKey: operation[:], epoch: &epoch, streamID: stream[:], position: &position,
	}
}

func messageDeleteReceiptOwner(t *testing.T, ctx context.Context, conn *pgx.Conn, phone string) int64 {
	t.Helper()
	var id int64
	if err := conn.QueryRow(ctx, `INSERT INTO users (phone) VALUES ($1) RETURNING id`, phone).Scan(&id); err != nil {
		t.Fatalf("create receipt owner: %v", err)
	}
	return id
}

// Raw SQL exercises the database constraints independently of generated insert queries.
func messageDeleteReceiptRawInsert(ctx context.Context, conn db.DBTX, f messageDeleteReceiptFixture) error {
	_, err := conn.Exec(ctx, `
		INSERT INTO message_delete_receipts (
			owner_id, auth_key_id, session_id, msg_id, method, id_space, revoke, target_ids,
			pts, pts_count, receipt_key, state, operation_key, epoch, stream_id, position,
			confirmed_at, local_completed_at, retain_until
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)`,
		f.ownerID, f.authKeyID, f.sessionID, f.msgID, f.method, f.idSpace, f.revoke, f.targets,
		f.pts, f.ptsCount, f.receiptKey, f.state, f.operationKey, f.epoch, f.streamID,
		f.position, f.confirmedAt, f.localCompletedAt, f.retainUntil)
	return err
}

func messageDeleteReceiptWantSQLState(t *testing.T, err error, code string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Fatalf("SQL error = %v, want SQLSTATE %s", err, code)
	}
}

func TestMessageDeleteReceiptIdentityIsUniqueWithinOwner(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn := profilePhotoConn(t, ctx)
	a := messageDeleteReceiptOwner(t, ctx, conn, "+15551795001")
	b := messageDeleteReceiptOwner(t, ctx, conn, "+15551795002")
	f := messageDeletePendingFixture(t, a)
	if err := messageDeleteReceiptRawInsert(ctx, conn, f); err != nil {
		t.Fatalf("insert original receipt: %v", err)
	}
	duplicate := messageDeletePendingFixture(t, a)
	duplicate.pts, duplicate.ptsCount = 99, 1
	messageDeleteReceiptWantSQLState(t, messageDeleteReceiptRawInsert(ctx, conn, duplicate), "23505")
	var pts, count int32
	var key []byte
	if err := conn.QueryRow(ctx, `SELECT pts, pts_count, receipt_key FROM message_delete_receipts
		WHERE owner_id=$1 AND auth_key_id=$2 AND session_id=$3 AND msg_id=$4`,
		a, f.authKeyID, f.sessionID, f.msgID).Scan(&pts, &count, &key); err != nil {
		t.Fatalf("read original receipt: %v", err)
	}
	if pts != 7 || count != 2 || !bytes.Equal(key, f.receiptKey) {
		t.Fatalf("duplicate replaced original result/key: %d/%d/%x", pts, count, key)
	}
	other := messageDeletePendingFixture(t, b)
	if err := messageDeleteReceiptRawInsert(ctx, conn, other); err != nil {
		t.Fatalf("another owner cannot use the same transport tuple: %v", err)
	}
	// A key is opaque and unique even when the transport tuple differs.
	f.msgID += 4
	messageDeleteReceiptWantSQLState(t, messageDeleteReceiptRawInsert(ctx, conn, f), "23505")
}

func TestMessageDeleteReceiptEnvelopeConstraints(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn := profilePhotoConn(t, ctx)
	owner := messageDeleteReceiptOwner(t, ctx, conn, "+15551795003")
	cases := []struct {
		name   string
		mutate func(*messageDeleteReceiptFixture)
	}{
		{"short auth key", func(f *messageDeleteReceiptFixture) { f.authKeyID = []byte{1} }},
		{"long auth key", func(f *messageDeleteReceiptFixture) { f.authKeyID = bytes.Repeat([]byte{1}, 9) }},
		{"zero receipt key", func(f *messageDeleteReceiptFixture) { f.receiptKey = make([]byte, 16) }},
		{"short receipt key", func(f *messageDeleteReceiptFixture) { f.receiptKey = []byte{1} }},
		{"other method", func(f *messageDeleteReceiptFixture) { f.method = "channels.deleteMessages" }},
		{"other id space", func(f *messageDeleteReceiptFixture) { f.idSpace = "channel_message" }},
		{"negative pts", func(f *messageDeleteReceiptFixture) { f.pts = -1 }},
		{"negative count", func(f *messageDeleteReceiptFixture) { f.ptsCount = -1 }},
		{"empty changed targets", func(f *messageDeleteReceiptFixture) { f.targets = []int32{} }},
		{"unsorted targets", func(f *messageDeleteReceiptFixture) { f.targets = []int32{9, 4} }},
		{"duplicate targets", func(f *messageDeleteReceiptFixture) { f.targets = []int32{4, 4} }},
		{"zero target", func(f *messageDeleteReceiptFixture) { f.targets = []int32{0} }},
		{"negative target", func(f *messageDeleteReceiptFixture) { f.targets = []int32{-1} }},
		{"too many targets", func(f *messageDeleteReceiptFixture) {
			f.targets = make([]int32, 101)
			for i := range f.targets {
				f.targets[i] = int32(i + 1) // #nosec G115 -- fixture is bounded to 101.
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := messageDeletePendingFixture(t, owner)
			tc.mutate(&f)
			messageDeleteReceiptWantSQLState(t, messageDeleteReceiptRawInsert(ctx, conn, f), "23514")
		})
	}
	// NULL elements and noncanonical array dimensions must not slip through CHECK's null semantics.
	for _, targets := range []string{"ARRAY[4,NULL]::integer[]", "' [0:1]={4,9}'::integer[]", "ARRAY[[4,9]]"} {
		f := messageDeletePendingFixture(t, owner)
		_, err := conn.Exec(ctx, `INSERT INTO message_delete_receipts
			(owner_id, auth_key_id, session_id, msg_id, method, id_space, revoke, target_ids,
			 pts, pts_count, receipt_key, state, operation_key, epoch, stream_id, position)
			VALUES ($1,$2,$3,$4,'messages.deleteMessages','owner_message',true,`+targets+`,7,2,$5,'pending',$6,3,$7,11)`,
			owner, f.authKeyID, f.sessionID, f.msgID, f.receiptKey, f.operationKey, f.streamID)
		messageDeleteReceiptWantSQLState(t, err, "23514")
	}
}

func TestMessageDeleteReceiptStateConstraints(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn := profilePhotoConn(t, ctx)
	owner := messageDeleteReceiptOwner(t, ctx, conn, "+15551795004")
	now := time.Now().UTC().Truncate(time.Microsecond)
	deadline := now.Add(97 * 24 * time.Hour)
	cases := []struct {
		name   string
		mutate func(*messageDeleteReceiptFixture)
	}{
		{"unknown state", func(f *messageDeleteReceiptFixture) { f.state = "complete" }},
		{"pending without key", func(f *messageDeleteReceiptFixture) { f.operationKey = nil }},
		{"pending without epoch", func(f *messageDeleteReceiptFixture) { f.epoch = nil }},
		{"pending without stream", func(f *messageDeleteReceiptFixture) { f.streamID = nil }},
		{"pending without position", func(f *messageDeleteReceiptFixture) { f.position = nil }},
		{"pending with confirmation", func(f *messageDeleteReceiptFixture) { f.confirmedAt = &now }},
		{"pending with local completion", func(f *messageDeleteReceiptFixture) { f.localCompletedAt = &now }},
		{"pending with expiry", func(f *messageDeleteReceiptFixture) { f.retainUntil = &deadline }},
		{"confirmed without timestamp", func(f *messageDeleteReceiptFixture) { f.state = "confirmed"; f.retainUntil = &deadline }},
		{"confirmed without reference", func(f *messageDeleteReceiptFixture) {
			f.state, f.confirmedAt, f.retainUntil, f.operationKey = "confirmed", &now, &deadline, nil
		}},
		{"confirmed with local anchor", func(f *messageDeleteReceiptFixture) {
			f.state, f.confirmedAt, f.retainUntil, f.localCompletedAt = "confirmed", &now, &deadline, &now
		}},
		{"no change without local anchor", func(f *messageDeleteReceiptFixture) { f.state = "local_no_change" }},
		{"zero operation key", func(f *messageDeleteReceiptFixture) { f.operationKey = make([]byte, 16) }},
		{"short operation key", func(f *messageDeleteReceiptFixture) { f.operationKey = []byte{1} }},
		{"zero stream", func(f *messageDeleteReceiptFixture) { f.streamID = make([]byte, 16) }},
		{"short stream", func(f *messageDeleteReceiptFixture) { f.streamID = []byte{1} }},
		{"zero epoch", func(f *messageDeleteReceiptFixture) { zero := int64(0); f.epoch = &zero }},
		{"zero position", func(f *messageDeleteReceiptFixture) { zero := int64(0); f.position = &zero }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := messageDeletePendingFixture(t, owner)
			tc.mutate(&f)
			messageDeleteReceiptWantSQLState(t, messageDeleteReceiptRawInsert(ctx, conn, f), "23514")
		})
	}
}

func TestMessageDeleteReceiptTerminalRetentionConstraints(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn := profilePhotoConn(t, ctx)
	owner := messageDeleteReceiptOwner(t, ctx, conn, "+15551795005")
	anchor := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, state := range []string{"confirmed", "local_no_change"} {
		t.Run(state, func(t *testing.T) {
			for i, delta := range []time.Duration{-time.Microsecond, 0, time.Microsecond, 10 * 24 * time.Hour} {
				f := messageDeletePendingFixture(t, owner)
				f.msgID += int64(i * 4)
				f.state = state
				deadline := anchor.Add(97*24*time.Hour + delta)
				f.retainUntil = &deadline
				if state == "confirmed" {
					f.confirmedAt = &anchor
				} else {
					f.sessionID = 1
					f.localCompletedAt = &anchor
					f.operationKey, f.epoch, f.streamID, f.position = nil, nil, nil, nil
					f.targets, f.pts, f.ptsCount = []int32{}, 0, 0
				}
				err := messageDeleteReceiptRawInsert(ctx, conn, f)
				if delta < 0 {
					messageDeleteReceiptWantSQLState(t, err, "23514")
				} else if err != nil {
					t.Fatalf("valid %s deadline at floor + %s: %v", state, delta, err)
				}
			}
		})
	}
	// Retention authority and its anchor cannot be missing or infinite.
	for _, column := range []string{"retain_until", "confirmed_at"} {
		_, err := conn.Exec(ctx, `UPDATE message_delete_receipts SET `+column+`=NULL WHERE state='confirmed'`)
		messageDeleteReceiptWantSQLState(t, err, "23514")
		_, err = conn.Exec(ctx, `UPDATE message_delete_receipts SET `+column+`='infinity' WHERE state='confirmed'`)
		messageDeleteReceiptWantSQLState(t, err, "23514")
	}
	// A no-change row keeps its local anchor; adding a capture/confirmation is invalid.
	_, err := conn.Exec(ctx, `UPDATE message_delete_receipts SET
		state='confirmed', confirmed_at=local_completed_at,
		operation_key=decode('11111111111111111111111111111111','hex'), epoch=3,
		stream_id=decode('22222222222222222222222222222222','hex'), position=11
		WHERE state='local_no_change'`)
	messageDeleteReceiptWantSQLState(t, err, "23514")
}

func TestMessageDeleteReceiptNoChangeStateAndAnchorAreImmutable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		assignments string
		rejected    bool
		retainDelta time.Duration
	}{
		{
			name: "complete_confirmation",
			assignments: `state='confirmed', local_completed_at=NULL, confirmed_at=local_completed_at,
				operation_key=decode('11111111111111111111111111111111','hex'), epoch=3,
				stream_id=decode('22222222222222222222222222222222','hex'), position=11`,
			rejected: true,
		},
		{
			name: "complete_pending_transition",
			assignments: `state='pending', local_completed_at=NULL, retain_until=NULL,
				operation_key=decode('11111111111111111111111111111111','hex'), epoch=3,
				stream_id=decode('22222222222222222222222222222222','hex'), position=11`,
			rejected: true,
		},
		{
			name:        "later_completion_anchor",
			assignments: `local_completed_at=local_completed_at+INTERVAL '1 day', retain_until=retain_until+INTERVAL '1 day'`,
			rejected:    true,
		},
		{
			name:        "earlier_completion_anchor",
			assignments: `local_completed_at=local_completed_at-INTERVAL '1 day'`,
			rejected:    true,
		},
		{
			name:        "same_state_and_anchor",
			assignments: `state=state, local_completed_at=local_completed_at`,
		},
		{
			name:        "retention_extension",
			assignments: `retain_until=retain_until+INTERVAL '1 day'`,
			retainDelta: 24 * time.Hour,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			conn := profilePhotoConn(t, ctx)
			owner := messageDeleteReceiptOwner(t, ctx, conn, "+15551795021")
			f := messageDeletePendingFixture(t, owner)
			anchor := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			deadline := anchor.Add(97 * 24 * time.Hour)
			f.state, f.localCompletedAt, f.retainUntil = "local_no_change", &anchor, &deadline
			f.operationKey, f.epoch, f.streamID, f.position = nil, nil, nil, nil
			// Nonempty normalized targets let the complete transition satisfy every row CHECK.
			if err := messageDeleteReceiptRawInsert(ctx, conn, f); err != nil {
				t.Fatalf("insert nonempty no-change receipt: %v", err)
			}
			_, err := conn.Exec(ctx, `UPDATE message_delete_receipts SET `+tc.assignments+` WHERE receipt_key=$1`, f.receiptKey)
			if tc.rejected {
				messageDeleteReceiptWantSQLState(t, err, "23514")
			} else if err != nil {
				t.Fatalf("valid no-change update: %v", err)
			}
			got, err := db.New(conn).MessageDeleteReceiptByOwnerIdentity(ctx, messageDeleteIdentity(f))
			if err != nil {
				t.Fatalf("read receipt after update: %v", err)
			}
			if got.State != "local_no_change" || !got.LocalCompletedAt.Valid || !got.LocalCompletedAt.Time.Equal(anchor) ||
				!got.RetainUntil.Valid || !got.RetainUntil.Time.Equal(deadline.Add(tc.retainDelta)) ||
				got.ConfirmedAt.Valid || len(got.OperationKey) != 0 || got.Epoch != nil || len(got.StreamID) != 0 || got.Position != nil ||
				!slices.Equal(got.TargetIds, f.targets) || got.Pts != f.pts || got.PtsCount != f.ptsCount {
				t.Fatalf("no-change receipt changed state, anchor or result: %+v", got)
			}
		})
	}
}

func TestMessageDeleteCapacityRefusalStorageConstraints(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn := profilePhotoConn(t, ctx)
	a := messageDeleteReceiptOwner(t, ctx, conn, "+15551795006")
	b := messageDeleteReceiptOwner(t, ctx, conn, "+15551795007")
	_, err := conn.Exec(ctx, `INSERT INTO message_delete_capacity_refusals (owner_id, total) VALUES ($1,-1)`, a)
	messageDeleteReceiptWantSQLState(t, err, "23514")
	if _, err := conn.Exec(ctx, `INSERT INTO message_delete_capacity_refusals (owner_id, total) VALUES ($1,$2),($3,0)`, a, int64(math.MaxInt64), b); err != nil {
		t.Fatalf("store saturated and zero totals: %v", err)
	}
	var total int64
	if err := conn.QueryRow(ctx, `SELECT total FROM message_delete_capacity_refusals WHERE owner_id=$1`, a).Scan(&total); err != nil || total != math.MaxInt64 {
		t.Fatalf("saturated total = %d, err %v", total, err)
	}
	if err := conn.QueryRow(ctx, `SELECT total FROM message_delete_capacity_refusals WHERE owner_id=$1`, b).Scan(&total); err != nil || total != 0 {
		t.Fatalf("other owner's total = %d, err %v", total, err)
	}
	_, err = conn.Exec(ctx, `INSERT INTO message_delete_capacity_refusals (owner_id, total) VALUES ($1,1)`, a)
	messageDeleteReceiptWantSQLState(t, err, "23505")
}

func TestMessageDeleteReceiptMigrationReplaysOnDisposableSchema(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn, _ := profilePhotoDatabaseBefore(t, ctx, messageDeleteReceiptMigration)
	for _, table := range messageDeleteReceiptTables {
		if erasureLedgerPresent(t, ctx, conn, table) {
			t.Fatalf("%s exists before migration", table)
		}
	}
	body := profilePhotoReadMigration(t, messageDeleteReceiptMigration)
	started := time.Now()
	if _, err := conn.Exec(ctx, body); err != nil {
		t.Fatalf("apply receipt migration: %v", err)
	}
	t.Logf("additive receipt migration applied in %s on the pre-receipt disposable schema", time.Since(started))
	for _, table := range messageDeleteReceiptTables {
		if !erasureLedgerPresent(t, ctx, conn, table) {
			t.Fatalf("%s absent after migration", table)
		}
	}
	owner := messageDeleteReceiptOwner(t, ctx, conn, "+15551795008")
	f := messageDeletePendingFixture(t, owner)
	if err := messageDeleteReceiptRawInsert(ctx, conn, f); err != nil {
		t.Fatalf("insert replayed receipt: %v", err)
	}
	messageDeleteReceiptWantSQLState(t, messageDeleteReceiptRawInsert(ctx, conn, f), "23505")
	f = messageDeletePendingFixture(t, owner)
	f.receiptKey = make([]byte, 16)
	messageDeleteReceiptWantSQLState(t, messageDeleteReceiptRawInsert(ctx, conn, f), "23514")
	_, err := conn.Exec(ctx, `INSERT INTO message_delete_capacity_refusals (owner_id,total) VALUES ($1,-1)`, owner)
	messageDeleteReceiptWantSQLState(t, err, "23514")
	if _, err := conn.Exec(ctx, body); err == nil {
		t.Fatal("receipt migration silently reapplied")
	}
}

func TestMessageDeleteReceiptNoChangeGuardMigrationPreservesRetainedRows(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	conn, _ := profilePhotoDatabaseBefore(t, ctx, messageDeleteReceiptNoChangeMigration)
	owner := messageDeleteReceiptOwner(t, ctx, conn, "+15551795022")
	f := messageDeletePendingFixture(t, owner)
	anchor := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	deadline := anchor.Add(97 * 24 * time.Hour)
	f.state, f.localCompletedAt, f.retainUntil = "local_no_change", &anchor, &deadline
	f.operationKey, f.epoch, f.streamID, f.position = nil, nil, nil, nil
	if err := messageDeleteReceiptRawInsert(ctx, conn, f); err != nil {
		t.Fatalf("insert pre-guard no-change receipt: %v", err)
	}
	const capacity = int64(65536)
	setupStarted := time.Now()
	if _, err := conn.Exec(ctx, `INSERT INTO message_delete_receipts (
		owner_id, auth_key_id, session_id, msg_id, method, id_space, revoke, target_ids,
		pts, pts_count, receipt_key, state, local_completed_at, retain_until
	) SELECT $1::bigint,$2,$3,1700000000000+n*4,'messages.deleteMessages','owner_message',true,$4,
		7,2,decode(md5('no-change:' || n::text),'hex'),'local_no_change',
		$5::timestamptz,$5::timestamptz+INTERVAL '8380800 seconds'
	FROM generate_series(1,$6::bigint) AS fixture(n)`,
		owner, f.authKeyID, f.sessionID, f.targets, anchor, capacity-1); err != nil {
		t.Fatalf("populate pre-guard owner at C: %v", err)
	}
	t.Logf("pre-guard disposable owner at C=%d setup=%s", capacity, time.Since(setupStarted))

	body := profilePhotoReadMigration(t, messageDeleteReceiptNoChangeMigration)
	started := time.Now()
	if _, err := conn.Exec(ctx, body); err != nil {
		t.Fatalf("apply no-change validation migration: %v", err)
	}
	t.Logf("validation-only migration applied with C=%d retained rows in %s", capacity, time.Since(started))
	var count, preserved int64
	if err := conn.QueryRow(ctx, `SELECT count(*), count(*) FILTER (
		WHERE state='local_no_change' AND local_completed_at=$2 AND retain_until=$3
			AND operation_key IS NULL AND confirmed_at IS NULL AND target_ids=$4 AND pts=7 AND pts_count=2
	) FROM message_delete_receipts WHERE owner_id=$1`, owner, anchor, deadline, f.targets).Scan(&count, &preserved); err != nil {
		t.Fatalf("read retained rows after migration: %v", err)
	}
	if count != capacity || preserved != capacity {
		t.Fatalf("migration kept %d/%d unchanged rows, want %d", preserved, count, capacity)
	}
	_, err := conn.Exec(ctx, `UPDATE message_delete_receipts SET
		state='confirmed', local_completed_at=NULL, confirmed_at=local_completed_at,
		operation_key=decode('11111111111111111111111111111111','hex'), epoch=3,
		stream_id=decode('22222222222222222222222222222222','hex'), position=11
		WHERE receipt_key=$1`, f.receiptKey)
	messageDeleteReceiptWantSQLState(t, err, "23514")
	_, err = conn.Exec(ctx, `UPDATE message_delete_receipts SET
		local_completed_at=local_completed_at+INTERVAL '1 day', retain_until=retain_until+INTERVAL '1 day'
		WHERE receipt_key=$1`, f.receiptKey)
	messageDeleteReceiptWantSQLState(t, err, "23514")
	if _, err := conn.Exec(ctx, body); err == nil {
		t.Fatal("no-change validation migration silently reapplied")
	}
}

func messageDeletePendingParams(f messageDeleteReceiptFixture) db.InsertMessageDeletePendingReceiptParams {
	return db.InsertMessageDeletePendingReceiptParams{
		OwnerID: f.ownerID, AuthKeyID: f.authKeyID, SessionID: f.sessionID, MsgID: f.msgID,
		Method: f.method, IDSpace: f.idSpace, Revoke: f.revoke, TargetIds: f.targets,
		Pts: f.pts, PtsCount: f.ptsCount, ReceiptKey: f.receiptKey,
		OperationKey: f.operationKey, Epoch: f.epoch, StreamID: f.streamID, Position: f.position,
	}
}

func messageDeleteIdentity(f messageDeleteReceiptFixture) db.MessageDeleteReceiptByOwnerIdentityParams {
	return db.MessageDeleteReceiptByOwnerIdentityParams{
		OwnerID: f.ownerID, AuthKeyID: f.authKeyID, SessionID: f.sessionID, MsgID: f.msgID,
	}
}

func assertMessageDeletePersistenceEmpty(t *testing.T, ctx context.Context, conn db.DBTX, activity string) {
	t.Helper()
	for _, table := range messageDeleteReceiptTables {
		var count int64
		// table is drawn only from the fixed receipt/accounting relation list.
		if err := conn.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&count); err != nil {
			t.Fatalf("%s count after %s: %v", table, activity, err)
		}
		if count != 0 {
			t.Fatalf("%s wrote %d rows into %s", activity, count, table)
		}
	}
}

func TestMessageDeleteReceiptQueriesPreserveResultAndOwnerIsolation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn := profilePhotoConn(t, ctx)
	a := messageDeleteReceiptOwner(t, ctx, conn, "+15551795009")
	b := messageDeleteReceiptOwner(t, ctx, conn, "+15551795010")
	q := db.New(conn)
	f := messageDeletePendingFixture(t, a)
	if _, err := q.InsertMessageDeletePendingReceipt(ctx, messageDeletePendingParams(f)); err != nil {
		t.Fatalf("insert pending through generated access: %v", err)
	}
	duplicate := messageDeletePendingFixture(t, a)
	duplicate.pts, duplicate.ptsCount = 99, 1
	_, err := q.InsertMessageDeletePendingReceipt(ctx, messageDeletePendingParams(duplicate))
	messageDeleteReceiptWantSQLState(t, err, "23505")
	got, err := q.MessageDeleteReceiptByOwnerIdentity(ctx, messageDeleteIdentity(f))
	if err != nil {
		t.Fatalf("read pending result: %v", err)
	}
	if got.OwnerID != a || got.SessionID != math.MinInt64 || got.MsgID != 1700000000000 ||
		!bytes.Equal(got.AuthKeyID, []byte{0xff, 0, 1, 2, 3, 4, 5, 0x80}) ||
		got.Method != "messages.deleteMessages" || got.IDSpace != "owner_message" || !got.Revoke ||
		!slices.Equal(got.TargetIds, []int32{4, 9}) || got.Pts != 7 || got.PtsCount != 2 ||
		!bytes.Equal(got.ReceiptKey, f.receiptKey) || got.State != "pending" ||
		!bytes.Equal(got.OperationKey, f.operationKey) || got.Epoch == nil || *got.Epoch != 3 ||
		!bytes.Equal(got.StreamID, f.streamID) || got.Position == nil || *got.Position != 11 ||
		got.ConfirmedAt.Valid || got.LocalCompletedAt.Valid || got.RetainUntil.Valid {
		t.Fatalf("stored pending receipt lost identity, exact result, or reference: %+v", got)
	}
	otherIdentity := messageDeleteIdentity(f)
	otherIdentity.OwnerID = b
	if _, err := q.MessageDeleteReceiptByOwnerIdentity(ctx, otherIdentity); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("another owner read A's receipt: %v", err)
	}
	other := messageDeletePendingFixture(t, b)
	other.pts, other.ptsCount = 13, 1
	if _, err := q.InsertMessageDeletePendingReceipt(ctx, messageDeletePendingParams(other)); err != nil {
		t.Fatalf("persist B's independent same tuple: %v", err)
	}
	got, err = q.MessageDeleteReceiptByOwnerIdentity(ctx, otherIdentity)
	if err != nil || got.OwnerID != b || got.Pts != 13 || got.PtsCount != 1 || !bytes.Equal(got.ReceiptKey, other.receiptKey) {
		t.Fatalf("B's result = %+v, err %v", got, err)
	}
	for _, owner := range []int64{a, b} {
		if count, err := q.MessageDeleteReceiptOwnerCount(ctx, owner); err != nil || count != 1 {
			t.Fatalf("owner %d count = %d, err %v, want 1", owner, count, err)
		}
	}
	// Each original transport component participates in identity, independently.
	for _, vary := range []func(*messageDeleteReceiptFixture){
		func(f *messageDeleteReceiptFixture) { f.authKeyID = bytes.Repeat([]byte{0x7f}, 8) },
		func(f *messageDeleteReceiptFixture) { f.sessionID++ },
		func(f *messageDeleteReceiptFixture) { f.msgID += 4 },
	} {
		variant := messageDeletePendingFixture(t, a)
		vary(&variant)
		if _, err := q.MessageDeleteReceiptByOwnerIdentity(ctx, messageDeleteIdentity(variant)); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("absent transport identity matched another row: %v", err)
		}
		if _, err := q.InsertMessageDeletePendingReceipt(ctx, messageDeletePendingParams(variant)); err != nil {
			t.Fatalf("distinct transport identity refused: %v", err)
		}
		got, err := q.MessageDeleteReceiptByOwnerIdentity(ctx, messageDeleteIdentity(variant))
		if err != nil || !bytes.Equal(got.ReceiptKey, variant.receiptKey) {
			t.Fatalf("distinct tuple returned wrong receipt: %+v, err %v", got, err)
		}
	}
}

func TestMessageDeleteReceiptNoChangeUsesDatabaseAnchorAndOriginalCount(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn := profilePhotoConn(t, ctx)
	owner := messageDeleteReceiptOwner(t, ctx, conn, "+15551795011")
	q := db.New(conn)
	if _, err := conn.Exec(ctx, `SET TIME ZONE 'Europe/Paris'`); err != nil {
		t.Fatalf("set fixture timezone: %v", err)
	}
	var before, after time.Time
	if err := conn.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&before); err != nil {
		t.Fatalf("read database time before completion: %v", err)
	}
	key := erasureLedgerOpKey(t)
	params := db.InsertMessageDeleteNoChangeReceiptParams{
		OwnerID: owner, AuthKeyID: bytes.Repeat([]byte{0x80}, 8), SessionID: -42, MsgID: 1700000000000,
		Method: "messages.deleteMessages", IDSpace: "owner_message", TargetIds: []int32{}, ReceiptKey: key[:],
	}
	empty, err := q.InsertMessageDeleteNoChangeReceipt(ctx, params)
	if err != nil {
		t.Fatalf("save empty no-change receipt: %v", err)
	}
	if err := conn.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&after); err != nil {
		t.Fatalf("read database time after completion: %v", err)
	}
	if empty.State != "local_no_change" || empty.Pts != 0 || empty.PtsCount != 0 || len(empty.TargetIds) != 0 ||
		!bytes.Equal(empty.ReceiptKey, key[:]) || !empty.LocalCompletedAt.Valid ||
		empty.LocalCompletedAt.Time.Before(before) || empty.LocalCompletedAt.Time.After(after) ||
		empty.ConfirmedAt.Valid || len(empty.OperationKey) != 0 || empty.Epoch != nil || len(empty.StreamID) != 0 || empty.Position != nil ||
		!empty.RetainUntil.Valid || !empty.RetainUntil.Time.Equal(empty.LocalCompletedAt.Time.Add(97*24*time.Hour)) {
		t.Fatalf("no-change lost database completion/floor or gained a capture: %+v", empty)
	}
	// The first raw group vector [4,4] can have normalized [4] and count 2.
	key = erasureLedgerOpKey(t)
	params.MsgID += 4
	params.TargetIds, params.PtsCount, params.ReceiptKey = []int32{4}, 2, key[:]
	longer := after.Add(120 * 24 * time.Hour)
	params.RetainUntil = pgtype.Timestamptz{Time: longer, Valid: true}
	group, err := q.InsertMessageDeleteNoChangeReceipt(ctx, params)
	if err != nil || group.Pts != 0 || group.PtsCount != 2 || !slices.Equal(group.TargetIds, []int32{4}) ||
		!group.RetainUntil.Time.Equal(longer) {
		t.Fatalf("no-change raw count/inventory deadline = %+v, err %v", group, err)
	}
	_, err = q.InsertMessageDeleteNoChangeReceipt(ctx, params)
	messageDeleteReceiptWantSQLState(t, err, "23505")
	got, err := q.MessageDeleteReceiptByOwnerIdentity(ctx, db.MessageDeleteReceiptByOwnerIdentityParams{
		OwnerID: owner, AuthKeyID: params.AuthKeyID, SessionID: params.SessionID, MsgID: params.MsgID,
	})
	if err != nil || got.PtsCount != 2 || got.LocalCompletedAt != group.LocalCompletedAt || got.RetainUntil != group.RetainUntil {
		t.Fatalf("duplicate rewrote completion/result/deadline: %+v, err %v", got, err)
	}
	outbox, markers, completions, counters := erasureLedgerCounts(t, ctx, q)
	if outbox != 0 || markers != 0 || completions != 0 || counters != 0 {
		t.Fatalf("no-change persistence allocated a capture: %d/%d/%d/%d", outbox, markers, completions, counters)
	}
}

func TestMessageDeleteReceiptCountsAllRetainedStates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn := profilePhotoConn(t, ctx)
	a := messageDeleteReceiptOwner(t, ctx, conn, "+15551795012")
	b := messageDeleteReceiptOwner(t, ctx, conn, "+15551795013")
	c := messageDeleteReceiptOwner(t, ctx, conn, "+15551795014")
	q := db.New(conn)
	for i, state := range []string{"pending", "confirmed", "local_no_change"} {
		f := messageDeletePendingFixture(t, a)
		f.msgID += int64(i * 4)
		f.state = state
		anchor := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
		deadline := anchor.Add(97 * 24 * time.Hour)
		switch state {
		case "confirmed":
			f.confirmedAt, f.retainUntil = &anchor, &deadline
		case "local_no_change":
			f.localCompletedAt, f.retainUntil = &anchor, &deadline
			f.operationKey, f.epoch, f.streamID, f.position = nil, nil, nil, nil
		}
		if err := messageDeleteReceiptRawInsert(ctx, conn, f); err != nil {
			t.Fatalf("seed %s fixture: %v", state, err)
		}
		got, err := q.MessageDeleteReceiptByOwnerIdentity(ctx, messageDeleteIdentity(f))
		if err != nil || got.State != state || got.Pts != 7 || got.PtsCount != 2 {
			t.Fatalf("%s lost original caller result: %+v, err %v", state, got, err)
		}
	}
	other := messageDeletePendingFixture(t, b)
	if _, err := q.InsertMessageDeletePendingReceipt(ctx, messageDeletePendingParams(other)); err != nil {
		t.Fatalf("seed B: %v", err)
	}
	for _, tc := range []struct{ owner, want int64 }{{a, 3}, {b, 1}, {c, 0}} {
		if got, err := q.MessageDeleteReceiptOwnerCount(ctx, tc.owner); err != nil || got != tc.want {
			t.Fatalf("owner %d retained count = %d, err %v, want %d", tc.owner, got, err, tc.want)
		}
	}
	if _, err := conn.Exec(ctx, `INSERT INTO message_delete_capacity_refusals (owner_id,total) VALUES ($1,$2),($3,0)`, a, int64(math.MaxInt64), b); err != nil {
		t.Fatalf("seed refusal totals: %v", err)
	}
	for _, tc := range []struct{ owner, want int64 }{{a, math.MaxInt64}, {b, 0}} {
		if got, err := q.MessageDeleteCapacityRefusalByOwner(ctx, tc.owner); err != nil || got != tc.want {
			t.Fatalf("owner %d refusal total = %d, err %v, want %d", tc.owner, got, err, tc.want)
		}
	}
	if _, err := q.MessageDeleteCapacityRefusalByOwner(ctx, c); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("C read another owner's accounting: %v", err)
	}
}

func TestMessageDeleteReceiptPersistenceRollsBack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn := profilePhotoConn(t, ctx)
	owner := messageDeleteReceiptOwner(t, ctx, conn, "+15551795015")
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin disposable transaction: %v", err)
	}
	defer func() {
		if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Errorf("rollback: %v", err)
		}
	}()
	q := db.New(tx)
	f := messageDeletePendingFixture(t, owner)
	if _, err := q.InsertMessageDeletePendingReceipt(ctx, messageDeletePendingParams(f)); err != nil {
		t.Fatalf("insert transactional pending receipt: %v", err)
	}
	key := erasureLedgerOpKey(t)
	if _, err := q.InsertMessageDeleteNoChangeReceipt(ctx, db.InsertMessageDeleteNoChangeReceiptParams{
		OwnerID: owner, AuthKeyID: f.authKeyID, SessionID: f.sessionID, MsgID: f.msgID + 4,
		Method: f.method, IDSpace: f.idSpace, TargetIds: []int32{}, ReceiptKey: key[:],
	}); err != nil {
		t.Fatalf("insert transactional local completion: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO message_delete_capacity_refusals (owner_id,total) VALUES ($1,1)`, owner); err != nil {
		t.Fatalf("insert transactional refusal fixture: %v", err)
	}
	if count, err := q.MessageDeleteReceiptOwnerCount(ctx, owner); err != nil || count != 2 {
		t.Fatalf("uncommitted count = %d, err %v", count, err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("force rollback: %v", err)
	}
	assertMessageDeletePersistenceEmpty(t, ctx, conn, "a rolled-back fixture")
}

func TestMessageDeleteReceiptSurvivesAuthKeyRebindingAndDeletion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	a := mustUser(t, s, "+15551795016")
	b := mustUser(t, s, "+15551795017")
	const keyID = int64(0x1001)
	if err := s.SaveAuthKey(ctx, keyID, []byte("disposable receipt auth key")); err != nil {
		t.Fatalf("save auth key: %v", err)
	}
	if err := s.BindAuthKeyUser(ctx, keyID, a.ID); err != nil {
		t.Fatalf("bind A: %v", err)
	}
	q := db.New(store.StorePool(s))
	f := messageDeletePendingFixture(t, a.ID)
	f.authKeyID = []byte{1, 0x10, 0, 0, 0, 0, 0, 0}
	if _, err := q.InsertMessageDeletePendingReceipt(ctx, messageDeletePendingParams(f)); err != nil {
		t.Fatalf("save A's receipt: %v", err)
	}
	if err := s.BindAuthKeyUser(ctx, keyID, b.ID); err != nil {
		t.Fatalf("rebind key to B: %v", err)
	}
	identityB := messageDeleteIdentity(f)
	identityB.OwnerID = b.ID
	if _, err := q.MessageDeleteReceiptByOwnerIdentity(ctx, identityB); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("rebound owner read A's receipt: %v", err)
	}
	other := messageDeletePendingFixture(t, b.ID)
	other.authKeyID = f.authKeyID
	if _, err := q.InsertMessageDeletePendingReceipt(ctx, messageDeletePendingParams(other)); err != nil {
		t.Fatalf("B cannot persist same tuple after rebinding: %v", err)
	}
	if err := s.DeleteAuthKey(ctx, keyID); err != nil {
		t.Fatalf("delete original auth key: %v", err)
	}
	for _, fixture := range []messageDeleteReceiptFixture{f, other} {
		got, err := q.MessageDeleteReceiptByOwnerIdentity(ctx, messageDeleteIdentity(fixture))
		if err != nil || got.OwnerID != fixture.ownerID || !bytes.Equal(got.ReceiptKey, fixture.receiptKey) || got.Pts != 7 || got.PtsCount != 2 {
			t.Fatalf("auth-key deletion changed receipt: %+v, err %v", got, err)
		}
	}
}

func TestMessageDeleteReceiptOwnerForeignKeysDoNotBlockAccountDeletion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn := profilePhotoConn(t, ctx)
	a := messageDeleteReceiptOwner(t, ctx, conn, "+15551795018")
	b := messageDeleteReceiptOwner(t, ctx, conn, "+15551795019")
	q := db.New(conn)
	for _, owner := range []int64{a, b} {
		if _, err := q.InsertMessageDeletePendingReceipt(ctx, messageDeletePendingParams(messageDeletePendingFixture(t, owner))); err != nil {
			t.Fatalf("seed owner receipt: %v", err)
		}
		if _, err := conn.Exec(ctx, `INSERT INTO message_delete_capacity_refusals (owner_id,total) VALUES ($1,17)`, owner); err != nil {
			t.Fatalf("seed owner accounting: %v", err)
		}
	}
	if _, err := conn.Exec(ctx, `DELETE FROM users WHERE id=$1`, a); err != nil {
		t.Fatalf("new storage blocked existing user deletion: %v", err)
	}
	if count, err := q.MessageDeleteReceiptOwnerCount(ctx, a); err != nil || count != 0 {
		t.Fatalf("deleted owner's receipt remains: %d, err %v", count, err)
	}
	if _, err := q.MessageDeleteCapacityRefusalByOwner(ctx, a); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("deleted owner's accounting remains: %v", err)
	}
	if count, err := q.MessageDeleteReceiptOwnerCount(ctx, b); err != nil || count != 1 {
		t.Fatalf("unrelated owner's receipt changed: %d, err %v", count, err)
	}
	if total, err := q.MessageDeleteCapacityRefusalByOwner(ctx, b); err != nil || total != 17 {
		t.Fatalf("unrelated owner's accounting changed: %d, err %v", total, err)
	}
	// The cascades do not admit orphan rows after account deletion.
	_, err := q.InsertMessageDeletePendingReceipt(ctx, messageDeletePendingParams(messageDeletePendingFixture(t, a)))
	messageDeleteReceiptWantSQLState(t, err, "23503")
	_, err = conn.Exec(ctx, `INSERT INTO message_delete_capacity_refusals (owner_id,total) VALUES ($1,0)`, a)
	messageDeleteReceiptWantSQLState(t, err, "23503")
}

func TestMessageDeleteReceiptFreshSchemaExposesOnlyAcceptedLocalFields(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn := erasureLedgerConn(t, ctx)
	rows, err := conn.Query(ctx, `SELECT table_name || '.' || column_name || ':' || data_type
		FROM information_schema.columns WHERE table_schema='public' AND table_name=ANY($1)
		ORDER BY table_name, column_name`, messageDeleteReceiptTables)
	if err != nil {
		t.Fatalf("read receipt/accounting schema: %v", err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			t.Fatalf("scan local column: %v", err)
		}
		columns = append(columns, column)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read local columns: %v", err)
	}
	want := []string{
		"message_delete_capacity_refusals.owner_id:bigint", "message_delete_capacity_refusals.total:bigint",
		"message_delete_receipts.auth_key_id:bytea", "message_delete_receipts.confirmed_at:timestamp with time zone",
		"message_delete_receipts.epoch:bigint", "message_delete_receipts.id_space:text",
		"message_delete_receipts.local_completed_at:timestamp with time zone", "message_delete_receipts.method:text",
		"message_delete_receipts.msg_id:bigint", "message_delete_receipts.operation_key:bytea",
		"message_delete_receipts.owner_id:bigint", "message_delete_receipts.position:bigint",
		"message_delete_receipts.pts:integer", "message_delete_receipts.pts_count:integer",
		"message_delete_receipts.receipt_key:bytea", "message_delete_receipts.retain_until:timestamp with time zone",
		"message_delete_receipts.revoke:boolean", "message_delete_receipts.session_id:bigint",
		"message_delete_receipts.state:text", "message_delete_receipts.stream_id:bytea", "message_delete_receipts.target_ids:ARRAY",
	}
	if !slices.Equal(columns, want) {
		t.Fatalf("local persistence gained or lost fields:\ngot: %s\nwant: %s", strings.Join(columns, ","), strings.Join(want, ","))
	}
	assertMessageDeletePersistenceEmpty(t, ctx, conn, "fresh schema replay")
}
