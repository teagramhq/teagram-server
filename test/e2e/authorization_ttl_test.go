package e2e_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/rsakey"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestAuthorizationTTLChoicesAreOwnerScoped(t *testing.T) {
	t.Parallel()
	f := newSmokeFixture(t)
	const phoneA, phoneB = "+15551260831", "+15551260832"
	seedSmokeUsers(t, f, phoneA, phoneB)
	a1 := newSmokeClient(t, f, "authorization-ttl-a1", phoneA)
	a2 := newSmokeClient(t, f, "authorization-ttl-a2", phoneA)
	b := newSmokeClient(t, f, "authorization-ttl-b", phoneB)

	for _, days := range []int{7, 30, 90, 180, 365} {
		if err := a1.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
			ok, err := api.AccountSetAuthorizationTTL(ctx, days)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("setAuthorizationTTL(%d) returned false", days)
			}
			return nil
		}); err != nil {
			t.Fatalf("set TTL %d: %v", days, err)
		}
		for _, client := range []*smokeClient{a1, a2} {
			if err := client.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
				auths, err := api.AccountGetAuthorizations(ctx)
				if err != nil {
					return err
				}
				if auths.AuthorizationTTLDays != days {
					return fmt.Errorf("account.getAuthorizations TTL = %d, want %d", auths.AuthorizationTTLDays, days)
				}
				return nil
			}); err != nil {
				t.Fatalf("read TTL %d from %s: %v", days, client.label, err)
			}
		}
	}

	if err := a1.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		_, err := api.AccountSetAuthorizationTTL(ctx, 30)
		return err
	}); err != nil {
		t.Fatalf("set TTL before invalid values: %v", err)
	}
	for _, days := range []int{0, 1, -7, 366, 400} {
		err := a1.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
			_, err := api.AccountSetAuthorizationTTL(ctx, days)
			return err
		})
		if !tgerr.Is(err, "TTL_DAYS_INVALID") {
			t.Fatalf("set unsupported TTL %d error = %v, want TTL_DAYS_INVALID", days, err)
		}
		if err := a1.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
			auths, err := api.AccountGetAuthorizations(ctx)
			if err != nil {
				return err
			}
			if auths.AuthorizationTTLDays != 30 {
				return fmt.Errorf("invalid TTL %d changed stored value to %d, want 30", days, auths.AuthorizationTTLDays)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}

	if err := b.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		auths, err := api.AccountGetAuthorizations(ctx)
		if err != nil {
			return err
		}
		if auths.AuthorizationTTLDays != 365 {
			return fmt.Errorf("unrelated account TTL = %d, want default 365", auths.AuthorizationTTLDays)
		}
		ok, err := api.AccountSetAuthorizationTTL(ctx, 90)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("other account setAuthorizationTTL returned false")
		}
		return nil
	}); err != nil {
		t.Fatalf("check independent account TTL: %v", err)
	}
	if err := a2.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		auths, err := api.AccountGetAuthorizations(ctx)
		if err != nil {
			return err
		}
		if auths.AuthorizationTTLDays != 30 {
			return fmt.Errorf("other account changed owner's TTL to %d, want 30", auths.AuthorizationTTLDays)
		}
		return nil
	}); err != nil {
		t.Fatalf("verify owner TTL after other account write: %v", err)
	}

	unbound := f.savedSessionClient(&session.StorageMemory{})
	if err := unbound.Run(f.ctx, func(ctx context.Context) error {
		_, err := tg.NewClient(unbound).AccountSetAuthorizationTTL(ctx, 30)
		if !tgerr.Is(err, "AUTH_KEY_UNREGISTERED") {
			return fmt.Errorf("unbound setAuthorizationTTL error = %w, want AUTH_KEY_UNREGISTERED", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("unbound setAuthorizationTTL: %v", err)
	}
}

func TestAuthorizationTTLSweepRevokesIdleSessionAcrossReplicas(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	key, err := rsakey.Bootstrap(t.TempDir() + "/key.pem")
	if err != nil {
		t.Fatal(err)
	}
	dsn := pgtest.DSN(t)
	stA, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithBlobStore(testBlobs(t)))
	if err != nil {
		t.Fatalf("open replica A store: %v", err)
	}
	t.Cleanup(func() {
		if err := stA.Close(); err != nil {
			t.Errorf("close replica A store: %v", err)
		}
	})
	stB, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithBlobStore(testBlobs(t)))
	if err != nil {
		t.Fatalf("open replica B store: %v", err)
	}
	t.Cleanup(func() {
		if err := stB.Close(); err != nil {
			t.Errorf("close replica B store: %v", err)
		}
	})

	const dcID = 2
	codes := newMultiCodeSink()
	lnA := mustListen(t, ctx, "127.0.0.1:0")
	lnB := mustListen(t, ctx, "127.0.0.1:0")
	portA, portB := tcpPort(t, lnA), tcpPort(t, lnB)
	regA, stopA := bootServerWithRegistry(t, ctx, key, dcID, stA, dsn, codes.Logger(), lnA)
	t.Cleanup(stopA)
	regB, stopB := bootServerWithRegistry(t, ctx, key, dcID, stB, dsn, codes.Logger(), lnB)
	t.Cleanup(stopB)

	const phoneA, phoneB = "+15551260841", "+15551260842"
	seedUsernameUsers(t, ctx, stA, phoneA, phoneB)
	type liveTTLClient struct {
		id      int64
		session *session.StorageMemory
		cmds    chan command
		errCh   chan error
		stopped bool
	}
	start := func(port int, phone string) *liveTTLClient {
		client := &liveTTLClient{
			session: &session.StorageMemory{},
			cmds:    make(chan command),
			errCh:   make(chan error, 1),
		}
		c := createClient(port, key, dcID, newUpdateCollector(), client.session)
		idCh := make(chan int64, 1)
		go func() { client.errCh <- runInteractive(ctx, c, flowFor(phone, codes), idCh, client.cmds) }()
		select {
		case client.id = <-idCh:
		case err := <-client.errCh:
			t.Fatalf("login client %s: %v", phone, err)
		case <-ctx.Done():
			t.Fatalf("login client %s: %v", phone, ctx.Err())
		}
		return client
	}
	call := func(client *liveTTLClient, fn func(context.Context, *tg.Client) error) error {
		done := make(chan error, 1)
		select {
		case client.cmds <- command{fn: fn, done: done}:
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
	clientKey := func(client *liveTTLClient) int64 {
		data, err := (&session.Loader{Storage: client.session}).Load(ctx)
		if err != nil {
			t.Fatalf("load client session: %v", err)
		}
		if len(data.AuthKeyID) != 8 {
			t.Fatalf("auth key id length = %d, want 8", len(data.AuthKeyID))
		}
		var id [8]byte
		copy(id[:], data.AuthKeyID)
		return mtproto.AuthKeyIDInt64(id)
	}
	clients := make([]*liveTTLClient, 0, 3)
	stopClient := func(client *liveTTLClient) {
		if client.stopped {
			return
		}
		close(client.cmds)
		select {
		case runErr := <-client.errCh:
			if runErr != nil && !errors.Is(runErr, context.Canceled) && !errors.Is(runErr, context.DeadlineExceeded) {
				t.Logf("client %d run ended with: %v", client.id, runErr)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("client %d did not stop", client.id)
		}
		client.stopped = true
	}
	t.Cleanup(func() {
		for _, client := range clients {
			stopClient(client)
		}
	})

	current := start(portA, phoneA)
	clients = append(clients, current)
	idle := start(portB, phoneA)
	clients = append(clients, idle)
	other := start(portB, phoneB)
	clients = append(clients, other)
	if current.id != idle.id {
		t.Fatalf("same-account sessions have user ids %d and %d", current.id, idle.id)
	}
	waitForOwnerConnections(t, ctx, regA, current.id, 1, "authorization TTL replica A")
	waitForOwnerConnections(t, ctx, regB, current.id, 1, "authorization TTL replica B")

	if err := call(current, func(ctx context.Context, api *tg.Client) error {
		ok, err := api.AccountSetAuthorizationTTL(ctx, 30)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("account.setAuthorizationTTL returned false")
		}
		return nil
	}); err != nil {
		t.Fatalf("set owner TTL: %v", err)
	}
	if err := call(idle, func(ctx context.Context, api *tg.Client) error {
		auths, err := api.AccountGetAuthorizations(ctx)
		if err != nil {
			return err
		}
		if auths.AuthorizationTTLDays != 30 {
			return fmt.Errorf("second session TTL = %d, want 30", auths.AuthorizationTTLDays)
		}
		return nil
	}); err != nil {
		t.Fatalf("read owner TTL on second replica: %v", err)
	}
	if err := call(other, func(ctx context.Context, api *tg.Client) error {
		auths, err := api.AccountGetAuthorizations(ctx)
		if err != nil {
			return err
		}
		if auths.AuthorizationTTLDays != 365 {
			return fmt.Errorf("unrelated account TTL = %d, want 365", auths.AuthorizationTTLDays)
		}
		_, err = api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: peerUser(other.id, current.id), Message: "history survives session expiry", RandomID: 60842,
		})
		return err
	}); err != nil {
		t.Fatalf("verify unrelated account and seed history: %v", err)
	}

	currentKeyID, idleKeyID, otherKeyID := clientKey(current), clientKey(idle), clientKey(other)
	if currentKeyID == idleKeyID || currentKeyID == otherKeyID || idleKeyID == otherKeyID {
		t.Fatal("independent sessions reused an auth key")
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to age sessions: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("close session age connection: %v", err)
		}
	})
	for _, keyID := range []int64{currentKeyID, idleKeyID, otherKeyID} {
		if _, err := conn.Exec(ctx, `UPDATE auth_keys SET last_seen_at = now() - interval '31 days' WHERE id = $1`, keyID); err != nil {
			t.Fatalf("age auth key %d: %v", keyID, err)
		}
	}
	if err := call(current, func(ctx context.Context, api *tg.Client) error {
		ok, err := api.AccountSetAuthorizationTTL(ctx, 30)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("account.setAuthorizationTTL after backdate returned false")
		}
		return nil
	}); err != nil {
		t.Fatalf("refresh caller while setting TTL: %v", err)
	}

	removed, err := stA.SweepExpiredAuthorizations(ctx, store.ExpiredAuthorizationSweepBatch)
	if err != nil {
		t.Fatalf("sweep expired authorizations: %v", err)
	}
	if len(removed) != 1 || removed[0].ID != idleKeyID || removed[0].UserID != current.id {
		t.Fatalf("swept authorizations = %+v, want only idle key %d for owner %d", removed, idleKeyID, current.id)
	}
	waitNoConn(t, regB, current.id, 3*time.Second, "idle authorization sweep on replica B")
	if len(regA.Conns(current.id)) == 0 {
		t.Fatal("the caller's active session was evicted")
	}
	if len(regB.Conns(other.id)) == 0 {
		t.Fatal("unrelated account's session was evicted")
	}
	if _, exists, err := stA.AuthKeyByID(ctx, idleKeyID); err != nil || exists {
		t.Fatalf("idle auth key after sweep exists=%v err=%v, want revoked", exists, err)
	}
	if _, exists, err := stA.AuthKeyByID(ctx, currentKeyID); err != nil || !exists {
		t.Fatalf("caller auth key after sweep exists=%v err=%v, want retained", exists, err)
	}
	if _, exists, err := stA.AuthKeyByID(ctx, otherKeyID); err != nil || !exists {
		t.Fatalf("other account auth key after sweep exists=%v err=%v, want retained", exists, err)
	}
	if err := call(current, func(ctx context.Context, api *tg.Client) error {
		auths, err := api.AccountGetAuthorizations(ctx)
		if err != nil {
			return err
		}
		if auths.AuthorizationTTLDays != 30 || len(auths.Authorizations) != 1 || !auths.Authorizations[0].Current {
			return fmt.Errorf("caller authorizations after sweep = %+v, want TTL 30 and only current session", auths)
		}
		return nil
	}); err != nil {
		t.Fatalf("active caller after sweep: %v", err)
	}
	if err := call(other, func(ctx context.Context, api *tg.Client) error {
		auths, err := api.AccountGetAuthorizations(ctx)
		if err != nil {
			return err
		}
		if auths.AuthorizationTTLDays != 365 {
			return fmt.Errorf("other account TTL after sweep = %d, want 365", auths.AuthorizationTTLDays)
		}
		return nil
	}); err != nil {
		t.Fatalf("unrelated account after sweep: %v", err)
	}

	stopClient(idle)
	refreshed := createClient(portB, key, dcID, newUpdateCollector(), idle.session)
	if err := refreshed.Run(ctx, func(ctx context.Context) error {
		status, err := refreshed.Auth().Status(ctx)
		if err != nil {
			return err
		}
		if status.Authorized {
			return errors.New("revoked key remained authorized after reconnect")
		}
		if err := refreshed.Auth().IfNecessary(ctx, flowFor(phoneA, codes)); err != nil {
			return err
		}
		historyResult, err := refreshed.API().MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
			Peer: peerUser(current.id, other.id), Limit: 10,
		})
		if err != nil {
			return err
		}
		history, ok := historyResult.(*tg.MessagesMessages)
		if !ok {
			return fmt.Errorf("history response = %T, want *tg.MessagesMessages", historyResult)
		}
		for _, item := range history.Messages {
			if message, ok := item.(*tg.Message); ok && message.Message == "history survives session expiry" {
				return nil
			}
		}
		return errors.New("signed-in device could not read its existing history")
	}); err != nil {
		t.Fatalf("sign in again and read history: %v", err)
	}
}
