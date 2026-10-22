package e2e_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/gotd/td/tg"
)

func testSmokeBasicGroupReactions(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	const phoneA, phoneB = "+15551047011", "+15551047012"
	seedSmokeUsers(t, f, phoneA, phoneB)
	a, b := newSmokeClient(t, f, "A1", phoneA), newSmokeClient(t, f, "B1", phoneB)

	var chatID int64
	if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		created, err := client.MessagesCreateChat(ctx, &tg.MessagesCreateChatRequest{
			Title: "Reaction smoke",
			Users: []tg.InputUserClass{inputUser(a.id, b.id)},
		})
		if err != nil {
			return err
		}
		updates, ok := created.Updates.(*tg.Updates)
		if !ok || len(updates.Chats) != 1 {
			return fmt.Errorf("createChat updates = %#v, want one chat", created.Updates)
		}
		chat, ok := updates.Chats[0].(*tg.Chat)
		if !ok {
			return fmt.Errorf("createChat chat = %T, want *tg.Chat", updates.Chats[0])
		}
		chatID = chat.ID
		return nil
	}); err != nil {
		t.Fatalf("create reaction group: %v", err)
	}

	if err := b.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		full, err := client.MessagesGetFullChat(ctx, chatID)
		if err != nil {
			return err
		}
		info, ok := full.FullChat.(*tg.ChatFull)
		if !ok {
			return fmt.Errorf("full chat = %T, want *tg.ChatFull", full.FullChat)
		}
		available, ok := info.GetAvailableReactions()
		if !ok {
			return errors.New("default full chat omitted available_reactions")
		}
		if _, ok := available.(*tg.ChatReactionsAll); !ok {
			return fmt.Errorf("default reactions = %T, want *tg.ChatReactionsAll", available)
		}
		return nil
	}); err != nil {
		t.Fatalf("read default group reactions: %v", err)
	}

	const messageText = "reaction round trip"
	var aMessageID int
	if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		result, err := client.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: &tg.InputPeerChat{ChatID: chatID}, Message: messageText, RandomID: 1047011,
		})
		if err != nil {
			return err
		}
		message, _, ok := outgoingMessage(t, result, messageText)
		if !ok {
			return fmt.Errorf("group send result omitted %q", messageText)
		}
		aMessageID = message.ID
		return nil
	}); err != nil {
		t.Fatalf("send reaction target: %v", err)
	}
	var bMessageID int
	if err := b.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		result, err := client.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
			Peer: &tg.InputPeerChat{ChatID: chatID}, Limit: 20,
		})
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
		t.Fatalf("read reaction target as B: %v", err)
	}

	if err := b.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		_, err := client.MessagesSendReaction(ctx, &tg.MessagesSendReactionRequest{
			Peer: &tg.InputPeerChat{ChatID: chatID}, MsgID: bMessageID,
			Reaction: []tg.ReactionClass{&tg.ReactionEmoji{Emoticon: "❤"}},
		})
		return err
	}); err != nil {
		t.Fatalf("B send heart: %v", err)
	}

	readCount := func(want int) {
		t.Helper()
		if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
			result, err := client.MessagesGetMessagesReactions(ctx, &tg.MessagesGetMessagesReactionsRequest{
				Peer: &tg.InputPeerChat{ChatID: chatID}, ID: []int{aMessageID},
			})
			if err != nil {
				return err
			}
			updates, ok := result.(*tg.Updates)
			if !ok || len(updates.Updates) != 1 {
				return fmt.Errorf("getMessagesReactions = %#v, want one update", result)
			}
			update, ok := updates.Updates[0].(*tg.UpdateMessageReactions)
			if !ok {
				return fmt.Errorf("reaction update = %T, want *tg.UpdateMessageReactions", updates.Updates[0])
			}
			if len(update.Reactions.Results) != want {
				return fmt.Errorf("reaction results = %d, want %d", len(update.Reactions.Results), want)
			}
			if want == 1 {
				if update.Reactions.Results[0].Count != 1 {
					return fmt.Errorf("heart count = %d, want 1", update.Reactions.Results[0].Count)
				}
				emoji, ok := update.Reactions.Results[0].Reaction.(*tg.ReactionEmoji)
				if !ok || emoji.Emoticon != "❤" {
					return fmt.Errorf("reaction = %#v, want heart", update.Reactions.Results[0].Reaction)
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("A reads reaction count %d: %v", want, err)
		}
	}
	readCount(1)

	if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		result, err := client.MessagesSetChatAvailableReactions(ctx, &tg.MessagesSetChatAvailableReactionsRequest{
			Peer: &tg.InputPeerChat{ChatID: chatID},
			AvailableReactions: &tg.ChatReactionsSome{Reactions: []tg.ReactionClass{
				&tg.ReactionEmoji{Emoticon: "❤"},
			}},
		})
		if err == nil && result == nil {
			return errors.New("setChatAvailableReactions returned nil Updates")
		}
		return err
	}); err != nil {
		t.Fatalf("set group reaction list: %v", err)
	}
	assertChannelRPCError(t, f.ctx, b.cmds, "REACTION_INVALID", func(ctx context.Context, client *tg.Client) error {
		_, err := client.MessagesSendReaction(ctx, &tg.MessagesSendReactionRequest{
			Peer: &tg.InputPeerChat{ChatID: chatID}, MsgID: bMessageID,
			Reaction: []tg.ReactionClass{&tg.ReactionEmoji{Emoticon: "👍"}},
		})
		return err
	})
	readCount(1)

	if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		_, err := client.MessagesSetChatAvailableReactions(ctx, &tg.MessagesSetChatAvailableReactionsRequest{
			Peer: &tg.InputPeerChat{ChatID: chatID}, AvailableReactions: &tg.ChatReactionsNone{},
		})
		return err
	}); err != nil {
		t.Fatalf("disable group reactions: %v", err)
	}
	if err := b.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		_, err := client.MessagesSendReaction(ctx, &tg.MessagesSendReactionRequest{
			Peer: &tg.InputPeerChat{ChatID: chatID}, MsgID: bMessageID,
		})
		return err
	}); err != nil {
		t.Fatalf("B clears reaction while reactions are disabled: %v", err)
	}
	readCount(0)
}
