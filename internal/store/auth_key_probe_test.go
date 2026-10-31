package store_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestBrowserAcceptanceProbeRoleCanOnlyReadBoundedAuthKeyData(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	defer func() { _ = admin.Close(ctx) }() //nolint:errcheck // best-effort close

	var canLogin, superuser, createDB, createRole, replication, bypassRLS, inherit bool
	err = admin.QueryRow(ctx, `
		SELECT rolcanlogin, rolsuper, rolcreatedb, rolcreaterole, rolreplication, rolbypassrls, rolinherit
		FROM pg_roles
		WHERE rolname = 'browser_acceptance_probe'
	`).Scan(&canLogin, &superuser, &createDB, &createRole, &replication, &bypassRLS, &inherit)
	if err != nil {
		t.Fatalf("read browser acceptance probe role: %v", err)
	}
	if !canLogin || superuser || createDB || createRole || replication || bypassRLS || inherit {
		t.Fatalf("browser acceptance probe role attributes: LOGIN=%v SUPERUSER=%v CREATEDB=%v CREATEROLE=%v REPLICATION=%v BYPASSRLS=%v INHERIT=%v", canLogin, superuser, createDB, createRole, replication, bypassRLS, inherit)
	}

	var hasTablePrivileges, hasSequencePrivileges, hasDefaultPrivileges bool
	if err := admin.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM pg_class AS relation
			JOIN pg_namespace AS namespace ON namespace.oid = relation.relnamespace
			WHERE namespace.nspname <> 'information_schema'
			  AND left(namespace.nspname, 3) <> 'pg_'
			  AND relation.relkind IN ('r', 'p', 'v', 'm', 'f')
			  AND (
				  has_table_privilege('browser_acceptance_probe', relation.oid, 'SELECT')
				  OR has_table_privilege('browser_acceptance_probe', relation.oid, 'INSERT')
				  OR has_table_privilege('browser_acceptance_probe', relation.oid, 'UPDATE')
				  OR has_table_privilege('browser_acceptance_probe', relation.oid, 'DELETE')
			  )
		), EXISTS (
			SELECT 1
			FROM pg_class AS relation
			JOIN pg_namespace AS namespace ON namespace.oid = relation.relnamespace
			WHERE namespace.nspname <> 'information_schema'
			  AND left(namespace.nspname, 3) <> 'pg_'
			  AND relation.relkind = 'S'
			  AND (
				  has_sequence_privilege('browser_acceptance_probe', relation.oid, 'USAGE')
				  OR has_sequence_privilege('browser_acceptance_probe', relation.oid, 'SELECT')
				  OR has_sequence_privilege('browser_acceptance_probe', relation.oid, 'UPDATE')
			  )
		), EXISTS (
			SELECT 1
			FROM pg_default_acl AS defaults
			CROSS JOIN LATERAL aclexplode(defaults.defaclacl) AS privilege
			WHERE privilege.grantee = (
				SELECT oid FROM pg_roles WHERE rolname = 'browser_acceptance_probe'
			)
		)
	`).Scan(&hasTablePrivileges, &hasSequencePrivileges, &hasDefaultPrivileges); err != nil {
		t.Fatalf("check probe role privileges: %v", err)
	}
	if hasTablePrivileges || hasSequencePrivileges || hasDefaultPrivileges {
		t.Fatalf("probe role retains privileges: table=%v sequence=%v default=%v", hasTablePrivileges, hasSequencePrivileges, hasDefaultPrivileges)
	}

	var functionCount, safeFunctionCount int
	if err := admin.QueryRow(ctx, `
		SELECT count(*), count(*) FILTER (WHERE
			p.pronargs = 1
			AND p.proargtypes[0] = 'bigint'::regtype
			AND p.provolatile = 's'
			AND p.prosecdef
			AND p.proconfig @> ARRAY['search_path=pg_catalog']
			AND NOT EXISTS (
				SELECT 1
				FROM aclexplode(COALESCE(p.proacl, acldefault('f', p.proowner))) AS privilege
				WHERE privilege.grantee = 0 AND privilege.privilege_type = 'EXECUTE'
			)
		)
		FROM pg_proc AS p
		JOIN pg_namespace AS namespace ON namespace.oid = p.pronamespace
		WHERE namespace.nspname = 'public'
		  AND p.proname IN (
			  'browser_acceptance_auth_key_bound_count',
			  'browser_acceptance_pending_sign_in_count',
			  'browser_acceptance_bound_auth_key_ids'
		  )
	`).Scan(&functionCount, &safeFunctionCount); err != nil {
		t.Fatalf("check probe functions: %v", err)
	}
	if functionCount != 3 || safeFunctionCount != 3 {
		t.Fatalf("safe probe functions = %d/%d, want 3/3", safeFunctionCount, functionCount)
	}

	users, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithoutBlobStore())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := users.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	user, err := users.CreateUser(ctx, "+1555"+pgtest.RandomHex())
	if err != nil {
		t.Fatalf("create probe user: %v", err)
	}
	otherUser, err := users.CreateUser(ctx, "+1555"+pgtest.RandomHex())
	if err != nil {
		t.Fatalf("create unrelated user: %v", err)
	}
	userID, otherUserID := user.ID, otherUser.ID
	createdAt1 := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	createdAt2 := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	for _, key := range []struct {
		id        int64
		userID    *int64
		pendingID *int64
		createdAt time.Time
	}{
		{id: 91001, userID: &userID, createdAt: createdAt1},
		{id: 91002, userID: &userID, createdAt: createdAt2},
		{id: 91003, userID: &otherUserID, createdAt: createdAt1},
		{id: 91004, pendingID: &userID, createdAt: createdAt2},
	} {
		if _, err := admin.Exec(ctx, `
			INSERT INTO auth_keys (id, key_value, user_id, ever_bound_at, pending_user_id, created_at)
			VALUES ($1, $2, $3, CASE WHEN $3::bigint IS NULL THEN NULL::timestamptz ELSE $5::timestamptz END, $4, $5)
		`, key.id, []byte("test key material"), key.userID, key.pendingID, key.createdAt); err != nil {
			t.Fatalf("insert auth key %d: %v", key.id, err)
		}
	}

	probe, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("probe connect: %v", err)
	}
	defer func() { _ = probe.Close(ctx) }() //nolint:errcheck // best-effort close
	if _, err := probe.Exec(ctx, `SET ROLE browser_acceptance_probe`); err != nil {
		t.Fatalf("set probe role: %v", err)
	}
	if _, err := probe.Exec(ctx, `SELECT key_value FROM public.auth_keys LIMIT 1`); err == nil {
		t.Fatal("probe role read auth_keys directly")
	} else {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Fatalf("direct auth_keys read error = %v, want permission denied", err)
		}
	}

	var boundCount, pendingCount int64
	if err := probe.QueryRow(ctx, `SELECT public.browser_acceptance_auth_key_bound_count($1)`, userID).Scan(&boundCount); err != nil {
		t.Fatalf("read bound auth-key count: %v", err)
	}
	if boundCount != 2 {
		t.Errorf("bound auth-key count = %d, want 2", boundCount)
	}
	if err := probe.QueryRow(ctx, `SELECT public.browser_acceptance_pending_sign_in_count($1)`, userID).Scan(&pendingCount); err != nil {
		t.Fatalf("read pending sign-in count: %v", err)
	}
	if pendingCount != 1 {
		t.Errorf("pending sign-in count = %d, want 1", pendingCount)
	}

	rows, err := probe.Query(ctx, `
		SELECT id, created_at
		FROM public.browser_acceptance_bound_auth_key_ids($1)
		ORDER BY id
	`, userID)
	if err != nil {
		t.Fatalf("read bound auth-key IDs: %v", err)
	}
	defer rows.Close()
	want := []struct {
		id        int64
		createdAt time.Time
	}{{91001, createdAt1}, {91002, createdAt2}}
	for i, expected := range want {
		if !rows.Next() {
			t.Fatalf("bound auth-key IDs ended after %d rows: %v", i, rows.Err())
		}
		var id int64
		var createdAt time.Time
		if err := rows.Scan(&id, &createdAt); err != nil {
			t.Fatalf("scan bound auth-key row %d: %v", i, err)
		}
		if id != expected.id || !createdAt.Equal(expected.createdAt) {
			t.Errorf("bound auth-key row %d = (%d, %s), want (%d, %s)", i, id, createdAt, expected.id, expected.createdAt)
		}
	}
	if rows.Next() {
		var id int64
		var createdAt time.Time
		if err := rows.Scan(&id, &createdAt); err != nil {
			t.Fatalf("scan unexpected bound auth-key row: %v", err)
		}
		t.Errorf("unexpected auth-key ID %d for user %d", id, userID)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate bound auth-key IDs: %v", err)
	}
}

func TestBrowserAcceptanceProbeMigrationRejectsOwnedObjects(t *testing.T) {
	// Migration 79 changes a cluster-wide role, so keep its replays serial with
	// the other tests that apply migrations to disposable databases.
	migration, err := os.ReadFile(filepath.Join("..", "..", "migrations", "20261013000079_browser_acceptance_probe.sql"))
	if err != nil {
		t.Fatalf("read probe migration: %v", err)
	}
	// Migration attempts share the cluster-wide probe role, so run cases serially.
	for _, test := range []struct {
		name   string
		setup  string
		object string
	}{
		{"table with implicit privileges", `ALTER TABLE public.auth_keys OWNER TO browser_acceptance_probe`, "auth_keys"},
		{"table with explicit privileges", `ALTER TABLE public.auth_keys OWNER TO browser_acceptance_probe; GRANT SELECT ON public.auth_keys TO CURRENT_USER`, "auth_keys"},
		{"view", `CREATE VIEW public.browser_acceptance_owned_view AS SELECT key_value FROM public.auth_keys; ALTER VIEW public.browser_acceptance_owned_view OWNER TO browser_acceptance_probe`, "browser_acceptance_owned_view"},
		{"sequence", `CREATE SEQUENCE public.browser_acceptance_owned_sequence; ALTER SEQUENCE public.browser_acceptance_owned_sequence OWNER TO browser_acceptance_probe`, "browser_acceptance_owned_sequence"},
		{"function", `CREATE FUNCTION public.browser_acceptance_owned_function() RETURNS INTEGER LANGUAGE SQL AS 'SELECT 1'; ALTER FUNCTION public.browser_acceptance_owned_function() OWNER TO browser_acceptance_probe`, "browser_acceptance_owned_function"},
		{"schema", `CREATE SCHEMA browser_acceptance_owned_schema AUTHORIZATION browser_acceptance_probe`, "browser_acceptance_owned_schema"},
		{"type", `CREATE TYPE public.browser_acceptance_owned_type AS ENUM ('test'); ALTER TYPE public.browser_acceptance_owned_type OWNER TO browser_acceptance_probe`, "browser_acceptance_owned_type"},
		{"database", "", "database"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			conn, err := pgx.Connect(ctx, pgtest.DSN(t))
			if err != nil {
				t.Fatalf("connect to ownership regression database: %v", err)
			}
			defer func() { _ = conn.Close(ctx) }() //nolint:errcheck // best-effort close
			if _, err := conn.Exec(ctx, `
				DROP FUNCTION public.browser_acceptance_auth_key_bound_count(BIGINT);
				DROP FUNCTION public.browser_acceptance_pending_sign_in_count(BIGINT);
				DROP FUNCTION public.browser_acceptance_bound_auth_key_ids(BIGINT)
			`); err != nil {
				t.Fatalf("remove probe functions before reapplying migration: %v", err)
			}
			setup := test.setup
			if test.name == "database" {
				var database string
				if err := conn.QueryRow(ctx, `SELECT current_database()`).Scan(&database); err != nil {
					t.Fatalf("read regression database name: %v", err)
				}
				setup = `ALTER DATABASE ` + database + ` OWNER TO browser_acceptance_probe`
			}
			if _, err := conn.Exec(ctx, setup); err != nil {
				t.Fatalf("give probe ownership of %s: %v", test.object, err)
			}
			if test.name == "table with implicit privileges" {
				var defaultACL bool
				if err := conn.QueryRow(ctx, `SELECT relacl IS NULL FROM pg_class WHERE oid = 'public.auth_keys'::regclass`).Scan(&defaultACL); err != nil {
					t.Fatalf("check implicit table privileges: %v", err)
				}
				if !defaultACL {
					t.Fatal("ownership fixture has explicit table privileges")
				}
				if _, err := conn.Exec(ctx, `SET ROLE browser_acceptance_probe; SELECT key_value FROM public.auth_keys; RESET ROLE`); err != nil {
					t.Fatalf("read credential column through implicit ownership privileges: %v", err)
				}
			}
			_, err = conn.Exec(ctx, string(migration))
			if err == nil {
				t.Fatalf("probe migration accepted ownership of %s", test.object)
			}
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "55000" ||
				!strings.Contains(pgErr.Message, "browser_acceptance_probe owns") ||
				!strings.Contains(pgErr.Message, test.object) ||
				!strings.Contains(pgErr.Hint, "ownership") {
				t.Fatalf("owned %s migration error = %v, want ownership refusal with recovery hint", test.object, err)
			}
		})
	}
}

func TestBrowserAcceptanceProbeMigrationWorksWithExistingRole(t *testing.T) {
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, pgtest.AdminDSN())
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	defer func() { _ = admin.Close(ctx) }() //nolint:errcheck // best-effort close
	t.Cleanup(lockMigrationReplays(t, ctx))
	parentRole := "browser_probe_parent_" + pgtest.RandomHex()
	memberRole := "browser_probe_member_" + pgtest.RandomHex()
	defaultGrantorRole := "browser_probe_grantor_" + pgtest.RandomHex()
	delegateRole := "browser_probe_delegate_" + pgtest.RandomHex()
	for _, role := range []string{parentRole, memberRole, defaultGrantorRole, delegateRole} {
		if _, err := admin.Exec(ctx, `CREATE ROLE `+role); err != nil {
			t.Fatalf("create temporary membership role %s: %v", role, err)
		}
	}
	t.Cleanup(func() {
		conn, err := pgx.Connect(context.Background(), pgtest.AdminDSN())
		if err != nil {
			t.Logf("membership cleanup connect: %v", err)
			return
		}
		defer func() { _ = conn.Close(context.Background()) }() //nolint:errcheck // best-effort close
		for _, statement := range []string{
			`REVOKE ` + parentRole + ` FROM ` + delegateRole + ` GRANTED BY ` + memberRole,
			`REVOKE ` + parentRole + ` FROM ` + memberRole + ` GRANTED BY browser_acceptance_probe`,
			`REVOKE browser_acceptance_probe FROM ` + memberRole + ` GRANTED BY ` + defaultGrantorRole,
			`REVOKE browser_acceptance_probe FROM ` + defaultGrantorRole + ` CASCADE`,
			`REVOKE ` + parentRole + ` FROM browser_acceptance_probe GRANTED BY ` + defaultGrantorRole,
			`REVOKE ` + parentRole + ` FROM ` + memberRole + ` GRANTED BY ` + defaultGrantorRole,
			`REVOKE ` + parentRole + ` FROM ` + defaultGrantorRole,
			`REVOKE ` + parentRole + ` FROM browser_acceptance_probe`,
			`REVOKE browser_acceptance_probe FROM ` + memberRole,
			`DROP ROLE IF EXISTS ` + parentRole,
			`DROP ROLE IF EXISTS ` + memberRole,
			`DROP ROLE IF EXISTS ` + defaultGrantorRole,
			`DROP ROLE IF EXISTS ` + delegateRole,
		} {
			if _, err := conn.Exec(context.Background(), statement); err != nil {
				t.Logf("membership cleanup %q: %v", statement, err)
			}
		}
	})

	database := "t_" + pgtest.RandomHex()
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+database); err != nil {
		t.Fatalf("create migration database: %v", err)
	}
	pgtest.RegisterDatabaseCleanup(t, database)

	migrationName := "20261013000079_browser_acceptance_probe.sql"
	entries, err := os.ReadDir(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	conn, err := pgx.Connect(ctx, pgtest.DSNFrom(database))
	if err != nil {
		t.Fatalf("connect to migration database: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }() //nolint:errcheck // best-effort close
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") || entry.Name() >= migrationName {
			continue
		}
		body, err := os.ReadFile(filepath.Join("..", "..", "migrations", entry.Name()))
		if err != nil {
			t.Fatalf("read migration %s: %v", entry.Name(), err)
		}
		if _, err := conn.Exec(ctx, string(body)); err != nil {
			t.Fatalf("apply migration %s: %v", entry.Name(), err)
		}
	}

	for _, statement := range []string{
		`CREATE TABLE public.browser_acceptance_probe_column_table (key_value BYTEA)`,
		`CREATE SEQUENCE public.browser_acceptance_probe_test_sequence`,
		`CREATE FUNCTION public.browser_acceptance_probe_test_function() RETURNS INTEGER LANGUAGE SQL AS 'SELECT 1'`,
		`REVOKE EXECUTE ON FUNCTION public.browser_acceptance_probe_test_function() FROM PUBLIC`,
		`GRANT SELECT ON auth_keys TO browser_acceptance_probe`,
		`GRANT USAGE, SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA public TO browser_acceptance_probe`,
		`CREATE VIEW public.browser_acceptance_probe_test_view AS SELECT id FROM auth_keys`,
		`GRANT SELECT ON public.browser_acceptance_probe_test_view TO browser_acceptance_probe`,
		`CREATE SCHEMA pgdata`,
		`CREATE VIEW pgdata.browser_acceptance_probe_credentials AS SELECT key_value FROM public.auth_keys`,
		`GRANT USAGE ON SCHEMA pgdata TO browser_acceptance_probe, ` + memberRole,
		`GRANT SELECT ON pgdata.browser_acceptance_probe_credentials TO browser_acceptance_probe, ` + memberRole,
		`ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON TABLES TO browser_acceptance_probe`,
		`ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE, SELECT, UPDATE ON SEQUENCES TO browser_acceptance_probe`,
		`ALTER DEFAULT PRIVILEGES FOR ROLE ` + defaultGrantorRole + ` GRANT ALL ON TABLES TO browser_acceptance_probe WITH GRANT OPTION`,
		`ALTER DEFAULT PRIVILEGES FOR ROLE ` + defaultGrantorRole + ` GRANT ALL ON SEQUENCES TO browser_acceptance_probe WITH GRANT OPTION`,
		`ALTER DEFAULT PRIVILEGES FOR ROLE ` + defaultGrantorRole + ` GRANT ALL ON FUNCTIONS TO browser_acceptance_probe WITH GRANT OPTION`,
		`ALTER DEFAULT PRIVILEGES FOR ROLE ` + defaultGrantorRole + ` GRANT ALL ON TYPES TO browser_acceptance_probe WITH GRANT OPTION`,
		`ALTER DEFAULT PRIVILEGES FOR ROLE ` + defaultGrantorRole + ` GRANT ALL ON SCHEMAS TO browser_acceptance_probe WITH GRANT OPTION`,
		`ALTER DEFAULT PRIVILEGES FOR ROLE ` + defaultGrantorRole + ` IN SCHEMA public GRANT ALL ON TABLES TO browser_acceptance_probe WITH GRANT OPTION`,
		`ALTER DEFAULT PRIVILEGES FOR ROLE ` + defaultGrantorRole + ` IN SCHEMA public GRANT ALL ON SEQUENCES TO browser_acceptance_probe WITH GRANT OPTION`,
		`ALTER DEFAULT PRIVILEGES FOR ROLE ` + defaultGrantorRole + ` IN SCHEMA public GRANT ALL ON FUNCTIONS TO browser_acceptance_probe WITH GRANT OPTION`,
		`ALTER DEFAULT PRIVILEGES FOR ROLE ` + defaultGrantorRole + ` IN SCHEMA public GRANT ALL ON TYPES TO browser_acceptance_probe WITH GRANT OPTION`,
		`ALTER DEFAULT PRIVILEGES FOR ROLE ` + defaultGrantorRole + ` IN SCHEMA public GRANT SELECT ON TABLES TO ` + memberRole,
		`GRANT SELECT ON auth_keys TO ` + defaultGrantorRole + ` WITH GRANT OPTION`,
		`GRANT SELECT ON public.browser_acceptance_probe_test_view TO ` + defaultGrantorRole + ` WITH GRANT OPTION`,
		`GRANT USAGE, SELECT, UPDATE ON SEQUENCE public.browser_acceptance_probe_test_sequence TO ` + defaultGrantorRole + ` WITH GRANT OPTION`,
		`GRANT EXECUTE ON FUNCTION public.browser_acceptance_probe_test_function() TO ` + defaultGrantorRole + ` WITH GRANT OPTION`,
		`GRANT CREATE ON SCHEMA public TO ` + defaultGrantorRole + ` WITH GRANT OPTION`,
		`GRANT CREATE ON DATABASE ` + database + ` TO ` + defaultGrantorRole + ` WITH GRANT OPTION`,
		`SET ROLE ` + defaultGrantorRole,
		`GRANT SELECT ON auth_keys TO browser_acceptance_probe, ` + memberRole,
		`GRANT SELECT ON public.browser_acceptance_probe_test_view TO browser_acceptance_probe, ` + memberRole,
		`GRANT USAGE, SELECT, UPDATE ON SEQUENCE public.browser_acceptance_probe_test_sequence TO browser_acceptance_probe, ` + memberRole,
		`GRANT EXECUTE ON FUNCTION public.browser_acceptance_probe_test_function() TO browser_acceptance_probe, ` + memberRole,
		`GRANT CREATE ON SCHEMA public TO browser_acceptance_probe, ` + memberRole,
		`GRANT CREATE ON DATABASE ` + database + ` TO browser_acceptance_probe, ` + memberRole,
		`RESET ROLE`,
		`GRANT SELECT (key_value) ON public.browser_acceptance_probe_column_table TO ` + parentRole + ` WITH GRANT OPTION`,
		`SET ROLE ` + parentRole,
		`GRANT SELECT (key_value) ON public.browser_acceptance_probe_column_table TO browser_acceptance_probe, ` + memberRole,
		`RESET ROLE`,
		`GRANT SELECT ON auth_keys TO ` + parentRole,
		`GRANT ` + parentRole + ` TO ` + defaultGrantorRole + ` WITH ADMIN OPTION`,
		`GRANT browser_acceptance_probe TO ` + defaultGrantorRole + ` WITH ADMIN OPTION`,
		`SET ROLE ` + defaultGrantorRole,
		`GRANT ` + parentRole + ` TO browser_acceptance_probe WITH ADMIN TRUE, INHERIT FALSE`,
		`GRANT ` + parentRole + ` TO ` + memberRole,
		`GRANT browser_acceptance_probe TO ` + memberRole,
		`RESET ROLE`,
		`GRANT ` + parentRole + ` TO browser_acceptance_probe WITH INHERIT FALSE`,
		`GRANT browser_acceptance_probe TO ` + memberRole,
		`SET ROLE browser_acceptance_probe`,
		`GRANT ` + parentRole + ` TO ` + memberRole + ` WITH ADMIN OPTION`,
		`RESET ROLE`,
		`SET ROLE ` + memberRole,
		`GRANT ` + parentRole + ` TO ` + delegateRole,
		`RESET ROLE`,
	} {
		if _, err := conn.Exec(ctx, statement); err != nil {
			t.Fatalf("seed existing role privilege with %q: %v", statement, err)
		}
	}

	body, err := os.ReadFile(filepath.Join("..", "..", "migrations", migrationName))
	if err != nil {
		t.Fatalf("read probe migration: %v", err)
	}
	var migrationExecutor string
	if err := conn.QueryRow(ctx, `SELECT current_user`).Scan(&migrationExecutor); err != nil {
		t.Fatalf("read migration executor: %v", err)
	}
	if _, err := conn.Exec(ctx, string(body)); err != nil {
		t.Fatalf("apply probe migration with existing role: %v", err)
	}
	var currentUser string
	if err := conn.QueryRow(ctx, `SELECT current_user`).Scan(&currentUser); err != nil {
		t.Fatalf("read role after migration: %v", err)
	}
	if currentUser != migrationExecutor {
		t.Fatalf("migration left role %s active, want %s", currentUser, migrationExecutor)
	}
	var ownedProbeFunctions int
	if err := conn.QueryRow(ctx, `
		SELECT count(*)
		FROM pg_proc AS function
		JOIN pg_namespace AS namespace ON namespace.oid = function.pronamespace
		JOIN pg_roles AS owner ON owner.oid = function.proowner
		WHERE namespace.nspname = 'public'
		  AND owner.rolname = $1
		  AND function.proname IN (
			  'browser_acceptance_auth_key_bound_count',
			  'browser_acceptance_pending_sign_in_count',
			  'browser_acceptance_bound_auth_key_ids'
		  )
	`, migrationExecutor).Scan(&ownedProbeFunctions); err != nil {
		t.Fatalf("read probe function owners: %v", err)
	}
	if ownedProbeFunctions != 3 {
		t.Fatalf("probe functions owned by migration executor = %d, want 3", ownedProbeFunctions)
	}
	probe, err := pgx.Connect(ctx, pgtest.DSNFrom(database))
	if err != nil {
		t.Fatalf("connect as delegated-grant probe: %v", err)
	}
	defer func() { _ = probe.Close(ctx) }() //nolint:errcheck // best-effort close
	if _, err := probe.Exec(ctx, `SET SESSION AUTHORIZATION browser_acceptance_probe`); err != nil {
		t.Fatalf("set delegated-grant probe session: %v", err)
	}
	if _, err := probe.Exec(ctx, `SET ROLE `+parentRole); err == nil {
		t.Error("probe can SET ROLE to the delegated auth-key reader")
		if _, err := probe.Exec(ctx, `SELECT key_value FROM public.auth_keys`); err != nil {
			t.Fatalf("read credentials through delegated membership: %v", err)
		}
		if _, err := probe.Exec(ctx, `RESET ROLE`); err != nil {
			t.Fatalf("reset delegated reader role: %v", err)
		}
	} else {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Fatalf("probe SET ROLE reader error = %v, want permission denied", err)
		}
	}
	for _, query := range []string{
		`SELECT key_value FROM public.auth_keys`,
		`SELECT key_value FROM public.browser_acceptance_probe_column_table`,
		`SELECT key_value FROM pgdata.browser_acceptance_probe_credentials`,
	} {
		if _, err := probe.Exec(ctx, query); err == nil {
			t.Fatalf("probe migration left delegated key_value access through %q", query)
		} else {
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
				t.Fatalf("delegated read %q error = %v, want permission denied", query, err)
			}
		}
	}
	var memberCanReadAuthKeys, grantorCanDelegateAuthKeys bool
	if err := conn.QueryRow(ctx, `
		SELECT has_table_privilege($1, 'public.auth_keys', 'SELECT'),
		       has_table_privilege($2, 'public.auth_keys', 'SELECT WITH GRANT OPTION')
	`, memberRole, defaultGrantorRole).Scan(&memberCanReadAuthKeys, &grantorCanDelegateAuthKeys); err != nil {
		t.Fatalf("check unrelated delegated auth-key grants: %v", err)
	}
	if !memberCanReadAuthKeys || !grantorCanDelegateAuthKeys {
		t.Fatalf("probe cleanup changed unrelated grants: reader=%v grantor=%v", memberCanReadAuthKeys, grantorCanDelegateAuthKeys)
	}
	for _, check := range []struct {
		name  string
		query string
	}{
		{"column", `SELECT has_column_privilege('browser_acceptance_probe', 'public.browser_acceptance_probe_column_table', 'key_value', 'SELECT'), has_column_privilege($1, 'public.browser_acceptance_probe_column_table', 'key_value', 'SELECT')`},
		{"view", `SELECT has_table_privilege('browser_acceptance_probe', 'public.browser_acceptance_probe_test_view', 'SELECT'), has_table_privilege($1, 'public.browser_acceptance_probe_test_view', 'SELECT')`},
		{"pgdata view", `SELECT has_table_privilege('browser_acceptance_probe', 'pgdata.browser_acceptance_probe_credentials', 'SELECT'), has_table_privilege($1, 'pgdata.browser_acceptance_probe_credentials', 'SELECT')`},
		{"sequence", `SELECT has_sequence_privilege('browser_acceptance_probe', 'public.browser_acceptance_probe_test_sequence', 'USAGE, SELECT, UPDATE'), has_sequence_privilege($1, 'public.browser_acceptance_probe_test_sequence', 'USAGE, SELECT, UPDATE')`},
		{"function", `SELECT has_function_privilege('browser_acceptance_probe', 'public.browser_acceptance_probe_test_function()', 'EXECUTE'), has_function_privilege($1, 'public.browser_acceptance_probe_test_function()', 'EXECUTE')`},
		{"schema", `SELECT has_schema_privilege('browser_acceptance_probe', 'public', 'CREATE'), has_schema_privilege($1, 'public', 'CREATE')`},
		{"database", `SELECT has_database_privilege('browser_acceptance_probe', current_database(), 'CREATE'), has_database_privilege($1, current_database(), 'CREATE')`},
	} {
		var probeAllowed, memberAllowed bool
		if err := conn.QueryRow(ctx, check.query, memberRole).Scan(&probeAllowed, &memberAllowed); err != nil {
			t.Fatalf("check delegated %s privileges: %v", check.name, err)
		}
		if probeAllowed || !memberAllowed {
			t.Errorf("delegated %s privileges: probe=%v reader=%v, want false/true", check.name, probeAllowed, memberAllowed)
		}
	}

	var canRead, canReadView, canUseSequence bool
	if err := conn.QueryRow(ctx, `SELECT has_table_privilege('browser_acceptance_probe', 'public.auth_keys', 'SELECT')`).Scan(&canRead); err != nil {
		t.Fatalf("check existing role table access: %v", err)
	}
	if canRead {
		t.Fatal("probe migration left direct auth_keys SELECT on existing role")
	}
	if err := conn.QueryRow(ctx, `SELECT has_table_privilege('browser_acceptance_probe', 'public.browser_acceptance_probe_test_view', 'SELECT')`).Scan(&canReadView); err != nil {
		t.Fatalf("check existing role view access: %v", err)
	}
	if canReadView {
		t.Fatal("probe migration left SELECT on an existing view")
	}
	if err := conn.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM pg_class AS relation
			JOIN pg_namespace AS namespace ON namespace.oid = relation.relnamespace
			WHERE namespace.nspname <> 'information_schema'
			  AND left(namespace.nspname, 3) <> 'pg_'
			  AND relation.relkind = 'S'
			  AND (
				  has_sequence_privilege('browser_acceptance_probe', relation.oid, 'USAGE')
				  OR has_sequence_privilege('browser_acceptance_probe', relation.oid, 'SELECT')
				  OR has_sequence_privilege('browser_acceptance_probe', relation.oid, 'UPDATE')
			  )
		)
	`).Scan(&canUseSequence); err != nil {
		t.Fatalf("check existing role sequence access: %v", err)
	}
	if canUseSequence {
		t.Fatal("probe migration left sequence privileges on existing role")
	}
	var hasDefaultPrivileges bool
	if err := conn.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM pg_default_acl AS defaults
			CROSS JOIN LATERAL aclexplode(defaults.defaclacl) AS privilege
			WHERE privilege.grantee = (
				SELECT oid FROM pg_roles WHERE rolname = 'browser_acceptance_probe'
			)
		)
	`).Scan(&hasDefaultPrivileges); err != nil {
		t.Fatalf("check existing role default privileges: %v", err)
	}
	if hasDefaultPrivileges {
		t.Fatal("probe migration left default privileges on existing role")
	}
	var hasMembership bool
	if err := conn.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM pg_auth_members AS membership
		JOIN pg_roles AS granted_role ON granted_role.oid = membership.roleid
		JOIN pg_roles AS member_role ON member_role.oid = membership.member
		WHERE granted_role.rolname = 'browser_acceptance_probe'
		   OR member_role.rolname = 'browser_acceptance_probe'
		)
	`).Scan(&hasMembership); err != nil {
		t.Fatalf("check existing role memberships: %v", err)
	}
	if hasMembership {
		t.Fatal("probe migration left role memberships on existing role")
	}
	member, err := pgx.Connect(ctx, pgtest.DSNFrom(database))
	if err != nil {
		t.Fatalf("connect as delegated probe member: %v", err)
	}
	defer func() { _ = member.Close(ctx) }() //nolint:errcheck // best-effort close
	if _, err := member.Exec(ctx, `SET SESSION AUTHORIZATION `+memberRole); err != nil {
		t.Fatalf("set delegated member session: %v", err)
	}
	if _, err := member.Exec(ctx, `SET ROLE browser_acceptance_probe`); err == nil {
		t.Fatal("delegated member can still SET ROLE to the probe")
	} else {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Fatalf("member SET ROLE probe error = %v, want permission denied", err)
		}
	}
	var unrelatedMembership bool
	if err := conn.QueryRow(ctx, `SELECT pg_has_role($1::name, $2::name, 'MEMBER')`, memberRole, parentRole).Scan(&unrelatedMembership); err != nil {
		t.Fatalf("check unrelated delegated membership: %v", err)
	}
	if !unrelatedMembership {
		t.Fatal("probe migration removed an unrelated delegated membership")
	}
	var delegatedMemberships int
	if err := conn.QueryRow(ctx, `
		SELECT count(*)
		FROM pg_auth_members AS membership
		JOIN pg_roles AS granted_role ON granted_role.oid = membership.roleid
		JOIN pg_roles AS member_role ON member_role.oid = membership.member
		JOIN pg_roles AS grantor_role ON grantor_role.oid = membership.grantor
		WHERE granted_role.rolname = $1
		  AND (
			  (member_role.rolname = $2 AND grantor_role.rolname = 'browser_acceptance_probe')
			  OR (member_role.rolname = $3 AND grantor_role.rolname = $2)
		  )
	`, parentRole, memberRole, delegateRole).Scan(&delegatedMemberships); err != nil {
		t.Fatalf("check onward probe membership grants: %v", err)
	}
	if delegatedMemberships != 0 {
		t.Fatalf("probe migration left %d onward membership grants", delegatedMemberships)
	}

	for _, statement := range []string{
		`GRANT CREATE ON SCHEMA public TO ` + defaultGrantorRole,
		`SET ROLE ` + defaultGrantorRole,
		`CREATE TABLE public.browser_acceptance_future_table (id BIGINT)`,
		`CREATE SEQUENCE public.browser_acceptance_future_sequence`,
		`RESET ROLE`,
	} {
		if _, err := conn.Exec(ctx, statement); err != nil {
			t.Fatalf("create future objects with %q: %v", statement, err)
		}
	}
	var probeCanReadFutureTable, probeCanUseFutureSequence, memberCanReadFutureTable bool
	if err := conn.QueryRow(ctx, `
		SELECT has_table_privilege('browser_acceptance_probe', 'public.browser_acceptance_future_table', 'SELECT, INSERT, UPDATE, DELETE'),
		       has_sequence_privilege('browser_acceptance_probe', 'public.browser_acceptance_future_sequence', 'USAGE, SELECT, UPDATE'),
		       has_table_privilege($1, 'public.browser_acceptance_future_table', 'SELECT')
	`, memberRole).Scan(&probeCanReadFutureTable, &probeCanUseFutureSequence, &memberCanReadFutureTable); err != nil {
		t.Fatalf("check future object privileges: %v", err)
	}
	if probeCanReadFutureTable || probeCanUseFutureSequence {
		t.Fatalf("probe gained future object privileges: table=%v sequence=%v", probeCanReadFutureTable, probeCanUseFutureSequence)
	}
	if !memberCanReadFutureTable {
		t.Fatal("probe migration removed an unrelated default table grant")
	}
}
