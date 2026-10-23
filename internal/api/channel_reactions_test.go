package api_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/gotd/td/mt"
	"github.com/gotd/td/tg"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/store"
)

type channelReactionFixture struct {
	s       *store.Store
	dsn     string
	creator store.User
	a       store.User
	b       store.User
	channel store.Channel
	post    store.ChannelMessage
}

func newChannelReactionFixture(t *testing.T, megagroup bool) channelReactionFixture {
	t.Helper()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	creator, err := s.CreateUser(ctx, "+15551923001")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	a, err := s.CreateUser(ctx, "+15551923002")
	if err != nil {
		t.Fatalf("create A: %v", err)
	}
	b, err := s.CreateUser(ctx, "+15551923003")
	if err != nil {
		t.Fatalf("create B: %v", err)
	}
	channel, err := s.CreateChannel(ctx, creator.ID, "Channel reactions", "", megagroup)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	joinChannelByInvite(t, s, channel, a.ID)
	joinChannelByInvite(t, s, channel, b.ID)
	post, _, _, err := s.PostChannelMessage(ctx, channel.ID, creator.ID, "react to this", 992301, nil, 0)
	if err != nil {
		t.Fatalf("post channel message: %v", err)
	}
	return channelReactionFixture{s: s, dsn: dsn, creator: creator, a: a, b: b, channel: channel, post: post}
}

func setChannelReactions(t *testing.T, s *store.Store, userID, channelID int64, reactions tg.ChatReactionsClass) (*tg.Updates, *mt.RPCError) {
	t.Helper()
	return callReactionUpdates(t, s, userID, "messages.setChatAvailableReactions", &tg.MessagesSetChatAvailableReactionsRequest{
		Peer:               api.InputPeerChannel(userID, channelID),
		AvailableReactions: reactions,
	})
}

func channelReactionRowCount(t *testing.T, ctx context.Context, dsn string, channelID, localID int64) int {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect for channel reaction count: %v", err)
	}
	defer func() {
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close channel reaction count connection: %v", err)
		}
	}()
	var count int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM channel_message_reactions WHERE channel_id=$1 AND local_id=$2`, channelID, localID).Scan(&count); err != nil {
		t.Fatalf("count channel reaction rows: %v", err)
	}
	return count
}

func channelStateSnapshot(t *testing.T, ctx context.Context, dsn string, channelID int64) (int64, int) {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect for channel state: %v", err)
	}
	defer func() {
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close channel state connection: %v", err)
		}
	}()
	var pts int64
	var events int
	if err := conn.QueryRow(ctx, `SELECT pts FROM channel_state WHERE channel_id=$1`, channelID).Scan(&pts); err != nil {
		t.Fatalf("read channel pts: %v", err)
	}
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM channel_events WHERE channel_id=$1`, channelID).Scan(&events); err != nil {
		t.Fatalf("count channel events: %v", err)
	}
	return pts, events
}

func waitForChannelReactionLockWait(t *testing.T, ctx context.Context, dsn string) {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect while waiting for reaction lock: %v", err)
	}
	defer func() {
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close reaction lock wait connection: %v", err)
		}
	}()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiters int
		if err := conn.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND wait_event_type='Lock'`).Scan(&waiters); err != nil {
			t.Fatalf("count database lock waiters: %v", err)
		}
		if waiters > 0 {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("reaction did not wait on the locked channel rows")
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("waiting for reaction lock: %v", ctx.Err())
		}
	}
}

func assertChannelReactionCounts(t *testing.T, got tg.MessageReactions, canSeeList bool, want map[string]struct {
	count  int
	chosen bool
}) {
	t.Helper()
	if len(got.Results) != len(want) {
		t.Fatalf("reaction results = %d, want %d: %+v", len(got.Results), len(want), got.Results)
	}
	seen := make(map[string]bool, len(got.Results))
	for _, result := range got.Results {
		emoji, ok := result.Reaction.(*tg.ReactionEmoji)
		if !ok {
			t.Fatalf("reaction type = %T, want *tg.ReactionEmoji", result.Reaction)
		}
		expected, ok := want[emoji.Emoticon]
		if !ok {
			t.Fatalf("unexpected reaction %q", emoji.Emoticon)
		}
		if result.Count != expected.count {
			t.Errorf("%s count = %d, want %d", emoji.Emoticon, result.Count, expected.count)
		}
		chosenOrder, hasChosenOrder := result.GetChosenOrder()
		if hasChosenOrder != expected.chosen || (expected.chosen && chosenOrder == 0) {
			t.Errorf("%s chosen order = %d present=%v, want chosen=%v", emoji.Emoticon, chosenOrder, hasChosenOrder, expected.chosen)
		}
		seen[emoji.Emoticon] = true
	}
	_, hasRecent := got.GetRecentReactions()
	_, hasTopReactors := got.GetTopReactors()
	if hasRecent || got.GetCanSeeList() != canSeeList || hasTopReactors {
		t.Errorf("reaction response can_see_list=%v recent=%v top_reactors=%v, want can_see_list=%v", got.GetCanSeeList(), hasRecent, hasTopReactors, canSeeList)
	}
	for emoticon := range want {
		if !seen[emoticon] {
			t.Errorf("missing reaction %q", emoticon)
		}
	}
}

func oneChannelReactionUpdate(t *testing.T, updates *tg.Updates, method string) *tg.UpdateMessageReactions {
	t.Helper()
	if updates == nil || len(updates.Updates) != 1 {
		t.Fatalf("%s updates = %#v, want one update", method, updates)
	}
	update, ok := updates.Updates[0].(*tg.UpdateMessageReactions)
	if !ok {
		t.Fatalf("%s update = %T, want *tg.UpdateMessageReactions", method, updates.Updates[0])
	}
	return update
}

func TestChannelReactionsRoundTrip(t *testing.T) {
	t.Parallel()
	for _, megagroup := range []bool{false, true} {
		name := "broadcast"
		if megagroup {
			name = "megagroup"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newChannelReactionFixture(t, megagroup)
			peerA := api.InputPeerChannel(f.a.ID, f.channel.ID)
			peerB := api.InputPeerChannel(f.b.ID, f.channel.ID)
			beforePts, beforeEvents := channelStateSnapshot(t, context.Background(), f.dsn, f.channel.ID)

			updates, rpc := sendReaction(t, f.s, f.a.ID, peerA, f.post.LocalID, &tg.ReactionEmoji{Emoticon: "🔥"})
			updates = requireReactionUpdates(t, updates, rpc, "messages.sendReaction")
			if len(updates.Updates) != 1 {
				t.Fatalf("sendReaction updates = %d, want one update", len(updates.Updates))
			}
			update, ok := updates.Updates[0].(*tg.UpdateMessageReactions)
			if !ok {
				t.Fatalf("sendReaction update = %T, want *tg.UpdateMessageReactions", updates.Updates[0])
			}
			if update.MsgID != int(f.post.LocalID) {
				t.Fatalf("sendReaction message id = %d, want %d", update.MsgID, f.post.LocalID)
			}
			if peer, ok := update.Peer.(*tg.PeerChannel); !ok || peer.ChannelID != f.channel.ID {
				t.Fatalf("sendReaction peer = %#v, want channel %d", update.Peer, f.channel.ID)
			}
			assertChannelReactionCounts(t, update.Reactions, megagroup, map[string]struct {
				count  int
				chosen bool
			}{"🔥": {count: 1, chosen: true}})
			if got := channelReactionRowCount(t, context.Background(), f.dsn, f.channel.ID, f.post.LocalID); got != 1 {
				t.Fatalf("one member reaction rows = %d, want 1", got)
			}

			readAsB := getReactions(t, f.s, f.b.ID, peerB, int(f.post.LocalID))
			if len(readAsB.Updates) != 1 {
				t.Fatalf("B getMessagesReactions updates = %d, want 1", len(readAsB.Updates))
			}
			bUpdate := oneChannelReactionUpdate(t, readAsB, "B getMessagesReactions")
			assertChannelReactionCounts(t, bUpdate.Reactions, megagroup, map[string]struct {
				count  int
				chosen bool
			}{"🔥": {count: 1}})

			for _, tc := range []struct {
				name string
				read func() (tg.MessageReactions, error)
			}{
				{
					name: "getHistory",
					read: func() (tg.MessageReactions, error) {
						result, err := api.GetHistoryForTest(f.s, f.b.ID, &tg.MessagesGetHistoryRequest{Peer: peerB, Limit: 10})
						if err != nil {
							return tg.MessageReactions{}, err
						}
						page, ok := result.(*tg.MessagesChannelMessages)
						if !ok {
							return tg.MessageReactions{}, fmt.Errorf("response = %T, want *tg.MessagesChannelMessages", result)
						}
						return channelMessageReactions(t, page.Messages, int(f.post.LocalID))
					},
				},
				{
					name: "channels.getMessages",
					read: func() (tg.MessageReactions, error) {
						result, err := api.GetChannelMessagesForTest(f.s, f.b.ID, &tg.ChannelsGetMessagesRequest{
							Channel: api.InputChannel(f.b.ID, f.channel.ID),
							ID:      []tg.InputMessageClass{&tg.InputMessageID{ID: int(f.post.LocalID)}},
						})
						if err != nil {
							return tg.MessageReactions{}, err
						}
						page, ok := result.(*tg.MessagesChannelMessages)
						if !ok {
							return tg.MessageReactions{}, fmt.Errorf("response = %T, want *tg.MessagesChannelMessages", result)
						}
						return channelMessageReactions(t, page.Messages, int(f.post.LocalID))
					},
				},
			} {
				t.Run(tc.name, func(t *testing.T) {
					got, err := tc.read()
					if err != nil {
						t.Fatalf("%s: %v", tc.name, err)
					}
					assertChannelReactionCounts(t, got, megagroup, map[string]struct {
						count  int
						chosen bool
					}{"🔥": {count: 1}})
				})
			}

			updates, rpc = sendReaction(t, f.s, f.a.ID, peerA, f.post.LocalID, &tg.ReactionEmoji{Emoticon: "❤"})
			updates = requireReactionUpdates(t, updates, rpc, "messages.sendReaction replace")
			update = oneChannelReactionUpdate(t, updates, "messages.sendReaction replace")
			assertChannelReactionCounts(t, update.Reactions, megagroup, map[string]struct {
				count  int
				chosen bool
			}{"❤": {count: 1, chosen: true}})

			updates, rpc = sendReaction(t, f.s, f.b.ID, peerB, f.post.LocalID, &tg.ReactionEmoji{Emoticon: "🔥"})
			updates = requireReactionUpdates(t, updates, rpc, "messages.sendReaction second member")
			update = oneChannelReactionUpdate(t, updates, "messages.sendReaction second member")
			assertChannelReactionCounts(t, update.Reactions, megagroup, map[string]struct {
				count  int
				chosen bool
			}{"❤": {count: 1}, "🔥": {count: 1, chosen: true}})
			for _, tc := range []struct {
				name string
				read func() (tg.MessageReactions, error)
			}{
				{
					name: "getHistory with own choice",
					read: func() (tg.MessageReactions, error) {
						result, err := api.GetHistoryForTest(f.s, f.b.ID, &tg.MessagesGetHistoryRequest{Peer: peerB, Limit: 10})
						if err != nil {
							return tg.MessageReactions{}, err
						}
						page, ok := result.(*tg.MessagesChannelMessages)
						if !ok {
							return tg.MessageReactions{}, fmt.Errorf("response = %T, want *tg.MessagesChannelMessages", result)
						}
						return channelMessageReactions(t, page.Messages, int(f.post.LocalID))
					},
				},
				{
					name: "channels.getMessages with own choice",
					read: func() (tg.MessageReactions, error) {
						result, err := api.GetChannelMessagesForTest(f.s, f.b.ID, &tg.ChannelsGetMessagesRequest{
							Channel: api.InputChannel(f.b.ID, f.channel.ID),
							ID:      []tg.InputMessageClass{&tg.InputMessageID{ID: int(f.post.LocalID)}},
						})
						if err != nil {
							return tg.MessageReactions{}, err
						}
						page, ok := result.(*tg.MessagesChannelMessages)
						if !ok {
							return tg.MessageReactions{}, fmt.Errorf("response = %T, want *tg.MessagesChannelMessages", result)
						}
						return channelMessageReactions(t, page.Messages, int(f.post.LocalID))
					},
				},
			} {
				got, err := tc.read()
				if err != nil {
					t.Fatalf("%s: %v", tc.name, err)
				}
				assertChannelReactionCounts(t, got, megagroup, map[string]struct {
					count  int
					chosen bool
				}{"❤": {count: 1}, "🔥": {count: 1, chosen: true}})
			}

			updates, rpc = sendReaction(t, f.s, f.a.ID, peerA, f.post.LocalID)
			updates = requireReactionUpdates(t, updates, rpc, "messages.sendReaction clear")
			update = oneChannelReactionUpdate(t, updates, "messages.sendReaction clear")
			assertChannelReactionCounts(t, update.Reactions, megagroup, map[string]struct {
				count  int
				chosen bool
			}{"🔥": {count: 1}})
			afterPts, afterEvents := channelStateSnapshot(t, context.Background(), f.dsn, f.channel.ID)
			if afterPts != beforePts || afterEvents != beforeEvents {
				t.Fatalf("reaction advanced channel log from pts/events %d/%d to %d/%d", beforePts, beforeEvents, afterPts, afterEvents)
			}
		})
	}
}

func TestChannelFullDefaultsAvailableReactionsToAll(t *testing.T) {
	t.Parallel()
	for _, megagroup := range []bool{false, true} {
		name := "broadcast"
		if megagroup {
			name = "megagroup"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newChannelReactionFixture(t, megagroup)
			result, err := api.GetFullChannelForTest(f.s, f.a.ID, &tg.ChannelsGetFullChannelRequest{
				Channel: api.InputChannel(f.a.ID, f.channel.ID),
			})
			if err != nil {
				t.Fatalf("getFullChannel: %v", err)
			}
			full, ok := result.(*tg.MessagesChatFull)
			if !ok {
				t.Fatalf("response = %T, want *tg.MessagesChatFull", result)
			}
			channelFull, ok := full.FullChat.(*tg.ChannelFull)
			if !ok {
				t.Fatalf("full chat = %T, want *tg.ChannelFull", full.FullChat)
			}
			available, ok := channelFull.GetAvailableReactions()
			if !ok {
				t.Fatal("channelFull omitted available_reactions")
			}
			if _, ok := available.(*tg.ChatReactionsAll); !ok {
				t.Fatalf("available reactions = %T, want *tg.ChatReactionsAll", available)
			}
		})
	}
}

func TestSetChannelAvailableReactionsUsesCreatorOrAdminChangeInfo(t *testing.T) {
	t.Parallel()
	f := newChannelReactionFixture(t, true)
	ctx := context.Background()
	all := &tg.ChatReactionsAll{}
	if _, rpc := setChannelReactions(t, f.s, f.a.ID, f.channel.ID, all); rpc == nil || rpc.ErrorMessage != "CHAT_ADMIN_REQUIRED" {
		t.Fatalf("ordinary megagroup member setting = %v, want CHAT_ADMIN_REQUIRED", rpc)
	}
	if err := f.s.SetChannelRoleWithRights(ctx, f.channel.ID, f.creator.ID, f.b.ID, 1, false); err != nil {
		t.Fatalf("promote admin without change-info: %v", err)
	}
	if _, rpc := setChannelReactions(t, f.s, f.b.ID, f.channel.ID, all); rpc == nil || rpc.ErrorMessage != "CHAT_ADMIN_REQUIRED" {
		t.Fatalf("admin without change-info setting = %v, want CHAT_ADMIN_REQUIRED", rpc)
	}
	if err := f.s.SetChannelRoleWithRights(ctx, f.channel.ID, f.creator.ID, f.b.ID, 1, true); err != nil {
		t.Fatalf("grant admin change-info: %v", err)
	}
	some := &tg.ChatReactionsSome{Reactions: []tg.ReactionClass{&tg.ReactionEmoji{Emoticon: "❤"}}}
	updates, rpc := setChannelReactions(t, f.s, f.b.ID, f.channel.ID, some)
	requireReactionUpdates(t, updates, rpc, "messages.setChatAvailableReactions")

	result, err := api.GetFullChannelForTest(f.s, f.a.ID, &tg.ChannelsGetFullChannelRequest{
		Channel: api.InputChannel(f.a.ID, f.channel.ID),
	})
	if err != nil {
		t.Fatalf("getFullChannel after setting: %v", err)
	}
	fullResponse, ok := result.(*tg.MessagesChatFull)
	if !ok {
		t.Fatalf("response = %T, want *tg.MessagesChatFull", result)
	}
	full, ok := fullResponse.FullChat.(*tg.ChannelFull)
	if !ok {
		t.Fatalf("full chat = %T, want *tg.ChannelFull", fullResponse.FullChat)
	}
	available, ok := full.GetAvailableReactions()
	if !ok {
		t.Fatal("channelFull omitted available_reactions after setting")
	}
	chosen, ok := available.(*tg.ChatReactionsSome)
	if !ok || len(chosen.Reactions) != 1 {
		t.Fatalf("available reactions = %#v, want chatReactionsSome with heart", available)
	}
	emoji, ok := chosen.Reactions[0].(*tg.ReactionEmoji)
	if !ok || emoji.Emoticon != "❤" {
		t.Fatalf("stored available reaction = %#v, want heart", chosen.Reactions[0])
	}
	outsider, err := f.s.CreateUser(ctx, "+15551923004")
	if err != nil {
		t.Fatalf("create outsider: %v", err)
	}
	if _, rpc := sendReaction(t, f.s, outsider.ID, api.InputPeerChannel(outsider.ID, f.channel.ID), f.post.LocalID, &tg.ReactionEmoji{Emoticon: "👍"}); rpc == nil || rpc.ErrorMessage != "PEER_ID_INVALID" {
		t.Fatalf("outsider probes excluded reaction setting: error = %v, want PEER_ID_INVALID", rpc)
	}
	if _, rpc := sendReaction(t, f.s, f.a.ID, api.InputPeerChannel(f.a.ID, f.channel.ID), f.post.LocalID, &tg.ReactionEmoji{Emoticon: "👍"}); rpc == nil || rpc.ErrorMessage != "REACTION_INVALID" {
		t.Fatalf("member sends excluded reaction: error = %v, want REACTION_INVALID", rpc)
	}
	if got := channelReactionRowCount(t, ctx, f.dsn, f.channel.ID, f.post.LocalID); got != 0 {
		t.Fatalf("rejected channel reactions wrote %d rows, want none", got)
	}
	updates, rpc = setChannelReactions(t, f.s, f.creator.ID, f.channel.ID, some)
	requireReactionUpdates(t, updates, rpc, "creator set channel available reactions")

	if _, rpc := setChannelReactions(t, f.s, f.a.ID, f.channel.ID, all); rpc == nil || rpc.ErrorMessage != "CHAT_ADMIN_REQUIRED" {
		t.Fatalf("ordinary member with default change-info setting = %v, want CHAT_ADMIN_REQUIRED", rpc)
	}
	invalid := &tg.ChatReactionsSome{Reactions: []tg.ReactionClass{&tg.ReactionCustomEmoji{DocumentID: 1}}}
	if _, rpc := setChannelReactions(t, f.s, f.creator.ID, f.channel.ID, invalid); rpc == nil || rpc.ErrorMessage != "REACTION_INVALID" {
		t.Fatalf("unsupported channel reaction setting = %v, want REACTION_INVALID", rpc)
	}
}

func TestChannelReactionClearUsesReadGate(t *testing.T) {
	t.Parallel()
	f := newChannelReactionFixture(t, false)
	ctx := context.Background()
	peerA := api.InputPeerChannel(f.a.ID, f.channel.ID)
	updates, rpc := sendReaction(t, f.s, f.a.ID, peerA, f.post.LocalID, &tg.ReactionEmoji{Emoticon: "🔥"})
	requireReactionUpdates(t, updates, rpc, "seed channel reaction")

	outsider, err := f.s.CreateUser(ctx, "+15551923005")
	if err != nil {
		t.Fatalf("create outsider: %v", err)
	}
	if _, rpc := sendReaction(t, f.s, outsider.ID, api.InputPeerChannel(outsider.ID, f.channel.ID), f.post.LocalID); rpc == nil || rpc.ErrorMessage != "PEER_ID_INVALID" {
		t.Fatalf("outsider clear error = %v, want PEER_ID_INVALID", rpc)
	}
	if got := channelReactionRowCount(t, ctx, f.dsn, f.channel.ID, f.post.LocalID); got != 1 {
		t.Fatalf("outsider clear changed reaction row count to %d, want 1", got)
	}

	channelExec(t, ctx, f.dsn,
		`UPDATE channel_participants SET available_min_id=$1 WHERE channel_id=$2 AND user_id=$3`,
		f.post.LocalID, f.channel.ID, f.a.ID)
	if _, rpc := sendReaction(t, f.s, f.a.ID, peerA, f.post.LocalID); rpc == nil || rpc.ErrorMessage != "MESSAGE_ID_INVALID" {
		t.Fatalf("below-floor clear error = %v, want MESSAGE_ID_INVALID", rpc)
	}
	if got := channelReactionRowCount(t, ctx, f.dsn, f.channel.ID, f.post.LocalID); got != 1 {
		t.Fatalf("below-floor clear changed reaction row count to %d, want 1", got)
	}
}

func TestChannelReactionWriteGateMatchesChannelReadErrors(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		want   string
		mutate func(*testing.T, channelReactionFixture) store.User
		msgID  func(channelReactionFixture) int64
	}{
		{
			name: "nonmember",
			want: "PEER_ID_INVALID",
			mutate: func(t *testing.T, f channelReactionFixture) store.User {
				t.Helper()
				outsider, err := f.s.CreateUser(context.Background(), "+15551923004")
				if err != nil {
					t.Fatalf("create outsider: %v", err)
				}
				return outsider
			},
			msgID: func(f channelReactionFixture) int64 { return f.post.LocalID },
		},
		{
			name: "banned member",
			want: "PEER_ID_INVALID",
			mutate: func(t *testing.T, f channelReactionFixture) store.User {
				t.Helper()
				banChannelMember(t, context.Background(), f.dsn, f.channel.ID, f.a.ID, time.Now().Add(time.Hour))
				return f.a
			},
			msgID: func(f channelReactionFixture) int64 { return f.post.LocalID },
		},
		{
			name: "below history floor",
			want: "MESSAGE_ID_INVALID",
			mutate: func(t *testing.T, f channelReactionFixture) store.User {
				t.Helper()
				channelExec(t, context.Background(), f.dsn,
					`UPDATE channel_participants SET available_min_id=$1 WHERE channel_id=$2 AND user_id=$3`,
					f.post.LocalID, f.channel.ID, f.a.ID)
				return f.a
			},
			msgID: func(f channelReactionFixture) int64 { return f.post.LocalID },
		},
		{
			name: "deleted post",
			want: "MESSAGE_ID_INVALID",
			mutate: func(t *testing.T, f channelReactionFixture) store.User {
				t.Helper()
				if _, _, err := f.s.DeleteChannelMessages(context.Background(), f.channel.ID, f.creator.ID, []int64{f.post.LocalID}); err != nil {
					t.Fatalf("delete post: %v", err)
				}
				return f.a
			},
			msgID: func(f channelReactionFixture) int64 { return f.post.LocalID },
		},
		{
			name: "expired post",
			want: "MESSAGE_ID_INVALID",
			mutate: func(t *testing.T, f channelReactionFixture) store.User {
				t.Helper()
				channelExec(t, context.Background(), f.dsn,
					`UPDATE channel_messages SET expires_at=now()-interval '1 second' WHERE channel_id=$1 AND local_id=$2`,
					f.channel.ID, f.post.LocalID)
				return f.a
			},
			msgID: func(f channelReactionFixture) int64 { return f.post.LocalID },
		},
		{
			name: "missing post",
			want: "MESSAGE_ID_INVALID",
			mutate: func(t *testing.T, f channelReactionFixture) store.User {
				t.Helper()
				return f.a
			},
			msgID: func(f channelReactionFixture) int64 { return f.post.LocalID + 100 },
		},
		{
			name: "deleted channel",
			want: "PEER_ID_INVALID",
			mutate: func(t *testing.T, f channelReactionFixture) store.User {
				t.Helper()
				if _, err := f.s.DeleteChannel(context.Background(), f.channel.ID, f.creator.ID); err != nil {
					t.Fatalf("delete channel: %v", err)
				}
				return f.a
			},
			msgID: func(f channelReactionFixture) int64 { return f.post.LocalID },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newChannelReactionFixture(t, false)
			user := tc.mutate(t, f)
			_, rpc := sendReaction(t, f.s, user.ID, api.InputPeerChannel(user.ID, f.channel.ID), tc.msgID(f), &tg.ReactionEmoji{Emoticon: "❤"})
			if rpc == nil || rpc.ErrorMessage != tc.want {
				t.Fatalf("sendReaction error = %v, want %s", rpc, tc.want)
			}
		})
	}
}

func TestChannelReactionRowsAreRemovedWithTombstonedPosts(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"message delete", "TTL expiry", "channel delete"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			f := newChannelReactionFixture(t, false)
			updates, rpc := sendReaction(t, f.s, f.a.ID, api.InputPeerChannel(f.a.ID, f.channel.ID), f.post.LocalID, &tg.ReactionEmoji{Emoticon: "🔥"})
			requireReactionUpdates(t, updates, rpc, "messages.sendReaction")
			if got := channelReactionRowCount(t, context.Background(), f.dsn, f.channel.ID, f.post.LocalID); got != 1 {
				t.Fatalf("seed reaction rows = %d, want 1", got)
			}
			switch path {
			case "message delete":
				if _, _, err := f.s.DeleteChannelMessages(context.Background(), f.channel.ID, f.creator.ID, []int64{f.post.LocalID}); err != nil {
					t.Fatalf("delete post: %v", err)
				}
			case "TTL expiry":
				channelExec(t, context.Background(), f.dsn,
					`UPDATE channel_messages SET expires_at=now()-interval '1 second' WHERE channel_id=$1 AND local_id=$2`,
					f.channel.ID, f.post.LocalID)
				if count, _, err := f.s.SweepExpiredChannelPosts(context.Background(), 10); err != nil || count != 1 {
					t.Fatalf("sweep expired posts = %d, err=%v; want 1 post", count, err)
				}
			case "channel delete":
				if _, err := f.s.DeleteChannel(context.Background(), f.channel.ID, f.creator.ID); err != nil {
					t.Fatalf("delete channel: %v", err)
				}
				for range 4 {
					_, active, err := f.s.SweepDeletedChannelPosts(context.Background())
					if err != nil {
						t.Fatalf("sweep deleted channel posts: %v", err)
					}
					if !active {
						break
					}
				}
			}
			if got := channelReactionRowCount(t, context.Background(), f.dsn, f.channel.ID, f.post.LocalID); got != 0 {
				t.Fatalf("reaction rows after %s = %d, want 0", path, got)
			}
		})
	}
}

func TestChannelReactionSerializesWithBanAndTombstone(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"ban", "tombstone"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			f := newChannelReactionFixture(t, false)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			conn, err := pgx.Connect(ctx, f.dsn)
			if err != nil {
				t.Fatalf("connect for %s transaction: %v", path, err)
			}
			defer func() {
				if err := conn.Close(ctx); err != nil {
					t.Errorf("close %s transaction connection: %v", path, err)
				}
			}()
			tx, err := conn.Begin(ctx)
			if err != nil {
				t.Fatalf("begin %s transaction: %v", path, err)
			}
			defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // cleans up the test transaction
			switch path {
			case "ban":
				if _, err := tx.Exec(ctx, `SELECT id FROM channels WHERE id=$1 FOR NO KEY UPDATE`, f.channel.ID); err != nil {
					t.Fatalf("lock channel for ban: %v", err)
				}
				if _, err := tx.Exec(ctx, `UPDATE channel_participants SET banned_until=now()+interval '1 hour' WHERE channel_id=$1 AND user_id=$2`, f.channel.ID, f.a.ID); err != nil {
					t.Fatalf("ban member: %v", err)
				}
			case "tombstone":
				if _, err := tx.Exec(ctx, `SELECT user_id FROM channel_participants WHERE channel_id=$1 AND user_id=$2 FOR SHARE`, f.channel.ID, f.a.ID); err != nil {
					t.Fatalf("lock participant before tombstone: %v", err)
				}
				if _, err := tx.Exec(ctx, `UPDATE channel_messages SET deleted=true WHERE channel_id=$1 AND local_id=$2`, f.channel.ID, f.post.LocalID); err != nil {
					t.Fatalf("tombstone post: %v", err)
				}
			}

			type result struct {
				rpc *mt.RPCError
			}
			done := make(chan result, 1)
			go func() {
				_, rpc := sendReaction(t, f.s, f.a.ID, api.InputPeerChannel(f.a.ID, f.channel.ID), f.post.LocalID, &tg.ReactionEmoji{Emoticon: "🔥"})
				done <- result{rpc: rpc}
			}()
			select {
			case <-done:
				t.Fatalf("reaction returned before the %s transaction committed", path)
			case <-time.After(30 * time.Millisecond):
			}
			waitForChannelReactionLockWait(t, ctx, f.dsn)
			if err := tx.Commit(ctx); err != nil {
				t.Fatalf("commit %s transaction: %v", path, err)
			}
			select {
			case got := <-done:
				want := "PEER_ID_INVALID"
				if path == "tombstone" {
					want = "MESSAGE_ID_INVALID"
				}
				if got.rpc == nil || got.rpc.ErrorMessage != want {
					t.Fatalf("reaction after %s: error=%v, want %s", path, got.rpc, want)
				}
			case <-ctx.Done():
				t.Fatalf("reaction did not finish after %s committed: %v", path, ctx.Err())
			}
			if got := channelReactionRowCount(t, ctx, f.dsn, f.channel.ID, f.post.LocalID); got != 0 {
				t.Fatalf("reaction rows after %s = %d, want 0", path, got)
			}
		})
	}
}

func channelMessageReactions(t *testing.T, messages []tg.MessageClass, id int) (tg.MessageReactions, error) {
	t.Helper()
	for _, item := range messages {
		message, ok := item.(*tg.Message)
		if !ok || message.ID != id {
			continue
		}
		reactions, ok := message.GetReactions()
		if !ok {
			return tg.MessageReactions{}, fmt.Errorf("message %d omitted reactions", id)
		}
		return reactions, nil
	}
	return tg.MessageReactions{}, fmt.Errorf("messages omitted id %d", id)
}
