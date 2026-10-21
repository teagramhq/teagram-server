package e2e_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/store"
)

func TestChannelPreHistoryHidden(t *testing.T) {
	t.Parallel()
	testSmokeChannelPreHistoryHidden(t)
}

func testSmokeChannelPreHistoryHidden(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	const phoneCreator, phonePriorMember, phoneJoiner, phoneLaterJoiner = "+15552253001", "+15552253002", "+15552253003", "+15552253004"
	seedSmokeUsers(t, f, phoneCreator, phonePriorMember, phoneJoiner, phoneLaterJoiner)
	creator := newSmokeClient(t, f, "prehistory creator", phoneCreator)
	priorMember := newSmokeClient(t, f, "prehistory prior member", phonePriorMember)
	joiner := newSmokeClient(t, f, "prehistory joiner", phoneJoiner)
	laterJoiner := newSmokeClient(t, f, "prehistory later joiner", phoneLaterJoiner)
	exerciseSmokeChannelPreHistoryHidden(t, f, creator, priorMember, joiner, laterJoiner)
}

func exerciseSmokeChannelPreHistoryHidden(t *testing.T, f *smokeFixture, creator, priorMember, joiner, laterJoiner *smokeClient) {
	t.Helper()
	channelID := createMegagroup(t, f.ctx, creator.cmds, "Hidden history")
	inviteHash := exportChannelInvite(t, f.ctx, creator.id, creator.cmds, channelID)

	for i := 1; i <= 5; i++ {
		text := fmt.Sprintf("prehistory-post-%d", i)
		if err := creator.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
			_, err := client.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
				Peer:     peerChannel(creator.id, channelID),
				Message:  text,
				RandomID: int64(2253000 + i),
			})
			return err
		}); err != nil {
			t.Fatalf("send post %d: %v", i, err)
		}
	}
	if got := importChannelInvite(t, f.ctx, priorMember.cmds, inviteHash); got != channelID {
		t.Fatalf("prior member channel = %d, want %d", got, channelID)
	}
	if floor := channelHistoryFloor(t, f.ctx, f.store, channelID, priorMember.id); floor != 0 {
		t.Fatalf("prior member history floor before hiding = %d, want 0", floor)
	}

	assertToggle := func(client *smokeClient, enabled bool) {
		t.Helper()
		if err := client.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
			result, err := api.ChannelsTogglePreHistoryHidden(ctx, &tg.ChannelsTogglePreHistoryHiddenRequest{
				Channel: inputChannel(client.id, channelID),
				Enabled: enabled,
			})
			if err != nil {
				return err
			}
			updates, ok := result.(*tg.Updates)
			if !ok {
				return fmt.Errorf("togglePreHistoryHidden response = %T, want *tg.Updates", result)
			}
			for _, chat := range updates.Chats {
				channel, ok := chat.(*tg.Channel)
				if ok && channel.ID == channelID {
					return nil
				}
			}
			return fmt.Errorf("togglePreHistoryHidden omitted updated channel %d", channelID)
		}); err != nil {
			t.Fatalf("toggle prehistory hidden to %v: %v", enabled, err)
		}
		assertPreHistoryFlag(t, f.ctx, client, channelID, enabled)
	}

	assertToggle(creator, true)
	if floor := channelHistoryFloor(t, f.ctx, f.store, channelID, priorMember.id); floor != 0 {
		t.Fatalf("prior member history floor after hiding = %d, want 0", floor)
	}
	if got := importChannelInvite(t, f.ctx, joiner.cmds, inviteHash); got != channelID {
		t.Fatalf("joiner channel = %d, want %d", got, channelID)
	}
	assertChannelRPCError(t, f.ctx, joiner.cmds, "CHAT_ADMIN_REQUIRED", func(ctx context.Context, client *tg.Client) error {
		_, err := client.ChannelsTogglePreHistoryHidden(ctx, &tg.ChannelsTogglePreHistoryHiddenRequest{
			Channel: inputChannel(joiner.id, channelID),
			Enabled: false,
		})
		return err
	})

	if err := creator.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		_, err := client.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: peerChannel(creator.id, channelID), Message: "prehistory-post-6", RandomID: 2253006,
		})
		return err
	}); err != nil {
		t.Fatalf("send post 6: %v", err)
	}
	assertPreHistoryMessages(t, f.ctx, joiner, channelID, []string{"prehistory-post-6"})
	assertPreHistoryMessages(t, f.ctx, priorMember, channelID, []string{
		"prehistory-post-1", "prehistory-post-2", "prehistory-post-3",
		"prehistory-post-4", "prehistory-post-5", "prehistory-post-6",
	})

	assertToggle(creator, false)
	assertPreHistoryMessages(t, f.ctx, joiner, channelID, []string{
		"prehistory-post-1", "prehistory-post-2", "prehistory-post-3",
		"prehistory-post-4", "prehistory-post-5", "prehistory-post-6",
	})
	if floor := channelHistoryFloor(t, f.ctx, f.store, channelID, priorMember.id); floor != 0 {
		t.Fatalf("prior member history floor after unhiding = %d, want 0", floor)
	}

	if laterJoiner == nil {
		return
	}

	assertToggle(creator, true)
	if got := importChannelInvite(t, f.ctx, laterJoiner.cmds, inviteHash); got != channelID {
		t.Fatalf("later joiner channel = %d, want %d", got, channelID)
	}
	if floor := channelHistoryFloor(t, f.ctx, f.store, channelID, laterJoiner.id); floor == 0 {
		t.Fatal("later joiner has no hidden-history floor before username is set")
	}

	if err := creator.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		_, err := client.ChannelsUpdateUsername(ctx, &tg.ChannelsUpdateUsernameRequest{
			Channel: inputChannel(creator.id, channelID), Username: "prehistorypublic",
		})
		return err
	}); err != nil {
		t.Fatalf("set username on hidden group: %v", err)
	}
	assertPreHistoryFlag(t, f.ctx, creator, channelID, false)
	for _, memberID := range []int64{priorMember.id, joiner.id, laterJoiner.id} {
		if floor := channelHistoryFloor(t, f.ctx, f.store, channelID, memberID); floor != 0 {
			t.Fatalf("member %d history floor after setting username = %d, want 0", memberID, floor)
		}
	}
	assertPreHistoryMessages(t, f.ctx, laterJoiner, channelID, []string{
		"prehistory-post-1", "prehistory-post-2", "prehistory-post-3",
		"prehistory-post-4", "prehistory-post-5", "prehistory-post-6",
	})
	assertChannelRPCError(t, f.ctx, creator.cmds, "CHAT_LINK_EXISTS", func(ctx context.Context, client *tg.Client) error {
		_, err := client.ChannelsTogglePreHistoryHidden(ctx, &tg.ChannelsTogglePreHistoryHiddenRequest{
			Channel: inputChannel(creator.id, channelID),
			Enabled: true,
		})
		return err
	})
}

func assertPreHistoryFlag(t *testing.T, ctx context.Context, client *smokeClient, channelID int64, want bool) {
	t.Helper()
	if err := client.call(ctx, func(ctx context.Context, api *tg.Client) error {
		result, err := api.ChannelsGetFullChannel(ctx, inputChannel(client.id, channelID))
		if err != nil {
			return err
		}
		full, ok := result.FullChat.(*tg.ChannelFull)
		if !ok {
			return fmt.Errorf("getFullChannel full chat = %T, want *tg.ChannelFull", result.FullChat)
		}
		if full.GetHiddenPrehistory() != want {
			return fmt.Errorf("hidden_prehistory = %v, want %v", full.GetHiddenPrehistory(), want)
		}
		return nil
	}); err != nil {
		t.Fatalf("get full channel hidden_prehistory: %v", err)
	}
}

func assertPreHistoryMessages(t *testing.T, ctx context.Context, client *smokeClient, channelID int64, want []string) {
	t.Helper()
	if err := client.call(ctx, func(ctx context.Context, api *tg.Client) error {
		result, err := api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
			Peer:  peerChannel(client.id, channelID),
			Limit: 100,
		})
		if err != nil {
			return err
		}
		page, ok := result.(*tg.MessagesChannelMessages)
		if !ok {
			return fmt.Errorf("getHistory result = %T, want *tg.MessagesChannelMessages", result)
		}
		got := make(map[string]int, len(page.Messages))
		for _, class := range page.Messages {
			if message, ok := class.(*tg.Message); ok {
				got[message.Message]++
			}
		}
		if len(got) != len(want) {
			return fmt.Errorf("getHistory message texts = %v, want %v", got, want)
		}
		for _, text := range want {
			if got[text] != 1 {
				return fmt.Errorf("getHistory message %q count = %d, want 1; all texts %v", text, got[text], got)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("get channel history: %v", err)
	}
}

func channelHistoryFloor(t *testing.T, ctx context.Context, st *store.Store, channelID, userID int64) int64 {
	t.Helper()
	member, found, err := st.ChannelMemberOf(ctx, channelID, userID)
	if err != nil {
		t.Fatalf("read channel member %d floor: %v", userID, err)
	}
	if !found {
		t.Fatalf("channel member %d not found", userID)
	}
	return member.AvailableMinID
}
