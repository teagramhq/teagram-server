package e2e_test

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/rsakey"
	"github.com/teagramhq/teagram-server/internal/store"
)

type resetAuthClient struct {
	session   *session.StorageMemory
	collector *updateCollector
	commands  chan command
	done      chan error
	userID    int64
	authKeyID int64
	evicted   bool
	stopOnce  sync.Once
	stopErr   error
}

func (c *resetAuthClient) stop() error {
	c.stopOnce.Do(func() {
		close(c.commands)
		c.stopErr = <-c.done
	})
	return c.stopErr
}

func resetAuthCall(ctx context.Context, client *resetAuthClient, fn func(context.Context, *tg.Client) error) error {
	done := make(chan error, 1)
	select {
	case client.commands <- command{fn: fn, done: done}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func startResetAuthClient(
	t *testing.T,
	ctx context.Context,
	key *rsa.PrivateKey,
	dcID, port int,
	phone string,
	codes *multiCodeSink,
	clients *[]*resetAuthClient,
) *resetAuthClient {
	t.Helper()
	sess := &session.StorageMemory{}
	collector := newUpdateCollector()
	client := newReplicaClient(key, dcID, port, sess, collector, nil)
	interactive := &resetAuthClient{
		session:   sess,
		collector: collector,
		commands:  make(chan command),
		done:      make(chan error, 1),
	}
	*clients = append(*clients, interactive)
	self := make(chan int64, 1)
	go func() {
		interactive.done <- runInteractive(ctx, client, replicaAuthFlow(codes, phone), self, interactive.commands)
	}()
	select {
	case interactive.userID = <-self:
	case <-ctx.Done():
		t.Fatalf("login %s: %v", phone, ctx.Err())
	}
	var err error
	interactive.authKeyID, err = passwordResetAuthKeyID(ctx, sess)
	if err != nil {
		t.Fatalf("load auth key for %s: %v", phone, err)
	}
	return interactive
}

func assertResetMessage(t *testing.T, ctx context.Context, client *resetAuthClient, text string, peerID int64) {
	t.Helper()
	message := recvOrCtx(t, ctx, client.collector.newMsg, "message "+text)
	if message.Message != text || message.Out {
		t.Fatalf("message = {text:%q out:%v}, want incoming %q", message.Message, message.Out, text)
	}
	peer, ok := message.PeerID.(*tg.PeerUser)
	if !ok || peer.UserID != peerID {
		t.Fatalf("message peer = %+v, want user %d", message.PeerID, peerID)
	}
}

func TestResetAuthorizationsAcrossReplicas(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	key, err := rsakey.Bootstrap(t.TempDir() + "/key.pem")
	if err != nil {
		t.Fatal(err)
	}
	dsn := pgtest.DSN(t)
	st, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithBlobStore(testBlobs(t)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("store close: %v", err)
		}
	})

	const dcID = 2
	codes := newMultiCodeSink()
	listener1 := mustListen(t, ctx, "127.0.0.1:0")
	listener2 := mustListen(t, ctx, "127.0.0.1:0")
	port1, port2 := tcpPort(t, listener1), tcpPort(t, listener2)
	registry1, stop1 := bootServerWithRegistry(t, ctx, key, dcID, st, dsn, codes.Logger(), listener1)
	t.Cleanup(stop1)
	registry2, stop2 := bootServerWithRegistry(t, ctx, key, dcID, st, dsn, codes.Logger(), listener2)
	t.Cleanup(stop2)

	const ownerPhone = "+15551296300"
	const foreignPhone = "+15551296301"
	seedPhoneUsers(t, ctx, st, ownerPhone, foreignPhone)
	var clients []*resetAuthClient
	t.Cleanup(func() {
		for _, client := range clients {
			if err := client.stop(); err != nil && !client.evicted && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("client run: %v", err)
			}
		}
	})

	removedA := startResetAuthClient(t, ctx, key, dcID, port1, ownerPhone, codes, &clients)
	removedB := startResetAuthClient(t, ctx, key, dcID, port2, ownerPhone, codes, &clients)
	caller := startResetAuthClient(t, ctx, key, dcID, port2, ownerPhone, codes, &clients)
	foreign1 := startResetAuthClient(t, ctx, key, dcID, port1, foreignPhone, codes, &clients)
	foreign2 := startResetAuthClient(t, ctx, key, dcID, port2, foreignPhone, codes, &clients)
	if caller.userID != removedA.userID || caller.userID != removedB.userID {
		t.Fatalf("owner login ids = %d, %d, %d", removedA.userID, removedB.userID, caller.userID)
	}
	ownerID := caller.userID

	waitForResetOwnerConnections(t, ctx, []*mtproto.SessionRegistry{registry1, registry2}, ownerID, 3)

	owner, ok, err := st.UserByPhone(ctx, ownerPhone)
	if err != nil || !ok {
		t.Fatalf("owner lookup: found=%v err=%v", ok, err)
	}
	const password = "reset-pending-password"
	verifier, salt1, salt2, err := testComputeSRPVerifier([]byte(password))
	if err != nil {
		t.Fatalf("compute password verifier: %v", err)
	}
	if err := st.UpsertPassword(ctx, store.UserPassword{UserID: owner.ID, Salt1: salt1, Salt2: salt2, Verifier: verifier}); err != nil {
		t.Fatalf("set owner password: %v", err)
	}

	// Stage a genuine phone-code password login and retain its valid proof. The
	// reset must remove the pending row so this proof cannot promote it later.
	pendingSession := &session.StorageMemory{}
	pendingClient := newReplicaClient(key, dcID, port1, pendingSession, nil, nil)
	var proof *tg.InputCheckPasswordSRP
	if err := pendingClient.Run(ctx, func(ctx context.Context) error {
		api := pendingClient.API()
		codeState, err := api.AuthSendCode(ctx, &tg.AuthSendCodeRequest{PhoneNumber: ownerPhone, APIID: 1, APIHash: "hash"})
		if err != nil {
			return fmt.Errorf("send code for pending login: %w", err)
		}
		sent, ok := codeState.(*tg.AuthSentCode)
		if !ok {
			return fmt.Errorf("sendCode response = %T, want *tg.AuthSentCode", codeState)
		}
		phoneCode, err := codes.wait(ctx, ownerPhone)
		if err != nil {
			return fmt.Errorf("wait for pending login code: %w", err)
		}
		_, err = api.AuthSignIn(ctx, &tg.AuthSignInRequest{PhoneNumber: ownerPhone, PhoneCodeHash: sent.PhoneCodeHash, PhoneCode: phoneCode})
		if !tgerr.Is(err, "SESSION_PASSWORD_NEEDED") {
			if err != nil {
				return fmt.Errorf("signIn for pending login: %w", err)
			}
			return errors.New("signIn completed without SESSION_PASSWORD_NEEDED")
		}
		challenge, err := api.AccountGetPassword(ctx)
		if err != nil {
			return fmt.Errorf("get pending login password challenge: %w", err)
		}
		proof, err = auth.PasswordHash([]byte(password), challenge.SRPID, challenge.SRPB, challenge.SecureRandom, challenge.CurrentAlgo)
		if err != nil {
			return fmt.Errorf("build valid pending login proof: %w", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("stage pending password login: %v", err)
	}
	pendingKeyID, err := passwordResetAuthKeyID(ctx, pendingSession)
	if err != nil {
		t.Fatalf("load pending auth key: %v", err)
	}
	pending, exists, err := st.AuthKeyByID(ctx, pendingKeyID)
	if err != nil || !exists || pending.PendingUserID != ownerID {
		t.Fatalf("pending auth key = %+v, exists=%v err=%v", pending, exists, err)
	}

	// Keep an unbound key live across the reset. It remains useful for allowed
	// anonymous calls and must not be selected by the owner-scoped delete.
	unboundSession := &session.StorageMemory{}
	unboundClient := newReplicaClient(key, dcID, port1, unboundSession, nil, nil)
	if err := unboundClient.Run(ctx, func(ctx context.Context) error {
		config, err := tg.NewClient(unboundClient).HelpGetConfig(ctx)
		if err != nil {
			return err
		}
		if config.Date <= 0 {
			return fmt.Errorf("help.getConfig date = %d, want positive", config.Date)
		}
		return nil
	}); err != nil {
		t.Fatalf("create unbound session: %v", err)
	}
	unboundKeyID, err := passwordResetAuthKeyID(ctx, unboundSession)
	if err != nil {
		t.Fatalf("load unbound auth key: %v", err)
	}

	listener, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect eviction observer: %v", err)
	}
	t.Cleanup(func() {
		if err := listener.Close(context.Background()); err != nil {
			t.Errorf("close eviction observer: %v", err)
		}
	})
	if _, err := listener.Exec(ctx, "LISTEN "+store.ChannelEvict); err != nil {
		t.Fatalf("listen for evictions: %v", err)
	}

	var evictionDeadline time.Time
	if err := resetAuthCall(ctx, caller, func(ctx context.Context, api *tg.Client) error {
		ok, err := api.AuthResetAuthorizations(ctx)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("auth.resetAuthorizations returned false")
		}
		evictionDeadline = time.Now().Add(3 * time.Second)
		return nil
	}); err != nil {
		t.Fatalf("reset other authorizations: %v", err)
	}
	evictionCtx, stopEviction := context.WithDeadline(ctx, evictionDeadline)
	defer stopEviction()
	waitForResetAuthKeyEviction(t, evictionCtx, registry1, ownerID, removedA.authKeyID)
	waitForResetAuthKeyEviction(t, evictionCtx, registry2, ownerID, removedB.authKeyID)
	if err := resetAuthCall(ctx, caller, func(ctx context.Context, api *tg.Client) error {
		auths, err := api.AccountGetAuthorizations(ctx)
		if err != nil {
			return err
		}
		if len(auths.Authorizations) != 1 || !auths.Authorizations[0].Current || auths.Authorizations[0].Hash != caller.authKeyID {
			return fmt.Errorf("authorizations after reset = %+v, want only caller key %d marked Current", auths.Authorizations, caller.authKeyID)
		}
		return nil
	}); err != nil {
		t.Fatalf("reset other authorizations: %v", err)
	}

	// Only the two bound owner keys were evicted. The pending row was deleted,
	// but it had no bound connection to publish an eviction for.
	wantEvictions := map[string]bool{
		store.EvictPayload(ownerID, removedA.authKeyID): true,
		store.EvictPayload(ownerID, removedB.authKeyID): true,
	}
	for range len(wantEvictions) {
		waitCtx, stop := context.WithTimeout(ctx, 5*time.Second)
		notification, err := listener.WaitForNotification(waitCtx)
		stop()
		if err != nil {
			t.Fatalf("wait for target eviction: %v", err)
		}
		if !wantEvictions[notification.Payload] {
			t.Fatalf("unexpected eviction payload %q", notification.Payload)
		}
		delete(wantEvictions, notification.Payload)
	}
	if len(wantEvictions) != 0 {
		t.Fatalf("missing evictions: %v", wantEvictions)
	}

	// Repeating the operation succeeds and emits no further target evictions.
	if err := resetAuthCall(ctx, caller, func(ctx context.Context, api *tg.Client) error {
		ok, err := api.AuthResetAuthorizations(ctx)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("retry auth.resetAuthorizations returned false")
		}
		return nil
	}); err != nil {
		t.Fatalf("retry reset: %v", err)
	}
	quietCtx, stopQuiet := context.WithTimeout(ctx, 200*time.Millisecond)
	_, err = listener.WaitForNotification(quietCtx)
	stopQuiet()
	if err == nil {
		t.Fatal("retry emitted an additional target eviction")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait for unexpected retry eviction: %v", err)
	}

	// The unrelated owner remains signed in on both replicas, and the unbound
	// anonymous key still serves an allowed call after the owner reset.
	foreignKeys, err := st.AuthKeysByUser(ctx, foreign1.userID)
	if err != nil || len(foreignKeys) != 2 {
		t.Fatalf("foreign owner sessions = %d, want 2; err=%v", len(foreignKeys), err)
	}
	if err := resetAuthCall(ctx, foreign1, func(ctx context.Context, api *tg.Client) error {
		auths, err := api.AccountGetAuthorizations(ctx)
		if err != nil {
			return err
		}
		if len(auths.Authorizations) != 2 {
			return fmt.Errorf("foreign authorizations = %d, want 2", len(auths.Authorizations))
		}
		return nil
	}); err != nil {
		t.Fatalf("foreign session after owner reset: %v", err)
	}
	if key, exists, err := st.AuthKeyByID(ctx, unboundKeyID); err != nil || !exists || key.UserID != 0 || key.PendingUserID != 0 {
		t.Fatalf("unbound key after reset = %+v, exists=%v err=%v", key, exists, err)
	}
	unboundAgain := newReplicaClient(key, dcID, port2, unboundSession, nil, nil)
	if err := unboundAgain.Run(ctx, func(ctx context.Context) error {
		config, err := tg.NewClient(unboundAgain).HelpGetConfig(ctx)
		if err != nil {
			return err
		}
		if config.Date <= 0 {
			return fmt.Errorf("post-reset help.getConfig date = %d, want positive", config.Date)
		}
		return nil
	}); err != nil {
		t.Fatalf("unbound key after reset: %v", err)
	}

	// B and both foreign sessions can still send and receive, while neither
	// terminated owner's silent connection sees a message sent after the reply.
	const ownerMessage = "owner after terminate all"
	if err := resetAuthCall(ctx, caller, func(ctx context.Context, api *tg.Client) error {
		_, err := api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: peerUser(caller.userID, foreign1.userID), Message: ownerMessage, RandomID: 1553001,
		})
		return err
	}); err != nil {
		t.Fatalf("caller send after reset: %v", err)
	}
	assertResetMessage(t, ctx, foreign1, ownerMessage, caller.userID)
	assertResetMessage(t, ctx, foreign2, ownerMessage, caller.userID)

	const foreignMessage1 = "foreign replica one after terminate all"
	if err := resetAuthCall(ctx, foreign1, func(ctx context.Context, api *tg.Client) error {
		_, err := api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: peerUser(foreign1.userID, caller.userID), Message: foreignMessage1, RandomID: 1553002,
		})
		return err
	}); err != nil {
		t.Fatalf("foreign replica one send after reset: %v", err)
	}
	assertResetMessage(t, ctx, caller, foreignMessage1, foreign1.userID)

	const foreignMessage2 = "foreign replica two after terminate all"
	if err := resetAuthCall(ctx, foreign2, func(ctx context.Context, api *tg.Client) error {
		_, err := api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: peerUser(foreign2.userID, caller.userID), Message: foreignMessage2, RandomID: 1553003,
		})
		return err
	}); err != nil {
		t.Fatalf("foreign replica two send after reset: %v", err)
	}
	assertResetMessage(t, ctx, caller, foreignMessage2, foreign2.userID)

	targetSilence := time.NewTimer(3 * time.Second)
	defer targetSilence.Stop()
	for {
		select {
		case message := <-removedA.collector.newMsg:
			t.Fatalf("first terminated session received post-reply message %q", message.Message)
		case message := <-removedB.collector.newMsg:
			t.Fatalf("second terminated session received post-reply message %q", message.Message)
		case <-targetSilence.C:
			goto targetsStayedSilent
		case <-ctx.Done():
			t.Fatalf("wait for terminated sessions: %v", ctx.Err())
		}
	}

targetsStayedSilent:
	waitForResetOwnerConnections(t, ctx, []*mtproto.SessionRegistry{registry1, registry2}, ownerID, 1)
	ownerKeys, err := st.AuthKeysByUser(ctx, ownerID)
	if err != nil || len(ownerKeys) != 1 || ownerKeys[0].ID != caller.authKeyID {
		t.Fatalf("owner keys after reset = %+v, want only caller key %d; err=%v", ownerKeys, caller.authKeyID, err)
	}
	if _, exists, err := st.AuthKeyByID(ctx, pendingKeyID); err != nil || exists {
		t.Fatalf("pending auth key after reset exists=%v err=%v", exists, err)
	}
	pendingAgain := newReplicaClient(key, dcID, port1, pendingSession, nil, nil)
	if err := pendingAgain.Run(ctx, func(ctx context.Context) error {
		_, err := pendingAgain.API().AuthCheckPassword(ctx, proof)
		if !tgerr.Is(err, "AUTH_KEY_UNREGISTERED") {
			if err != nil {
				return fmt.Errorf("valid password proof after reset: %w", err)
			}
			return errors.New("pending password login completed after terminate all")
		}
		return nil
	}); err != nil {
		t.Fatalf("valid password proof after reset: %v", err)
	}

	// Reconnects using each terminated session's cached key cannot regain access.
	for _, target := range []*resetAuthClient{removedA, removedB} {
		target.evicted = true
		if err := target.stop(); err != nil {
			t.Logf("terminated client run ended with: %v", err)
		}
		reconnected := newReplicaClient(key, dcID, port1, target.session, nil, nil)
		if err := reconnected.Run(ctx, func(ctx context.Context) error {
			_, err := reconnected.API().AccountGetAuthorizations(ctx)
			if !tgerr.Is(err, "AUTH_KEY_UNREGISTERED") {
				if err != nil {
					return fmt.Errorf("cached key reconnect error = %w, want AUTH_KEY_UNREGISTERED", err)
				}
				return errors.New("cached key reconnect remained authorized")
			}
			return nil
		}); err != nil {
			t.Fatalf("reconnect terminated session: %v", err)
		}
	}
}

func TestResetAuthorizationsRejectsRevokedInflightCaller(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	key, err := rsakey.Bootstrap(t.TempDir() + "/key.pem")
	if err != nil {
		t.Fatal(err)
	}
	dsn := pgtest.DSN(t)
	st, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithBlobStore(testBlobs(t)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("store close: %v", err)
		}
	})

	const dcID = 2
	const phone = "+15551296302"
	codes := newMultiCodeSink()
	listener := mustListen(t, ctx, "127.0.0.1:0")
	port := tcpPort(t, listener)
	registry, stopServer := bootServerWithRegistry(t, ctx, key, dcID, st, dsn, codes.Logger(), listener)
	t.Cleanup(stopServer)
	seedPhoneUsers(t, ctx, st, phone)

	var clients []*resetAuthClient
	t.Cleanup(func() {
		for _, client := range clients {
			if err := client.stop(); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("client run: %v", err)
			}
		}
	})
	winner := startResetAuthClient(t, ctx, key, dcID, port, phone, codes, &clients)
	caller := startResetAuthClient(t, ctx, key, dcID, port, phone, codes, &clients)
	target := startResetAuthClient(t, ctx, key, dcID, port, phone, codes, &clients)
	waitForResetOwnerConnections(t, ctx, []*mtproto.SessionRegistry{registry}, winner.userID, 3)
	if winner.userID != caller.userID || winner.userID != target.userID {
		t.Fatalf("owner ids = %d, %d, %d", winner.userID, caller.userID, target.userID)
	}

	blocker, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect owner-lock blocker: %v", err)
	}
	t.Cleanup(func() {
		if err := blocker.Close(context.Background()); err != nil {
			t.Errorf("close owner-lock blocker: %v", err)
		}
	})
	tx, err := blocker.Begin(ctx)
	if err != nil {
		t.Fatalf("begin owner-lock blocker: %v", err)
	}
	committed := false
	t.Cleanup(func() {
		if committed {
			return
		}
		if err := tx.Rollback(context.Background()); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Errorf("release owner-lock blocker: %v", err)
		}
	})
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", winner.userID); err != nil {
		t.Fatalf("hold owner lock: %v", err)
	}
	observer, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect lock observer: %v", err)
	}
	t.Cleanup(func() {
		if err := observer.Close(context.Background()); err != nil {
			t.Errorf("close lock observer: %v", err)
		}
	})

	callResult := make(chan error, 1)
	select {
	case caller.commands <- command{fn: func(ctx context.Context, api *tg.Client) error {
		ok, err := api.AuthResetAuthorizations(ctx)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("auth.resetAuthorizations returned false")
		}
		return nil
	}, done: callResult}:
	case <-ctx.Done():
		t.Fatalf("enqueue reset request: %v", ctx.Err())
	}
	waitForResetOwnerLockWait(t, ctx, observer)

	// Remove the key only after the request has passed frame authentication and
	// is queued behind the owner lock. Its transaction must recheck the caller.
	if err := st.DeleteAuthKey(ctx, caller.authKeyID); err != nil {
		t.Fatalf("revoke inflight caller key: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("release owner-lock blocker: %v", err)
	}
	committed = true

	select {
	case err := <-callResult:
		if !tgerr.Is(err, "AUTH_KEY_UNREGISTERED") {
			t.Fatalf("revoked inflight caller result = %v, want AUTH_KEY_UNREGISTERED", err)
		}
	case <-ctx.Done():
		t.Fatalf("wait for revoked inflight caller result: %v", ctx.Err())
	}

	for _, retained := range []int64{winner.authKeyID, target.authKeyID} {
		if key, exists, err := st.AuthKeyByID(ctx, retained); err != nil || !exists || key.UserID != winner.userID {
			t.Fatalf("winning owner's retained key %d = %+v, exists=%v err=%v", retained, key, exists, err)
		}
	}
	if _, exists, err := st.AuthKeyByID(ctx, caller.authKeyID); err != nil || exists {
		t.Fatalf("revoked inflight caller key exists=%v err=%v", exists, err)
	}
	if err := resetAuthCall(ctx, winner, func(ctx context.Context, api *tg.Client) error {
		auths, err := api.AccountGetAuthorizations(ctx)
		if err != nil {
			return err
		}
		if len(auths.Authorizations) != 2 {
			return fmt.Errorf("winning owner authorizations = %d, want winner and untouched target", len(auths.Authorizations))
		}
		return nil
	}); err != nil {
		t.Fatalf("winning caller after rejected in-flight reset: %v", err)
	}
}

func waitForResetOwnerConnections(t *testing.T, ctx context.Context, registries []*mtproto.SessionRegistry, ownerID int64, want int) {
	t.Helper()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		var keyIDs []int64
		for _, registry := range registries {
			for _, conn := range registry.Conns(ownerID) {
				keyIDs = append(keyIDs, conn.AuthKeyID())
			}
		}
		if len(keyIDs) == want {
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("owner %d has connections %v, want %d: %v", ownerID, keyIDs, want, ctx.Err())
		}
	}
}

func waitForResetAuthKeyEviction(t *testing.T, ctx context.Context, registry *mtproto.SessionRegistry, ownerID, authKeyID int64) {
	t.Helper()
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("reset eviction context has no deadline")
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if !time.Now().Before(deadline) {
			t.Fatalf("auth key %d remained in owner %d registry beyond the eviction deadline", authKeyID, ownerID)
		}
		found := false
		for _, conn := range registry.Conns(ownerID) {
			if conn.AuthKeyID() == authKeyID {
				found = true
				break
			}
		}
		if !found {
			if !time.Now().Before(deadline) {
				t.Fatalf("auth key %d disappeared from owner %d registry after the eviction deadline", authKeyID, ownerID)
			}
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("auth key %d remained in owner %d registry until eviction deadline: %v", authKeyID, ownerID, ctx.Err())
		}
	}
}

func waitForResetOwnerLockWait(t *testing.T, ctx context.Context, observer *pgx.Conn) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		err := observer.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1
				FROM pg_stat_activity
				WHERE datname = current_database()
				  AND wait_event_type = 'Lock'
				  AND query LIKE '%pg_advisory_xact_lock%'
			)
		`).Scan(&waiting)
		if err != nil {
			t.Fatalf("observe owner lock wait: %v", err)
		}
		if waiting {
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("reset request never waited for the owner lock: %v", ctx.Err())
		}
	}
}
