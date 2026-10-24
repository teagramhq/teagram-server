package store

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
)

type rollbackResultTx struct {
	pgx.Tx

	err error
}

func (tx rollbackResultTx) Rollback(context.Context) error {
	return tx.err
}

func TestRollbackNeverBoundAuthKeySweepBatchPreservesDeleteAndRollbackErrors(t *testing.T) {
	deleteErr := errors.New("delete failed")
	rollbackErr := errors.New("rollback failed")

	err := rollbackNeverBoundAuthKeySweepBatch(rollbackResultTx{err: rollbackErr}, deleteErr)
	if !errors.Is(err, deleteErr) {
		t.Errorf("error %v does not preserve delete failure", err)
	}
	if !errors.Is(err, rollbackErr) {
		t.Errorf("error %v does not report rollback failure", err)
	}
}

func TestRollbackNeverBoundAuthKeySweepBatchIgnoresClosedTransaction(t *testing.T) {
	deleteErr := errors.New("delete failed")

	err := rollbackNeverBoundAuthKeySweepBatch(rollbackResultTx{err: pgx.ErrTxClosed}, deleteErr)
	if !errors.Is(err, deleteErr) {
		t.Errorf("error %v does not preserve delete failure", err)
	}
	if errors.Is(err, pgx.ErrTxClosed) {
		t.Errorf("error %v includes ignored closed-transaction result", err)
	}
}
