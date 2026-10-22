package api_test

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/mt"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestPasswordRecoveryUnavailable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)

	accounts := make([]struct {
		userID      int64
		state       store.UserPassword
		replacement firstPasswordCandidate
	}, 0, 2)
	for i, account := range []struct {
		phone string
		email string
	}{{phone: "+15551297601", email: "stored@example.invalid"}, {phone: "+15551297602"}} {
		user, err := s.CreateUser(ctx, account.phone)
		if err != nil {
			t.Fatalf("create account: %v", err)
		}
		candidate := newFirstPasswordCandidate(t, "recovery", "recovery password", byte(0xa1+i*2))
		replacement := newFirstPasswordCandidate(t, "replacement", "replacement password", byte(0xb1+i*2))
		password := store.UserPassword{
			UserID:        user.ID,
			Salt1:         candidate.salt1,
			Salt2:         candidate.salt2,
			Verifier:      candidate.verifier,
			Hint:          candidate.hint,
			RecoveryEmail: account.email,
		}
		inserted, err := s.InsertPasswordIfAbsent(ctx, password)
		if err != nil || !inserted {
			t.Fatalf("store password for account: inserted=%t err=%v", inserted, err)
		}
		accounts = append(accounts, struct {
			userID      int64
			state       store.UserPassword
			replacement firstPasswordCandidate
		}{userID: user.ID, state: password, replacement: replacement})
	}

	for _, mode := range []config.RecoveryEmailMode{config.RecoveryEmailOff, config.RecoveryEmailContact} {
		t.Run(string(mode), func(t *testing.T) {
			var logs strings.Builder
			log := slog.New(slog.NewTextHandler(&logs, nil))
			dispatcher := api.NewWithPhotoThumbsAndDialogFilterSyncAndUploadPartTTLAndTurnstileAndWelcomeChannelAndSignIn(
				s, 2, &tg.Config{}, log, false, api.TestMaxFileBytes, nil, api.TestMaxUserStorageBytes,
				pgtest.PeerDeriver(), pgtest.PhotoDeriver(), config.RateLimitsConfig{}, config.RegistrationInvite,
				api.NewDialogFilterSync(), nil, config.DefaultUploadPartTTL, "", nil, "", false, mode,
			)

			for _, account := range accounts {
				before, ok, err := s.PasswordByUser(ctx, account.userID)
				if err != nil || !ok || !reflect.DeepEqual(before, account.state) {
					t.Fatalf("initial password state for user %d = %+v, ok=%t err=%v; want %+v", account.userID, before, ok, err, account.state)
				}

				recoverRequest := &tg.AuthRecoverPasswordRequest{Code: "recovery-code-marker"}
				newSettings := firstPasswordRPCRequest(account.replacement).NewSettings
				newSettings.Email = "recovery-email-input-marker"
				recoverRequest.SetNewSettings(newSettings)
				for _, caller := range []struct {
					name   string
					userID int64
				}{
					{name: "authorized", userID: account.userID},
					{name: "pendingLogin", userID: 0},
				} {
					t.Run(caller.name, func(t *testing.T) {
						for _, tc := range []struct {
							name string
							req  bin.Encoder
							want string
						}{
							{name: "requestPasswordRecovery", req: &tg.AuthRequestPasswordRecoveryRequest{}, want: "PASSWORD_RECOVERY_NA"},
							{name: "checkRecoveryPassword", req: &tg.AuthCheckRecoveryPasswordRequest{Code: "recovery-code-marker"}, want: "CODE_INVALID"},
							{name: "recoverPassword", req: recoverRequest, want: "CODE_INVALID"},
						} {
							t.Run(tc.name, func(t *testing.T) {
								code, message := dispatchPasswordRecoveryError(t, dispatcher, caller.userID, tc.req)
								if code != 400 || message != tc.want {
									t.Errorf("%s returned %d %s, want 400 %s", tc.name, code, message, tc.want)
								}
							})
						}

						after, ok, err := s.PasswordByUser(ctx, account.userID)
						if err != nil || !ok || !reflect.DeepEqual(after, before) {
							t.Errorf("%s recovery calls changed password state for user %d: before=%+v after=%+v ok=%t err=%v", caller.name, account.userID, before, after, ok, err)
						}
					})
				}
			}

			for _, secret := range []string{"stored@example.invalid", "recovery-code-marker", "recovery-email-input-marker"} {
				if strings.Contains(logs.String(), secret) {
					t.Errorf("logs contain recovery data %q", secret)
				}
			}
		})
	}
}

func dispatchPasswordRecoveryError(t *testing.T, handler mtproto.Handler, userID int64, req bin.Encoder) (int, string) {
	t.Helper()
	key := testKey()
	transport := &settingsDispatcherTransport{}
	conn := mtproto.NewTestConn(transport, key)
	var body bin.Buffer
	if err := req.Encode(&body); err != nil {
		t.Fatalf("encode password recovery request: %v", err)
	}
	const msgID = int64(1 << 32)
	err := handler.OnMessage(conn, &mtproto.Request{
		AuthKeyID: key.ID, UserID: userID, MsgID: msgID, Buf: &body, Ctx: context.Background(),
	})
	if err != nil {
		if rpc, ok := errors.AsType[*tgerr.Error](err); ok {
			return rpc.Code, rpc.Message
		}
		t.Fatalf("dispatch password recovery request: %v", err)
	}
	response := transport.result(t, key, msgID)
	var rpc mt.RPCError
	if err := rpc.Decode(&bin.Buffer{Buf: response}); err != nil {
		t.Fatalf("decode password recovery error: %v", err)
	}
	return rpc.ErrorCode, rpc.ErrorMessage
}
