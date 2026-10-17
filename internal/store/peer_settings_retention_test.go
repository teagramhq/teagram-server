package store_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/pgtest"
)

func TestPeerSettingsBarDismissalsCascadeOnEitherUserDelete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, pgtest.DSN(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	if _, err := conn.Exec(ctx, `UPDATE server_administration SET election_closed = true WHERE singleton_id = 1`); err != nil {
		t.Fatalf("close server administrator election: %v", err)
	}

	insertUser := func(tb testing.TB) int64 {
		tb.Helper()
		var userID int64
		if err := conn.QueryRow(ctx, `INSERT INTO users (phone) VALUES ($1) RETURNING id`,
			"+1555"+pgtest.RandomHex()).Scan(&userID); err != nil {
			tb.Fatalf("insert user: %v", err)
		}
		return userID
	}

	for _, deletedOwner := range []bool{true, false} {
		t.Run(map[bool]string{true: "owner", false: "peer"}[deletedOwner], func(t *testing.T) {
			ownerID, peerID := insertUser(t), insertUser(t)
			if _, err := conn.Exec(ctx, `INSERT INTO peer_settings_bar_dismissals (owner_id, peer_id) VALUES ($1, $2)`, ownerID, peerID); err != nil {
				t.Fatalf("insert dismissal: %v", err)
			}

			deletedUserID := peerID
			if deletedOwner {
				deletedUserID = ownerID
			}
			if _, err := conn.Exec(ctx, `DELETE FROM users WHERE id = $1`, deletedUserID); err != nil {
				t.Fatalf("delete %s: %v", map[bool]string{true: "owner", false: "peer"}[deletedOwner], err)
			}

			var remaining int
			if err := conn.QueryRow(ctx, `SELECT count(*) FROM peer_settings_bar_dismissals WHERE owner_id = $1 OR peer_id = $1`, deletedUserID).Scan(&remaining); err != nil {
				t.Fatalf("count remaining dismissals: %v", err)
			}
			if remaining != 0 {
				t.Fatalf("remaining dismissals for deleted user = %d, want 0", remaining)
			}
		})
	}
}
