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

func testSmokeChannelReactions(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	const phoneA, phoneB = "+15551047021", "+15551047022"
	seedSmokeUsers(t, f, phoneA, phoneB)
	a, b := newSmokeClient(t, f, "Channel reaction creator", phoneA), newSmokeClient(t, f, "Channel reaction member", phoneB)

	for _, megagroup := range []bool{false, true} {
		name := "broadcast"
		if megagroup {
			name = "megagroup"
		}
		t.Run(name, func(t *testing.T) {
			var channelID int64
			if megagroup {
				channelID = createMegagroup(t, f.ctx, a.cmds, "Megagroup reactions")
			} else {
				channelID = createBroadcastChannel(t, f.ctx, a.cmds, "Broadcast reactions")
			}
			invite := exportChannelInvite(t, f.ctx, a.id, a.cmds, channelID)
			if joined := importChannelInvite(t, f.ctx, b.cmds, invite); joined != channelID {
				t.Fatalf("joined channel = %d, want %d", joined, channelID)
			}
			peerA, peerB := peerChannel(a.id, channelID), peerChannel(b.id, channelID)

			if err := b.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
				full, err := client.ChannelsGetFullChannel(ctx, inputChannel(b.id, channelID))
				if err != nil {
					return err
				}
				info, ok := full.FullChat.(*tg.ChannelFull)
				if !ok {
					return fmt.Errorf("full channel = %T, want *tg.ChannelFull", full.FullChat)
				}
				available, ok := info.GetAvailableReactions()
				if !ok {
					return errors.New("default channel full info omitted available_reactions")
				}
				if _, ok := available.(*tg.ChatReactionsAll); !ok {
					return fmt.Errorf("default channel reactions = %T, want *tg.ChatReactionsAll", available)
				}
				return nil
			}); err != nil {
				t.Fatalf("read default channel reactions: %v", err)
			}

			const messageText = "channel reaction smoke post"
			var messageID int
			if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
				result, err := client.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
					Peer: peerA, Message: messageText, RandomID: 1047021 + channelID%1000,
				})
				if err != nil {
					return err
				}
				updates, ok := result.(*tg.Updates)
				if !ok {
					return fmt.Errorf("channel post result = %T, want *tg.Updates", result)
				}
				for _, update := range updates.Updates {
					created, ok := update.(*tg.UpdateNewChannelMessage)
					if !ok {
						continue
					}
					message, ok := created.Message.(*tg.Message)
					if ok && message.Message == messageText {
						messageID = message.ID
						return nil
					}
				}
				return errors.New("channel post result omitted the new post")
			}); err != nil {
				t.Fatalf("send channel reaction target: %v", err)
			}

			if err := b.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
				result, err := client.MessagesSendReaction(ctx, &tg.MessagesSendReactionRequest{
					Peer: peerB, MsgID: messageID,
					Reaction: []tg.ReactionClass{&tg.ReactionEmoji{Emoticon: "🔥"}},
				})
				if err != nil {
					return err
				}
				updates, ok := result.(*tg.Updates)
				if !ok || len(updates.Updates) != 1 {
					return fmt.Errorf("sendReaction result = %#v, want one reaction update", result)
				}
				update, ok := updates.Updates[0].(*tg.UpdateMessageReactions)
				if !ok {
					return fmt.Errorf("sendReaction update = %T, want *tg.UpdateMessageReactions", updates.Updates[0])
				}
				return checkSmokeChannelReactionCounts(update.Reactions, 1, true)
			}); err != nil {
				t.Fatalf("member reacts to channel post: %v", err)
			}

			readAsA := func(wantCount int) func(context.Context, *tg.Client) error {
				return func(ctx context.Context, client *tg.Client) error {
					result, err := client.MessagesGetMessagesReactions(ctx, &tg.MessagesGetMessagesReactionsRequest{
						Peer: peerA, ID: []int{messageID},
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
					return checkSmokeChannelReactionCounts(update.Reactions, wantCount, false)
				}
			}
			if err := a.call(f.ctx, readAsA(1)); err != nil {
				t.Fatalf("creator reads channel reaction count: %v", err)
			}

			for _, source := range []string{"history", "channels.getMessages"} {
				if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
					var result tg.MessagesMessagesClass
					var err error
					if source == "history" {
						result, err = client.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{Peer: peerA, Limit: 10})
					} else {
						result, err = client.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
							Channel: inputChannel(a.id, channelID),
							ID:      []tg.InputMessageClass{&tg.InputMessageID{ID: messageID}},
						})
					}
					if err != nil {
						return err
					}
					page, ok := result.(*tg.MessagesChannelMessages)
					if !ok {
						return fmt.Errorf("%s result = %T, want *tg.MessagesChannelMessages", source, result)
					}
					for _, item := range page.Messages {
						message, ok := item.(*tg.Message)
						if ok && message.ID == messageID {
							reactions, ok := message.GetReactions()
							if !ok {
								return fmt.Errorf("%s message omitted reactions", source)
							}
							return checkSmokeChannelReactionCounts(reactions, 1, false)
						}
					}
					return fmt.Errorf("%s omitted message %d", source, messageID)
				}); err != nil {
					t.Fatalf("read channel post through %s: %v", source, err)
				}
			}

			if err := b.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
				result, err := client.MessagesSendReaction(ctx, &tg.MessagesSendReactionRequest{Peer: peerB, MsgID: messageID})
				if err != nil {
					return err
				}
				updates, ok := result.(*tg.Updates)
				if !ok || len(updates.Updates) != 1 {
					return fmt.Errorf("clear reaction result = %#v, want one reaction update", result)
				}
				update, ok := updates.Updates[0].(*tg.UpdateMessageReactions)
				if !ok {
					return fmt.Errorf("clear reaction update = %T, want *tg.UpdateMessageReactions", updates.Updates[0])
				}
				return checkSmokeChannelReactionCounts(update.Reactions, 0, false)
			}); err != nil {
				t.Fatalf("clear channel reaction: %v", err)
			}
			if err := a.call(f.ctx, readAsA(0)); err != nil {
				t.Fatalf("creator reads cleared channel reaction: %v", err)
			}
		})
	}
}

func checkSmokeChannelReactionCounts(reactions tg.MessageReactions, wantCount int, chosen bool) error {
	if wantCount == 0 {
		if len(reactions.Results) != 0 {
			return fmt.Errorf("reaction results = %d, want empty", len(reactions.Results))
		}
		return nil
	}
	if len(reactions.Results) != 1 || reactions.Results[0].Count != wantCount {
		return fmt.Errorf("reaction results = %+v, want one count of %d", reactions.Results, wantCount)
	}
	emoji, ok := reactions.Results[0].Reaction.(*tg.ReactionEmoji)
	if !ok || emoji.Emoticon != "🔥" {
		return fmt.Errorf("reaction = %#v, want fire", reactions.Results[0].Reaction)
	}
	_, hasChosen := reactions.Results[0].GetChosenOrder()
	if hasChosen != chosen {
		return fmt.Errorf("reaction chosen = %v, want %v", hasChosen, chosen)
	}
	return nil
}

func testSmokeAvailableReactions(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	const phone = "+15551048021"
	seedSmokeUsers(t, f, phone)
	client := newSmokeClient(t, f, "available reactions", phone)
	if err := client.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		result, err := api.MessagesGetAvailableReactions(ctx, 0)
		if err != nil {
			return fmt.Errorf("messages.getAvailableReactions: %w", err)
		}
		available, ok := result.(*tg.MessagesAvailableReactions)
		if !ok || len(available.Reactions) == 0 {
			return fmt.Errorf("messages.getAvailableReactions = %T, want non-empty availableReactions", result)
		}
		var heart *tg.AvailableReaction
		for _, reaction := range available.Reactions {
			if reaction.Reaction == "❤" {
				heart = &reaction
				break
			}
		}
		if heart == nil {
			return errors.New("available reactions omitted ❤")
		}
		for _, item := range []struct {
			name     string
			document tg.DocumentClass
			webp     bool
		}{
			{name: "static_icon", document: heart.StaticIcon, webp: true},
			{name: "select_animation", document: heart.SelectAnimation},
		} {
			document, ok := item.document.(*tg.Document)
			if !ok {
				return fmt.Errorf("❤ %s = %T, want *tg.Document", item.name, item.document)
			}
			fileResult, err := api.UploadGetFile(ctx, &tg.UploadGetFileRequest{
				Location: &tg.InputDocumentFileLocation{
					ID: document.ID, AccessHash: document.AccessHash, FileReference: document.FileReference,
				},
				Offset: 0,
				Limit:  256 * 1024,
			})
			if err != nil {
				return fmt.Errorf("upload.getFile for ❤ %s: %w", item.name, err)
			}
			file, ok := fileResult.(*tg.UploadFile)
			if !ok {
				return fmt.Errorf("upload.getFile for ❤ %s = %T, want *tg.UploadFile", item.name, fileResult)
			}
			if item.webp {
				if len(file.Bytes) < 12 || string(file.Bytes[:4]) != "RIFF" || string(file.Bytes[8:12]) != "WEBP" {
					return errors.New("❤ static_icon does not have WebP magic bytes")
				}
			} else if len(file.Bytes) < 2 || file.Bytes[0] != 0x1f || file.Bytes[1] != 0x8b {
				return errors.New("❤ select_animation does not have gzip magic bytes")
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("available reaction assets smoke: %v", err)
	}
}
