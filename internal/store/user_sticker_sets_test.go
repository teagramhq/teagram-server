package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/teagramhq/teagram-server/internal/store"
)

func createStickerSetWithTestFingerprints(
	t *testing.T,
	ctx context.Context,
	s *store.Store,
	set store.UserStickerSet,
	items []store.UserStickerSetItem,
	files ...store.File,
) (store.UserStickerSet, error) {
	t.Helper()
	if len(items) != len(files) {
		t.Fatalf("sticker item count = %d, file count = %d", len(items), len(files))
	}
	fingerprints := make([]store.StickerFileFingerprint, 0, len(files))
	for _, file := range files {
		_, fingerprint, err := s.ReadStickerFileForSetWithFingerprint(ctx, file)
		if err != nil {
			t.Fatalf("read sticker file for set: %v", err)
		}
		fingerprints = append(fingerprints, fingerprint)
	}
	return s.CreateUserStickerSet(ctx, set, items, fingerprints)
}

func createPollCustomEmojiSet(
	t *testing.T,
	ctx context.Context,
	s *store.Store,
	owner store.User,
	title, shortName string,
	first, second store.File,
) store.UserStickerSet {
	t.Helper()
	set, err := createStickerSetWithTestFingerprints(t, ctx, s, store.UserStickerSet{
		CreatorID: owner.ID, Title: title, ShortName: shortName, Kind: "emojis",
	}, []store.UserStickerSetItem{
		{FileID: first.ID, AccessHash: first.AccessHash, Size: first.Size, Emoji: "⭐", Format: "TGS", Width: 100, Height: 100},
		{FileID: second.ID, AccessHash: second.AccessHash, Size: second.Size, Emoji: "✨", Format: "TGS", Width: 100, Height: 100},
	}, first, second)
	if err != nil {
		t.Fatalf("create poll custom emoji set: %v", err)
	}
	return set
}

func pollDraftWithCustomEmojiIDs(fileIDs ...int64) store.PollDraft {
	draft := ordinaryPollDraft()
	draft.DescriptionEntities = make([]store.PollDescriptionEntity, len(fileIDs))
	for i, fileID := range fileIDs {
		draft.DescriptionEntities[i] = store.PollDescriptionEntity{
			Type: store.PollDescriptionEntityCustomEmoji, Offset: i, Length: 1, Argument: fileID,
		}
	}
	return draft
}

func assertPollCustomEmojiReferences(t *testing.T, s *store.Store, ownerID int64, fileIDs []int64) {
	t.Helper()
	got, err := s.UserStickerSetMessageReferencedFileIDs(context.Background(), ownerID, fileIDs)
	if err != nil {
		t.Fatalf("resolve poll custom emoji references for %d: %v", ownerID, err)
	}
	if len(got) != len(fileIDs) {
		t.Errorf("poll custom emoji references for %d = %v, want %v", ownerID, got, fileIDs)
	}
	for _, fileID := range fileIDs {
		if _, ok := got[fileID]; !ok {
			t.Errorf("poll custom emoji document %d is not visible to %d", fileID, ownerID)
		}
	}
}

func assertPollCustomEmojiDownloads(t *testing.T, s *store.Store, ownerID int64, files []store.File, wantDownload bool) {
	t.Helper()
	for _, file := range files {
		_, err := s.FileForDownload(context.Background(), file.ID, file.AccessHash, ownerID)
		if wantDownload && err != nil {
			t.Errorf("download poll custom emoji %d as %d: %v", file.ID, ownerID, err)
		}
		if !wantDownload && !errors.Is(err, store.ErrFileNotFound) {
			t.Errorf("download poll custom emoji %d as %d = %v, want ErrFileNotFound", file.ID, ownerID, err)
		}
	}
}

func TestPollDescriptionCustomEmojiFollowsPrivateMessageVisibility(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	owner := mustUser(t, s, "+15559202001")
	sender := mustUser(t, s, "+15559202002")
	recipient := mustUser(t, s, "+15559202003")
	stranger := mustUser(t, s, "+15559202008")

	removedItem := stickerSetFileWithBytes(t, s, owner, []byte("hello world"))
	removedKeep := stickerSetFileWithBytes(t, s, owner, []byte("hello world"))
	deletedItem := stickerSetFileWithBytes(t, s, owner, []byte("hello world"))
	deletedKeep := stickerSetFileWithBytes(t, s, owner, []byte("hello world"))
	createPollCustomEmojiSet(t, ctx, s, owner, "Removed", "poll_private_removed", removedItem, removedKeep)
	deletedSet := createPollCustomEmojiSet(t, ctx, s, owner, "Deleted", "poll_private_deleted", deletedItem, deletedKeep)

	fileIDs := []int64{removedItem.ID, deletedItem.ID}
	files := []store.File{removedItem, deletedItem}
	senderMessage, _, _, duplicate, err := s.SendUserPollMessage(
		ctx, sender.ID, recipient.ID, 92201, "poll", pollDraftWithCustomEmojiIDs(fileIDs...),
	)
	if err != nil || duplicate {
		t.Fatalf("send private poll: duplicate=%t err=%v", duplicate, err)
	}
	if _, err = s.RemoveUserStickerSetItem(ctx, owner.ID, removedItem.ID, removedItem.AccessHash); err != nil {
		t.Fatalf("remove poll emoji item: %v", err)
	}
	if err = s.DeleteUserStickerSet(ctx, owner.ID, deletedSet.ID); err != nil {
		t.Fatalf("delete poll emoji set: %v", err)
	}
	for _, viewerID := range []int64{sender.ID, recipient.ID} {
		assertPollCustomEmojiReferences(t, s, viewerID, fileIDs)
		assertPollCustomEmojiDownloads(t, s, viewerID, files, true)
	}
	assertNoPollCustomEmojiReferences(t, s, stranger.ID, fileIDs)
	assertPollCustomEmojiDownloads(t, s, stranger.ID, files, false)

	if _, err = s.DeleteMessages(ctx, sender.ID, []int64{senderMessage.LocalID}, true); err != nil {
		t.Fatalf("revoke private poll: %v", err)
	}
	for _, viewerID := range []int64{sender.ID, recipient.ID} {
		assertNoPollCustomEmojiReferences(t, s, viewerID, fileIDs)
		assertPollCustomEmojiDownloads(t, s, viewerID, files, false)
	}
}

func TestPollDescriptionCustomEmojiFollowsChannelVisibility(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	owner := mustUser(t, s, "+15559202004")
	sender := mustUser(t, s, "+15559202005")
	member := mustUser(t, s, "+15559202006")
	outsider := mustUser(t, s, "+15559202007")

	removedItem := stickerSetFileWithBytes(t, s, owner, []byte("hello world"))
	removedKeep := stickerSetFileWithBytes(t, s, owner, []byte("hello world"))
	deletedItem := stickerSetFileWithBytes(t, s, owner, []byte("hello world"))
	deletedKeep := stickerSetFileWithBytes(t, s, owner, []byte("hello world"))
	createPollCustomEmojiSet(t, ctx, s, owner, "Removed", "poll_channel_removed", removedItem, removedKeep)
	deletedSet := createPollCustomEmojiSet(t, ctx, s, owner, "Deleted", "poll_channel_deleted", deletedItem, deletedKeep)

	fileIDs := []int64{removedItem.ID, deletedItem.ID}
	files := []store.File{removedItem, deletedItem}
	channel, _, err := store.SeedChannelWithMember(ctx, s, sender.ID, member.ID)
	if err != nil {
		t.Fatalf("seed poll channel: %v", err)
	}
	post, _, _, duplicate, err := s.PostChannelPollAs(
		ctx, channel.ID, sender.ID, 92202, "poll", pollDraftWithCustomEmojiIDs(fileIDs...),
	)
	if err != nil || duplicate {
		t.Fatalf("post channel poll: duplicate=%t err=%v", duplicate, err)
	}
	if _, err = s.RemoveUserStickerSetItem(ctx, owner.ID, removedItem.ID, removedItem.AccessHash); err != nil {
		t.Fatalf("remove poll emoji item: %v", err)
	}
	if err = s.DeleteUserStickerSet(ctx, owner.ID, deletedSet.ID); err != nil {
		t.Fatalf("delete poll emoji set: %v", err)
	}
	for _, viewerID := range []int64{sender.ID, member.ID} {
		assertPollCustomEmojiReferences(t, s, viewerID, fileIDs)
		assertPollCustomEmojiDownloads(t, s, viewerID, files, true)
	}
	assertNoPollCustomEmojiReferences(t, s, outsider.ID, fileIDs)
	assertPollCustomEmojiDownloads(t, s, outsider.ID, files, false)

	left, err := s.LeaveChannel(ctx, channel.ID, member.ID)
	if err != nil || !left {
		t.Fatalf("member leave channel: left=%t err=%v", left, err)
	}
	assertNoPollCustomEmojiReferences(t, s, member.ID, fileIDs)
	assertPollCustomEmojiDownloads(t, s, member.ID, files, false)
	assertPollCustomEmojiReferences(t, s, sender.ID, fileIDs)
	assertPollCustomEmojiDownloads(t, s, sender.ID, files, true)

	if _, _, err = s.DeleteChannelMessages(ctx, channel.ID, sender.ID, []int64{post.LocalID}); err != nil {
		t.Fatalf("revoke channel poll: %v", err)
	}
	assertNoPollCustomEmojiReferences(t, s, sender.ID, fileIDs)
	assertPollCustomEmojiDownloads(t, s, sender.ID, files, false)
}

func assertNoPollCustomEmojiReferences(t *testing.T, s *store.Store, ownerID int64, fileIDs []int64) {
	t.Helper()
	got, err := s.UserStickerSetMessageReferencedFileIDs(context.Background(), ownerID, fileIDs)
	if err != nil {
		t.Fatalf("resolve revoked poll custom emoji references for %d: %v", ownerID, err)
	}
	if len(got) != 0 {
		t.Errorf("revoked poll custom emoji references for %d = %v, want none", ownerID, got)
	}
}

func TestRemoveStickerSetThumbnailItemClearsReferenceAndChangesHash(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	owner := mustUser(t, s, "+15559201001")
	thumbnailItem := stickerSetFileWithBytes(t, s, owner, []byte("hello world"))
	remainingItem := stickerSetFileWithBytes(t, s, owner, []byte("hello world"))

	items := []store.UserStickerSetItem{
		{FileID: thumbnailItem.ID, AccessHash: thumbnailItem.AccessHash, Size: thumbnailItem.Size, Emoji: "⭐", Format: "TGS", Width: 100, Height: 100},
		{FileID: remainingItem.ID, AccessHash: remainingItem.AccessHash, Size: remainingItem.Size, Emoji: "✨", Format: "TGS", Width: 100, Height: 100},
	}
	set, err := createStickerSetWithTestFingerprints(t, ctx, s, store.UserStickerSet{
		CreatorID: owner.ID, Title: "Emoji", ShortName: "emoji_thumb", Kind: "emojis",
	}, items, thumbnailItem, remainingItem)
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
	removedItem := stickerSetFileWithBytes(t, s, owner, []byte("hello world"))
	remainingItem := stickerSetFileWithBytes(t, s, owner, []byte("hello world"))
	items := []store.UserStickerSetItem{
		{FileID: removedItem.ID, AccessHash: removedItem.AccessHash, Size: removedItem.Size, Emoji: "⭐", Format: "TGS", Width: 100, Height: 100},
		{FileID: remainingItem.ID, AccessHash: remainingItem.AccessHash, Size: remainingItem.Size, Emoji: "✨", Format: "TGS", Width: 100, Height: 100},
	}
	_, err := createStickerSetWithTestFingerprints(t, ctx, s, store.UserStickerSet{
		CreatorID: owner.ID, Title: "Emoji", ShortName: "emoji_removed_reference", Kind: "emojis",
	}, items, removedItem, remainingItem)
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

// A stored custom emoji entity keeps its document downloadable for the caller
// who can see the message, after the item is removed or its set is deleted.
// The sticker_document_id and file_id download branches never see these
// references, so FileForDownload must find them in the entities themselves.
func TestCustomEmojiEntityKeepsRemovedDocumentDownloadable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	owner := mustUser(t, s, "+15559201011")
	sender := mustUser(t, s, "+15559201012")
	recipient := mustUser(t, s, "+15559201013")
	stranger := mustUser(t, s, "+15559201014")
	removedItem := stickerSetFileWithBytes(t, s, owner, []byte("hello world"))
	removedKept := stickerSetFileWithBytes(t, s, owner, []byte("hello world"))
	deletedItem := stickerSetFileWithBytes(t, s, owner, []byte("hello world"))
	deletedKept := stickerSetFileWithBytes(t, s, owner, []byte("hello world"))
	emojiItem := func(f store.File) store.UserStickerSetItem {
		return store.UserStickerSetItem{FileID: f.ID, AccessHash: f.AccessHash, Size: f.Size, Emoji: "⭐", Format: "TGS", Width: 100, Height: 100}
	}
	removedItems := []store.UserStickerSetItem{emojiItem(removedItem), emojiItem(removedKept)}
	_, err := createStickerSetWithTestFingerprints(t, ctx, s, store.UserStickerSet{
		CreatorID: owner.ID, Title: "Removed", ShortName: "emoji_entity_removed", Kind: "emojis",
	}, removedItems, removedItem, removedKept)
	if err != nil {
		t.Fatalf("create removed emoji set: %v", err)
	}
	deletedItems := []store.UserStickerSetItem{emojiItem(deletedItem), emojiItem(deletedKept)}
	deletedSet, err := createStickerSetWithTestFingerprints(t, ctx, s, store.UserStickerSet{
		CreatorID: owner.ID, Title: "Deleted", ShortName: "emoji_entity_deleted", Kind: "emojis",
	}, deletedItems, deletedItem, deletedKept)
	if err != nil {
		t.Fatalf("create deleted emoji set: %v", err)
	}
	channel := mustChannel(t, s, sender.ID, "emoji entity download")
	removedEntities := []store.PollDescriptionEntity{{
		Type: store.PollDescriptionEntityCustomEmoji, Offset: 0, Length: 1, Argument: removedItem.ID,
	}}
	deletedEntities := []store.PollDescriptionEntity{{
		Type: store.PollDescriptionEntityCustomEmoji, Offset: 0, Length: 1, Argument: deletedItem.ID,
	}}
	sentRemoved, _, _, duplicate, err := s.SendMessageWithEntities(ctx, sender.ID, recipient.ID, "⭐", 92100, 0, 0, removedEntities)
	if err != nil || duplicate {
		t.Fatalf("send removed emoji message: duplicate=%t err=%v", duplicate, err)
	}
	if _, _, _, duplicate, err = s.SendMessageWithEntities(ctx, sender.ID, recipient.ID, "⭐", 92101, 0, 0, deletedEntities); err != nil || duplicate {
		t.Fatalf("send deleted emoji message: duplicate=%t err=%v", duplicate, err)
	}
	if _, _, duplicate, err = s.PostChannelMessageAsWithEntities(ctx, channel.ID, sender.ID, "⭐", 92102, nil, 0, removedEntities); err != nil || duplicate {
		t.Fatalf("post removed emoji channel message: duplicate=%t err=%v", duplicate, err)
	}
	if _, err = s.RemoveUserStickerSetItem(ctx, owner.ID, removedItem.ID, removedItem.AccessHash); err != nil {
		t.Fatalf("remove emoji item: %v", err)
	}
	if err = s.DeleteUserStickerSet(ctx, owner.ID, deletedSet.ID); err != nil {
		t.Fatalf("delete emoji set: %v", err)
	}

	for _, f := range []store.File{removedItem, deletedItem} {
		if _, err := s.FileForDownload(ctx, f.ID, f.AccessHash, recipient.ID); err != nil {
			t.Fatalf("recipient download of file %d: %v", f.ID, err)
		}
		if _, err := s.FileForDownload(ctx, f.ID, f.AccessHash, sender.ID); err != nil {
			t.Fatalf("sender download of file %d: %v", f.ID, err)
		}
		if _, err := s.FileForDownload(ctx, f.ID, f.AccessHash, stranger.ID); !errors.Is(err, store.ErrFileNotFound) {
			t.Fatalf("stranger download of file %d: want ErrFileNotFound, got %v", f.ID, err)
		}
	}

	// Revoking the private message leaves the channel post as the only route,
	// so the sender keeps the document through channel visibility alone.
	if _, err = s.DeleteMessages(ctx, sender.ID, []int64{sentRemoved.LocalID}, true); err != nil {
		t.Fatalf("delete private emoji message: %v", err)
	}
	if _, err := s.FileForDownload(ctx, removedItem.ID, removedItem.AccessHash, recipient.ID); !errors.Is(err, store.ErrFileNotFound) {
		t.Fatalf("recipient after revoke: want ErrFileNotFound, got %v", err)
	}
	if _, err := s.FileForDownload(ctx, removedItem.ID, removedItem.AccessHash, sender.ID); err != nil {
		t.Fatalf("sender download through channel post: %v", err)
	}
}

func TestMessageEditsRejectOnlyNewRemovedCustomEmojiReferences(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	owner := mustUser(t, s, "+15559201005")
	sender := mustUser(t, s, "+15559201006")
	recipient := mustUser(t, s, "+15559201007")
	priorItem := stickerSetFileWithBytes(t, s, owner, []byte("hello world"))
	removedItem := stickerSetFileWithBytes(t, s, owner, []byte("hello world"))
	remainingItem := stickerSetFileWithBytes(t, s, owner, []byte("hello world"))
	items := []store.UserStickerSetItem{
		{FileID: priorItem.ID, AccessHash: priorItem.AccessHash, Size: priorItem.Size, Emoji: "⭐", Format: "TGS", Width: 100, Height: 100},
		{FileID: removedItem.ID, AccessHash: removedItem.AccessHash, Size: removedItem.Size, Emoji: "✨", Format: "TGS", Width: 100, Height: 100},
		{FileID: remainingItem.ID, AccessHash: remainingItem.AccessHash, Size: remainingItem.Size, Emoji: "🌙", Format: "TGS", Width: 100, Height: 100},
	}
	_, err := createStickerSetWithTestFingerprints(t, ctx, s, store.UserStickerSet{
		CreatorID: owner.ID, Title: "Emoji", ShortName: "emoji_edit_removed_reference", Kind: "emojis",
	}, items, priorItem, removedItem, remainingItem)
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
