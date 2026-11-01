//nolint:testpackage // Tests seed and explain PostgreSQL search plans through the store's private pool.
package store

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/pgtest"
)

func TestUserStickerSetSearchCandidatesCapsBroadMatches(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, err := Open(ctx, pgtest.DSN(t), pgtest.EncKey(), WithoutBlobStore())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	owner, err := s.CreateUser(ctx, "+15551810017")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	for i := range 60 {
		_, err := s.pool.Exec(ctx, `
			INSERT INTO user_created_sticker_sets (access_hash, creator_id, title, short_name, kind, item_count)
			VALUES ($1, $2, 'broad sticker match', $3, 'regular', 1)
		`, int64(8_000_000+i), owner.ID, fmt.Sprintf("broad_set_%02d", i))
		if err != nil {
			t.Fatalf("insert matching set %d: %v", i, err)
		}
	}

	candidates, err := s.UserStickerSetSearchCandidates(ctx, "sticker")
	if err != nil {
		t.Fatalf("search candidates: %v", err)
	}
	if len(candidates) == 0 || len(candidates) > 50 {
		t.Fatalf("broad search returned %d candidates, want between 1 and 50", len(candidates))
	}
}

func TestUserStickerSetSearchTreatsLikeWildcardsLiterally(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, err := Open(ctx, pgtest.DSN(t), pgtest.EncKey(), WithoutBlobStore())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	owner, err := s.CreateUser(ctx, "+15551810018")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	for i, title := range []string{"100% cats", "100 cats"} {
		_, err := s.pool.Exec(ctx, `
			INSERT INTO user_created_sticker_sets (access_hash, creator_id, title, short_name, kind, item_count)
			VALUES ($1, $2, $3, $4, 'regular', 1)
		`, int64(9_000_000+i), owner.ID, title, fmt.Sprintf("literal_percent_%d", i))
		if err != nil {
			t.Fatalf("insert percent set %d: %v", i, err)
		}
	}

	candidates, err := s.UserStickerSetSearchCandidates(ctx, "100%")
	if err != nil {
		t.Fatalf("search literal percent: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("literal percent search returned %d candidates, want only the title containing %%", len(candidates))
	}
}

func TestUserStickerSetSearchUsesSubstringIndexes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	s, err := Open(ctx, dsn, pgtest.EncKey(), WithoutBlobStore())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	owner, err := s.CreateUser(ctx, "+15551810019")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	const searchFixtureSets = 100_000
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO user_created_sticker_sets (access_hash, creator_id, title, short_name, kind, item_count)
		SELECT 20_000_000 + sample.n, $1, 'common sticker set ' || sample.n,
		       'sticker_set_' || sample.n, 'regular', 1
		FROM generate_series(1, $2::bigint) AS sample(n)
	`, owner.ID, searchFixtureSets); err != nil {
		t.Fatalf("seed %d sticker sets: %v", searchFixtureSets, err)
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO user_created_sticker_sets (access_hash, creator_id, title, short_name, kind, item_count)
		VALUES (21_000_001, $1, 'rare needle title', 'needle_set', 'regular', 1)
	`, owner.ID); err != nil {
		t.Fatalf("seed selective sticker-set match: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `ANALYZE user_created_sticker_sets`); err != nil {
		t.Fatalf("analyze sticker-set fixtures: %v", err)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close test database connection: %v", err)
		}
	})
	rows, err := conn.Query(ctx, `
		EXPLAIN (COSTS OFF)
		SELECT id
		FROM user_created_sticker_sets
		WHERE is_live = true
		  AND (lower(title) LIKE '%' || lower($1) || '%' ESCAPE E'\\'
		       OR lower(short_name) LIKE '%' || lower($1) || '%' ESCAPE E'\\')
		ORDER BY id
		LIMIT 50
	`, "needle")
	if err != nil {
		t.Fatalf("explain sticker-set search: %v", err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("read query plan: %v", err)
		}
		plan.WriteString(line)
		plan.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate query plan: %v", err)
	}
	for _, index := range []string{
		"user_created_sticker_sets_live_title_trgm_idx",
		"user_created_sticker_sets_live_short_name_trgm_idx",
	} {
		if !strings.Contains(plan.String(), index) {
			t.Errorf("search plan does not use %s:\n%s", index, plan.String())
		}
	}
	candidates, err := s.UserStickerSetSearchCandidates(ctx, "sticker")
	if err != nil {
		t.Fatalf("search at fixture volume: %v", err)
	}
	if len(candidates) != 50 {
		t.Fatalf("search at fixture volume returned %d candidates, want the 50-row cap", len(candidates))
	}
}
