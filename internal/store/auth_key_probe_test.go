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
			  AND namespace.nspname NOT LIKE 'pg_%'
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
			  AND namespace.nspname NOT LIKE 'pg_%'
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
			INSERT INTO auth_keys (id, key_value, user_id, pending_user_id, created_at)
			VALUES ($1, $2, $3, $4, $5)
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

func TestBrowserAcceptanceProbeMigrationWorksWithExistingRole(t *testing.T) {
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, pgtest.AdminDSN())
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	defer func() { _ = admin.Close(ctx) }() //nolint:errcheck // best-effort close
	parentRole := "browser_probe_parent_" + pgtest.RandomHex()
	memberRole := "browser_probe_member_" + pgtest.RandomHex()
	for _, role := range []string{parentRole, memberRole} {
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
			`REVOKE ` + parentRole + ` FROM browser_acceptance_probe`,
			`REVOKE browser_acceptance_probe FROM ` + memberRole,
			`DROP ROLE IF EXISTS ` + parentRole,
			`DROP ROLE IF EXISTS ` + memberRole,
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
	t.Cleanup(func() {
		conn, err := pgx.Connect(context.Background(), pgtest.AdminDSN())
		if err != nil {
			t.Logf("cleanup connect: %v", err)
			return
		}
		defer func() { _ = conn.Close(context.Background()) }() //nolint:errcheck // best-effort close
		if _, err := conn.Exec(context.Background(), `DROP DATABASE IF EXISTS `+database+` WITH (FORCE)`); err != nil {
			t.Logf("cleanup drop %s: %v", database, err)
		}
	})

	migrationName := "20261013000078_browser_acceptance_probe.sql"
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
		`GRANT SELECT ON auth_keys TO browser_acceptance_probe`,
		`GRANT USAGE, SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA public TO browser_acceptance_probe`,
		`CREATE VIEW public.browser_acceptance_probe_test_view AS SELECT id FROM auth_keys`,
		`GRANT SELECT ON public.browser_acceptance_probe_test_view TO browser_acceptance_probe`,
		`ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON TABLES TO browser_acceptance_probe`,
		`ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE, SELECT, UPDATE ON SEQUENCES TO browser_acceptance_probe`,
		`GRANT SELECT ON auth_keys TO ` + parentRole,
		`GRANT ` + parentRole + ` TO browser_acceptance_probe`,
		`GRANT browser_acceptance_probe TO ` + memberRole,
	} {
		if _, err := conn.Exec(ctx, statement); err != nil {
			t.Fatalf("seed existing role privilege with %q: %v", statement, err)
		}
	}

	body, err := os.ReadFile(filepath.Join("..", "..", "migrations", migrationName))
	if err != nil {
		t.Fatalf("read probe migration: %v", err)
	}
	if _, err := conn.Exec(ctx, string(body)); err != nil {
		t.Fatalf("apply probe migration with existing role: %v", err)
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
			  AND namespace.nspname NOT LIKE 'pg_%'
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
}
