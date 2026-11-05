package e2e_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/config"
)

func testSmokeCustomEmojiActions(t *testing.T) {
	t.Helper()
	catalogue, _ := loadSmokeStickerBundleCatalogue(t)
	f := newSmokeFixtureWithSetup(t, config.RegistrationClosed, func(f *smokeFixture) {
		f.stickerSets = catalogue
	})
	const phoneA, phoneB = "+15551049001", "+15551049002"
	seedSmokeUsers(t, f, phoneA, phoneB)
	a, b := newSmokeClient(t, f, "custom emoji actor", phoneA), newSmokeClient(t, f, "custom emoji peer", phoneB)
	const setShortName = "openmoji_emoji_animals_nature"
	document := getSmokeBundleStickerDocument(t, f, a, setShortName, 1)
	if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		result, err := client.MessagesInstallStickerSet(ctx, &tg.MessagesInstallStickerSetRequest{
			Stickerset: &tg.InputStickerSetShortName{ShortName: setShortName},
		})
		if err != nil {
			return err
		}
		if _, ok := result.(*tg.MessagesStickerSetInstallResultSuccess); !ok {
			return fmt.Errorf("install custom emoji set = %T, want successful install", result)
		}
		return nil
	}); err != nil {
		t.Fatalf("install custom emoji set: %v", err)
	}

	var chatID int64
	if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		created, err := client.MessagesCreateChat(ctx, &tg.MessagesCreateChatRequest{
			Title: "Custom emoji actions",
			Users: []tg.InputUserClass{inputUser(a.id, b.id)},
		})
		if err != nil {
			return err
		}
		updates, ok := created.Updates.(*tg.Updates)
		if !ok || len(updates.Chats) != 1 {
			return fmt.Errorf("create chat updates = %#v, want one chat", created.Updates)
		}
		chat, ok := updates.Chats[0].(*tg.Chat)
		if !ok {
			return fmt.Errorf("created chat = %T, want *tg.Chat", updates.Chats[0])
		}
		chatID = chat.ID
		return nil
	}); err != nil {
		t.Fatalf("create custom emoji chat: %v", err)
	}

	const messageText = "custom emoji reaction target"
	var aMessageID int
	if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		result, err := client.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: &tg.InputPeerChat{ChatID: chatID}, Message: messageText, RandomID: 1049001,
		})
		if err != nil {
			return err
		}
		message, _, ok := outgoingMessage(t, result, messageText)
		if !ok {
			return fmt.Errorf("send result omitted %q", messageText)
		}
		aMessageID = message.ID
		return nil
	}); err != nil {
		t.Fatalf("send custom emoji reaction target: %v", err)
	}
	var bMessageID int
	if err := b.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		result, err := client.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{Peer: &tg.InputPeerChat{ChatID: chatID}, Limit: 20})
		if err != nil {
			return err
		}
		messages, ok := result.(*tg.MessagesMessages)
		if !ok {
			return fmt.Errorf("group history = %T, want *tg.MessagesMessages", result)
		}
		for _, item := range messages.Messages {
			message, ok := item.(*tg.Message)
			if ok && message.Message == messageText {
				bMessageID = message.ID
				return nil
			}
		}
		return fmt.Errorf("B's group history omitted %q", messageText)
	}); err != nil {
		t.Fatalf("read custom emoji target as B: %v", err)
	}

	assertChannelRPCError(t, f.ctx, a.cmds, "REACTION_INVALID", func(ctx context.Context, client *tg.Client) error {
		_, err := client.MessagesSendReaction(ctx, &tg.MessagesSendReactionRequest{
			Peer: &tg.InputPeerChat{ChatID: chatID}, MsgID: aMessageID,
			Reaction: []tg.ReactionClass{&tg.ReactionCustomEmoji{DocumentID: document.ID}},
		})
		return err
	})
	if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		_, err := client.MessagesSetChatAvailableReactions(ctx, &tg.MessagesSetChatAvailableReactionsRequest{
			Peer:               &tg.InputPeerChat{ChatID: chatID},
			AvailableReactions: &tg.ChatReactionsAll{AllowCustom: true},
		})
		return err
	}); err != nil {
		t.Fatalf("allow custom emoji reactions in chat: %v", err)
	}
	if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		_, err := client.MessagesSendReaction(ctx, &tg.MessagesSendReactionRequest{
			Peer: &tg.InputPeerChat{ChatID: chatID}, MsgID: aMessageID,
			Reaction: []tg.ReactionClass{&tg.ReactionCustomEmoji{DocumentID: document.ID}},
		})
		return err
	}); err != nil {
		t.Fatalf("send custom emoji reaction: %v", err)
	}
	if err := b.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		result, err := client.MessagesGetMessagesReactions(ctx, &tg.MessagesGetMessagesReactionsRequest{
			Peer: &tg.InputPeerChat{ChatID: chatID}, ID: []int{bMessageID},
		})
		if err != nil {
			return err
		}
		updates, ok := result.(*tg.Updates)
		if !ok || len(updates.Updates) != 1 {
			return fmt.Errorf("getMessagesReactions = %#v, want one reaction update", result)
		}
		update, ok := updates.Updates[0].(*tg.UpdateMessageReactions)
		if !ok || len(update.Reactions.Results) != 1 || update.Reactions.Results[0].Count != 1 {
			return fmt.Errorf("custom reaction update = %#v, want one count", updates.Updates[0])
		}
		reaction, ok := update.Reactions.Results[0].Reaction.(*tg.ReactionCustomEmoji)
		if !ok || reaction.DocumentID != document.ID {
			return fmt.Errorf("reaction = %#v, want custom emoji document %d", update.Reactions.Results[0].Reaction, document.ID)
		}
		return nil
	}); err != nil {
		t.Fatalf("read custom emoji reaction as B: %v", err)
	}

	if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		updated, err := client.AccountUpdateEmojiStatus(ctx, &tg.EmojiStatus{DocumentID: document.ID})
		if err != nil {
			return err
		}
		if !updated {
			return errors.New("updateEmojiStatus returned false")
		}
		return nil
	}); err != nil {
		t.Fatalf("set custom emoji status: %v", err)
	}
	if err := b.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		users, err := client.UsersGetUsers(ctx, []tg.InputUserClass{inputUser(b.id, a.id)})
		if err != nil {
			return err
		}
		if len(users) != 1 {
			return fmt.Errorf("users.getUsers returned %d entries, want 1", len(users))
		}
		user, ok := users[0].(*tg.User)
		if !ok {
			return fmt.Errorf("users.getUsers entry = %T, want *tg.User", users[0])
		}
		status, present := user.GetEmojiStatus()
		custom, ok := status.(*tg.EmojiStatus)
		if !present || !ok || custom.DocumentID != document.ID {
			return fmt.Errorf("user emoji status = %#v present=%t, want document %d", status, present, document.ID)
		}
		return nil
	}); err != nil {
		t.Fatalf("read custom emoji status as B: %v", err)
	}
}
