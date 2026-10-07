package store_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/pgtest"
)

func TestPollDescriptionMigrationUpgradesMainSchema(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const (
		mainSchemaMigration      = "20261006000063_dialog_pins.sql"
		pollDescriptionMigration = "20261007000064_poll_description_entities.sql"
	)
	migrationsDir := filepath.Join("..", "..", "migrations")
	entries, err := os.ReadDir(migrationsDir)
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	if pollDescriptionMigration <= mainSchemaMigration {
		t.Fatalf("poll description migration %s must follow main schema migration %s", pollDescriptionMigration, mainSchemaMigration)
	}
	if _, err := os.Stat(filepath.Join(migrationsDir, mainSchemaMigration)); err != nil {
		t.Fatalf("main schema migration %s is missing: %v", mainSchemaMigration, err)
	}
	pollMigration, err := os.ReadFile(filepath.Join(migrationsDir, pollDescriptionMigration))
	if err != nil {
		t.Fatalf("read poll description migration: %v", err)
	}

	admin, err := pgx.Connect(ctx, pgtest.AdminDSN())
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	defer func() {
		if err := admin.Close(ctx); err != nil {
			t.Errorf("close admin: %v", err)
		}
	}()
	dbName := "t_" + pgtest.RandomHex()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create disposable database: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		conn, err := pgx.Connect(cleanupCtx, pgtest.AdminDSN())
		if err != nil {
			t.Logf("cleanup connect: %v", err)
			return
		}
		defer func() { _ = conn.Close(cleanupCtx) }() //nolint:errcheck // best-effort cleanup
		if _, err := conn.Exec(cleanupCtx, "DROP DATABASE IF EXISTS "+dbName+" WITH (FORCE)"); err != nil {
			t.Logf("cleanup database %s: %v", dbName, err)
		}
	})

	conn, err := pgx.Connect(ctx, pgtest.DSNFrom(dbName))
	if err != nil {
		t.Fatalf("connect disposable database: %v", err)
	}
	defer func() {
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close disposable database: %v", err)
		}
	}()
	mainSchemaApplied := false
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") || entry.Name() > mainSchemaMigration {
			continue
		}
		body, readErr := os.ReadFile(filepath.Join(migrationsDir, entry.Name()))
		if readErr != nil {
			t.Fatalf("read migration %s: %v", entry.Name(), readErr)
		}
		if _, err := conn.Exec(ctx, string(body)); err != nil {
			t.Fatalf("apply main migration %s: %v", entry.Name(), err)
		}
		mainSchemaApplied = mainSchemaApplied || entry.Name() == mainSchemaMigration
	}
	if !mainSchemaApplied {
		t.Fatalf("main schema migration %s was not applied", mainSchemaMigration)
	}
	if _, err := conn.Exec(ctx, `UPDATE server_administration SET election_closed = true WHERE singleton_id = 1`); err != nil {
		t.Fatalf("close empty administrator election for fixture: %v", err)
	}
	var creatorID int64
	if err := conn.QueryRow(ctx, `INSERT INTO users (phone) VALUES ($1) RETURNING id`, "+1555"+pgtest.RandomHex()).Scan(&creatorID); err != nil {
		t.Fatalf("seed existing poll owner: %v", err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO polls (id, creator_id, source_local_id, question) VALUES (1, $1, 1, 'question')`, creatorID); err != nil {
		t.Fatalf("seed existing poll: %v", err)
	}
	if _, err := conn.Exec(ctx, string(pollMigration)); err != nil {
		t.Fatalf("apply poll description migration after main schema: %v", err)
	}

	var version string
	var entityCount int
	if err := conn.QueryRow(ctx, `
		SELECT description_entities->>'version', jsonb_array_length(description_entities->'entities')
		FROM polls WHERE id = 1
	`).Scan(&version, &entityCount); err != nil {
		t.Fatalf("read migrated poll description entities: %v", err)
	}
	if version != "1" || entityCount != 0 {
		t.Fatalf("migrated description entity default = version %q, %d entities; want version 1 and no entities", version, entityCount)
	}
}
