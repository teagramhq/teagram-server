package api_test

import (
	"context"
	"testing"

	"github.com/gotd/td/tg"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestGetAuthorizationsHidesStoredSessionIPByDefault(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	owner, err := s.CreateUser(ctx, "+15551296006")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	key := boundKey(ctx, t, s, owner.ID, 86)
	otherKey := boundKey(ctx, t, s, owner.ID, 87)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to seed historical session IP: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("close metadata connection: %v", err)
		}
	})
	for _, id := range []int64{key.IntID(), otherKey.IntID()} {
		if _, err := conn.Exec(ctx, `
			UPDATE auth_keys
			SET device_model = 'MacBook Pro', platform = 'tdesktop', system_version = 'macOS 15',
			    api_id = 1, app_version = '6.1', ip = '203.0.113.7'
			WHERE id = $1
		`, id); err != nil {
			t.Fatalf("seed historical session metadata for %d: %v", id, err)
		}
	}

	result, err := api.GetAuthorizationsForTest(s, owner.ID, key.ID)
	if err != nil {
		t.Fatalf("get authorizations: %v", err)
	}
	got, ok := result.(*tg.AccountAuthorizations)
	if !ok || len(got.Authorizations) != 2 {
		t.Fatalf("authorization result = %T %+v, want two sessions", result, result)
	}
	for _, auth := range got.Authorizations {
		if auth.IP != "" {
			t.Errorf("authorization IP = %q, want empty while session IP collection is off", auth.IP)
		}
		if auth.DeviceModel != "MacBook Pro" || auth.Platform != "tdesktop" || auth.SystemVersion != "macOS 15" ||
			auth.APIID != 1 || auth.AppVersion != "6.1" {
			t.Errorf("authorization labels = %+v, want the default-on labels", auth)
		}
	}
}

func TestGetAuthorizationsHidesStoredDeviceLabelsWhenDisabled(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSNWithOptions(t, store.WithSessionIP(true), store.WithSessionDeviceLabels(false))
	owner, err := s.CreateUser(ctx, "+15551296008")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	key := boundKey(ctx, t, s, owner.ID, 88)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to seed session metadata: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("close metadata connection: %v", err)
		}
	})
	if _, err := conn.Exec(ctx, `
		UPDATE auth_keys
		SET device_model = 'MacBook Pro', platform = 'tdesktop', system_version = 'macOS 15',
		    api_id = 1, app_version = '6.1', ip = '203.0.113.8'
		WHERE id = $1
	`, key.IntID()); err != nil {
		t.Fatalf("seed historical session metadata: %v", err)
	}

	result, err := api.GetAuthorizationsForTest(s, owner.ID, key.ID)
	if err != nil {
		t.Fatalf("get authorizations: %v", err)
	}
	got, ok := result.(*tg.AccountAuthorizations)
	if !ok || len(got.Authorizations) != 1 {
		t.Fatalf("authorization result = %T %+v, want one session", result, result)
	}
	auth := got.Authorizations[0]
	if auth.DeviceModel != "" || auth.Platform != "" || auth.SystemVersion != "" || auth.APIID != 0 || auth.AppVersion != "" {
		t.Errorf("authorization labels = %+v, want empty while session labels are disabled", auth)
	}
	if auth.IP != "203.0.113.8" {
		t.Errorf("authorization IP = %q, want the independently enabled session IP", auth.IP)
	}
}
