package api_test

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/crypto"
	gotdsrp "github.com/gotd/td/crypto/srp"
	"github.com/gotd/td/mt"
	gotdauth "github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/srp"
	"github.com/teagramhq/teagram-server/internal/store"
)

type firstPasswordCandidate struct {
	password string
	salt1    []byte
	salt2    []byte
	verifier []byte
	hint     string
	email    string
}

func newFirstPasswordCandidate(t *testing.T, label, password string, seed byte) firstPasswordCandidate {
	t.Helper()
	salt1 := bytes.Repeat([]byte{seed}, 32)
	salt2 := bytes.Repeat([]byte{seed + 1}, 32)
	verifier, augmentedSalt1, err := gotdsrp.NewSRP(cryptorand.Reader).NewHash([]byte(password), gotdsrp.Input{
		Salt1: salt1,
		Salt2: salt2,
		G:     srp.G,
		P:     srp.PBytes(),
	})
	if err != nil {
		t.Fatalf("generate SRP verifier for %s: %v", label, err)
	}
	return firstPasswordCandidate{
		password: password,
		salt1:    augmentedSalt1,
		salt2:    salt2,
		verifier: verifier,
		hint:     label + " hint",
		email:    label + "@example.invalid",
	}
}

func firstPasswordRequest(t *testing.T, candidate firstPasswordCandidate) *bin.Buffer {
	t.Helper()
	var buf bin.Buffer
	if err := firstPasswordRPCRequest(candidate).Encode(&buf); err != nil {
		t.Fatalf("encode first password request: %v", err)
	}
	return &buf
}

func firstPasswordRPCRequest(candidate firstPasswordCandidate) *tg.AccountUpdatePasswordSettingsRequest {
	return &tg.AccountUpdatePasswordSettingsRequest{
		Password: &tg.InputCheckPasswordEmpty{},
		NewSettings: tg.AccountPasswordInputSettings{
			NewAlgo: &tg.PasswordKdfAlgoSHA256SHA256PBKDF2HMACSHA512iter100000SHA256ModPow{
				Salt1: candidate.salt1,
				Salt2: candidate.salt2,
				G:     srp.G,
				P:     srp.PBytes(),
			},
			NewPasswordHash: candidate.verifier,
			Hint:            candidate.hint,
			Email:           candidate.email,
		},
	}
}

func openPasswordRaceStore(t *testing.T, dsn, appName string) *store.Store {
	t.Helper()
	separator := "&"
	if !strings.Contains(dsn, "?") {
		separator = "?"
	}
	s, err := store.Open(context.Background(), dsn+separator+"application_name="+appName, pgtest.EncKey(), store.WithoutBlobStore())
	if err != nil {
		t.Fatalf("open password race store %q: %v", appName, err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close password race store %q: %v", appName, err)
		}
	})
	return s
}

func createProvisionalPasswordAccount(t *testing.T, ctx context.Context, s *store.Store, handle string) *store.User {
	t.Helper()
	user, err := s.CreateUsernameUser(ctx, handle, "First", "Password")
	if err != nil {
		t.Fatalf("create provisional user: %v", err)
	}
	if err := s.ClaimUsername(ctx, user.ID, handle); err != nil {
		t.Fatalf("claim provisional username: %v", err)
	}
	return &user
}

func waitForPasswordInsertWaiters(t *testing.T, ctx context.Context, conn *pgx.Conn, appNames []string, want int, waitEvent string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var count int
		err := conn.QueryRow(ctx, `
			SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database()
			  AND application_name = ANY($1::text[])
			  AND wait_event_type = 'Lock'
			  AND ($2 = '' OR wait_event = $2)
			  AND query ILIKE '%INSERT INTO user_passwords%'
		`, appNames, waitEvent).Scan(&count)
		if err != nil {
			t.Fatalf("inspect first password setup waiters: %v", err)
		}
		if count >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	rows, err := conn.Query(ctx, `
		SELECT pid, application_name, coalesce(wait_event_type, ''), coalesce(wait_event, ''), coalesce(left(query, 240), '')
		FROM pg_stat_activity
		WHERE datname = current_database() AND pid <> pg_backend_pid()
	`)
	if err != nil {
		t.Fatalf("timed out waiting for %d first password insert waiters; inspect sessions: %v", want, err)
	}
	defer rows.Close()
	var sessions []string
	for rows.Next() {
		var pid int
		var appName, eventType, event, query string
		if err := rows.Scan(&pid, &appName, &eventType, &event, &query); err != nil {
			t.Fatalf("timed out waiting for %d first password insert waiters; scan sessions: %v", want, err)
		}
		sessions = append(sessions, fmt.Sprintf("pid=%d app=%q wait=%s/%s query=%q", pid, appName, eventType, event, query))
	}
	t.Fatalf("timed out waiting for %d first password insert waiters; sessions: %s", want, strings.Join(sessions, "; "))
}

func passwordHashInvalid(err error) bool {
	var rpc *tgerr.Error
	return errors.As(err, &rpc) && rpc.Code == 400 && rpc.Message == "PASSWORD_HASH_INVALID"
}

func requireStoredPasswordCandidate(t *testing.T, ctx context.Context, s *store.Store, userID int64, want firstPasswordCandidate) {
	t.Helper()
	got, ok, err := s.PasswordByUser(ctx, userID)
	if err != nil || !ok {
		t.Fatalf("load first password: found=%v err=%v", ok, err)
	}
	if !bytes.Equal(got.Salt1, want.salt1) || !bytes.Equal(got.Salt2, want.salt2) ||
		!bytes.Equal(got.Verifier, want.verifier) || got.Hint != want.hint ||
		got.RecoveryEmail != want.email || !got.HasRecovery {
		t.Fatalf("stored first password changed: got salts=(%x,%x) hint=%q email=%q recovery=%v", got.Salt1, got.Salt2, got.Hint, got.RecoveryEmail, got.HasRecovery)
	}
}

func TestConcurrentFirstPasswordSetupKeepsExactlyOneWinner(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	dsn := pgtest.DSN(t)
	storeA := openPasswordRaceStore(t, dsn, "password_race_a")
	storeB := openPasswordRaceStore(t, dsn, "password_race_b")
	user := createProvisionalPasswordAccount(t, ctx, storeA, "pwconcurrent")

	const creatingKeyID = int64(0x22670001)
	if err := storeA.SaveAuthKey(ctx, creatingKeyID, make([]byte, 256)); err != nil {
		t.Fatalf("save creating auth key: %v", err)
	}
	if err := storeA.BindAuthKeyUser(ctx, creatingKeyID, user.ID); err != nil {
		t.Fatalf("bind creating auth key: %v", err)
	}

	candidateA := newFirstPasswordCandidate(t, "candidate-a", "first password A", 0x31)
	candidateB := newFirstPasswordCandidate(t, "candidate-b", "first password B", 0x51)

	lockConn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect password write blocker: %v", err)
	}
	t.Cleanup(func() {
		if err := lockConn.Close(context.Background()); err != nil {
			t.Errorf("close password write blocker: %v", err)
		}
	})
	if _, err := lockConn.Exec(ctx, `
		CREATE FUNCTION pause_simultaneous_password_setup() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF current_setting('application_name', true) IN ('password_race_a', 'password_race_b') THEN
				PERFORM pg_advisory_xact_lock(2267, 2);
			END IF;
			RETURN NEW;
		END
		$$
	`); err != nil {
		t.Fatalf("install simultaneous password setup function: %v", err)
	}
	if _, err := lockConn.Exec(ctx, `
		CREATE TRIGGER pause_simultaneous_password_setup
		BEFORE INSERT ON user_passwords
		FOR EACH ROW EXECUTE FUNCTION pause_simultaneous_password_setup()
	`); err != nil {
		t.Fatalf("install simultaneous password setup trigger: %v", err)
	}
	if _, err := lockConn.Exec(ctx, "SELECT pg_advisory_lock(2267, 2)"); err != nil {
		t.Fatalf("hold simultaneous password setup: %v", err)
	}
	lockHeld := true
	defer func() {
		if lockHeld {
			_, err := lockConn.Exec(context.Background(), "SELECT pg_advisory_unlock(2267, 2)")
			if err != nil {
				t.Errorf("release simultaneous password setup: %v", err)
			}
		}
	}()

	type result struct {
		candidate firstPasswordCandidate
		err       error
	}
	results := make(chan result, 2)
	requestA := firstPasswordRequest(t, candidateA)
	requestB := firstPasswordRequest(t, candidateB)
	go func() {
		_, err := api.UpdatePasswordSettingsForTestWithContext(ctx, storeA, user.ID, requestA)
		results <- result{candidate: candidateA, err: err}
	}()
	go func() {
		_, err := api.UpdatePasswordSettingsForTestWithContext(ctx, storeB, user.ID, requestB)
		results <- result{candidate: candidateB, err: err}
	}()

	waitForPasswordInsertWaiters(t, ctx, lockConn, []string{"password_race_a", "password_race_b"}, 2, "advisory")
	var unlocked bool
	if err := lockConn.QueryRow(ctx, "SELECT pg_advisory_unlock(2267, 2)").Scan(&unlocked); err != nil || !unlocked {
		t.Fatalf("release simultaneous password writes: unlocked=%v err=%v", unlocked, err)
	}
	lockHeld = false

	successes, conflicts := 0, 0
	var winner firstPasswordCandidate
	for range 2 {
		select {
		case got := <-results:
			switch {
			case got.err == nil:
				successes++
				winner = got.candidate
			case passwordHashInvalid(got.err):
				conflicts++
			default:
				t.Fatalf("first password setup failed unexpectedly: %v", got.err)
			}
		case <-ctx.Done():
			t.Fatalf("first password setup responses timed out: %v", ctx.Err())
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("first password responses: successes=%d conflicts=%d, want one each", successes, conflicts)
	}
	requireStoredPasswordCandidate(t, ctx, storeA, user.ID, winner)

	key, found, err := storeA.AuthKeyByID(ctx, creatingKeyID)
	if err != nil || !found || key.UserID != user.ID {
		t.Fatalf("creating auth key binding after setup: key=%+v found=%v err=%v", key, found, err)
	}
	if key.Provisional {
		t.Fatal("creating auth key remained provisional after first password commit")
	}

	assertFirstPasswordLogin(t, ctx, storeA, user.ID, "pwconcurrent", winner.password, true, 2)
	loser := candidateA
	if winner.password == candidateA.password {
		loser = candidateB
	}
	assertFirstPasswordLogin(t, ctx, storeA, user.ID, "pwconcurrent", loser.password, false, 3)
}

func TestDelayedFirstPasswordSetupCannotReplaceCommittedVerifier(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	dsn := pgtest.DSN(t)
	storeA := openPasswordRaceStore(t, dsn, "password_delayed_winner")
	storeB := openPasswordRaceStore(t, dsn, "password_delayed_loser")
	user := createProvisionalPasswordAccount(t, ctx, storeA, "pwdelayed")

	lockConn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect delayed password blocker: %v", err)
	}
	t.Cleanup(func() {
		if err := lockConn.Close(context.Background()); err != nil {
			t.Errorf("close delayed password blocker: %v", err)
		}
	})
	if _, err := lockConn.Exec(ctx, `
		CREATE FUNCTION pause_delayed_password_setup() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF current_setting('application_name', true) = 'password_delayed_loser' THEN
				PERFORM pg_advisory_xact_lock(2267, 1);
			END IF;
			RETURN NEW;
		END
		$$;
	`); err != nil {
		t.Fatalf("install delayed password setup function: %v", err)
	}
	if _, err := lockConn.Exec(ctx, `
		CREATE TRIGGER pause_delayed_password_setup
		BEFORE INSERT ON user_passwords
		FOR EACH ROW EXECUTE FUNCTION pause_delayed_password_setup()
	`); err != nil {
		t.Fatalf("install delayed password setup trigger: %v", err)
	}
	if _, err := lockConn.Exec(ctx, "SELECT pg_advisory_lock(2267, 1)"); err != nil {
		t.Fatalf("hold delayed password setup: %v", err)
	}
	defer func() {
		_, err := lockConn.Exec(context.Background(), "SELECT pg_advisory_unlock(2267, 1)")
		if err != nil {
			t.Errorf("release delayed password setup: %v", err)
		}
	}()

	delayed := newFirstPasswordCandidate(t, "delayed", "delayed password", 0x71)
	winner := newFirstPasswordCandidate(t, "winner", "winning password", 0x91)
	type result struct{ err error }
	resultCh := make(chan result, 1)
	delayedRequest := firstPasswordRequest(t, delayed)
	go func() {
		_, err := api.UpdatePasswordSettingsForTestWithContext(ctx, storeB, user.ID, delayedRequest)
		resultCh <- result{err: err}
	}()
	waitForPasswordInsertWaiters(t, ctx, lockConn, []string{"password_delayed_loser"}, 1, "advisory")

	if _, err := api.UpdatePasswordSettingsForTestWithContext(ctx, storeA, user.ID, firstPasswordRequest(t, winner)); err != nil {
		t.Fatalf("commit winning first password while delayed request waits: %v", err)
	}
	var unlocked bool
	if err := lockConn.QueryRow(ctx, "SELECT pg_advisory_unlock(2267, 1)").Scan(&unlocked); err != nil || !unlocked {
		t.Fatalf("release delayed password setup: unlocked=%v err=%v", unlocked, err)
	}

	select {
	case got := <-resultCh:
		if !passwordHashInvalid(got.err) {
			t.Fatalf("delayed first password error = %v, want PASSWORD_HASH_INVALID", got.err)
		}
	case <-ctx.Done():
		t.Fatalf("delayed first password response timed out: %v", ctx.Err())
	}
	requireStoredPasswordCandidate(t, ctx, storeA, user.ID, winner)

	if _, err := api.UpdatePasswordSettingsForTestWithContext(ctx, storeB, user.ID, firstPasswordRequest(t, delayed)); !passwordHashInvalid(err) {
		t.Fatalf("repeat first password setup error = %v, want PASSWORD_HASH_INVALID", err)
	}
	requireStoredPasswordCandidate(t, ctx, storeA, user.ID, winner)
}

func assertFirstPasswordLogin(t *testing.T, ctx context.Context, s *store.Store, userID int64, handle, password string, wantSuccess bool, keyByte byte) {
	t.Helper()
	var keyID [8]byte
	keyID[0] = 0x22
	keyID[6] = 0x67
	keyID[7] = keyByte
	if err := s.SaveAuthKey(ctx, mtproto.AuthKeyIDInt64(keyID), make([]byte, 256)); err != nil {
		t.Fatalf("save fresh login key: %v", err)
	}
	hash, _, err := s.IssueCodeForUsername(ctx, handle)
	if err != nil {
		t.Fatalf("issue fresh login code: %v", err)
	}
	_, err = api.SignInForTestWithLimits(s, keyID, netip.MustParseAddr("192.0.2.226"), store.RateLimitConfig{}, &tg.AuthSignInRequest{
		PhoneNumber:   handle,
		PhoneCodeHash: hash,
	})
	if !isSessionPasswordNeeded(err) {
		t.Fatalf("fresh signIn error = %v, want SESSION_PASSWORD_NEEDED", err)
	}

	h := api.SharedHandlersForTest(s)
	var getPasswordBuf bin.Buffer
	if err := (&tg.AccountGetPasswordRequest{}).Encode(&getPasswordBuf); err != nil {
		t.Fatalf("encode getPassword: %v", err)
	}
	passwordResult, err := api.HandleGetPassword(h, &mtproto.Request{Ctx: ctx, AuthKeyID: keyID, Buf: &getPasswordBuf})
	if err != nil {
		t.Fatalf("getPassword: %v", err)
	}
	passwordState, ok := passwordResult.(*tg.AccountPassword)
	if !ok || !passwordState.HasPassword {
		t.Fatalf("getPassword result = %T, want password challenge", passwordResult)
	}
	srpB, ok := passwordState.GetSRPB()
	if !ok || len(srpB) == 0 {
		t.Fatal("getPassword returned no SRP challenge")
	}
	proof, err := gotdauth.PasswordHash([]byte(password), passwordState.SRPID, srpB,
		passwordState.SecureRandom, passwordState.CurrentAlgo)
	if err != nil {
		t.Fatalf("generate SRP proof: %v", err)
	}
	var checkBuf bin.Buffer
	if err := (&tg.AuthCheckPasswordRequest{Password: proof}).Encode(&checkBuf); err != nil {
		t.Fatalf("encode checkPassword: %v", err)
	}
	result, err := api.HandleCheckPassword(h, &mtproto.Request{Ctx: ctx, AuthKeyID: keyID, Buf: &checkBuf})
	if wantSuccess {
		if err != nil {
			t.Fatalf("winner SRP login failed: %v", err)
		}
		authorization, ok := result.(*tg.AuthAuthorization)
		if !ok || authorization.User.GetID() != userID {
			t.Fatalf("winner SRP result = %T, want authorization for user %d", result, userID)
		}
		return
	}
	if !passwordHashInvalid(err) {
		t.Fatalf("loser SRP login error = %v, want PASSWORD_HASH_INVALID", err)
	}
}

func persistedTestKey(seed byte) crypto.AuthKey {
	var raw crypto.Key
	for i := range raw {
		raw[i] = seed + byte(i)
	}
	return raw.WithID()
}

func dispatchWithPersistedAuthKey(t *testing.T, ctx context.Context, h mtproto.Handler, s *store.Store, key crypto.AuthKey, req bin.Encoder) []byte {
	t.Helper()
	keyID := mtproto.AuthKeyIDInt64(key.ID)
	persisted, found, err := s.AuthKeyByID(ctx, keyID)
	if err != nil || !found {
		t.Fatalf("load persisted auth key before dispatch: found=%v err=%v", found, err)
	}
	var keyValue crypto.Key
	copy(keyValue[:], persisted.Value)
	reconnectedKey := keyValue.WithID()
	if reconnectedKey.ID != key.ID {
		t.Fatal("persisted auth key material changed its ID")
	}
	transport := &settingsDispatcherTransport{}
	conn := mtproto.NewTestConn(transport, reconnectedKey)
	var buf bin.Buffer
	if err := req.Encode(&buf); err != nil {
		t.Fatalf("encode dispatched request: %v", err)
	}
	const msgID = int64(1 << 32)
	if err := h.OnMessage(conn, &mtproto.Request{
		Ctx:         ctx,
		AuthKeyID:   reconnectedKey.ID,
		UserID:      persisted.UserID,
		Provisional: persisted.Provisional,
		MsgID:       msgID,
		Buf:         &buf,
	}); err != nil {
		t.Fatalf("dispatch request with persisted auth key: %v", err)
	}
	return transport.result(t, reconnectedKey, msgID)
}

func assertDispatchRPCError(t *testing.T, body []byte, want string) {
	t.Helper()
	var rpc mt.RPCError
	if err := rpc.Decode(&bin.Buffer{Buf: body}); err != nil {
		t.Fatalf("decode RPC error (want %s): %v", want, err)
	}
	if rpc.ErrorMessage != want {
		t.Fatalf("RPC error = %d %s, want %s", rpc.ErrorCode, rpc.ErrorMessage, want)
	}
}

func TestProvisionalCreatingKeyReconnectsAndLiftsOperationGate(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	dsn := pgtest.DSN(t)
	storeA := openPasswordRaceStore(t, dsn, "password_reconnect_original")
	storeB := openPasswordRaceStore(t, dsn, "password_reconnect_replica")
	user := createProvisionalPasswordAccount(t, ctx, storeA, "pwreconnect")
	recipient, err := storeA.CreateUser(ctx, "+15552267001")
	if err != nil {
		t.Fatalf("create message recipient: %v", err)
	}
	key := persistedTestKey(0x26)
	keyID := mtproto.AuthKeyIDInt64(key.ID)
	if err := storeA.SaveAuthKey(ctx, keyID, key.Value[:]); err != nil {
		t.Fatalf("save creating auth key: %v", err)
	}
	if err := storeA.BindAuthKeyUser(ctx, keyID, user.ID); err != nil {
		t.Fatalf("bind creating auth key: %v", err)
	}

	before, found, err := storeB.AuthKeyByID(ctx, keyID)
	if err != nil || !found || before.UserID != user.ID || !before.Provisional {
		t.Fatalf("reconnected provisional key = %+v found=%v err=%v", before, found, err)
	}
	hB := fullChannelDispatcher(storeB)
	blockedBody := dispatchWithPersistedAuthKey(t, ctx, hB, storeB, key, &tg.MessagesSendMessageRequest{
		Peer: api.InputPeerUser(user.ID, recipient.ID), Message: "before setup", RandomID: 1,
	})
	assertDispatchRPCError(t, blockedBody, "AUTH_KEY_UNREGISTERED")

	candidate := newFirstPasswordCandidate(t, "reconnected", "reconnected password", 0xa1)
	setupBody := dispatchWithPersistedAuthKey(t, ctx, hB, storeB, key, firstPasswordRPCRequest(candidate))
	var setupResult tg.BoolTrue
	if err := setupResult.Decode(&bin.Buffer{Buf: setupBody}); err != nil {
		t.Fatalf("decode password setup response: %v", err)
	}

	after, found, err := storeA.AuthKeyByID(ctx, keyID)
	if err != nil || !found || after.UserID != user.ID || after.Provisional {
		t.Fatalf("creating key after committed setup = %+v found=%v err=%v", after, found, err)
	}
	if _, exists, err := storeA.PasswordByUser(ctx, user.ID); err != nil || !exists {
		t.Fatalf("read committed password: exists=%v err=%v", exists, err)
	}

	hA := fullChannelDispatcher(storeA)
	activeBody := dispatchWithPersistedAuthKey(t, ctx, hA, storeA, key, &tg.MessagesSendMessageRequest{
		Peer: api.InputPeerUser(user.ID, recipient.ID), Message: "after setup", RandomID: 2,
	})
	var updates tg.Updates
	if err := updates.Decode(&bin.Buffer{Buf: activeBody}); err != nil {
		t.Fatalf("operation after password setup did not pass the provisional gate: %v", err)
	}
}

func TestIndependentUnboundKeyCannotSetProvisionalPasswordThroughDispatch(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	s := openStore(t)
	user := createProvisionalPasswordAccount(t, ctx, s, "pwunbound")
	creatorKey := persistedTestKey(0x37)
	creatorKeyID := mtproto.AuthKeyIDInt64(creatorKey.ID)
	if err := s.SaveAuthKey(ctx, creatorKeyID, creatorKey.Value[:]); err != nil {
		t.Fatalf("save creating auth key: %v", err)
	}
	if err := s.BindAuthKeyUser(ctx, creatorKeyID, user.ID); err != nil {
		t.Fatalf("bind creating auth key: %v", err)
	}
	independentKey := persistedTestKey(0x58)
	independentKeyID := mtproto.AuthKeyIDInt64(independentKey.ID)
	if err := s.SaveAuthKey(ctx, independentKeyID, independentKey.Value[:]); err != nil {
		t.Fatalf("save independent auth key: %v", err)
	}
	h := fullChannelDispatcher(s)

	hash, _, err := s.IssueCodeForUsername(ctx, "pwunbound")
	if err != nil {
		t.Fatalf("issue sign-in code: %v", err)
	}
	signInBody := dispatchWithPersistedAuthKey(t, ctx, h, s, independentKey, &tg.AuthSignInRequest{
		PhoneNumber: "pwunbound", PhoneCodeHash: hash,
	})
	assertDispatchRPCError(t, signInBody, "INTERNAL")

	candidate := newFirstPasswordCandidate(t, "unbound", "unbound password", 0xc1)
	updateBody := dispatchWithPersistedAuthKey(t, ctx, h, s, independentKey, firstPasswordRPCRequest(candidate))
	assertDispatchRPCError(t, updateBody, "AUTH_KEY_UNREGISTERED")

	stillUnbound, found, err := s.AuthKeyByID(ctx, independentKeyID)
	if err != nil || !found || stillUnbound.UserID != 0 || stillUnbound.PendingUserID != 0 {
		t.Fatalf("independent key after denied setup = %+v found=%v err=%v", stillUnbound, found, err)
	}
	if _, exists, err := s.PasswordByUser(ctx, user.ID); err != nil || exists {
		t.Fatalf("denied independent key set a verifier: exists=%v err=%v", exists, err)
	}
}

func TestPasswordChangeStillRequiresCurrentSRPProof(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	s := openStore(t)
	user, err := s.CreateUser(ctx, "+15552267002")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	oldPassword := newFirstPasswordCandidate(t, "old", "current password", 0xd1)
	newPassword := newFirstPasswordCandidate(t, "new", "replacement password", 0xe1)
	if err := s.UpsertPassword(ctx, store.UserPassword{
		UserID: user.ID, Salt1: oldPassword.salt1, Salt2: oldPassword.salt2,
		Verifier: oldPassword.verifier, Hint: oldPassword.hint,
		RecoveryEmail: oldPassword.email, HasRecovery: true,
	}); err != nil {
		t.Fatalf("seed current password: %v", err)
	}
	var authKeyID [8]byte
	authKeyID[7] = 0x62
	if err := s.SaveAuthKey(ctx, mtproto.AuthKeyIDInt64(authKeyID), make([]byte, 256)); err != nil {
		t.Fatalf("save auth key: %v", err)
	}
	if err := s.BindAuthKeyUser(ctx, mtproto.AuthKeyIDInt64(authKeyID), user.ID); err != nil {
		t.Fatalf("bind auth key: %v", err)
	}
	h := api.SharedHandlersForTest(s)
	makeProof := func(password string) *tg.InputCheckPasswordSRP {
		t.Helper()
		var getPasswordBuf bin.Buffer
		if err := (&tg.AccountGetPasswordRequest{}).Encode(&getPasswordBuf); err != nil {
			t.Fatalf("encode getPassword: %v", err)
		}
		result, err := api.HandleGetPassword(h, &mtproto.Request{
			Ctx: ctx, UserID: user.ID, AuthKeyID: authKeyID, Buf: &getPasswordBuf,
		})
		if err != nil {
			t.Fatalf("getPassword: %v", err)
		}
		state, ok := result.(*tg.AccountPassword)
		if !ok || !state.HasPassword {
			t.Fatalf("getPassword result = %T, want password challenge", result)
		}
		srpB, ok := state.GetSRPB()
		if !ok || len(srpB) == 0 {
			t.Fatal("getPassword returned no SRP challenge")
		}
		proof, err := gotdauth.PasswordHash([]byte(password), state.SRPID, srpB, state.SecureRandom, state.CurrentAlgo)
		if err != nil {
			t.Fatalf("generate SRP proof: %v", err)
		}
		return proof
	}
	update := func(proof *tg.InputCheckPasswordSRP) error {
		t.Helper()
		req := firstPasswordRPCRequest(newPassword)
		req.Password = proof
		var buf bin.Buffer
		if err := req.Encode(&buf); err != nil {
			t.Fatalf("encode password change: %v", err)
		}
		_, err := api.UpdatePasswordSettingsWithProofLimits(s, user.ID, authKeyID, store.RateLimitConfig{}, &buf)
		return err
	}

	if err := update(makeProof("incorrect current password")); !passwordHashInvalid(err) {
		t.Fatalf("password change with invalid SRP proof error = %v, want PASSWORD_HASH_INVALID", err)
	}
	requireStoredPasswordCandidate(t, ctx, s, user.ID, oldPassword)
	if err := update(makeProof(oldPassword.password)); err != nil {
		t.Fatalf("password change with valid current proof: %v", err)
	}
	requireStoredPasswordCandidate(t, ctx, s, user.ID, newPassword)
}
