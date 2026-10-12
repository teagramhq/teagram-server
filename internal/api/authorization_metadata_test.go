package api_test

import (
	"context"
	"net/netip"
	"testing"

	"github.com/gotd/td/tg"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestGetAuthorizationsReturnsDeviceMetadataOnlyToOwner(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	owner, err := s.CreateUser(ctx, "+15551296081")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	other, err := s.CreateUser(ctx, "+15551296082")
	if err != nil {
		t.Fatalf("create other user: %v", err)
	}
	key := boundKey(ctx, t, s, owner.ID, 81)

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
		    api_id = 1, app_version = '6.1', ip = '203.0.113.7'
		WHERE id = $1
	`, key.IntID()); err != nil {
		t.Fatalf("seed session metadata: %v", err)
	}

	result, err := api.GetAuthorizationsForTest(s, owner.ID, key.ID)
	if err != nil {
		t.Fatalf("get owner authorizations: %v", err)
	}
	got, ok := result.(*tg.AccountAuthorizations)
	if !ok || len(got.Authorizations) != 1 {
		t.Fatalf("owner authorization result = %T %+v, want one authorization", result, result)
	}
	auth := got.Authorizations[0]
	if auth.DeviceModel != "MacBook Pro" || auth.Platform != "tdesktop" || auth.SystemVersion != "macOS 15" ||
		auth.APIID != 1 || auth.AppName != "" || auth.AppVersion != "6.1" || auth.IP != "203.0.113.7" ||
		auth.Country != "" || auth.Region != "" {
		t.Fatalf("authorization metadata = %+v, want only the stored device fields and IP", auth)
	}

	otherResult, err := api.GetAuthorizationsForTest(s, other.ID, key.ID)
	if err != nil {
		t.Fatalf("get other user's authorizations: %v", err)
	}
	otherAuths, ok := otherResult.(*tg.AccountAuthorizations)
	if !ok || len(otherAuths.Authorizations) != 0 {
		t.Fatalf("other user's authorizations = %T %+v, want no sessions", otherResult, otherResult)
	}
}

func TestAuthorizationMetadataDoesNotCarryAcrossLogoutAndRebind(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := openStoreDSN(t)
	owner, err := s.CreateUser(ctx, "+15551296083")
	if err != nil {
		t.Fatalf("create first owner: %v", err)
	}
	other, err := s.CreateUser(ctx, "+15551296084")
	if err != nil {
		t.Fatalf("create second owner: %v", err)
	}
	key := boundKey(ctx, t, s, owner.ID, 83)
	if err := s.BindAuthKeyUserWithMetadata(ctx, key.IntID(), owner.ID, store.AuthorizationMetadata{
		DeviceModel: "A device", Platform: "A platform", SystemVersion: "A system",
		APIID: 1, AppVersion: "A version", IP: "203.0.113.8",
	}); err != nil {
		t.Fatalf("set first owner's session metadata: %v", err)
	}
	if _, _, err := s.LogOutAuthKey(ctx, key.IntID()); err != nil {
		t.Fatalf("log out first owner: %v", err)
	}
	if err := s.BindAuthKeyUserWithMetadata(ctx, key.IntID(), other.ID, store.AuthorizationMetadata{
		DeviceModel: "B device", Platform: "B platform", SystemVersion: "B system",
		APIID: 2, AppVersion: "B version", IP: "203.0.113.9",
	}); err != nil {
		t.Fatalf("bind second owner: %v", err)
	}
	if err := s.TouchAuthKeyWithIP(ctx, key.IntID(), netip.MustParseAddr("203.0.113.8"), owner.ID); err != nil {
		t.Fatalf("touch from former owner's connection: %v", err)
	}

	result, err := api.GetAuthorizationsForTest(s, other.ID, key.ID)
	if err != nil {
		t.Fatalf("get second owner's authorizations: %v", err)
	}
	got, ok := result.(*tg.AccountAuthorizations)
	if !ok {
		t.Fatalf("second owner's authorization result = %T, want AccountAuthorizations", result)
	}
	if len(got.Authorizations) != 1 {
		t.Fatalf("second owner has %d sessions, want 1", len(got.Authorizations))
	}
	auth := got.Authorizations[0]
	if auth.DeviceModel != "B device" || auth.Platform != "B platform" || auth.SystemVersion != "B system" ||
		auth.APIID != 2 || auth.AppVersion != "B version" || auth.IP != "203.0.113.9" {
		t.Fatalf("second owner's session metadata = %+v, want only the new owner's values", auth)
	}
	formerOwnerResult, err := api.GetAuthorizationsForTest(s, owner.ID, key.ID)
	if err != nil {
		t.Fatalf("get former owner's authorizations: %v", err)
	}
	formerOwnerAuths, ok := formerOwnerResult.(*tg.AccountAuthorizations)
	if !ok {
		t.Fatalf("former owner's authorization result = %T, want AccountAuthorizations", formerOwnerResult)
	}
	if auths := formerOwnerAuths.Authorizations; len(auths) != 0 {
		t.Fatalf("former owner still has %d sessions, want none", len(auths))
	}
}
