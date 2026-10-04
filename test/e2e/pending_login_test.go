package e2e_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/rsakey"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestPendingLoginCrossReplicaLeaseAndExpiry(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Minute)
	defer cancel()

	key, err := rsakey.Bootstrap(filepath.Join(t.TempDir(), "key.pem"))
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
	const phone = "+15551296701"
	const password = "pending-lease-password"
	codes := newCodeSink()
	listenerA := mustListen(t, ctx, "127.0.0.1:0")
	listenerB := mustListen(t, ctx, "127.0.0.1:0")
	portA, portB := tcpPort(t, listenerA), tcpPort(t, listenerB)
	t.Cleanup(bootServerWithDelivery(t, ctx, key, dcID, st, dsn, codes.Logger(), listenerA))
	t.Cleanup(bootServerWithDelivery(t, ctx, key, dcID, st, dsn, codes.Logger(), listenerB))

	user, err := st.CreateUser(ctx, phone)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	verifier, salt1, salt2, err := testComputeSRPVerifier([]byte(password))
	if err != nil {
		t.Fatalf("compute password verifier: %v", err)
	}
	if err := st.UpsertPassword(ctx, store.UserPassword{UserID: user.ID, Salt1: salt1, Salt2: salt2, Verifier: verifier}); err != nil {
		t.Fatalf("set password: %v", err)
	}

	newClient := func(port int, sess telegram.SessionStorage) *telegram.Client {
		return telegram.NewClient(1, "hash", telegram.Options{
			DC:             dcID,
			DCList:         dcs.List{Options: []tg.DCOption{{ID: dcID, IPAddress: "127.0.0.1", Port: port}}},
			PublicKeys:     []telegram.PublicKey{{RSA: &key.PublicKey}},
			Resolver:       dcs.Plain(dcs.PlainOptions{}),
			SessionStorage: sess,
		})
	}
	codeAuth := auth.CodeAuthenticatorFunc(func(ctx context.Context, _ *tg.AuthSentCode) (string, error) {
		return codes.wait(ctx)
	})
	sess := &session.StorageMemory{}
	clientA := newClient(portA, sess)
	flow := auth.NewFlow(auth.CodeOnly(phone, codeAuth), auth.SendCodeOptions{})
	if err := clientA.Run(ctx, func(ctx context.Context) error {
		return clientA.Auth().IfNecessary(ctx, flow)
	}); !errors.Is(err, auth.ErrPasswordNotProvided) {
		t.Fatalf("signIn on replica A = %v, want SESSION_PASSWORD_NEEDED", err)
	}
	keyID := pendingAuthKeyID(t, ctx, dsn, user.ID)
	initial, ok, err := st.AuthKeyByID(ctx, keyID)
	if err != nil || !ok || initial.PendingUserID != user.ID || initial.PendingStartedAt.IsZero() {
		t.Fatalf("initial pending state = %+v, ok=%v err=%v", initial, ok, err)
	}

	// Simulate a reconnect at T+9m without sleeping. Replica B must retain only
	// the final minute of the original database-started lease.
	if err := agePendingLogin(t, ctx, dsn, keyID, 9*time.Minute); err != nil {
		t.Fatal(err)
	}
	clientB := newClient(portB, sess)
	if err := clientB.Run(ctx, func(ctx context.Context) error {
		challenge, err := clientB.API().AccountGetPassword(ctx)
		if err != nil {
			return fmt.Errorf("getPassword on replica B at T+9m: %w", err)
		}
		srpB, ok := challenge.GetSRPB()
		if !ok {
			return errors.New("getPassword returned no SRP B")
		}
		proof, err := auth.PasswordHash([]byte(password), challenge.SRPID, srpB, challenge.SecureRandom, challenge.CurrentAlgo)
		if err != nil {
			return fmt.Errorf("build proof: %w", err)
		}
		response, err := clientB.API().AuthCheckPassword(ctx, proof)
		if err != nil {
			return fmt.Errorf("checkPassword on replica B at T+9m: %w", err)
		}
		if _, ok := response.(*tg.AuthAuthorization); !ok {
			return fmt.Errorf("checkPassword response = %T, want authorization", response)
		}
		return nil
	}); err != nil {
		t.Fatalf("cross-replica completion within the original lease: %v", err)
	}
	bound, ok, err := st.AuthKeyByID(ctx, keyID)
	if err != nil || !ok || bound.UserID != user.ID || bound.PendingUserID != 0 {
		t.Fatalf("promoted key after T+9m proof = %+v, ok=%v err=%v", bound, ok, err)
	}

	clientB = newClient(portB, sess)
	var proof *tg.InputCheckPasswordSRP
	if err := clientB.Run(ctx, func(ctx context.Context) error {
		if err := stagePhoneLogin(ctx, clientB.API(), phone, codes); err != nil {
			return err
		}
		state, ok, err := st.AuthKeyByID(ctx, keyID)
		if err != nil {
			return fmt.Errorf("read fresh signIn state: %w", err)
		}
		if !ok || state.PendingUserID != user.ID || !state.PendingStartedAt.After(initial.PendingStartedAt) {
			return fmt.Errorf("fresh signIn state = %+v, ok=%v", state, ok)
		}
		challenge, err := clientB.API().AccountGetPassword(ctx)
		if err != nil {
			return fmt.Errorf("getPassword before expiry: %w", err)
		}
		srpB, ok := challenge.GetSRPB()
		if !ok {
			return errors.New("getPassword returned no SRP B")
		}
		proof, err = auth.PasswordHash([]byte(password), challenge.SRPID, srpB, challenge.SecureRandom, challenge.CurrentAlgo)
		if err != nil {
			return fmt.Errorf("build expiring proof: %w", err)
		}
		if err := agePendingLogin(t, ctx, dsn, keyID, 10*time.Minute); err != nil {
			return err
		}
		if _, err := clientB.API().AuthCheckPassword(ctx, proof); !tgerr.Is(err, "AUTH_KEY_UNREGISTERED") {
			if err != nil {
				return fmt.Errorf("valid proof at the deadline: %w", err)
			}
			return errors.New("valid proof at the deadline succeeded, want AUTH_KEY_UNREGISTERED")
		}
		return nil
	}); err != nil {
		t.Fatalf("expired checkPassword: %v", err)
	}
	expired, ok, err := st.AuthKeyByID(ctx, keyID)
	if err != nil || !ok || expired.UserID != 0 || expired.PendingUserID != 0 || !expired.PendingStartedAt.IsZero() {
		t.Fatalf("expired proof state = %+v, ok=%v err=%v; want unbound and cleared", expired, ok, err)
	}

	clientB = newClient(portB, sess)
	if err := clientB.Run(ctx, func(ctx context.Context) error {
		if err := stagePhoneLogin(ctx, clientB.API(), phone, codes); err != nil {
			return err
		}
		if err := agePendingLogin(t, ctx, dsn, keyID, 10*time.Minute); err != nil {
			return err
		}
		if _, err := clientB.API().AccountGetPassword(ctx); !tgerr.Is(err, "AUTH_KEY_UNREGISTERED") {
			if err != nil {
				return fmt.Errorf("getPassword at the deadline: %w", err)
			}
			return errors.New("getPassword at the deadline succeeded, want AUTH_KEY_UNREGISTERED")
		}
		return nil
	}); err != nil {
		t.Fatalf("expired getPassword: %v", err)
	}
	cleared, ok, err := st.AuthKeyByID(ctx, keyID)
	if err != nil || !ok || cleared.UserID != 0 || cleared.PendingUserID != 0 || !cleared.PendingStartedAt.IsZero() {
		t.Fatalf("expired getPassword state = %+v, ok=%v err=%v; want unbound and cleared", cleared, ok, err)
	}
}

func stagePhoneLogin(ctx context.Context, api *tg.Client, phone string, codes *codeSink) error {
	sent, err := api.AuthSendCode(ctx, &tg.AuthSendCodeRequest{
		PhoneNumber: phone,
		APIID:       1,
		APIHash:     "hash",
	})
	if err != nil {
		return fmt.Errorf("sendCode: %w", err)
	}
	codeState, ok := sent.(*tg.AuthSentCode)
	if !ok {
		return fmt.Errorf("sendCode response = %T, want *tg.AuthSentCode", sent)
	}
	phoneCode, err := codes.wait(ctx)
	if err != nil {
		return fmt.Errorf("wait for phone code: %w", err)
	}
	_, err = api.AuthSignIn(ctx, &tg.AuthSignInRequest{
		PhoneNumber:   phone,
		PhoneCodeHash: codeState.PhoneCodeHash,
		PhoneCode:     phoneCode,
	})
	if !isSessionPasswordNeeded(err) {
		if err != nil {
			return fmt.Errorf("signIn: %w", err)
		}
		return errors.New("signIn succeeded, want SESSION_PASSWORD_NEEDED")
	}
	return nil
}

func pendingAuthKeyID(t *testing.T, ctx context.Context, dsn string, userID int64) int64 {
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
	var keyID int64
	if err := conn.QueryRow(ctx, `SELECT id FROM auth_keys WHERE pending_user_id = $1`, userID).Scan(&keyID); err != nil {
		t.Fatalf("find pending auth key: %v", err)
	}
	return keyID
}

func agePendingLogin(t *testing.T, ctx context.Context, dsn string, keyID int64, age time.Duration) error {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
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
		return fmt.Errorf("age pending login: %w", err)
	}
	return nil
}
