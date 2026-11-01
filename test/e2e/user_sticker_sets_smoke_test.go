package e2e_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/config"
)

func testSmokeUserCreatedStickerSets(t *testing.T) {
	t.Helper()
	f := newSmokeFixtureWithSetup(t, config.RegistrationClosed, nil)
	const ownerPhone, memberPhone = "+15551049911", "+15551049912"
	seedSmokeUsers(t, f, ownerPhone, memberPhone)
	ownerRequests := &clientRequestRecorder{}
	memberRequests := &clientRequestRecorder{}
	owner := newSmokeClientWithRequests(t, f, "user-created sticker set owner", ownerPhone, ownerRequests)
	member := newSmokeClientWithRequests(t, f, "custom emoji recipient", memberPhone, memberRequests)

	stickerBytes := smokeCustomEmojiTGS(t)
	const uploadID = int64(1049911)
	parts := uploadSmokeMediaParts(t, f.ctx, owner, uploadID, stickerBytes, len(stickerBytes))
	var document *tg.Document
	if err := owner.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		result, err := client.MessagesUploadMedia(ctx, &tg.MessagesUploadMediaRequest{
			Peer: &tg.InputPeerSelf{},
			Media: &tg.InputMediaUploadedDocument{
				File:       &tg.InputFile{ID: uploadID, Parts: parts, Name: "star.tgs"},
				MimeType:   "application/x-tgsticker",
				Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeFilename{FileName: "star.tgs"}, &tg.DocumentAttributeAnimated{}},
			},
		})
		if err != nil {
			return err
		}
		media, ok := result.(*tg.MessageMediaDocument)
		if !ok {
			return fmt.Errorf("uploadMedia result = %T, want *tg.MessageMediaDocument", result)
		}
		var okDocument bool
		document, okDocument = media.Document.(*tg.Document)
		if !okDocument || document.ID <= 0 || document.AccessHash == 0 {
			return fmt.Errorf("uploaded sticker document = %#v, want stored document identity", media.Document)
		}
		return nil
	}); err != nil {
		t.Fatalf("upload custom emoji document: %v", err)
	}

	request := &tg.StickersCreateStickerSetRequest{
		UserID: &tg.InputUserSelf{}, Title: "Smoke emoji set", ShortName: "smoke_custom_emoji",
		Emojis: true, TextColor: true,
		Stickers: []tg.InputStickerSetItem{{
			Document: &tg.InputDocument{ID: document.ID, AccessHash: document.AccessHash, FileReference: document.FileReference},
			Emoji:    "⭐",
		}},
	}
	request.SetSoftware("teagram-smoke")
	var created *tg.MessagesStickerSet
	if err := fixtureClientCall(t, owner, "stickers_createStickerSet.1", func(ctx context.Context, client *tg.Client) error {
		result, err := client.StickersCreateStickerSet(ctx, request)
		if err != nil {
			return err
		}
		var ok bool
		created, ok = result.(*tg.MessagesStickerSet)
		if !ok || created.Set.ShortName != request.ShortName || created.Set.Count != 1 || !created.Set.Creator {
			return fmt.Errorf("createStickerSet result = %#v, want owner set %q with one sticker", result, request.ShortName)
		}
		return nil
	}); err != nil {
		t.Fatalf("create custom emoji set with captured client request: %v", err)
	}
	if !created.Set.Emojis || !created.Set.TextColor || len(created.Documents) != 1 {
		t.Fatalf("created custom emoji set = %+v with %d documents, want emoji/text-color set with one sticker", created.Set, len(created.Documents))
	}
	customEmojiDocument, ok := created.Documents[0].(*tg.Document)
	if !ok {
		t.Fatalf("created custom emoji document = %T, want *tg.Document", created.Documents[0])
	}
	customEmojiDocumentID := customEmojiDocument.ID
	if err := fixtureClientCall(t, member, "messages_getCustomEmojiDocuments.1", func(ctx context.Context, client *tg.Client) error {
		documents, err := client.MessagesGetCustomEmojiDocuments(ctx, []int64{customEmojiDocumentID, customEmojiDocumentID + 1000000})
		if err != nil {
			return err
		}
		if len(documents) != 1 {
			return fmt.Errorf("getCustomEmojiDocuments returned %d documents, want the one live set member", len(documents))
		}
		document, ok := documents[0].(*tg.Document)
		if !ok || document.ID != customEmojiDocumentID {
			return fmt.Errorf("getCustomEmojiDocuments document = %#v, want %d", documents[0], customEmojiDocumentID)
		}
		var customEmoji *tg.DocumentAttributeCustomEmoji
		for _, attribute := range document.Attributes {
			if value, ok := attribute.(*tg.DocumentAttributeCustomEmoji); ok {
				customEmoji = value
			}
		}
		if customEmoji == nil {
			return errors.New("getCustomEmojiDocuments omitted custom emoji attribute")
		}
		set, ok := customEmoji.Stickerset.(*tg.InputStickerSetID)
		if customEmoji.Alt != "⭐" || !customEmoji.GetFree() || !ok || set.ID != created.Set.ID || set.AccessHash != created.Set.AccessHash {
			return fmt.Errorf("getCustomEmojiDocuments attribute = %+v, want free star emoji in set %d", customEmoji, created.Set.ID)
		}
		return nil
	}); err != nil {
		t.Fatalf("get custom emoji documents as another non-Premium account: %v", err)
	}
	ref := &tg.InputStickerSetID{ID: created.Set.ID, AccessHash: created.Set.AccessHash}
	if err := owner.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		result, err := client.MessagesInstallStickerSet(ctx, &tg.MessagesInstallStickerSetRequest{Stickerset: ref})
		if err != nil {
			return err
		}
		if _, ok := result.(*tg.MessagesStickerSetInstallResultSuccess); !ok {
			return fmt.Errorf("installStickerSet result = %T, want success", result)
		}
		emojiResult, err := client.MessagesGetEmojiStickers(ctx, 0)
		if err != nil {
			return err
		}
		emojis, ok := emojiResult.(*tg.MessagesAllStickers)
		if !ok || len(emojis.Sets) != 1 || emojis.Sets[0].ID != created.Set.ID {
			return fmt.Errorf("getEmojiStickers result = %#v, want created set %d", emojiResult, created.Set.ID)
		}
		allResult, err := client.MessagesGetAllStickers(ctx, 0)
		if err != nil {
			return err
		}
		all, ok := allResult.(*tg.MessagesAllStickers)
		if !ok || len(all.Sets) != 0 {
			return fmt.Errorf("getAllStickers result = %#v, want custom emoji set filtered out", allResult)
		}
		return nil
	}); err != nil {
		t.Fatalf("install and list custom emoji set: %v", err)
	}

	const customEmojiText = "hi ⭐"
	sendCustomEmoji := func(fixture string, peer tg.InputPeerClass, randomID int64) {
		t.Helper()
		if err := fixtureClientCall(t, owner, fixture, func(ctx context.Context, client *tg.Client) error {
			result, err := client.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
				Peer: peer, Message: customEmojiText, RandomID: randomID,
				Entities: []tg.MessageEntityClass{&tg.MessageEntityCustomEmoji{
					Offset: 3, Length: 1, DocumentID: customEmojiDocumentID,
				}},
			})
			if err != nil {
				return err
			}
			updates, ok := result.(*tg.Updates)
			if !ok {
				return fmt.Errorf("send custom emoji response = %T, want *tg.Updates", result)
			}
			if err := smokeNoPremiumUsers(updates.Users, "messages.sendMessage"); err != nil {
				return err
			}
			return nil
		}); err != nil {
			t.Fatalf("send custom emoji using %s: %v", fixture, err)
		}
	}

	sendCustomEmoji("messages_sendMessage.customEmoji.private.1", peerUser(owner.id, member.id), 1049912)
	assertSmokeCustomEmojiReceived(t, f.ctx, member, peerUser(member.id, owner.id), customEmojiText, customEmojiDocumentID)

	var groupID int64
	if err := owner.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		createdChat, err := client.MessagesCreateChat(ctx, &tg.MessagesCreateChatRequest{
			Title: "Smoke custom emoji group",
			Users: []tg.InputUserClass{inputUser(owner.id, member.id)},
		})
		if err != nil {
			return err
		}
		updates, ok := createdChat.Updates.(*tg.Updates)
		if !ok || len(updates.Chats) != 1 {
			return fmt.Errorf("create custom emoji group result = %#v, want one chat", createdChat.Updates)
		}
		chat, ok := updates.Chats[0].(*tg.Chat)
		if !ok {
			return fmt.Errorf("create custom emoji group chat = %T, want *tg.Chat", updates.Chats[0])
		}
		groupID = chat.ID
		return nil
	}); err != nil {
		t.Fatalf("create custom emoji group: %v", err)
	}
	sendCustomEmoji("messages_sendMessage.customEmoji.group.1", &tg.InputPeerChat{ChatID: groupID}, 1049913)
	assertSmokeCustomEmojiReceived(t, f.ctx, member, &tg.InputPeerChat{ChatID: groupID}, customEmojiText, customEmojiDocumentID)

	channelID := createBroadcastChannel(t, f.ctx, owner.cmds, "Smoke custom emoji channel")
	if err := owner.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		_, err := client.ChannelsInviteToChannel(ctx, &tg.ChannelsInviteToChannelRequest{
			Channel: inputChannel(owner.id, channelID),
			Users:   []tg.InputUserClass{inputUser(owner.id, member.id)},
		})
		return err
	}); err != nil {
		t.Fatalf("invite custom emoji recipient to channel: %v", err)
	}
	sendCustomEmoji("messages_sendMessage.customEmoji.channel.1", peerChannel(owner.id, channelID), 1049914)
	assertSmokeCustomEmojiReceived(t, f.ctx, member, peerChannel(member.id, channelID), customEmojiText, customEmojiDocumentID)

	for _, client := range []*smokeClient{owner, member} {
		if err := client.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
			full, err := api.UsersGetFullUser(ctx, &tg.InputUserSelf{})
			if err != nil {
				return err
			}
			if len(full.Users) != 1 {
				return fmt.Errorf("users.getFullUser returned %d users, want the account only", len(full.Users))
			}
			return smokeNoPremiumUsers(full.Users, "users.getFullUser")
		}); err != nil {
			t.Fatalf("check custom emoji account Premium flag for user %d: %v", client.id, err)
		}
	}
}

func assertSmokeCustomEmojiReceived(t *testing.T, ctx context.Context, client *smokeClient, peer tg.InputPeerClass, text string, documentID int64) {
	t.Helper()
	var result tg.MessagesMessagesClass
	if err := client.call(ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		result, err = api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{Peer: peer, Limit: 10})
		return err
	}); err != nil {
		t.Fatalf("get custom emoji history for %T: %v", peer, err)
	}
	var messages []tg.MessageClass
	switch page := result.(type) {
	case *tg.MessagesMessages:
		messages = page.Messages
	case *tg.MessagesMessagesSlice:
		messages = page.Messages
	case *tg.MessagesChannelMessages:
		messages = page.Messages
	default:
		t.Fatalf("custom emoji history = %T, want messages page", result)
	}
	for _, class := range messages {
		message, ok := class.(*tg.Message)
		if ok && message.Message == text {
			assertSmokeCustomEmojiEntity(t, message, documentID)
			return
		}
	}
	t.Fatalf("history for %T omitted custom emoji message %q", peer, text)
}

func assertSmokeCustomEmojiEntity(t *testing.T, message *tg.Message, documentID int64) {
	t.Helper()
	if len(message.Entities) != 1 {
		t.Fatalf("custom emoji message entities = %+v, want one entity", message.Entities)
	}
	entity, ok := message.Entities[0].(*tg.MessageEntityCustomEmoji)
	if !ok || entity.Offset != 3 || entity.Length != 1 || entity.DocumentID != documentID {
		t.Fatalf("custom emoji entity = %#v, want offset 3 length 1 document %d", message.Entities[0], documentID)
	}
}

func smokeNoPremiumUsers(users []tg.UserClass, what string) error {
	for _, class := range users {
		user, ok := class.(*tg.User)
		if !ok {
			return fmt.Errorf("%s user = %T, want *tg.User", what, class)
		}
		if user.GetPremium() {
			return fmt.Errorf("%s marked user %d Premium after custom emoji use", what, user.ID)
		}
	}
	return nil
}

func smokeCustomEmojiTGS(t *testing.T) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := gzip.NewWriter(&output)
	if _, err := writer.Write([]byte(`{"tgs":1,"fr":60,"ip":0,"op":1,"w":100,"h":100,"layers":[],"assets":[]}`)); err != nil {
		t.Fatalf("write smoke custom emoji TGS file: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close smoke custom emoji TGS file: %v", err)
	}
	if output.Len() == 0 {
		t.Fatal("smoke custom emoji TGS file is empty")
	}
	return output.Bytes()
}
