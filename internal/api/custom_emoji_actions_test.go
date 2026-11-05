package api_test

import (
	"context"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/mt"
	"github.com/gotd/td/tg"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

type customEmojiActionFixture struct {
	s          *store.Store
	blobs      blob.Store
	dsn        string
	pair       reactionPair
	set        *tg.MessagesStickerSet
	doc        *tg.Document
	regularDoc *tg.Document
}

func newCustomEmojiActionFixture(t *testing.T) customEmojiActionFixture {
	t.Helper()
	blobs := testBlobs(t)
	dsn := pgtest.DSN(t)
	s := openStickerStore(t, blobs, dsn)
	pair := seedReactionPair(t, s, "+15551348101", "+15551348102", "")
	file := storedStickerDocument(t, s, blobs, pair.a.ID, validStickerTGSSize(t, 100, 100))
	request := createStickerRequest("custom_emoji_actions", file, false, true)
	request.TextColor = true
	set := createStickerSet(t, fullChannelDispatcher(s), pair.a.ID, request)
	doc, ok := set.Documents[0].(*tg.Document)
	if !ok {
		t.Fatalf("custom emoji document = %T, want *tg.Document", set.Documents[0])
	}
	regularFile := storedStickerDocument(t, s, blobs, pair.a.ID, validStickerTGS(t))
	regularSet := createStickerSet(t, fullChannelDispatcher(s), pair.a.ID, createStickerRequest("regular_action_sticker", regularFile, false, false))
	regularDoc, ok := regularSet.Documents[0].(*tg.Document)
	if !ok {
		t.Fatalf("regular sticker document = %T, want *tg.Document", regularSet.Documents[0])
	}
	return customEmojiActionFixture{s: s, blobs: blobs, dsn: dsn, pair: pair, set: set, doc: doc, regularDoc: regularDoc}
}

func callUpdateEmojiStatus(t *testing.T, s *store.Store, userID int64, status tg.EmojiStatusClass) string {
	t.Helper()
	body := dispatchChatMethod(t, fullChannelDispatcher(s), userID, "account.updateEmojiStatus", &tg.AccountUpdateEmojiStatusRequest{EmojiStatus: status})
	var ok tg.BoolTrue
	if err := ok.Decode(&bin.Buffer{Buf: body}); err == nil {
		return ""
	}
	var rpc mt.RPCError
	if err := rpc.Decode(&bin.Buffer{Buf: body}); err != nil {
		t.Fatalf("decode account.updateEmojiStatus response: %v", err)
	}
	return rpc.ErrorMessage
}

func emojiStatusFor(t *testing.T, s *store.Store, viewerID, ownerID int64) (*tg.User, tg.EmojiStatusClass, bool) {
	t.Helper()
	users := getUsersForTest(t, s, viewerID, api.InputUser(viewerID, ownerID))
	if len(users.Elems) != 1 {
		t.Fatalf("users.getUsers returned %d users, want 1", len(users.Elems))
	}
	user, ok := users.Elems[0].(*tg.User)
	if !ok {
		t.Fatalf("users.getUsers user = %T, want *tg.User", users.Elems[0])
	}
	status, present := user.GetEmojiStatus()
	return user, status, present
}

func setCustomEmojiSetLive(t *testing.T, fixture customEmojiActionFixture, live bool) {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), fixture.dsn)
	if err != nil {
		t.Fatalf("connect to custom emoji test database: %v", err)
	}
	defer func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("close custom emoji test database connection: %v", err)
		}
	}()
	if _, err := conn.Exec(context.Background(), `UPDATE user_created_sticker_sets SET is_live = $2 WHERE id = $1`, fixture.set.Set.ID, live); err != nil {
		t.Fatalf("set custom emoji set live=%t: %v", live, err)
	}
}

func TestCustomEmojiReactionUsesLiveUserSetAndRemainsReadableAfterTakedown(t *testing.T) {
	t.Parallel()
	fixture := newCustomEmojiActionFixture(t)
	pair := fixture.pair
	peerFromA := api.InputPeerUser(pair.a.ID, pair.b.ID)
	peerFromB := api.InputPeerUser(pair.b.ID, pair.a.ID)

	if _, rpc := sendReaction(t, fixture.s, pair.a.ID, peerFromA, pair.aLocalID, &tg.ReactionCustomEmoji{DocumentID: fixture.doc.ID + 1_000_000}); rpc == nil || rpc.ErrorMessage != "REACTION_INVALID" {
		t.Fatalf("reaction with an unknown custom emoji = %v, want REACTION_INVALID", rpc)
	}
	if _, rpc := sendReaction(t, fixture.s, pair.a.ID, peerFromA, pair.aLocalID, &tg.ReactionCustomEmoji{DocumentID: fixture.regularDoc.ID}); rpc == nil || rpc.ErrorMessage != "REACTION_INVALID" {
		t.Fatalf("reaction with a regular sticker document = %v, want REACTION_INVALID", rpc)
	}
	if _, rpc := sendReaction(t, fixture.s, pair.a.ID, peerFromA, pair.aLocalID, &tg.ReactionCustomEmoji{DocumentID: fixture.doc.ID}); rpc != nil {
		t.Fatalf("send live custom emoji reaction: %s", rpc.ErrorMessage)
	}
	updates := getReactions(t, fixture.s, pair.b.ID, peerFromB, int(pair.bLocalID))
	assertCustomEmojiReaction(t, updates, fixture.doc.ID)

	// One reaction per user and message remains the limit when the reaction is custom.
	if _, rpc := sendReaction(t, fixture.s, pair.a.ID, peerFromA, pair.aLocalID, &tg.ReactionCustomEmoji{DocumentID: fixture.doc.ID}); rpc != nil {
		t.Fatalf("replace custom emoji reaction: %s", rpc.ErrorMessage)
	}
	updates = getReactions(t, fixture.s, pair.b.ID, peerFromB, int(pair.bLocalID))
	assertCustomEmojiReaction(t, updates, fixture.doc.ID)

	setCustomEmojiSetLive(t, fixture, false)
	if _, rpc := sendReaction(t, fixture.s, pair.a.ID, peerFromA, pair.aLocalID, &tg.ReactionCustomEmoji{DocumentID: fixture.doc.ID}); rpc == nil || rpc.ErrorMessage != "REACTION_INVALID" {
		t.Fatalf("reaction with a taken-down custom emoji = %v, want REACTION_INVALID", rpc)
	}
	updates = getReactions(t, fixture.s, pair.b.ID, peerFromB, int(pair.bLocalID))
	assertCustomEmojiReaction(t, updates, fixture.doc.ID)

	body := dispatchChatMethod(t, fullChannelDispatcher(fixture.s), pair.b.ID, "messages.getCustomEmojiDocuments", &tg.MessagesGetCustomEmojiDocumentsRequest{
		DocumentID: []int64{fixture.doc.ID},
	})
	var documents tg.DocumentClassVector
	if err := documents.Decode(&bin.Buffer{Buf: body}); err != nil {
		t.Fatalf("decode getCustomEmojiDocuments after takedown: %v", err)
	}
	if len(documents.Elems) != 0 {
		t.Fatalf("getCustomEmojiDocuments returned %d taken-down documents, want none", len(documents.Elems))
	}
	_, err := api.GetFileForTest(fixture.s, pair.b.ID, fixture.blobs, &tg.UploadGetFileRequest{
		Location: &tg.InputDocumentFileLocation{ID: fixture.doc.ID, AccessHash: fixture.doc.AccessHash}, Limit: 1024,
	})
	if got := rpcMessage(t, err); got != "LOCATION_INVALID" {
		t.Fatalf("download taken-down custom emoji = %q, want LOCATION_INVALID", got)
	}
}

func assertCustomEmojiReaction(t *testing.T, updates *tg.Updates, documentID int64) {
	t.Helper()
	if len(updates.Updates) != 1 {
		t.Fatalf("getMessagesReactions updates = %d, want 1", len(updates.Updates))
	}
	update, ok := updates.Updates[0].(*tg.UpdateMessageReactions)
	if !ok {
		t.Fatalf("getMessagesReactions update = %T, want *tg.UpdateMessageReactions", updates.Updates[0])
	}
	if len(update.Reactions.Results) != 1 || update.Reactions.Results[0].Count != 1 {
		t.Fatalf("custom emoji reaction results = %+v, want one reaction with count 1", update.Reactions.Results)
	}
	reaction, ok := update.Reactions.Results[0].Reaction.(*tg.ReactionCustomEmoji)
	if !ok || reaction.DocumentID != documentID {
		t.Fatalf("reaction = %#v, want custom emoji document %d", update.Reactions.Results[0].Reaction, documentID)
	}
}

func TestUpdateEmojiStatusReturnsAndClearsCustomEmojiStatus(t *testing.T) {
	t.Parallel()
	fixture := newCustomEmojiActionFixture(t)
	pair := fixture.pair

	if got := callUpdateEmojiStatus(t, fixture.s, pair.a.ID, &tg.EmojiStatus{DocumentID: fixture.doc.ID + 1_000_000}); got != "INPUT_REQUEST_INVALID" {
		t.Fatalf("update status with an unknown document = %q, want INPUT_REQUEST_INVALID", got)
	}
	if got := callUpdateEmojiStatus(t, fixture.s, pair.a.ID, &tg.EmojiStatus{DocumentID: fixture.regularDoc.ID}); got != "INPUT_REQUEST_INVALID" {
		t.Fatalf("update status with a regular sticker document = %q, want INPUT_REQUEST_INVALID", got)
	}
	if got := callUpdateEmojiStatus(t, fixture.s, pair.a.ID, &tg.EmojiStatus{DocumentID: fixture.doc.ID}); got != "" {
		t.Fatalf("update live custom emoji status = %q, want success", got)
	}
	_, status, present := emojiStatusFor(t, fixture.s, pair.b.ID, pair.a.ID)
	custom, ok := status.(*tg.EmojiStatus)
	if !present || !ok || custom.DocumentID != fixture.doc.ID {
		t.Fatalf("user emoji status = %#v present=%t, want document %d", status, present, fixture.doc.ID)
	}

	if got := callUpdateEmojiStatus(t, fixture.s, pair.a.ID, &tg.EmojiStatusEmpty{}); got != "" {
		t.Fatalf("clear emoji status = %q, want success", got)
	}
	_, status, present = emojiStatusFor(t, fixture.s, pair.b.ID, pair.a.ID)
	if present {
		if custom, ok := status.(*tg.EmojiStatus); !ok || custom.DocumentID == fixture.doc.ID {
			t.Fatalf("emoji status after clear = %#v, want no custom emoji status", status)
		}
	}

	if got := callUpdateEmojiStatus(t, fixture.s, pair.a.ID, &tg.EmojiStatus{DocumentID: fixture.doc.ID}); got != "" {
		t.Fatalf("restore custom emoji status = %q, want success", got)
	}
	setCustomEmojiSetLive(t, fixture, false)
	_, status, present = emojiStatusFor(t, fixture.s, pair.b.ID, pair.a.ID)
	custom, ok = status.(*tg.EmojiStatus)
	if !present || !ok || custom.DocumentID != fixture.doc.ID {
		t.Fatalf("emoji status after set takedown = %#v present=%t, want document %d", status, present, fixture.doc.ID)
	}
	if got := callUpdateEmojiStatus(t, fixture.s, pair.a.ID, &tg.EmojiStatus{DocumentID: fixture.doc.ID}); got != "INPUT_REQUEST_INVALID" {
		t.Fatalf("set taken-down custom emoji status = %q, want INPUT_REQUEST_INVALID", got)
	}
}

func TestBasicChatCustomEmojiReactionRequiresAllowedCustomReactions(t *testing.T) {
	t.Parallel()
	fixture := newCustomEmojiActionFixture(t)
	pair := fixture.pair
	chatID := createChatForTest(t, fixture.s, pair.a.ID, "Custom emoji reactions", pair.b.ID)
	reply, err := api.SendMessageForTest(fixture.s, pair.a.ID, &tg.MessagesSendMessageRequest{
		Peer: api.InputPeerChat(pair.a.ID, chatID), Message: "react here", RandomID: 771001,
	})
	if err != nil {
		t.Fatalf("send group message: %v", err)
	}
	messageID := sentLocalID(t, reply)
	peer := api.InputPeerChat(pair.a.ID, chatID)
	if _, rpc := sendReaction(t, fixture.s, pair.a.ID, peer, messageID, &tg.ReactionCustomEmoji{DocumentID: fixture.doc.ID}); rpc == nil || rpc.ErrorMessage != "REACTION_INVALID" {
		t.Fatalf("custom emoji reaction with default chat settings = %v, want REACTION_INVALID", rpc)
	}
	if _, rpc := setChatReactions(t, fixture.s, pair.a.ID, chatID, &tg.ChatReactionsAll{AllowCustom: true}); rpc != nil {
		t.Fatalf("allow custom chat reactions: %s", rpc.ErrorMessage)
	}
	if _, rpc := sendReaction(t, fixture.s, pair.a.ID, peer, messageID, &tg.ReactionCustomEmoji{DocumentID: fixture.doc.ID}); rpc != nil {
		t.Fatalf("send custom emoji after allowing it: %s", rpc.ErrorMessage)
	}
	if _, rpc := setChatReactions(t, fixture.s, pair.a.ID, chatID, &tg.ChatReactionsSome{Reactions: []tg.ReactionClass{&tg.ReactionEmoji{Emoticon: "❤"}}}); rpc != nil {
		t.Fatalf("set standard-only chat reactions: %s", rpc.ErrorMessage)
	}
	if _, rpc := sendReaction(t, fixture.s, pair.a.ID, peer, messageID, &tg.ReactionCustomEmoji{DocumentID: fixture.doc.ID}); rpc == nil || rpc.ErrorMessage != "REACTION_INVALID" {
		t.Fatalf("custom emoji reaction excluded by chat setting = %v, want REACTION_INVALID", rpc)
	}
	if _, rpc := setChatReactions(t, fixture.s, pair.a.ID, chatID, &tg.ChatReactionsSome{Reactions: []tg.ReactionClass{&tg.ReactionCustomEmoji{DocumentID: fixture.doc.ID}}}); rpc != nil {
		t.Fatalf("allow one custom chat reaction: %s", rpc.ErrorMessage)
	}
	if _, rpc := sendReaction(t, fixture.s, pair.a.ID, peer, messageID, &tg.ReactionCustomEmoji{DocumentID: fixture.doc.ID}); rpc != nil {
		t.Fatalf("send custom emoji allowed by explicit chat setting: %s", rpc.ErrorMessage)
	}
}

func TestChannelCustomEmojiReactionRespectsAllowedReactions(t *testing.T) {
	t.Parallel()
	fixture := newCustomEmojiActionFixture(t)
	ctx := context.Background()
	channel, err := fixture.s.CreateChannel(ctx, fixture.pair.a.ID, "Custom emoji reactions", "", true)
	if err != nil {
		t.Fatalf("create megagroup: %v", err)
	}
	joinChannelByInvite(t, fixture.s, channel, fixture.pair.b.ID)
	post, _, _, err := fixture.s.PostChannelMessage(ctx, channel.ID, fixture.pair.a.ID, "react here", 771002, nil, 0)
	if err != nil {
		t.Fatalf("post in megagroup: %v", err)
	}
	peer := api.InputPeerChannel(fixture.pair.b.ID, channel.ID)
	if _, rpc := sendReaction(t, fixture.s, fixture.pair.b.ID, peer, post.LocalID, &tg.ReactionCustomEmoji{DocumentID: fixture.doc.ID}); rpc == nil || rpc.ErrorMessage != "REACTION_INVALID" {
		t.Fatalf("custom reaction with default channel setting = %v, want REACTION_INVALID", rpc)
	}
	updates, rpc := setChannelReactions(t, fixture.s, fixture.pair.a.ID, channel.ID, &tg.ChatReactionsAll{AllowCustom: true})
	requireReactionUpdates(t, updates, rpc, "allow custom channel reactions")
	if _, rpc := sendReaction(t, fixture.s, fixture.pair.b.ID, peer, post.LocalID, &tg.ReactionCustomEmoji{DocumentID: fixture.doc.ID}); rpc != nil {
		t.Fatalf("send allowed custom channel reaction: %s", rpc.ErrorMessage)
	}
	assertCustomEmojiReaction(t, getReactions(t, fixture.s, fixture.pair.a.ID, api.InputPeerChannel(fixture.pair.a.ID, channel.ID), int(post.LocalID)), fixture.doc.ID)

	standardOnly := &tg.ChatReactionsSome{Reactions: []tg.ReactionClass{&tg.ReactionEmoji{Emoticon: "❤"}}}
	updates, rpc = setChannelReactions(t, fixture.s, fixture.pair.a.ID, channel.ID, standardOnly)
	requireReactionUpdates(t, updates, rpc, "exclude custom channel reactions")
	if _, rpc := sendReaction(t, fixture.s, fixture.pair.b.ID, peer, post.LocalID, &tg.ReactionCustomEmoji{DocumentID: fixture.doc.ID}); rpc == nil || rpc.ErrorMessage != "REACTION_INVALID" {
		t.Fatalf("custom reaction excluded by channel setting = %v, want REACTION_INVALID", rpc)
	}

	customOnly := &tg.ChatReactionsSome{Reactions: []tg.ReactionClass{&tg.ReactionCustomEmoji{DocumentID: fixture.doc.ID}}}
	updates, rpc = setChannelReactions(t, fixture.s, fixture.pair.a.ID, channel.ID, customOnly)
	requireReactionUpdates(t, updates, rpc, "allow custom reaction in channel subset")
	if _, rpc := sendReaction(t, fixture.s, fixture.pair.b.ID, peer, post.LocalID, &tg.ReactionCustomEmoji{DocumentID: fixture.doc.ID}); rpc != nil {
		t.Fatalf("send custom channel reaction in allowed subset: %s", rpc.ErrorMessage)
	}
}
