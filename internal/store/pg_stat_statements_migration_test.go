package store_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/pgtest"
)

func TestMigrationsInstallPgStatStatements(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, pgtest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := conn.Close(ctx); err != nil {
			t.Error(err)
		}
	})

	var version int
	if err := conn.QueryRow(ctx, `SELECT current_setting('server_version_num')::int`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if major := version / 10000; major != 18 {
		t.Fatalf("pgtest server major version = %d, want 18", major)
	}

	var installed bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'pg_stat_statements')`).Scan(&installed); err != nil {
		t.Fatal(err)
	}
	if !installed {
		t.Fatal("pg_stat_statements extension is not installed")
	}

	var probeCanRead bool
	if err := conn.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM pg_class AS relation
			JOIN pg_namespace AS namespace ON namespace.oid = relation.relnamespace
			WHERE namespace.nspname = 'public'
			  AND relation.relname IN ('pg_stat_statements', 'pg_stat_statements_info')
			  AND has_table_privilege('browser_acceptance_probe', relation.oid, 'SELECT')
		) OR EXISTS (
			SELECT 1
			FROM pg_proc AS routine
			JOIN pg_namespace AS namespace ON namespace.oid = routine.pronamespace
			WHERE namespace.nspname = 'public'
			  AND routine.proname IN ('pg_stat_statements', 'pg_stat_statements_info')
			  AND has_function_privilege('browser_acceptance_probe', routine.oid, 'EXECUTE')
		)
	`).Scan(&probeCanRead); err != nil {
		t.Fatal(err)
	}
	if probeCanRead {
		t.Fatal("browser acceptance probe can read pg_stat_statements")
	}

	var statsRoleCanRead bool
	if err := conn.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM pg_class AS relation
			JOIN pg_namespace AS namespace ON namespace.oid = relation.relnamespace
			WHERE namespace.nspname = 'public'
			  AND relation.relname IN ('pg_stat_statements', 'pg_stat_statements_info')
			  AND has_table_privilege('pg_read_all_stats', relation.oid, 'SELECT')
		) AND EXISTS (
			SELECT 1
			FROM pg_proc AS routine
			JOIN pg_namespace AS namespace ON namespace.oid = routine.pronamespace
			WHERE namespace.nspname = 'public'
			  AND routine.proname IN ('pg_stat_statements', 'pg_stat_statements_info')
			  AND has_function_privilege('pg_read_all_stats', routine.oid, 'EXECUTE')
		)
	`).Scan(&statsRoleCanRead); err != nil {
		t.Fatal(err)
	}
	if !statsRoleCanRead {
		t.Fatal("pg_read_all_stats cannot read pg_stat_statements")
	}
}
