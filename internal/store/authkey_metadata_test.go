package store_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/pgtest"
)

func TestAuthorizationMetadataClearsOnEveryUnbind(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	s := openStore(t, dsn)
	owner, err := s.CreateUser(ctx, "+15551296091")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to seed session metadata: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("close metadata connection: %v", err)
		}
	})

	seed := func(id int64) {
		t.Helper()
		if err := s.SaveAuthKey(ctx, id, []byte("auth key")); err != nil {
			t.Fatalf("save auth key %d: %v", id, err)
		}
		if err := s.BindAuthKeyUser(ctx, id, owner.ID); err != nil {
			t.Fatalf("bind auth key %d: %v", id, err)
		}
		if _, err := conn.Exec(ctx, `
			UPDATE auth_keys
			SET device_model = 'MacBook Pro', platform = 'tdesktop', system_version = 'macOS 15',
			    api_id = 1, app_version = '6.1', ip = '203.0.113.7'
			WHERE id = $1
		`, id); err != nil {
			t.Fatalf("seed authorization metadata for key %d: %v", id, err)
		}
	}
	assertCleared := func(id int64) {
		t.Helper()
		var ownerID *int64
		var deviceModel, platform, systemVersion, appVersion, ip *string
		var apiID *int32
		if err := conn.QueryRow(ctx, `
			SELECT user_id, device_model, platform, system_version, api_id, app_version, ip
			FROM auth_keys WHERE id = $1
		`, id).Scan(&ownerID, &deviceModel, &platform, &systemVersion, &apiID, &appVersion, &ip); err != nil {
			t.Fatalf("read unbound key %d metadata: %v", id, err)
		}
		if ownerID != nil || deviceModel != nil || platform != nil || systemVersion != nil || apiID != nil || appVersion != nil || ip != nil {
			t.Fatalf("unbound key %d retained metadata: owner=%v device=%v platform=%v system=%v api=%v app=%v ip=%v",
				id, ownerID, deviceModel, platform, systemVersion, apiID, appVersion, ip)
		}
	}

	for _, test := range []struct {
		name string
		id   int64
		run  func() error
	}{
		{name: "logout", id: 0x7201, run: func() error {
			_, _, err := s.LogOutAuthKey(ctx, 0x7201)
			return err
		}},
		{name: "reset authorization", id: 0x7202, run: func() error {
			_, err := s.ResetAuthorization(ctx, owner.ID, 0x7202)
			return err
		}},
		{name: "stage password login", id: 0x7203, run: func() error {
			return s.SetPendingUser(ctx, 0x7203, owner.ID)
		}},
		{name: "reset other authorizations", id: 0x7205, run: func() error {
			_, err := resetAuthorizations(t, s, owner.ID, 0x7204)
			return err
		}},
		{name: "expiry sweep", id: 0x7206, run: func() error {
			if _, err := conn.Exec(ctx, `UPDATE users SET authorization_ttl_days = 7 WHERE id = $1`, owner.ID); err != nil {
				return fmt.Errorf("set short TTL: %w", err)
			}
			if _, err := conn.Exec(ctx, `UPDATE auth_keys SET last_seen_at = now() - interval '8 days' WHERE id = $1`, 0x7206); err != nil {
				return fmt.Errorf("expire key: %w", err)
			}
			_, err := s.SweepExpiredAuthorizations(ctx, 10)
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			id := test.id
			if test.name == "reset other authorizations" {
				if err := s.SaveAuthKey(ctx, 0x7204, []byte("caller key")); err != nil {
					t.Fatalf("save caller key: %v", err)
				}
				if err := s.BindAuthKeyUser(ctx, 0x7204, owner.ID); err != nil {
					t.Fatalf("bind caller key: %v", err)
				}
			}
			seed(id)
			if err := test.run(); err != nil {
				t.Fatalf("unbind through %s: %v", test.name, err)
			}
			assertCleared(id)
		})
	}
}
