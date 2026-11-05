package store

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/teagramhq/teagram-server/internal/pgtest"
)

func TestTurnstileActionValid(t *testing.T) {
	tests := []struct {
		action TurnstileAction
		want   bool
	}{
		{action: TurnstileActionSignIn, want: true},
		{action: TurnstileActionSignUp, want: true},
		{action: "", want: false},
		{action: "other", want: false},
	}
	for _, test := range tests {
		t.Run(string(test.action), func(t *testing.T) {
			if got := test.action.Valid(); got != test.want {
				t.Fatalf("TurnstileAction(%q).Valid() = %t, want %t", test.action, got, test.want)
			}
		})
	}
}

func TestPhoneCodesRejectInvalidTurnstileAction(t *testing.T) {
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

	_, err = s.pool.Exec(ctx, `
		INSERT INTO phone_codes (phone, code_hash, code, expires_at, turnstile_action)
		VALUES ('invalid_turnstile_action', 'invalid-action-hash', '12345', now() + interval '5 minutes', 'other')
	`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("insert invalid Turnstile action error = %v, want check-constraint violation", err)
	}
}
