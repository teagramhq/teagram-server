package store_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestPasswordCRUD(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()

	u, err := s.CreateUser(ctx, "+15551250001")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	// Absent row: no cloud password.
	if _, ok, err := s.PasswordByUser(ctx, u.ID); err != nil || ok {
		t.Fatalf("absent password: ok=%v err=%v", ok, err)
	}

	want := store.UserPassword{
		UserID:   u.ID,
		Salt1:    []byte("salt-one-0123456789abcdef"),
		Salt2:    []byte("salt-two-0123456789abcdef"),
		Verifier: []byte{0x00, 0xde, 0xad, 0xbe, 0xef, 0x00},
		Hint:     "my hint",
	}
	if err := s.UpsertPassword(ctx, want); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, ok, err := s.PasswordByUser(ctx, u.ID)
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if !bytes.Equal(got.Verifier, want.Verifier) {
		t.Errorf("verifier round-trip: got %x want %x", got.Verifier, want.Verifier)
	}
	if !bytes.Equal(got.Salt1, want.Salt1) || !bytes.Equal(got.Salt2, want.Salt2) {
		t.Errorf("salts round-trip mismatch")
	}
	if got.Hint != want.Hint {
		t.Errorf("hint: got %q want %q", got.Hint, want.Hint)
	}

	// Change: replace verifier/hint.
	want.Verifier = []byte{0x11, 0x22, 0x33}
	want.Hint = "new hint"
	if err := s.UpsertPassword(ctx, want); err != nil {
		t.Fatalf("upsert change: %v", err)
	}
	got, _, err = s.PasswordByUser(ctx, u.ID)
	if err != nil {
		t.Fatalf("get after change: %v", err)
	}
	if !bytes.Equal(got.Verifier, want.Verifier) || got.Hint != "new hint" {
		t.Errorf("change not applied: verifier=%x hint=%q", got.Verifier, got.Hint)
	}

	// Remove.
	found, err := s.DeletePassword(ctx, u.ID)
	if err != nil || !found {
		t.Fatalf("delete: found=%v err=%v", found, err)
	}
	if _, ok, err := s.PasswordByUser(ctx, u.ID); err != nil || ok {
		t.Fatalf("password still present after delete: ok=%v err=%v", ok, err)
	}
	// Deleting again reports not found.
	if found, err := s.DeletePassword(ctx, u.ID); err != nil || found {
		t.Fatalf("second delete: found=%v err=%v", found, err)
	}
}

func TestPasswordVerifierEncryptedAtRest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	s, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithBlobStore(testBlobs(t)))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() {
		if cerr := s.Close(); cerr != nil {
			t.Errorf("close: %v", cerr)
		}
	})

	u, err := s.CreateUser(ctx, "+15551250002")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	verifier := []byte("super-secret-verifier-material")
	if err := s.UpsertPassword(ctx, store.UserPassword{
		UserID: u.ID, Salt1: []byte("a"), Salt2: []byte("b"), Verifier: verifier,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Read the raw stored bytes via a direct connection: they must not contain
	// the plaintext verifier.
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("raw connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }() //nolint:errcheck // best-effort close
	var raw []byte
	if err := conn.QueryRow(ctx, `SELECT verifier FROM user_passwords WHERE user_id = $1`, u.ID).Scan(&raw); err != nil {
		t.Fatalf("read raw verifier: %v", err)
	}
	if bytes.Contains(raw, verifier) {
		t.Fatal("verifier stored in plaintext")
	}
}

func TestPendingUserSetPromoteClear(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	const keyID = int64(0x3001)

	u, err := s.CreateUser(ctx, "+15551250003")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := s.SaveAuthKey(ctx, keyID, []byte("k")); err != nil {
		t.Fatalf("save key: %v", err)
	}

	// Set pending: the key stays unbound (UserID 0).
	if err := s.SetPendingUser(ctx, keyID, u.ID); err != nil {
		t.Fatalf("set pending: %v", err)
	}
	got, _, err := s.AuthKeyByID(ctx, keyID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.UserID != 0 {
		t.Fatalf("pending must not authorize: UserID=%d", got.UserID)
	}

	// Promote: pending → user_id.
	if err := s.PromotePendingUser(ctx, keyID, u.ID, got.PendingStartedAt, 10*time.Minute); err != nil {
		t.Fatalf("promote: %v", err)
	}
	got, _, err = s.AuthKeyByID(ctx, keyID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.UserID != u.ID {
		t.Fatalf("promote did not bind: UserID=%d want %d", got.UserID, u.ID)
	}

	// Second promote is a no-op (pending already cleared) → fail closed.
	if err := s.PromotePendingUser(ctx, keyID, u.ID, got.PendingStartedAt, 10*time.Minute); !errors.Is(err, store.ErrAuthKeyNotFound) {
		t.Fatalf("re-promote: got %v want ErrAuthKeyNotFound", err)
	}
}

func TestSetPendingUserPersistsAndRefreshesDatabaseStart(t *testing.T) {
	t.Parallel()
	dsn := pgtest.DSN(t)
	s := openStore(t, dsn)
	ctx := context.Background()
	const keyID = int64(0x3007)

	u, err := s.CreateUser(ctx, "+15551250017")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := s.SaveAuthKey(ctx, keyID, []byte("k")); err != nil {
		t.Fatalf("save key: %v", err)
	}
	readStart := func() string {
		t.Helper()
		conn, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		defer func() {
			if err := conn.Close(ctx); err != nil {
				t.Errorf("close: %v", err)
			}
		}()
		var start string
		if err := conn.QueryRow(ctx, `
			SELECT COALESCE(to_jsonb(k)->>'pending_started_at', '')
			FROM auth_keys k WHERE id = $1`, keyID).Scan(&start); err != nil {
			t.Fatalf("read pending start: %v", err)
		}
		return start
	}

	if err := s.SetPendingUser(ctx, keyID, u.ID); err != nil {
		t.Fatalf("first pending sign-in: %v", err)
	}
	first := readStart()
	if first == "" {
		t.Fatal("pending sign-in did not persist its database start time")
	}

	time.Sleep(2 * time.Millisecond)
	if err := s.SetPendingUser(ctx, keyID, u.ID); err != nil {
		t.Fatalf("fresh pending sign-in: %v", err)
	}
	second := readStart()
	if second == "" || second <= first {
		t.Fatalf("fresh sign-in start = %q, want later than %q", second, first)
	}
}

func TestAuthKeyByIDReturnsPostgresComputedPendingLease(t *testing.T) {
	t.Parallel()
	dsn := pgtest.DSN(t)
	s := openStore(t, dsn)
	ctx := context.Background()
	const keyID = int64(0x3009)
	const lease = 10 * time.Minute
	const elapsed = 9 * time.Minute

	u, err := s.CreateUser(ctx, "+15551250019")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := s.SaveAuthKey(ctx, keyID, []byte("k")); err != nil {
		t.Fatalf("save key: %v", err)
	}
	startedAt, stagedRemaining, err := s.StagePendingUser(ctx, keyID, u.ID, lease)
	if err != nil {
		t.Fatalf("stage pending user: %v", err)
	}
	if startedAt.IsZero() || stagedRemaining <= lease-time.Second || stagedRemaining > lease {
		t.Fatalf("staged pending lease = start %s, remaining %s; want database start and about %s remaining", startedAt, stagedRemaining, lease)
	}
	setPendingLoginAge(t, ctx, dsn, keyID, elapsed)

	got, ok, err := s.AuthKeyByIDWithPendingLease(ctx, keyID, lease)
	if err != nil || !ok {
		t.Fatalf("read pending key: ok=%v err=%v", ok, err)
	}
	wantRemaining := lease - elapsed
	if delta := got.PendingRemaining - wantRemaining; delta < -time.Second || delta > time.Second {
		t.Fatalf("Postgres-computed remaining lease = %s, differs from %s by %s", got.PendingRemaining, wantRemaining, delta)
	}
}

func TestPromotePendingUserMismatchFailsClosed(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	const keyID = int64(0x3002)

	victim, err := s.CreateUser(ctx, "+15551250004")
	if err != nil {
		t.Fatalf("create victim: %v", err)
	}
	attacker, err := s.CreateUser(ctx, "+15551250005")
	if err != nil {
		t.Fatalf("create attacker: %v", err)
	}
	if err := s.SaveAuthKey(ctx, keyID, []byte("k")); err != nil {
		t.Fatalf("save key: %v", err)
	}
	if err := s.SetPendingUser(ctx, keyID, victim.ID); err != nil {
		t.Fatalf("set pending: %v", err)
	}
	pending, ok, err := s.PendingLoginByID(ctx, keyID, 10*time.Minute)
	if err != nil || !ok {
		t.Fatalf("read pending login: ok=%v err=%v", ok, err)
	}
	// Promoting for a different user than the staged pending must not bind.
	if err := s.PromotePendingUser(ctx, keyID, attacker.ID, pending.StartedAt, 10*time.Minute); !errors.Is(err, store.ErrAuthKeyNotFound) {
		t.Fatalf("cross-user promote: got %v want ErrAuthKeyNotFound", err)
	}
	got, _, err := s.AuthKeyByID(ctx, keyID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.UserID != 0 {
		t.Fatalf("key authorized after mismatched promote: UserID=%d", got.UserID)
	}
}

func TestSetPendingUserClearsExistingBinding(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	const keyID = int64(0x3003)

	u, err := s.CreateUser(ctx, "+15551250007")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := s.SaveAuthKey(ctx, keyID, []byte("k")); err != nil {
		t.Fatalf("save key: %v", err)
	}
	if err := s.BindAuthKeyUser(ctx, keyID, u.ID); err != nil {
		t.Fatalf("bind: %v", err)
	}
	// A re-signIn that stages 2FA must drop the existing authorization: user_id
	// cleared, pending set, so the key is not authorized until checkPassword.
	if err := s.SetPendingUser(ctx, keyID, u.ID); err != nil {
		t.Fatalf("set pending: %v", err)
	}
	got, _, err := s.AuthKeyByID(ctx, keyID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.UserID != 0 {
		t.Fatalf("user_id not cleared on set pending: %d", got.UserID)
	}
	if got.PendingUserID != u.ID {
		t.Fatalf("pending not set: %d want %d", got.PendingUserID, u.ID)
	}
}

func TestSetPendingUserMissingKeyFailsClosed(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()

	u, err := s.CreateUser(ctx, "+15551250006")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := s.SetPendingUser(ctx, 0xdead, u.ID); !errors.Is(err, store.ErrAuthKeyNotFound) {
		t.Fatalf("set pending missing key: got %v want ErrAuthKeyNotFound", err)
	}
}

func TestPendingLoginExpiryAndLegacyRowsFailClosed(t *testing.T) {
	t.Parallel()
	dsn := pgtest.DSN(t)
	s := openStore(t, dsn)
	ctx := context.Background()
	const keyID = int64(0x3008)
	const lease = 10 * time.Minute

	u, err := s.CreateUser(ctx, "+15551250018")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := s.SaveAuthKey(ctx, keyID, []byte("k")); err != nil {
		t.Fatalf("save key: %v", err)
	}
	if err := s.SetPendingUser(ctx, keyID, u.ID); err != nil {
		t.Fatalf("stage pending user: %v", err)
	}
	setPendingLoginAge(t, ctx, dsn, keyID, lease)

	pending, ok, err := s.PendingLoginByID(ctx, keyID, lease)
	if err != nil || !ok {
		t.Fatalf("expired pending login: ok=%v err=%v", ok, err)
	}
	if pending.Active {
		t.Fatal("pending login at its deadline is still active")
	}
	if err := s.ClearExpiredPendingUser(ctx, keyID, pending.UserID, pending.StartedAt, lease); err != nil {
		t.Fatalf("clear expired pending: %v", err)
	}
	got, _, err := s.AuthKeyByID(ctx, keyID)
	if err != nil {
		t.Fatalf("read cleared key: %v", err)
	}
	if got.PendingUserID != 0 || !got.PendingStartedAt.IsZero() || got.UserID != 0 {
		t.Fatalf("expired key state = user %d, pending %d at %s; want unbound and clear", got.UserID, got.PendingUserID, got.PendingStartedAt)
	}

	if err := s.SetPendingUser(ctx, keyID, u.ID); err != nil {
		t.Fatalf("stage legacy pending user: %v", err)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := conn.Exec(ctx, `UPDATE auth_keys SET pending_started_at = NULL WHERE id = $1`, keyID); err != nil {
		t.Fatalf("make legacy pending row: %v", err)
	}
	if err := conn.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}
	pending, ok, err = s.PendingLoginByID(ctx, keyID, lease)
	if err != nil || !ok {
		t.Fatalf("legacy pending login: ok=%v err=%v", ok, err)
	}
	if pending.Active || !pending.StartedAt.IsZero() {
		t.Fatalf("legacy pending state = %+v, want inactive with no start", pending)
	}
	if err := s.ClearExpiredPendingUser(ctx, keyID, pending.UserID, pending.StartedAt, lease); err != nil {
		t.Fatalf("clear legacy pending: %v", err)
	}
	got, _, err = s.AuthKeyByID(ctx, keyID)
	if err != nil {
		t.Fatalf("read cleared legacy key: %v", err)
	}
	if got.PendingUserID != 0 || !got.PendingStartedAt.IsZero() {
		t.Fatalf("legacy pending state was retained: pending=%d at %s", got.PendingUserID, got.PendingStartedAt)
	}
}

func TestPromotePendingUserRequiresCurrentUnexpiredGeneration(t *testing.T) {
	t.Parallel()
	dsn := pgtest.DSN(t)
	s := openStore(t, dsn)
	ctx := context.Background()
	const keyID = int64(0x3009)
	const lease = 10 * time.Minute

	u, err := s.CreateUser(ctx, "+15551250019")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	other, err := s.CreateUser(ctx, "+15551250020")
	if err != nil {
		t.Fatalf("create other user: %v", err)
	}
	if err := s.SaveAuthKey(ctx, keyID, []byte("k")); err != nil {
		t.Fatalf("save key: %v", err)
	}
	if err := s.SetPendingUser(ctx, keyID, u.ID); err != nil {
		t.Fatalf("stage first pending user: %v", err)
	}
	first, ok, err := s.PendingLoginByID(ctx, keyID, lease)
	if err != nil || !ok {
		t.Fatalf("read first pending login: ok=%v err=%v", ok, err)
	}
	time.Sleep(2 * time.Millisecond)
	if err := s.SetPendingUser(ctx, keyID, u.ID); err != nil {
		t.Fatalf("stage fresh pending generation: %v", err)
	}
	if err := s.PromotePendingUser(ctx, keyID, u.ID, first.StartedAt, lease); !errors.Is(err, store.ErrAuthKeyNotFound) {
		t.Fatalf("promote old generation: got %v, want ErrAuthKeyNotFound", err)
	}
	current, ok, err := s.PendingLoginByID(ctx, keyID, lease)
	if err != nil || !ok || !current.Active || !current.StartedAt.After(first.StartedAt) {
		t.Fatalf("fresh pending generation = %+v, ok=%v err=%v", current, ok, err)
	}
	if err := s.SetPendingUser(ctx, keyID, other.ID); err != nil {
		t.Fatalf("stage different pending user: %v", err)
	}
	if err := s.PromotePendingUser(ctx, keyID, u.ID, current.StartedAt, lease); !errors.Is(err, store.ErrAuthKeyNotFound) {
		t.Fatalf("promote replaced user: got %v, want ErrAuthKeyNotFound", err)
	}
	setPendingLoginAge(t, ctx, dsn, keyID, lease)
	expired, ok, err := s.PendingLoginByID(ctx, keyID, lease)
	if err != nil || !ok || expired.Active {
		t.Fatalf("expired pending login = %+v, ok=%v err=%v", expired, ok, err)
	}
	if err := s.PromotePendingUser(ctx, keyID, other.ID, expired.StartedAt, lease); !errors.Is(err, store.ErrAuthKeyNotFound) {
		t.Fatalf("promote expired user: got %v, want ErrAuthKeyNotFound", err)
	}
	got, _, err := s.AuthKeyByID(ctx, keyID)
	if err != nil {
		t.Fatalf("read expired key: %v", err)
	}
	if got.UserID != 0 || got.PendingUserID != other.ID {
		t.Fatalf("expired promotion changed key: user=%d pending=%d", got.UserID, got.PendingUserID)
	}
}

func setPendingLoginAge(t *testing.T, ctx context.Context, dsn string, keyID int64, age time.Duration) {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() {
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close: %v", err)
		}
	}()
	_, err = conn.Exec(ctx, `
		UPDATE auth_keys
		SET pending_started_at = clock_timestamp() - ($2 * interval '1 microsecond')
		WHERE id = $1`, keyID, int64(age/time.Microsecond))
	if err != nil {
		t.Fatalf("age pending login: %v", err)
	}
}
