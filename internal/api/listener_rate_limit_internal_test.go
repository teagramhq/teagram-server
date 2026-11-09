package api

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestTailnetRequestBypassesCallerRateLimitHelpers(t *testing.T) {
	t.Parallel()

	limit := store.RateLimitConfig{Limit: 1, Window: time.Minute}
	h := &handlers{
		rateLimitSendCodeIP: store.SendCodeIPLimits{Calls: limit, Phones: limit},
	}
	r := &mtproto.Request{
		Ctx:         context.Background(),
		UserID:      17,
		ClientAddr:  netip.Addr{},
		TailnetPath: true,
	}

	if err := h.checkRateLimitCost(r, "tailnet-account", limit, 1); err != nil {
		t.Fatalf("tailnet account rate limit = %v, want allow", err)
	}
	if err := h.checkRateLimitCostWithDeadline(r, context.Background(), "tailnet-deadline", limit, 1); err != nil {
		t.Fatalf("tailnet deadline account rate limit = %v, want allow", err)
	}
	if err := h.checkRateLimitWindow(r, "tailnet-window", limit); err != nil {
		t.Fatalf("tailnet window account rate limit = %v, want allow", err)
	}
	if err := h.checkAndChargeRateLimitIP(r, "tailnet-ip", limit); err != nil {
		t.Fatalf("tailnet IP rate limit = %v, want allow", err)
	}
	reservation, denied, err := h.reserveRateLimitIP(r, "tailnet-ip-reserve", limit)
	if err != nil || denied != nil || reservation != nil {
		t.Fatalf("tailnet IP reserve = (%v, %v, %v), want no reservation or denial", reservation, denied, err)
	}
	if err := h.checkSendCodeIP(r, "tailnet-user"); err != nil {
		t.Fatalf("tailnet sendCode IP rate limit = %v, want allow", err)
	}
}

func TestTailnetRPCDoesNotSpendPublicAccountRateLimit(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s, err := store.Open(ctx, pgtest.DSN(t), pgtest.EncKey(), store.WithoutBlobStore())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})

	user, err := s.CreateUser(ctx, "+15551298001")
	if err != nil {
		t.Fatalf("create caller: %v", err)
	}
	h := &handlers{store: s}
	limit := store.RateLimitConfig{Limit: 1, Window: time.Minute}
	for range 10 {
		if err := h.checkRateLimitCost(&mtproto.Request{
			Ctx: ctx, UserID: user.ID, TailnetPath: true,
		}, "listener-path", limit, 1); err != nil {
			t.Fatalf("tailnet RPC rate limit = %v, want allow", err)
		}
	}
	public := &mtproto.Request{Ctx: ctx, UserID: user.ID}
	if err := h.checkRateLimitCost(public, "listener-path", limit, 1); err != nil {
		t.Fatalf("first public RPC = %v, want allow", err)
	}
	err = h.checkRateLimitCost(public, "listener-path", limit, 1)
	var rpc *tgerr.Error
	if !errors.As(err, &rpc) || rpc.Code != 420 {
		t.Fatal("second public RPC passed after tailnet calls; caller account budget was not preserved")
	}
}

func TestTailnetStillEnforcesTargetAccountPasswordFailureBudget(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s, err := store.Open(ctx, pgtest.DSN(t), pgtest.EncKey(), store.WithoutBlobStore())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})

	user, err := s.CreateUser(ctx, "+15551298002")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := s.UpsertPassword(ctx, store.UserPassword{
		UserID: user.ID, Salt1: []byte("salt1"), Salt2: []byte("salt2"), Verifier: make([]byte, 256),
	}); err != nil {
		t.Fatalf("set password: %v", err)
	}
	var authKeyID [8]byte
	authKeyID[7] = 1
	keyID := mtproto.AuthKeyIDInt64(authKeyID)
	if err := s.SaveAuthKey(ctx, keyID, []byte("key")); err != nil {
		t.Fatalf("save auth key: %v", err)
	}
	if err := s.SetPendingUser(ctx, keyID, user.ID); err != nil {
		t.Fatalf("set pending user: %v", err)
	}

	h := &handlers{
		store:                  s,
		log:                    slog.New(slog.DiscardHandler),
		rateLimitCheckPassword: store.RateLimitConfig{Limit: 1, Window: time.Minute},
		rateLimitCheckPasswordIP: store.RateLimitConfig{
			Limit: 1, Window: time.Minute,
		},
	}
	check := func() error {
		var body bin.Buffer
		if err := (&tg.AuthCheckPasswordRequest{Password: &tg.InputCheckPasswordSRP{
			A: make([]byte, 256), M1: make([]byte, 256),
		}}).Encode(&body); err != nil {
			t.Fatalf("encode request: %v", err)
		}
		_, err := h.handleCheckPassword(&mtproto.Request{
			Ctx: ctx, AuthKeyID: authKeyID, ClientAddr: netip.Addr{}, TailnetPath: true, Buf: &body,
		})
		return err
	}
	if err := check(); err == nil || tgerr.Is(err, "FLOOD_WAIT") {
		t.Fatalf("first tailnet password proof = %v, want proof failure after admission", err)
	}
	if err := check(); !tgerr.Is(err, "FLOOD_WAIT") {
		t.Fatalf("second tailnet password proof = %v, want target-account FLOOD_WAIT", err)
	}
}

func TestTailnetStillEnforcesTargetAccountPasswordProofBudget(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s, err := store.Open(ctx, pgtest.DSN(t), pgtest.EncKey(), store.WithoutBlobStore())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})

	publicUser, err := s.CreateUser(ctx, "+15551298003")
	if err != nil {
		t.Fatalf("create public user: %v", err)
	}
	tailnetUser, err := s.CreateUser(ctx, "+15551298004")
	if err != nil {
		t.Fatalf("create tailnet user: %v", err)
	}
	for _, user := range []store.User{publicUser, tailnetUser} {
		if err := s.UpsertPassword(ctx, store.UserPassword{
			UserID: user.ID, Salt1: []byte("salt1"), Salt2: []byte("salt2"), Verifier: make([]byte, 256),
		}); err != nil {
			t.Fatalf("set password for user %d: %v", user.ID, err)
		}
	}

	h := &handlers{
		store:                  s,
		log:                    slog.New(slog.DiscardHandler),
		rateLimitPasswordProof: store.RateLimitConfig{Limit: 1, Window: time.Minute},
	}
	check := func(userID int64, tailnet bool) error {
		var body bin.Buffer
		if err := (&tg.AccountGetPasswordSettingsRequest{Password: &tg.InputCheckPasswordSRP{
			A: make([]byte, 256), M1: make([]byte, 256),
		}}).Encode(&body); err != nil {
			t.Fatalf("encode request: %v", err)
		}
		_, err := h.handleGetPasswordSettings(&mtproto.Request{
			Ctx: ctx, UserID: userID, TailnetPath: tailnet, Buf: &body,
		})
		return err
	}

	for _, tc := range []struct {
		name    string
		userID  int64
		tailnet bool
	}{
		{name: "public", userID: publicUser.ID},
		{name: "tailnet", userID: tailnetUser.ID, tailnet: true},
	} {
		if err := check(tc.userID, tc.tailnet); !tgerr.Is(err, "SRP_ID_INVALID") {
			t.Fatalf("first %s password proof = %v, want SRP_ID_INVALID after admission", tc.name, err)
		}
		if err := check(tc.userID, tc.tailnet); !tgerr.Is(err, "FLOOD_WAIT") {
			t.Fatalf("second %s password proof = %v, want target-account FLOOD_WAIT", tc.name, err)
		}
	}
}
