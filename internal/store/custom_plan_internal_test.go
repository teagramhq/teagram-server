package store

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type recordingDBTX struct {
	args [][]any
}

func (r *recordingDBTX) Exec(_ context.Context, _ string, args ...any) (pgconn.CommandTag, error) {
	r.args = append(r.args, args)
	return pgconn.CommandTag{}, nil
}

func (r *recordingDBTX) Query(_ context.Context, _ string, args ...any) (pgx.Rows, error) {
	r.args = append(r.args, args)
	return nil, nil //nolint:nilnil // the recorder only inspects arguments
}

func (r *recordingDBTX) QueryRow(_ context.Context, _ string, args ...any) pgx.Row {
	r.args = append(r.args, args)
	return nil
}

func TestCustomPlanDBRunsEveryStatementUnprepared(t *testing.T) {
	t.Parallel()
	recorded := &recordingDBTX{}
	conn := customPlanDB{recorded}
	ctx := t.Context()
	if _, err := conn.Exec(ctx, "SELECT 1", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Query(ctx, "SELECT 1", 1); err != nil {
		t.Fatal(err)
	}
	_ = conn.QueryRow(ctx, "SELECT 1", 1)
	if len(recorded.args) != 3 {
		t.Fatalf("recorded %d calls, want 3", len(recorded.args))
	}
	for i, args := range recorded.args {
		if len(args) != 2 || args[0] != pgx.QueryExecModeExec || args[1] != 1 {
			t.Errorf("call %d args = %v, want exec mode followed by the query args", i, args)
		}
	}
}
