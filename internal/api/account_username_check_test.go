package api_test

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/mt"
	"github.com/gotd/td/tg"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/store"
)

func checkAccountUsername(
	t *testing.T, s *store.Store, userID int64, username string,
) (bool, *mt.RPCError) {
	t.Helper()
	return checkAccountUsernameWithAddr(t, fullChannelDispatcher(s), userID, netip.Addr{}, username)
}

func checkAccountUsernameWithAddr(
	t *testing.T, h mtproto.Handler, userID int64, addr netip.Addr, username string,
) (bool, *mt.RPCError) {
	t.Helper()
	body := dispatchSettingsWithAddr(t, h, settingsHandler{
		name: "account.checkUsername",
		request: func() bin.Encoder {
			return &tg.AccountCheckUsernameRequest{Username: username}
		},
	}, userID, false, addr)

	var available tg.BoolTrue
	if err := available.Decode(&bin.Buffer{Buf: body}); err == nil {
		return true, nil
	}
	var unavailable tg.BoolFalse
	if err := unavailable.Decode(&bin.Buffer{Buf: body}); err == nil {
		return false, nil
	}
	var rpc mt.RPCError
	if err := rpc.Decode(&bin.Buffer{Buf: body}); err != nil {
		t.Fatalf("decode account.checkUsername response: %v", err)
	}
	return false, &rpc
}

func checkUsernameLimitConfig(t *testing.T, limit store.RateLimitConfig) config.RateLimitsConfig {
	t.Helper()
	return config.RateLimitsConfig{CheckUsernameIP: limit}
}

func usernameLookupTotalCount(t *testing.T, pool *pgxpool.Pool, callerID int64) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM username_lookups WHERE caller_id = $1", callerID,
	).Scan(&count); err != nil {
		t.Fatalf("count username lookups: %v", err)
	}
	return count
}

func TestCheckAccountUsernameAvailabilityAndValidation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	caller, err := s.CreateUser(ctx, "+15551298101")
	if err != nil {
		t.Fatalf("caller: %v", err)
	}
	if _, err := api.UpdateUsernameForTest(s, caller.ID, "ownhandle"); err != nil {
		t.Fatalf("claim own username: %v", err)
	}

	userOwner, err := s.CreateUser(ctx, "+15551298102")
	if err != nil {
		t.Fatalf("user owner: %v", err)
	}
	if _, err := api.UpdateUsernameForTest(s, userOwner.ID, "takenhandle"); err != nil {
		t.Fatalf("claim user username: %v", err)
	}
	channelOwner, err := s.CreateUser(ctx, "+15551298103")
	if err != nil {
		t.Fatalf("channel owner: %v", err)
	}
	channel, err := s.CreateChannel(ctx, channelOwner.ID, "Taken handle", "", false)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	if _, err := api.EditChannelUsernameForTest(s, channelOwner.ID, &tg.ChannelsUpdateUsernameRequest{
		Channel:  api.InputChannel(channelOwner.ID, channel.ID),
		Username: "channelhandle",
	}); err != nil {
		t.Fatalf("claim channel username: %v", err)
	}

	for _, tc := range []struct {
		name string
		want bool
	}{
		{name: "freehandle", want: true},
		{name: "OwnHandle", want: true},
		{name: "TAKENHANDLE", want: false},
		{name: "CHANNELHANDLE", want: false},
	} {
		available, rpc := checkAccountUsername(t, s, caller.ID, tc.name)
		if rpc != nil || available != tc.want {
			t.Errorf("check %q = %t, %v; want %t", tc.name, available, rpc, tc.want)
		}
	}

	pool := usernameLookupPool(t, dsn)
	invalidUsernames := []string{"bad-name", "admin", "bot", "bots", "xbot", "x_Bot", "WeatherBOT"}
	before := 0
	for _, username := range invalidUsernames {
		before += usernameLookupCount(t, pool, caller.ID, username)
	}
	for _, username := range invalidUsernames {
		if _, rpc := checkAccountUsername(t, s, caller.ID, username); rpc == nil || rpc.ErrorMessage != "USERNAME_INVALID" {
			t.Errorf("check invalid %q error = %v, want USERNAME_INVALID", username, rpc)
		}
	}
	after := 0
	for _, username := range invalidUsernames {
		after += usernameLookupCount(t, pool, caller.ID, username)
	}
	if after != before {
		t.Errorf("invalid checks changed lookup rows from %d to %d", before, after)
	}

	if available, rpc := checkAccountUsername(t, s, 0, "validhandle"); rpc != nil || !available {
		t.Errorf("unauthenticated check = %t, %v; want available", available, rpc)
	}
}

func TestCheckPreAuthUsernameAvailabilityAndValidation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	owner, err := s.CreateUser(ctx, "+15551298105")
	if err != nil {
		t.Fatalf("owner: %v", err)
	}
	if _, err := api.UpdateUsernameForTest(s, owner.ID, "takenhandle"); err != nil {
		t.Fatalf("claim occupied username: %v", err)
	}

	h := fullChannelDispatcher(s)
	for _, tc := range []struct {
		username string
		want     bool
	}{
		{username: "ab", want: true},
		{username: "NewHandle", want: true},
		{username: "Name_123", want: true},
		{username: "TAKENHANDLE", want: false},
	} {
		available, rpc := checkAccountUsernameWithAddr(t, h, 0, netip.Addr{}, tc.username)
		if rpc != nil || available != tc.want {
			t.Errorf("pre-auth check %q = %t, %v; want %t", tc.username, available, rpc, tc.want)
		}
	}

	for _, username := range []string{"bad-name", "ADMIN", "ApIwS", "a", "1start", strings.Repeat("a", 33)} {
		if _, rpc := checkAccountUsernameWithAddr(t, h, 0, netip.Addr{}, username); rpc == nil || rpc.ErrorMessage != "USERNAME_INVALID" {
			t.Errorf("pre-auth check invalid %q error = %v, want USERNAME_INVALID", username, rpc)
		}
	}
}

func TestCheckPreAuthUsernameIsRateLimitedPerNetwork(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	limits := checkUsernameLimitConfig(t, store.RateLimitConfig{Limit: 1, Window: time.Hour})
	h := fullChannelDispatcherWithRateLimits(s, limits)
	addr := netip.MustParseAddr("198.51.100.70")

	if available, rpc := checkAccountUsernameWithAddr(t, h, 0, addr, "firsthandle"); rpc != nil || !available {
		t.Fatalf("first pre-auth check = %t, %v; want available", available, rpc)
	}
	if _, rpc := checkAccountUsernameWithAddr(t, h, 0, addr, "secondhandle"); rpc == nil || rpc.ErrorCode != 420 || !strings.HasPrefix(rpc.ErrorMessage, "FLOOD_WAIT_") {
		t.Fatalf("second pre-auth check error = %v, want FLOOD_WAIT", rpc)
	}
	if available, rpc := checkAccountUsernameWithAddr(t, h, 0, netip.MustParseAddr("198.51.100.71"), "thirdhandle"); rpc != nil || !available {
		t.Errorf("different network check = %t, %v; want available", available, rpc)
	}
	user, err := s.CreateUser(context.Background(), "+15551298106")
	if err != nil {
		t.Fatalf("authenticated user: %v", err)
	}
	if available, rpc := checkAccountUsernameWithAddr(t, h, user.ID, addr, "authenticatedhandle"); rpc != nil || !available {
		t.Errorf("authenticated check after IP quota exhaustion = %t, %v; want available", available, rpc)
	}
}

func TestCheckAccountUsernameUsesSharedLookupQuota(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)

	for i, tc := range []struct {
		name               string
		exhaustWithAccount bool
	}{
		{name: "account checks exhaust channel check", exhaustWithAccount: true},
		{name: "channel checks exhaust account check", exhaustWithAccount: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caller, err := s.CreateUser(ctx, fmt.Sprintf("+15551298%03d", i))
			if err != nil {
				t.Fatalf("caller: %v", err)
			}
			for i := range store.UsernameLookupBurstLimit {
				username := fmt.Sprintf("sharedbudget%02d", i)
				var available bool
				var rpc *mt.RPCError
				if tc.exhaustWithAccount {
					available, rpc = checkAccountUsername(t, s, caller.ID, username)
				} else {
					available, rpc = checkChannelUsername(t, s, caller.ID, false, &tg.ChannelsCheckUsernameRequest{
						Channel: &tg.InputChannelEmpty{}, Username: username,
					})
				}
				if rpc != nil || !available {
					t.Fatalf("quota check %d = %t, %v; want available", i, available, rpc)
				}
			}

			overQuota := "sharedbudgetover"
			var rpc *mt.RPCError
			if tc.exhaustWithAccount {
				_, rpc = checkChannelUsername(t, s, caller.ID, false, &tg.ChannelsCheckUsernameRequest{
					Channel: &tg.InputChannelEmpty{}, Username: overQuota,
				})
			} else {
				_, rpc = checkAccountUsername(t, s, caller.ID, overQuota)
			}
			if rpc == nil || rpc.ErrorMessage != "FLOOD_WAIT_86400" {
				t.Fatalf("cross-method check error = %v, want FLOOD_WAIT_86400", rpc)
			}
		})
	}
}

func TestCheckAccountUsernameMatchesImmutableUsernameAccount(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	caller := mustUsernameAccount(t, s, "operator")
	other, err := s.CreateUser(ctx, "+15551298104")
	if err != nil {
		t.Fatalf("other user: %v", err)
	}
	if _, err := api.UpdateUsernameForTest(s, other.ID, "occupiedhandle"); err != nil {
		t.Fatalf("claim occupied username: %v", err)
	}

	for _, tc := range []struct {
		username string
		want     string
	}{
		{username: "operator", want: "USERNAME_NOT_MODIFIED"},
		{username: "freehandle", want: "USERNAME_IMMUTABLE"},
		{username: "occupiedhandle", want: "USERNAME_IMMUTABLE"},
		{username: "Operator", want: "USERNAME_IMMUTABLE"},
		{username: "admin", want: "USERNAME_INVALID"},
	} {
		if _, rpc := checkAccountUsername(t, s, caller.ID, tc.username); rpc == nil || rpc.ErrorMessage != tc.want {
			t.Errorf("check %q error = %v, want %s", tc.username, rpc, tc.want)
		}
	}
	if _, rpc := checkAccountUsername(t, s, 0, "bad-name"); rpc == nil || rpc.ErrorMessage != "USERNAME_INVALID" {
		t.Errorf("unauthenticated invalid check error = %v, want USERNAME_INVALID", rpc)
	}

	pool := usernameLookupPool(t, dsn)
	if count := usernameLookupTotalCount(t, pool, caller.ID); count != 0 {
		t.Errorf("username-login checks charged %d lookups, want 0", count)
	}
}
