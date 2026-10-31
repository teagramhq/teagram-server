package store

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/teagramhq/teagram-server/internal/store/db"
)

func TestDialogPageQueriesUseCacheDescribeMode(t *testing.T) {
	wantErr := errors.New("stop after capturing query")
	recorder := &dialogPageQueryRecorder{err: wantErr}
	queries := db.New(dialogPageQueryExecModeDBTX{DBTX: recorder})

	_, err := queries.DialogsFirstPageWithPins(t.Context(), db.DialogsFirstPageWithPinsParams{
		OwnerID: 42, Lim: 10,
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("query error = %v, want %v", err, wantErr)
	}
	if len(recorder.args) != 4 {
		t.Fatalf("query args = %v, want query mode plus three parameters", recorder.args)
	}
	if got := recorder.args[0]; got != pgx.QueryExecModeCacheDescribe {
		t.Fatalf("query mode = %v, want %v", got, pgx.QueryExecModeCacheDescribe)
	}
	if got := recorder.args[1:]; got[0] != int64(42) || got[1] != false || got[2] != int32(10) {
		t.Fatalf("query parameters = %v, want [42 false 10]", got)
	}
}

type dialogPageQueryRecorder struct {
	args []any
	err  error
}

func (*dialogPageQueryRecorder) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	panic("unexpected Exec")
}

func (r *dialogPageQueryRecorder) Query(_ context.Context, _ string, args ...any) (pgx.Rows, error) {
	r.args = append([]any(nil), args...)
	return nil, r.err
}

func (*dialogPageQueryRecorder) QueryRow(context.Context, string, ...any) pgx.Row {
	panic("unexpected QueryRow")
}
