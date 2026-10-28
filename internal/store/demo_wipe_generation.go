package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teagramhq/teagram-server/internal/discovery"
)

const demoWipeGenerationQuery = `
	SELECT generation
	FROM demo_wipe_generation
	WHERE singleton = 1
`

// DemoWipeGeneration reads the generation seeded by the database migration.
func (s *Store) DemoWipeGeneration(ctx context.Context) (string, error) {
	return readDemoWipeGeneration(ctx, s.pool)
}

// ReadDemoWipeGeneration opens a short-lived pool for commands that need the
// public generation but do not otherwise open the Store.
func ReadDemoWipeGeneration(ctx context.Context, dsn string) (string, error) {
	if strings.TrimSpace(dsn) == "" {
		return "", errors.New("store: missing Postgres DSN for demo wipe generation")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return "", fmt.Errorf("store: open connection for demo wipe generation: %w", err)
	}
	defer pool.Close()
	return readDemoWipeGeneration(ctx, pool)
}

func readDemoWipeGeneration(ctx context.Context, queryer queryRower) (string, error) {
	var generation string
	if err := queryer.QueryRow(ctx, demoWipeGenerationQuery).Scan(&generation); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", errors.New("store: demo wipe generation row is missing")
		}
		return "", fmt.Errorf("store: read demo wipe generation: %w", err)
	}
	if err := discovery.ValidateWipeGeneration(generation); err != nil {
		return "", fmt.Errorf("store: invalid demo wipe generation: %w", err)
	}
	return generation, nil
}
