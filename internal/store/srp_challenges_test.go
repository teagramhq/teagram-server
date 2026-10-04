package store_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestSRPChallengeSharedSingleUseAndSealed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	first := openStore(t, dsn)
	second := openStore(t, dsn)

	user, err := first.CreateUser(ctx, "+15551294901")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	const authKeyID = int64(0x4901)
	const otherAuthKeyID = int64(0x4902)
	for _, id := range []int64{authKeyID, otherAuthKeyID} {
		if err := first.SaveAuthKey(ctx, id, []byte("test auth key")); err != nil {
			t.Fatalf("save auth key %d: %v", id, err)
		}
	}

	secret := []byte("server-only srp secret")
	public := []byte("public challenge")
	id, err := first.IssueSRPChallenge(ctx, authKeyID, store.SRPChallenge{
		UserID: user.ID, BSecret: secret, BPublic: public,
	}, 5*time.Minute)
	if err != nil {
		t.Fatalf("issue challenge: %v", err)
	}
	var storedSecret []byte
	if err := store.StorePool(first).QueryRow(ctx,
		`SELECT b_secret FROM srp_challenges WHERE srp_id = $1`, id,
	).Scan(&storedSecret); err != nil {
		t.Fatalf("read stored challenge secret: %v", err)
	}
	if bytes.Contains(storedSecret, secret) {
		t.Fatal("challenge secret stored in plaintext")
	}

	if _, ok, err := second.ConsumeSRPChallenge(ctx, id, otherAuthKeyID); err != nil || ok {
		t.Fatalf("consume with wrong auth key: ok=%v err=%v, want absent", ok, err)
	}
	got, ok, err := second.ConsumeSRPChallenge(ctx, id, authKeyID)
	if err != nil || !ok {
		t.Fatalf("consume on second store: ok=%v err=%v", ok, err)
	}
	if got.UserID != user.ID || !bytes.Equal(got.BSecret, secret) || !bytes.Equal(got.BPublic, public) {
		t.Fatalf("consumed challenge = %+v, want original challenge", got)
	}
	if _, ok, err := first.ConsumeSRPChallenge(ctx, id, authKeyID); err != nil || ok {
		t.Fatalf("replay challenge: ok=%v err=%v, want absent", ok, err)
	}
}

func TestSRPChallengeReplacementExpiryAndSweep(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	first := openStore(t, dsn)
	second := openStore(t, dsn)

	user, err := first.CreateUser(ctx, "+15551294902")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	const authKeyID = int64(0x4903)
	const otherAuthKeyID = int64(0x4904)
	for _, id := range []int64{authKeyID, otherAuthKeyID} {
		if err := first.SaveAuthKey(ctx, id, []byte("test auth key")); err != nil {
			t.Fatalf("save auth key %d: %v", id, err)
		}
	}
	issue := func(authKeyID int64, value string) int64 {
		t.Helper()
		id, err := first.IssueSRPChallenge(ctx, authKeyID, store.SRPChallenge{
			UserID: user.ID, BSecret: []byte(value + " secret"), BPublic: []byte(value + " public"),
		}, time.Hour)
		if err != nil {
			t.Fatalf("issue %s challenge: %v", value, err)
		}
		return id
	}

	firstID := issue(authKeyID, "first")
	otherKeyID := issue(otherAuthKeyID, "other key")
	latestID := issue(authKeyID, "latest")
	if _, ok, err := second.ConsumeSRPChallenge(ctx, firstID, authKeyID); err != nil || ok {
		t.Fatalf("consume replaced challenge: ok=%v err=%v, want absent", ok, err)
	}
	if _, ok, err := second.ConsumeSRPChallenge(ctx, otherKeyID, otherAuthKeyID); err != nil || !ok {
		t.Fatalf("consume independent auth-key challenge: ok=%v err=%v", ok, err)
	}
	if _, ok, err := second.ConsumeSRPChallenge(ctx, latestID, authKeyID); err != nil || !ok {
		t.Fatalf("consume latest challenge: ok=%v err=%v", ok, err)
	}

	expiredID := issue(authKeyID, "expired")
	if _, err := store.StorePool(first).Exec(ctx,
		`UPDATE srp_challenges SET expires_at = now() - interval '1 second' WHERE srp_id = $1`, expiredID,
	); err != nil {
		t.Fatalf("expire challenge: %v", err)
	}
	if _, ok, err := second.ConsumeSRPChallenge(ctx, expiredID, authKeyID); err != nil || ok {
		t.Fatalf("consume expired challenge: ok=%v err=%v, want absent", ok, err)
	}
	liveOtherKeyID := issue(otherAuthKeyID, "live")
	deleted, err := first.SweepExpiredSRPChallenges(ctx)
	if err != nil || deleted != 1 {
		t.Fatalf("sweep expired challenges: deleted=%d err=%v, want 1", deleted, err)
	}
	if _, ok, err := second.ConsumeSRPChallenge(ctx, expiredID, authKeyID); err != nil || ok {
		t.Fatalf("consume swept challenge: ok=%v err=%v, want absent", ok, err)
	}
	if _, ok, err := second.ConsumeSRPChallenge(ctx, liveOtherKeyID, otherAuthKeyID); err != nil || !ok {
		t.Fatalf("consume live independent challenge: ok=%v err=%v", ok, err)
	}
}
