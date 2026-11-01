package pgtest

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestDSNCleansDatabaseAfterPassAndFail(t *testing.T) {
	const failingSubtestDatabase = "PGTEST_FAILING_SUBTEST_DATABASE"
	if name := os.Getenv(failingSubtestDatabase); name != "" {
		t.Run("failing subtest", func(t *testing.T) {
			_ = cloneDatabase(t, name)
			t.Fatal("intentional failure after creating test database")
		})
		return
	}

	if err := Prewarm(); err != nil {
		t.Fatalf("prewarm: %v", err)
	}
	name := "t_pgtest_cleanup_" + randNameNoT()
	before := testDatabaseCount(t, name)
	if before != 0 {
		t.Fatalf("test database %q exists before cleanup test", name)
	}
	if !t.Run("passing subtest", func(t *testing.T) {
		_ = cloneDatabase(t, name)
	}) {
		t.Fatal("passing subtest failed")
	}
	assertTestDatabaseCount(t, name, before, "passing subtest")

	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "test", "-run=^TestDSNCleansDatabaseAfterPassAndFail$", "-count=1", ".")
	cmd.Env = append(os.Environ(), failingSubtestDatabase+"="+name)
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("failing child test passed unexpectedly:\n%s", output)
	}
	if !strings.Contains(string(output), "intentional failure after creating test database") {
		t.Fatalf("child failed for an unexpected reason: %v\n%s", err, output)
	}
	assertTestDatabaseCount(t, name, before, "failing subtest")
}

func testDatabaseCount(t *testing.T, name string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect to admin database: %v", err)
	}
	defer func() {
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close admin connection: %v", err)
		}
	}()

	var count int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM pg_database WHERE datname = $1`, name).Scan(&count); err != nil {
		t.Fatalf("count test database %q: %v", name, err)
	}
	return count
}

func assertTestDatabaseCount(t *testing.T, name string, want int, subtest string) {
	t.Helper()
	if got := testDatabaseCount(t, name); got != want {
		t.Fatalf("test database count for %s after %s = %d, want %d", name, subtest, got, want)
	}
}

func TestEnsureTemplateDropsBuildingDatabaseAfterMigrationFailure(t *testing.T) {
	if err := Prewarm(); err != nil {
		t.Fatalf("prewarm: %v", err)
	}

	previousName := templateName
	previousMigrations := migrations
	templateName = templatePrefix + "failed_" + randNameNoT()
	migrations = []migration{{name: "invalid.sql", sql: "THIS IS NOT SQL"}}
	defer func() {
		templateName = previousName
		migrations = previousMigrations
	}()

	building := templateName + "_building"
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		conn, err := pgx.Connect(ctx, adminDSN)
		if err != nil {
			t.Errorf("connect to remove failed template build: %v", err)
			return
		}
		defer func() {
			if err := conn.Close(ctx); err != nil {
				t.Errorf("close failed template cleanup connection: %v", err)
			}
		}()
		if _, err := conn.Exec(ctx, `DROP DATABASE IF EXISTS `+pgx.Identifier{building}.Sanitize()+` WITH (FORCE)`); err != nil {
			t.Errorf("drop failed template build %s: %v", building, err)
		}
	})

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	if err := ensureTemplate(ctx); err == nil {
		t.Fatal("ensureTemplate succeeded with an invalid migration")
	}

	conn, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect to admin database: %v", err)
	}
	defer func() {
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close admin connection: %v", err)
		}
	}()

	var exists bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT FROM pg_database WHERE datname = $1)`, building).Scan(&exists); err != nil {
		t.Fatalf("check failed template build: %v", err)
	}
	if exists {
		t.Fatalf("failed template build left database %q behind", building)
	}
}
