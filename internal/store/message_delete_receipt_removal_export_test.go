package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/store/db"
)

// MessageDeleteReceiptRemovalForTest is the disposable removal transaction
// boundary. Begin executes the conditional removal and leaves its transaction
// open, so a test orders retry lookup and admission against the uncommitted
// removal and observes the exact moment Commit or Rollback returns.
type MessageDeleteReceiptRemovalForTest struct {
	tx      pgx.Tx
	pid     uint32
	removed bool
}

// BeginMessageDeleteReceiptRemovalForTest runs the conditional removal in a
// transaction of its own. A nil clock uses the database transaction clock.
func BeginMessageDeleteReceiptRemovalForTest(
	ctx context.Context,
	s *Store,
	identity MessageDeleteReceiptLookupIdentity,
	inventoryOutcome string,
	inventoryDays int32,
	clock *time.Time,
) (*MessageDeleteReceiptRemovalForTest, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin receipt removal: %w", err)
	}
	removal := &MessageDeleteReceiptRemovalForTest{tx: tx, pid: tx.Conn().PgConn().PID()}
	params := db.RemoveEligibleMessageDeleteReceiptParams{
		OwnerID: identity.OwnerID, AuthKeyID: identity.AuthKeyID[:],
		SessionID: identity.SessionID, MsgID: identity.MsgID,
		InventoryOutcome: inventoryOutcome, InventoryDays: inventoryDays,
	}
	if clock != nil {
		nowUnixNano := clock.UnixNano()
		params.TestClockUnixNano = &nowUnixNano
	}
	rows, err := s.q.WithTx(tx).RemoveEligibleMessageDeleteReceipt(ctx, params)
	if err != nil {
		_ = tx.Rollback(ctx) //nolint:errcheck // best effort on the error path
		return nil, fmt.Errorf("remove message delete receipt: %w", err)
	}
	removal.removed = rows == 1
	return removal, nil
}

// Removed reports whether the uncommitted removal deleted the receipt.
func (r *MessageDeleteReceiptRemovalForTest) Removed() bool { return r.removed }

// BackendPID names the removal's database backend for lock inspection.
func (r *MessageDeleteReceiptRemovalForTest) BackendPID() uint32 { return r.pid }

func (r *MessageDeleteReceiptRemovalForTest) Commit(ctx context.Context) error {
	return r.tx.Commit(ctx)
}

func (r *MessageDeleteReceiptRemovalForTest) Rollback(ctx context.Context) error {
	return r.tx.Rollback(ctx)
}
