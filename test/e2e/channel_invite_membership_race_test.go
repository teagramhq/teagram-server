package e2e_test

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/rsakey"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestChannelInviteMembershipPushAfterRemovalDoesNotRevealChannel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
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
	listenerA := mustListen(t, ctx, "127.0.0.1:0")
	stopA := bootServerWithDelivery(t, ctx, key, dcID, st, dsn, codes.Logger(), listenerA)
	t.Cleanup(stopA)
	listenerB := mustListen(t, ctx, "127.0.0.1:0")
	gate, stopB := bootServerWithChannelMembershipDeliveryGate(t, ctx, key, dcID, st, dsn, codes.Logger(), listenerB)
	t.Cleanup(stopB)

	const phoneA = "+15551299991"
	const phoneB = "+15551299992"
	const phoneC = "+15551299993"
	seedUsernameUsers(t, ctx, st, phoneA, phoneB, phoneC)

	type runningClient struct {
		cmds chan command
		ids  chan int64
		err  chan error
		coll *updateCollector
	}
	var clients []runningClient
	startClient := func(port int, phone string) runningClient {
		client := runningClient{
			cmds: make(chan command),
			ids:  make(chan int64, 1),
			err:  make(chan error, 1),
			coll: newUpdateCollector(),
		}
		clients = append(clients, client)
		go func() {
			client.err <- runInteractive(ctx, createClient(port, key, dcID, client.coll, nil), flowFor(phone, codes), client.ids, client.cmds)
		}()
		return client
	}
	t.Cleanup(func() {
		for _, client := range clients {
			close(client.cmds)
		}
		for _, client := range clients {
			if err := <-client.err; err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("client run: %v", err)
			}
		}
	})

	login := func(client runningClient, label string) int64 {
		select {
		case id := <-client.ids:
			return id
		case <-ctx.Done():
			t.Fatalf("%s login timeout: %v", label, ctx.Err())
			return 0
		}
	}
	a := startClient(tcpPort(t, listenerA), phoneA)
	aID := login(a, "A")
	b := startClient(tcpPort(t, listenerB), phoneB)
	bID := login(b, "B")
	c := startClient(tcpPort(t, listenerA), phoneC)
	cID := login(c, "C")

	invite := func(t *testing.T, channelID int64) {
		t.Helper()
		execChannel(t, ctx, a.cmds, func(ctx context.Context, client *tg.Client) error {
			result, err := client.ChannelsInviteToChannel(ctx, &tg.ChannelsInviteToChannelRequest{
				Channel: inputChannel(aID, channelID),
				Users:   []tg.InputUserClass{inputUser(aID, bID)},
			})
			if err != nil {
				return err
			}
			if result == nil || result.Updates == nil {
				return errors.New("inviteToChannel response omitted updates")
			}
			return nil
		})
	}

	for _, tc := range []struct {
		name   string
		remove func(t *testing.T, channelID int64)
	}{
		{
			name: "leave",
			remove: func(t *testing.T, channelID int64) {
				t.Helper()
				execChannel(t, ctx, b.cmds, func(ctx context.Context, client *tg.Client) error {
					_, err := client.ChannelsLeaveChannel(ctx, inputChannel(bID, channelID))
					return err
				})
			},
		},
		{
			name: "kick",
			remove: func(t *testing.T, channelID int64) {
				t.Helper()
				execChannel(t, ctx, a.cmds, func(ctx context.Context, client *tg.Client) error {
					_, err := client.ChannelsEditBanned(ctx, &tg.ChannelsEditBannedRequest{
						Channel:      inputChannel(aID, channelID),
						Participant:  peerUser(aID, bID),
						BannedRights: tg.ChatBannedRights{ViewMessages: true, UntilDate: 0},
					})
					return err
				})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			channelID := createBroadcastChannel(t, ctx, a.cmds, "Invite removal "+tc.name)
			attempt := gate.arm(t, channelID)
			t.Cleanup(attempt.release)
			invite(t, channelID)
			select {
			case <-attempt.entered:
			case <-ctx.Done():
				t.Fatalf("membership delivery for channel %d did not reach the gate: %v", channelID, ctx.Err())
			}

			// Hold the membership notification before its delivery-time membership
			// lookup, then remove Bob through the RPC before allowing that lookup.
			tc.remove(t, channelID)
			execChannel(t, ctx, b.cmds, func(ctx context.Context, client *tg.Client) error {
				return assertChannelAbsentFromDialogs(ctx, client, channelID)
			})

			attempt.release()
			assertNoLateChannelMembershipUpdate(t, ctx, b.coll, channelID)
		})
	}

	t.Run("non-admin invite sends no target update", func(t *testing.T) {
		channelID := createBroadcastChannel(t, ctx, a.cmds, "Rejected member invite")
		invite(t, channelID)
		assertChannelRPCError(t, ctx, b.cmds, "PEER_ID_INVALID", func(ctx context.Context, client *tg.Client) error {
			_, err := client.ChannelsInviteToChannel(ctx, &tg.ChannelsInviteToChannelRequest{
				Channel: inputChannel(bID, channelID),
				Users:   []tg.InputUserClass{inputUser(bID, cID)},
			})
			return err
		})
		assertNoChannelUpdate(t, ctx, c.coll, channelID)
		execChannel(t, ctx, c.cmds, func(ctx context.Context, client *tg.Client) error {
			return assertChannelAbsentFromDialogs(ctx, client, channelID)
		})
	})
}

type channelMembershipDeliveryAttempt struct {
	channelID int64
	entered   chan struct{}
	released  chan struct{}
	once      sync.Once
}

func (a *channelMembershipDeliveryAttempt) release() {
	a.once.Do(func() { close(a.released) })
}

type channelMembershipDeliveryGate struct {
	mu     sync.Mutex
	active *channelMembershipDeliveryAttempt
}

func (g *channelMembershipDeliveryGate) arm(t *testing.T, channelID int64) *channelMembershipDeliveryAttempt {
	t.Helper()
	attempt := &channelMembershipDeliveryAttempt{
		channelID: channelID,
		entered:   make(chan struct{}),
		released:  make(chan struct{}),
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.active != nil {
		t.Fatal("channel membership delivery gate is already armed")
	}
	g.active = attempt
	return attempt
}

func (g *channelMembershipDeliveryGate) block(ctx context.Context, _ int64) {
	channelID, membershipUpdate := store.ChannelMembershipUpdateFromContext(ctx)
	if membershipUpdate {
		g.mu.Lock()
		attempt := g.active
		if attempt == nil || attempt.channelID != channelID {
			attempt = nil
		} else {
			g.active = nil
		}
		g.mu.Unlock()
		if attempt != nil {
			close(attempt.entered)
			select {
			case <-attempt.released:
			case <-ctx.Done():
				return
			}
		}
	}
}

func bootServerWithChannelMembershipDeliveryGate(
	t *testing.T,
	ctx context.Context,
	key *rsa.PrivateKey,
	dcID int,
	st *store.Store,
	dsn string,
	log *slog.Logger,
	ln net.Listener,
) (*channelMembershipDeliveryGate, func()) {
	t.Helper()
	gate := &channelMembershipDeliveryGate{}
	_, stop := bootServerWithLimitsAndRegistrationModeAndBlobsAndStatusWithDeliveryHook(
		t, ctx, key, dcID, st, dsn, log, ln, config.RateLimitsConfig{}, config.RegistrationClosed,
		testBlobs(t), false, gate.block, "",
	)
	return gate, stop
}

func assertChannelAbsentFromDialogs(ctx context.Context, client *tg.Client, channelID int64) error {
	result, err := client.MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{
		OffsetPeer: &tg.InputPeerEmpty{},
		Limit:      100,
	})
	if err != nil {
		return fmt.Errorf("getDialogs: %w", err)
	}
	var dialogs []tg.DialogClass
	var chats []tg.ChatClass
	switch response := result.(type) {
	case *tg.MessagesDialogs:
		dialogs, chats = response.Dialogs, response.Chats
	case *tg.MessagesDialogsSlice:
		dialogs, chats = response.Dialogs, response.Chats
	default:
		return fmt.Errorf("getDialogs response = %T, want dialogs or dialogsSlice", result)
	}
	for _, entry := range dialogs {
		dialog, ok := entry.(*tg.Dialog)
		if !ok {
			continue
		}
		peer, ok := dialog.Peer.(*tg.PeerChannel)
		if ok && peer.ChannelID == channelID {
			return fmt.Errorf("getDialogs retained channel %d", channelID)
		}
	}
	for _, entry := range chats {
		switch chat := entry.(type) {
		case *tg.Channel:
			if chat.ID == channelID {
				return fmt.Errorf("getDialogs chats retained channel %d", channelID)
			}
		case *tg.ChannelForbidden:
			if chat.ID == channelID {
				return fmt.Errorf("getDialogs chats retained forbidden channel %d", channelID)
			}
		}
	}
	return nil
}

func assertNoLateChannelMembershipUpdate(t *testing.T, ctx context.Context, collector *updateCollector, channelID int64) {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case update := <-collector.channelUpdate:
			if update.Update.ChannelID != channelID {
				continue
			}
			forbidden := false
			for _, chat := range update.Chats {
				switch channel := chat.(type) {
				case *tg.Channel:
					if channel.ID == channelID {
						t.Fatalf("late membership update disclosed channel %d to its former member", channelID)
					}
				case *tg.ChannelForbidden:
					if channel.ID == channelID {
						forbidden = true
					}
				}
			}
			if !forbidden {
				t.Fatalf("late membership update for channel %d was neither omitted nor forbidden", channelID)
			}
		case <-timer.C:
			return
		case <-ctx.Done():
			t.Fatalf("late membership push check timed out: %v", ctx.Err())
		}
	}
}

func assertNoChannelUpdate(t *testing.T, ctx context.Context, collector *updateCollector, channelID int64) {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case update := <-collector.channelUpdate:
			if update.Update.ChannelID == channelID {
				t.Fatalf("target received channel update %d after rejected invite", channelID)
			}
		case <-timer.C:
			return
		case <-ctx.Done():
			t.Fatalf("rejected invite push check timed out: %v", ctx.Err())
		}
	}
}
