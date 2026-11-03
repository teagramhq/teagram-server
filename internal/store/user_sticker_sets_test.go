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
	if _, err = s.RemoveUserStickerSetItem(ctx, owner.ID, removedItem.ID, removedItem.AccessHash); err != nil {
		t.Fatalf("remove emoji item: %v", err)
	}
	entities := []store.PollDescriptionEntity{{
		Type: store.PollDescriptionEntityCustomEmoji, Offset: 0, Length: 1, Argument: removedItem.ID,
	}}
	if _, _, _, _, err = s.SendMessageWithEntities(ctx, sender.ID, recipient.ID, "⭐", 92002, 0, 0, entities); !errors.Is(err, store.ErrMediaInvalid) {
		t.Fatalf("send message referencing removed emoji = %v, want ErrMediaInvalid", err)
	}

	channel := mustChannel(t, s, sender.ID, "emoji reference")
	if _, _, _, err = s.PostChannelMessageAsWithEntities(ctx, channel.ID, sender.ID, "⭐", 92003, nil, 0, entities); !errors.Is(err, store.ErrMediaInvalid) {
		t.Fatalf("post channel message referencing removed emoji = %v, want ErrMediaInvalid", err)
	}
}
