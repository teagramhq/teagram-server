package store_test

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

type storedSessionMetadata struct {
	userID        *int64
	pendingUserID *int64
	deviceModel   *string
	platform      *string
	systemVersion *string
	apiID         *int32
	appVersion    *string
	ip            *string
}

func TestDefaultSessionCollectionPolicyAppliesToEveryBindPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	s := openStore(t, dsn)
	conn := openSessionMetadataConn(t, dsn)
	owner, err := s.CreateUser(ctx, "+15551296001")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	metadata := store.AuthorizationMetadata{
		DeviceModel: "MacBook Pro", Platform: "tdesktop", SystemVersion: "macOS 15",
		APIID: 1, AppVersion: "6.1", IP: "203.0.113.7",
	}

	if err := s.SaveAuthKey(ctx, 0x7301, []byte("signin key")); err != nil {
		t.Fatalf("save sign-in key: %v", err)
	}
	if err := s.BindAuthKeyUserWithMetadata(ctx, 0x7301, owner.ID, metadata); err != nil {
		t.Fatalf("bind sign-in key: %v", err)
	}
	assertStoredSessionMetadata(t, conn, 0x7301, owner.ID, metadata, false, true)

	if err := s.SaveAuthKey(ctx, 0x7302, []byte("2FA key")); err != nil {
		t.Fatalf("save 2FA key: %v", err)
	}
	startedAt, _, err := s.StagePendingUser(ctx, 0x7302, owner.ID, time.Minute)
	if err != nil {
		t.Fatalf("stage pending login: %v", err)
	}
	if err := s.PromotePendingUserWithMetadata(ctx, 0x7302, owner.ID, startedAt, time.Minute, metadata); err != nil {
		t.Fatalf("promote pending login: %v", err)
	}
	assertStoredSessionMetadata(t, conn, 0x7302, owner.ID, metadata, false, true)

	if err := s.SaveAuthKey(ctx, 0x7303, []byte("signup key")); err != nil {
		t.Fatalf("save signup key: %v", err)
	}
	hash, _, err := s.IssueCodeForUsername(ctx, "session-defaults")
	if err != nil {
		t.Fatalf("issue signup code: %v", err)
	}
	signedUp, err := s.AdmitUsernameWithMetadata(ctx, "session-defaults", hash, 0x7303, "Session", "Defaults", false, metadata)
	if err != nil {
		t.Fatalf("admit username: %v", err)
	}
	assertStoredSessionMetadata(t, conn, 0x7303, signedUp.ID, metadata, false, true)
}

func TestSessionIPDisabledClearsOldIPOnBothTouchPaths(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	s := openStore(t, dsn)
	conn := openSessionMetadataConn(t, dsn)
	owner, err := s.CreateUser(ctx, "+15551296002")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	other, err := s.CreateUser(ctx, "+15551296003")
	if err != nil {
		t.Fatalf("create other user: %v", err)
	}

	for _, id := range []int64{0x7311, 0x7312, 0x7313} {
		if err := s.SaveAuthKey(ctx, id, []byte("auth key")); err != nil {
			t.Fatalf("save key %x: %v", id, err)
		}
		if err := s.BindAuthKeyUser(ctx, id, owner.ID); err != nil {
			t.Fatalf("bind key %x: %v", id, err)
		}
		seedStoredSessionMetadata(t, conn, id, owner.ID, store.AuthorizationMetadata{
			DeviceModel: "Old device", Platform: "old platform", SystemVersion: "old OS",
			APIID: 5, AppVersion: "old app", IP: "203.0.113.7",
		})
	}

	if err := s.TouchAuthKey(ctx, 0x7311); err != nil {
		t.Fatalf("plain activity touch: %v", err)
	}
	plain := readStoredSessionMetadata(t, conn, 0x7311)
	if plain.ip != nil || plain.deviceModel == nil || *plain.deviceModel != "Old device" {
		t.Fatalf("plain touch metadata = %+v, want IP cleared and labels retained", plain)
	}

	if err := s.TouchAuthKeyWithIP(ctx, 0x7312, netip.MustParseAddr("203.0.113.8"), owner.ID, &store.AuthorizationMetadata{
		DeviceModel: "New device", Platform: "new platform", SystemVersion: "new OS", APIID: 6, AppVersion: "new app",
	}); err != nil {
		t.Fatalf("owner-matching activity touch: %v", err)
	}
	matching := readStoredSessionMetadata(t, conn, 0x7312)
	if matching.ip != nil || matching.deviceModel == nil || *matching.deviceModel != "New device" {
		t.Fatalf("owner-matching touch metadata = %+v, want IP cleared and labels refreshed", matching)
	}

	if err := s.TouchAuthKeyWithIP(ctx, 0x7313, netip.MustParseAddr("203.0.113.9"), other.ID, &store.AuthorizationMetadata{
		DeviceModel: "Stale device", Platform: "stale platform", SystemVersion: "stale OS", APIID: 9, AppVersion: "stale app",
	}); err != nil {
		t.Fatalf("non-matching activity touch: %v", err)
	}
	nonmatching := readStoredSessionMetadata(t, conn, 0x7313)
	if nonmatching.userID == nil || *nonmatching.userID != owner.ID || nonmatching.ip != nil ||
		nonmatching.deviceModel == nil || *nonmatching.deviceModel != "Old device" {
		t.Fatalf("non-matching touch metadata = %+v, want owner and labels unchanged with IP cleared", nonmatching)
	}
}

func TestDisabledSessionDeviceLabelsStopFutureWrites(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	s, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithBlobStore(testBlobs(t)),
		store.WithSessionIP(true), store.WithSessionDeviceLabels(false))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	conn := openSessionMetadataConn(t, dsn)
	owner, err := s.CreateUser(ctx, "+15551296007")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	metadata := store.AuthorizationMetadata{
		DeviceModel: "MacBook Pro", Platform: "tdesktop", SystemVersion: "macOS 15",
		APIID: 1, AppVersion: "6.1", IP: "203.0.113.7",
	}

	if err := s.SaveAuthKey(ctx, 0x7341, []byte("signin key")); err != nil {
		t.Fatalf("save sign-in key: %v", err)
	}
	if err := s.BindAuthKeyUserWithMetadata(ctx, 0x7341, owner.ID, metadata); err != nil {
		t.Fatalf("bind sign-in key: %v", err)
	}
	assertStoredSessionMetadata(t, conn, 0x7341, owner.ID, metadata, true, false)

	if err := s.SaveAuthKey(ctx, 0x7342, []byte("2FA key")); err != nil {
		t.Fatalf("save 2FA key: %v", err)
	}
	startedAt, _, err := s.StagePendingUser(ctx, 0x7342, owner.ID, time.Minute)
	if err != nil {
		t.Fatalf("stage pending login: %v", err)
	}
	if err := s.PromotePendingUserWithMetadata(ctx, 0x7342, owner.ID, startedAt, time.Minute, metadata); err != nil {
		t.Fatalf("promote pending login: %v", err)
	}
	assertStoredSessionMetadata(t, conn, 0x7342, owner.ID, metadata, true, false)

	if err := s.SaveAuthKey(ctx, 0x7343, []byte("signup key")); err != nil {
		t.Fatalf("save signup key: %v", err)
	}
	hash, _, err := s.IssueCodeForUsername(ctx, "session-labels-off")
	if err != nil {
		t.Fatalf("issue signup code: %v", err)
	}
	signedUp, err := s.AdmitUsernameWithMetadata(ctx, "session-labels-off", hash, 0x7343, "Session", "Labels", false, metadata)
	if err != nil {
		t.Fatalf("admit username: %v", err)
	}
	assertStoredSessionMetadata(t, conn, 0x7343, signedUp.ID, metadata, true, false)

	seedStoredSessionMetadata(t, conn, 0x7341, owner.ID, metadata)
	if err := s.TouchAuthKeyWithIP(ctx, 0x7341, netip.MustParseAddr("203.0.113.8"), owner.ID, &store.AuthorizationMetadata{
		DeviceModel: "New device", Platform: "new platform", SystemVersion: "new OS", APIID: 2, AppVersion: "new app",
	}); err != nil {
		t.Fatalf("touch with labels disabled: %v", err)
	}
	got := readStoredSessionMetadata(t, conn, 0x7341)
	if got.ip == nil || *got.ip != "203.0.113.8" || got.deviceModel == nil || *got.deviceModel != metadata.DeviceModel ||
		got.platform == nil || *got.platform != metadata.Platform || got.systemVersion == nil || *got.systemVersion != metadata.SystemVersion ||
		got.apiID == nil || *got.apiID != metadata.APIID || got.appVersion == nil || *got.appVersion != metadata.AppVersion {
		t.Fatalf("touch changed labels or failed to write enabled IP: %+v", got)
	}
}

func TestSessionIPEnabledStaleTouchCannotRestoreMetadataAfterUnbindOrRebind(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	s, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithBlobStore(testBlobs(t)), store.WithSessionIP(true))
	if err != nil {
		t.Fatalf("open store with session IP enabled: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	conn := openSessionMetadataConn(t, dsn)
	ownerA, err := s.CreateUser(ctx, "+15551296004")
	if err != nil {
		t.Fatalf("create owner A: %v", err)
	}
	ownerB, err := s.CreateUser(ctx, "+15551296005")
	if err != nil {
		t.Fatalf("create owner B: %v", err)
	}

	for _, unbind := range []struct {
		name string
		run  func(int64) error
	}{
		{name: "logout", run: func(id int64) error { _, _, err := s.LogOutAuthKey(ctx, id); return err }},
		{name: "reset", run: func(id int64) error { _, err := s.ResetAuthorization(ctx, ownerA.ID, id); return err }},
	} {
		t.Run(unbind.name, func(t *testing.T) {
			id := int64(0x7320)
			if unbind.name == "reset" {
				id++
			}
			if err := s.SaveAuthKey(ctx, id, []byte("auth key")); err != nil {
				t.Fatalf("save key: %v", err)
			}
			oldMetadata := store.AuthorizationMetadata{
				DeviceModel: "Owner A device", Platform: "owner A platform", SystemVersion: "owner A OS",
				APIID: 1, AppVersion: "1.0", IP: "203.0.113.10",
			}
			if err := s.BindAuthKeyUserWithMetadata(ctx, id, ownerA.ID, oldMetadata); err != nil {
				t.Fatalf("bind owner A: %v", err)
			}
			seedStoredSessionMetadata(t, conn, id, ownerA.ID, oldMetadata)
			if err := unbind.run(id); err != nil {
				t.Fatalf("unbind owner A: %v", err)
			}
			stale := &store.AuthorizationMetadata{
				DeviceModel: "stale device", Platform: "stale platform", SystemVersion: "stale OS",
				APIID: 9, AppVersion: "stale app",
			}
			if err := s.TouchAuthKeyWithIP(ctx, id, netip.MustParseAddr("203.0.113.99"), ownerA.ID, stale); err != nil {
				t.Fatalf("stale touch after unbind: %v", err)
			}
			cleared := readStoredSessionMetadata(t, conn, id)
			if cleared.userID != nil || cleared.pendingUserID != nil || cleared.ip != nil || cleared.deviceModel != nil ||
				cleared.platform != nil || cleared.systemVersion != nil || cleared.apiID != nil || cleared.appVersion != nil {
				t.Fatalf("stale touch restored unbound metadata: %+v", cleared)
			}

			newMetadata := store.AuthorizationMetadata{
				DeviceModel: "Owner B device", Platform: "owner B platform", SystemVersion: "owner B OS",
				APIID: 2, AppVersion: "2.0", IP: "203.0.113.20",
			}
			if err := s.BindAuthKeyUserWithMetadata(ctx, id, ownerB.ID, newMetadata); err != nil {
				t.Fatalf("bind owner B: %v", err)
			}
			if err := s.TouchAuthKeyWithIP(ctx, id, netip.MustParseAddr("203.0.113.99"), ownerA.ID, stale); err != nil {
				t.Fatalf("stale touch after rebind: %v", err)
			}
			rebound := readStoredSessionMetadata(t, conn, id)
			assertStoredSessionMetadataValues(t, rebound, ownerB.ID, newMetadata, true, true)
		})
	}
}

func openSessionMetadataConn(t *testing.T, dsn string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect for session metadata: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("close session metadata connection: %v", err)
		}
	})
	return conn
}

func seedStoredSessionMetadata(t *testing.T, conn *pgx.Conn, id, ownerID int64, metadata store.AuthorizationMetadata) {
	t.Helper()
	if _, err := conn.Exec(context.Background(), `
		UPDATE auth_keys
		SET user_id = $2, device_model = $3, platform = $4, system_version = $5,
		    api_id = $6, app_version = $7, ip = $8
		WHERE id = $1
	`, id, ownerID, metadata.DeviceModel, metadata.Platform, metadata.SystemVersion, metadata.APIID, metadata.AppVersion, metadata.IP); err != nil {
		t.Fatalf("seed session metadata for key %x: %v", id, err)
	}
}

func assertStoredSessionMetadata(t *testing.T, conn *pgx.Conn, id, ownerID int64, want store.AuthorizationMetadata, wantIP, wantLabels bool) {
	t.Helper()
	assertStoredSessionMetadataValues(t, readStoredSessionMetadata(t, conn, id), ownerID, want, wantIP, wantLabels)
}

func assertStoredSessionMetadataValues(t *testing.T, got storedSessionMetadata, ownerID int64, want store.AuthorizationMetadata, wantIP, wantLabels bool) {
	t.Helper()
	if got.userID == nil || *got.userID != ownerID {
		t.Fatalf("session owner = %v, want %d", got.userID, ownerID)
	}
	if wantIP {
		if got.ip == nil || *got.ip != want.IP {
			t.Errorf("stored IP = %v, want %q", got.ip, want.IP)
		}
	} else if got.ip != nil {
		t.Errorf("stored IP = %q, want NULL", *got.ip)
	}
	if wantLabels {
		if got.deviceModel == nil || *got.deviceModel != want.DeviceModel || got.platform == nil || *got.platform != want.Platform ||
			got.systemVersion == nil || *got.systemVersion != want.SystemVersion || got.apiID == nil || *got.apiID != want.APIID ||
			got.appVersion == nil || *got.appVersion != want.AppVersion {
			t.Errorf("stored labels = %+v, want metadata %+v", got, want)
		}
	} else if got.deviceModel != nil || got.platform != nil || got.systemVersion != nil || got.apiID != nil || got.appVersion != nil {
		t.Errorf("stored labels = %+v, want NULL labels", got)
	}
}

func readStoredSessionMetadata(t *testing.T, conn *pgx.Conn, id int64) storedSessionMetadata {
	t.Helper()
	var got storedSessionMetadata
	if err := conn.QueryRow(context.Background(), `
		SELECT user_id, pending_user_id, device_model, platform, system_version, api_id, app_version, ip
		FROM auth_keys WHERE id = $1
	`, id).Scan(&got.userID, &got.pendingUserID, &got.deviceModel, &got.platform, &got.systemVersion, &got.apiID, &got.appVersion, &got.ip); err != nil {
		t.Fatalf("read session metadata for key %x: %v", id, err)
	}
	return got
}
