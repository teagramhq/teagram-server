package e2e_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/gotd/td/tg"
)

func testSmokeChannelDelete(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	const phoneAlice, phoneBob, phoneCarol = "+15551048901", "+15551048902", "+15551048903"
	seedSmokeUsers(t, f, phoneAlice, phoneBob, phoneCarol)
	alice := newSmokeClient(t, f, "delete creator", phoneAlice)
	bob := newSmokeClient(t, f, "delete admin", phoneBob)
	carol := newSmokeClient(t, f, "username claimant", phoneCarol)

	channelID := createBroadcastChannel(t, f.ctx, alice.cmds, "Delete me channel")
	if err := alice.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		_, err := client.ChannelsUpdateUsername(ctx, &tg.ChannelsUpdateUsernameRequest{
			Channel:  inputChannel(alice.id, channelID),
			Username: "delme",
		})
		return err
	}); err != nil {
		t.Fatalf("set public channel username: %v", err)
	}

	hash := exportChannelInvite(t, f.ctx, alice.id, alice.cmds, channelID)
	if joinedID := importChannelInvite(t, f.ctx, bob.cmds, hash); joinedID != channelID {
		t.Fatalf("Bob joined channel %d, want %d", joinedID, channelID)
	}
	if err := alice.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		_, err := client.ChannelsEditAdmin(ctx, &tg.ChannelsEditAdminRequest{
			Channel: inputChannel(alice.id, channelID),
			UserID:  inputUser(alice.id, bob.id),
			AdminRights: tg.ChatAdminRights{
				BanUsers: true,
			},
		})
		return err
	}); err != nil {
		t.Fatalf("promote Bob: %v", err)
	}
	assertChannelRPCError(t, f.ctx, bob.cmds, "CHAT_ADMIN_REQUIRED", func(ctx context.Context, client *tg.Client) error {
		_, err := client.ChannelsDeleteChannel(ctx, inputChannel(bob.id, channelID))
		return err
	})

	if err := alice.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		_, err := client.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer:     peerChannel(alice.id, channelID),
			Message:  "channel content before deletion",
			RandomID: 1048901,
		})
		return err
	}); err != nil {
		t.Fatalf("post before deletion: %v", err)
	}
	if err := alice.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		result, err := client.ChannelsDeleteChannel(ctx, inputChannel(alice.id, channelID))
		if err != nil {
			return err
		}
		if _, ok := result.(*tg.Updates); !ok {
			return fmt.Errorf("deleteChannel result = %T, want *tg.Updates", result)
		}
		return nil
	}); err != nil {
		t.Fatalf("delete channel as creator: %v", err)
	}

	for {
		update := recvOrCtx(t, f.ctx, bob.seen.channelUpdate, "Bob channel removal update")
		if update.Update.ChannelID != channelID {
			continue
		}
		if len(update.Chats) != 1 {
			t.Fatalf("Bob channel removal chats = %d, want one peer", len(update.Chats))
		}
		forbidden, ok := update.Chats[0].(*tg.ChannelForbidden)
		if !ok || forbidden.ID != channelID || forbidden.Title != "" {
			continue
		}
		break
	}
	assertChannelRPCError(t, f.ctx, bob.cmds, "PEER_ID_INVALID", func(ctx context.Context, client *tg.Client) error {
		_, err := client.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
			Peer:  peerChannel(bob.id, channelID),
			Limit: 10,
		})
		return err
	})
	if err := bob.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		return assertChannelAbsentFromDialogs(ctx, client, channelID)
	}); err != nil {
		t.Fatalf("Bob dialog list after deletion: %v", err)
	}

	if err := carol.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		available, err := client.ChannelsCheckUsername(ctx, &tg.ChannelsCheckUsernameRequest{
			Channel:  &tg.InputChannelEmpty{},
			Username: "delme",
		})
		if err != nil {
			return err
		}
		if available {
			return errors.New("deleted channel username became available to Carol")
		}
		return nil
	}); err != nil {
		t.Fatalf("check held username: %v", err)
	}
	carolChannelID := createBroadcastChannel(t, f.ctx, carol.cmds, "Carol channel")
	assertChannelRPCError(t, f.ctx, carol.cmds, "USERNAME_OCCUPIED", func(ctx context.Context, client *tg.Client) error {
		_, err := client.ChannelsUpdateUsername(ctx, &tg.ChannelsUpdateUsernameRequest{
			Channel:  inputChannel(carol.id, carolChannelID),
			Username: "delme",
		})
		return err
	})

	replacementID := createBroadcastChannel(t, f.ctx, alice.cmds, "Alice replacement channel")
	if err := alice.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		_, err := client.ChannelsUpdateUsername(ctx, &tg.ChannelsUpdateUsernameRequest{
			Channel:  inputChannel(alice.id, replacementID),
			Username: "delme",
		})
		return err
	}); err != nil {
		t.Fatalf("same creator reclaim username: %v", err)
	}
	if err := alice.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		resolved, err := client.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{Username: "delme"})
		if err != nil {
			return err
		}
		for _, chat := range resolved.Chats {
			if channel, ok := chat.(*tg.Channel); ok && channel.ID == replacementID {
				return nil
			}
		}
		return fmt.Errorf("resolveUsername did not return replacement channel %d", replacementID)
	}); err != nil {
		t.Fatalf("resolve creator's reclaimed username: %v", err)
	}
}
