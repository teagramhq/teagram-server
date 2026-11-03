package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/teagramhq/teagram-server/internal/store"
)

func TestRemoveStickerSetThumbnailItemClearsReferenceAndChangesHash(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	owner := mustUser(t, s, "+15559201001")
	thumbnailItem := storedFile(t, s, owner.ID)
	remainingItem := storedFile(t, s, owner.ID)

	set, err := s.CreateUserStickerSet(ctx, store.UserStickerSet{
		CreatorID: owner.ID, Title: "Emoji", ShortName: "emoji_thumb", Kind: "emojis",
	}, []store.UserStickerSetItem{
		{FileID: thumbnailItem.ID, AccessHash: thumbnailItem.AccessHash, Size: thumbnailItem.Size, Emoji: "⭐", Format: "TGS", Width: 100, Height: 100},
		{FileID: remainingItem.ID, AccessHash: remainingItem.AccessHash, Size: remainingItem.Size, Emoji: "✨", Format: "TGS", Width: 100, Height: 100},
	})
	if err != nil {
		t.Fatalf("create emoji sticker set: %v", err)
	}
	set, err = s.SetUserStickerSetThumb(ctx, owner.ID, set.ID, nil, thumbnailItem.ID)
	if err != nil {
		t.Fatalf("set emoji thumbnail item: %v", err)
	}
	beforeHash := set.Hash
	beforeThumbVersion := set.ThumbVersion

	updated, err := s.RemoveUserStickerSetItem(ctx, owner.ID, thumbnailItem.ID, thumbnailItem.AccessHash)
	if err != nil {
		t.Fatalf("remove emoji thumbnail item: %v", err)
	}
	if updated.ThumbDocumentID != 0 {
		t.Fatalf("removed thumbnail document ID = %d, want 0", updated.ThumbDocumentID)
	}
	if updated.ThumbVersion != beforeThumbVersion+1 {
		t.Fatalf("thumbnail version = %d, want %d", updated.ThumbVersion, beforeThumbVersion+1)
	}
	if updated.Hash == beforeHash {
		t.Fatalf("sticker set hash stayed %d after removing its thumbnail item", beforeHash)
	}

	persisted, err := s.UserStickerSetByID(ctx, set.ID, set.AccessHash)
	if err != nil {
		t.Fatalf("reload emoji sticker set: %v", err)
	}
	if persisted.ThumbDocumentID != 0 || persisted.ThumbVersion != updated.ThumbVersion || persisted.Hash != updated.Hash {
		t.Fatalf("persisted thumbnail state = id %d, version %d, hash %d; want id 0, version %d, hash %d",
			persisted.ThumbDocumentID, persisted.ThumbVersion, persisted.Hash, updated.ThumbVersion, updated.Hash)
	}
}

func TestRemovedCustomEmojiCannotBeReferencedByMessagesOrChannelPosts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	owner := mustUser(t, s, "+15559201002")
	sender := mustUser(t, s, "+15559201003")
	recipient := mustUser(t, s, "+15559201004")
	removedItem := storedFile(t, s, owner.ID)
	remainingItem := storedFile(t, s, owner.ID)
	_, err := s.CreateUserStickerSet(ctx, store.UserStickerSet{
		CreatorID: owner.ID, Title: "Emoji", ShortName: "emoji_removed_reference", Kind: "emojis",
	}, []store.UserStickerSetItem{
		{FileID: removedItem.ID, AccessHash: removedItem.AccessHash, Size: removedItem.Size, Emoji: "⭐", Format: "TGS", Width: 100, Height: 100},
		{FileID: remainingItem.ID, AccessHash: remainingItem.AccessHash, Size: remainingItem.Size, Emoji: "✨", Format: "TGS", Width: 100, Height: 100},
	})
	if err != nil {
		t.Fatalf("create emoji sticker set: %v", err)
	}
	channel := mustChannel(t, s, sender.ID, "emoji reference")
	entities := []store.PollDescriptionEntity{{
		Type: store.PollDescriptionEntityCustomEmoji, Offset: 0, Length: 1, Argument: removedItem.ID,
	}}
	firstMessage, _, _, duplicate, err := s.SendMessageWithEntities(ctx, sender.ID, recipient.ID, "⭐", 92000, 0, 0, entities)
	if err != nil || duplicate {
		t.Fatalf("send custom emoji message: duplicate=%t err=%v", duplicate, err)
	}
	firstPost, _, duplicate, err := s.PostChannelMessageAsWithEntities(ctx, channel.ID, sender.ID, "⭐", 92001, nil, 0, entities)
	if err != nil || duplicate {
		t.Fatalf("post custom emoji channel message: duplicate=%t err=%v", duplicate, err)
	}
	if _, err = s.RemoveUserStickerSetItem(ctx, owner.ID, removedItem.ID, removedItem.AccessHash); err != nil {
		t.Fatalf("remove emoji item: %v", err)
	}
	retriedMessage, _, _, duplicate, err := s.SendMessageWithEntities(ctx, sender.ID, recipient.ID, "⭐", 92000, 0, 0, entities)
	if err != nil || !duplicate || retriedMessage.LocalID != firstMessage.LocalID {
		t.Fatalf("retry removed emoji message = id %d duplicate=%t err=%v, want original id %d", retriedMessage.LocalID, duplicate, err, firstMessage.LocalID)
	}
	retriedPost, _, duplicate, err := s.PostChannelMessageAsWithEntities(ctx, channel.ID, sender.ID, "⭐", 92001, nil, 0, entities)
	if err != nil || !duplicate || retriedPost.LocalID != firstPost.LocalID {
		t.Fatalf("retry removed emoji channel post = id %d duplicate=%t err=%v, want original id %d", retriedPost.LocalID, duplicate, err, firstPost.LocalID)
	}
	if _, _, _, _, err = s.SendMessageWithEntities(ctx, sender.ID, recipient.ID, "⭐", 92002, 0, 0, entities); !errors.Is(err, store.ErrMediaInvalid) {
		t.Fatalf("send message referencing removed emoji = %v, want ErrMediaInvalid", err)
	}
	if _, _, _, err = s.PostChannelMessageAsWithEntities(ctx, channel.ID, sender.ID, "⭐", 92003, nil, 0, entities); !errors.Is(err, store.ErrMediaInvalid) {
		t.Fatalf("post channel message referencing removed emoji = %v, want ErrMediaInvalid", err)
	}
}

func TestMessageEditsRejectOnlyNewRemovedCustomEmojiReferences(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	owner := mustUser(t, s, "+15559201005")
	sender := mustUser(t, s, "+15559201006")
	recipient := mustUser(t, s, "+15559201007")
	priorItem := storedFile(t, s, owner.ID)
	removedItem := storedFile(t, s, owner.ID)
	remainingItem := storedFile(t, s, owner.ID)
	_, err := s.CreateUserStickerSet(ctx, store.UserStickerSet{
		CreatorID: owner.ID, Title: "Emoji", ShortName: "emoji_edit_removed_reference", Kind: "emojis",
	}, []store.UserStickerSetItem{
		{FileID: priorItem.ID, AccessHash: priorItem.AccessHash, Size: priorItem.Size, Emoji: "⭐", Format: "TGS", Width: 100, Height: 100},
		{FileID: removedItem.ID, AccessHash: removedItem.AccessHash, Size: removedItem.Size, Emoji: "✨", Format: "TGS", Width: 100, Height: 100},
		{FileID: remainingItem.ID, AccessHash: remainingItem.AccessHash, Size: remainingItem.Size, Emoji: "🌙", Format: "TGS", Width: 100, Height: 100},
	})
	if err != nil {
		t.Fatalf("create emoji sticker set: %v", err)
	}
	priorEntities := []store.PollDescriptionEntity{{
		Type: store.PollDescriptionEntityCustomEmoji, Offset: 0, Length: 1, Argument: priorItem.ID,
	}}
	directMessage, _, _, duplicate, err := s.SendMessageWithEntities(ctx, sender.ID, recipient.ID, "⭐", 92004, 0, 0, priorEntities)
	if err != nil || duplicate {
		t.Fatalf("send direct message with live emoji: duplicate=%t err=%v", duplicate, err)
	}
	chat, err := s.CreateChat(ctx, sender.ID, "emoji edit", []int64{recipient.ID})
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	chatMessage, _, _, err := s.SendChatMessage(ctx, store.FanOut{
		ChatID: chat.ID, FromID: sender.ID, Text: "⭐", RandomID: 92005, Entities: priorEntities,
	})
	if err != nil {
		t.Fatalf("send chat message with live emoji: %v", err)
	}
	if _, err = s.RemoveUserStickerSetItem(ctx, owner.ID, priorItem.ID, priorItem.AccessHash); err != nil {
		t.Fatalf("remove prior emoji item: %v", err)
	}
	if _, err = s.RemoveUserStickerSetItem(ctx, owner.ID, removedItem.ID, removedItem.AccessHash); err != nil {
		t.Fatalf("remove newly referenced emoji item: %v", err)
	}
	if _, _, err = s.EditMessageWithEntities(ctx, sender.ID, directMessage.LocalID, "⭐", priorEntities); err != nil {
		t.Errorf("retain an existing removed-set emoji in a direct message edit: %v", err)
	}
	if _, _, err = s.EditMessageWithEntities(ctx, sender.ID, chatMessage.LocalID, "⭐", priorEntities); err != nil {
		t.Errorf("retain an existing removed-set emoji in a chat message edit: %v", err)
	}
	newEntities := []store.PollDescriptionEntity{{
		Type: store.PollDescriptionEntityCustomEmoji, Offset: 0, Length: 1, Argument: removedItem.ID,
	}}
	if _, _, err = s.EditMessageWithEntities(ctx, sender.ID, directMessage.LocalID, "✨", newEntities); !errors.Is(err, store.ErrMediaInvalid) {
		t.Errorf("add removed emoji to direct message edit = %v, want ErrMediaInvalid", err)
	}
	if _, _, err = s.EditMessageWithEntities(ctx, sender.ID, chatMessage.LocalID, "✨", newEntities); !errors.Is(err, store.ErrMediaInvalid) {
		t.Errorf("add removed emoji to chat message edit = %v, want ErrMediaInvalid", err)
	}
	addedOccurrenceEntities := []store.PollDescriptionEntity{
		{Type: store.PollDescriptionEntityCustomEmoji, Offset: 0, Length: 1, Argument: priorItem.ID},
		{Type: store.PollDescriptionEntityCustomEmoji, Offset: 1, Length: 1, Argument: priorItem.ID},
	}
	if _, _, err = s.EditMessageWithEntities(ctx, sender.ID, directMessage.LocalID, "⭐⭐", addedOccurrenceEntities); !errors.Is(err, store.ErrMediaInvalid) {
		t.Errorf("add a second occurrence of removed emoji to direct edit = %v, want ErrMediaInvalid", err)
	}
	if _, _, err = s.EditMessageWithEntities(ctx, sender.ID, chatMessage.LocalID, "⭐⭐", addedOccurrenceEntities); !errors.Is(err, store.ErrMediaInvalid) {
		t.Errorf("add a second occurrence of removed emoji to chat edit = %v, want ErrMediaInvalid", err)
	}
}
