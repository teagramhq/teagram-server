package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// beginProfilePhotoMutation begins a transaction after taking the existing
// profile-domain advisory lock. It never touches the messaging owner lock, and
// both pool admission and lock contention share the same bounded wait.
func (s *Store) beginProfilePhotoMutation(ctx context.Context, ownerID int64) (*pgxpool.Conn, pgx.Tx, error) {
	return s.beginProfilePhotoMutationWithWait(ctx, ownerID, ProfilePhotoSelectionWait)
}

func (s *Store) beginProfilePhotoMutationWithWait(
	ctx context.Context, ownerID int64, wait time.Duration,
) (*pgxpool.Conn, pgx.Tx, error) {
	class, object, ok := profileOwnerLockKey(ownerID)
	if !ok {
		return nil, nil, ErrProfilePhotoUnavailable
	}
	lockCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	lockWaitExpired := func() bool {
		return ctx.Err() == nil && errors.Is(lockCtx.Err(), context.DeadlineExceeded)
	}
	busyOr := func(err error) error {
		switch {
		case lockWaitExpired():
			return ErrProfilePhotoBusy
		case ctx.Err() != nil:
			return ctx.Err()
		default:
			return err
		}
	}

	conn, err := s.pool.Acquire(lockCtx)
	if err != nil {
		if lockWaitExpired() {
			return nil, nil, ErrProfilePhotoBusy
		}
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		return nil, nil, fmt.Errorf("profile photo mutation: acquire connection: %w", err)
	}

	tx, err := conn.Begin(lockCtx)
	if err != nil {
		conn.Release()
		if lockWaitExpired() {
			return nil, nil, ErrProfilePhotoBusy
		}
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		return nil, nil, fmt.Errorf("profile photo mutation: begin transaction: %w", err)
	}

	for {
		var acquired bool
		err := tx.QueryRow(lockCtx, `SELECT pg_try_advisory_xact_lock($1::int, $2::int)`, class, object).Scan(&acquired)
		if err != nil {
			rollbackErr := tx.Rollback(context.Background())
			conn.Release()
			if mapped := busyOr(err); errors.Is(mapped, ErrProfilePhotoBusy) || errors.Is(mapped, context.Canceled) || errors.Is(mapped, context.DeadlineExceeded) {
				return nil, nil, mapped
			}
			if rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
				return nil, nil, fmt.Errorf("profile photo mutation: acquire profile-domain lock: %w; rollback: %w", err, rollbackErr)
			}
			return nil, nil, fmt.Errorf("profile photo mutation: acquire profile-domain lock: %w", err)
		}
		if acquired {
			return conn, tx, nil
		}

		timer := time.NewTimer(profilePhotoSelectionPoll)
		select {
		case <-lockCtx.Done():
			timer.Stop()
			rollbackErr := tx.Rollback(context.Background())
			conn.Release()
			if ctx.Err() != nil {
				return nil, nil, ctx.Err()
			}
			if rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
				return nil, nil, fmt.Errorf("profile photo mutation: lock wait expired: %w; rollback: %w", ErrProfilePhotoBusy, rollbackErr)
			}
			return nil, nil, ErrProfilePhotoBusy
		case <-timer.C:
		}
	}
}
