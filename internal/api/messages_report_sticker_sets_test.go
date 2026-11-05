package api_test

import (
	"context"
	"strings"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/mt"
	"github.com/gotd/td/tg"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestMessagesReportStartsOptionFlowForReadableMessage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	reporter, err := s.CreateUser(ctx, "+15559420001")
	if err != nil {
		t.Fatalf("create reporter: %v", err)
	}
	peer, err := s.CreateUser(ctx, "+15559420002")
	if err != nil {
		t.Fatalf("create peer: %v", err)
	}
	message, _, _, duplicate, err := s.SendMessage(ctx, reporter.ID, peer.ID, "ordinary message", 9420001, 0, 0)
	if err != nil {
		t.Fatalf("send test message: %v", err)
	}
	if duplicate {
		t.Fatal("send test message unexpectedly deduplicated")
	}

	request := &tg.MessagesReportRequest{
		Peer: api.InputPeerUser(reporter.ID, peer.ID),
		ID:   []int{int(message.LocalID)},
	}
	h := fullChannelDispatcher(s)
	choose, rpc := messagesReportCall(t, h, reporter.ID, request)
	if rpc != nil {
		t.Fatalf("messages.report returned RPC error %s, want an option menu", rpc.ErrorMessage)
	}
	menu, ok := choose.(*tg.ReportResultChooseOption)
	if !ok {
		t.Fatalf("messages.report result = %T, want option menu", choose)
	}
	var copyrightOption []byte
	for _, option := range menu.Options {
		if option.Text == "Copyright" {
			copyrightOption = option.Option
			break
		}
	}
	if len(copyrightOption) == 0 {
		t.Fatalf("report options = %+v, want Copyright", menu.Options)
	}

	request.Option = copyrightOption
	commentStep, rpc := messagesReportCall(t, h, reporter.ID, request)
	if rpc != nil {
		t.Fatalf("choose report option: %s", rpc.ErrorMessage)
	}
	addComment, ok := commentStep.(*tg.ReportResultAddComment)
	if !ok || !addComment.Optional {
		t.Fatalf("chosen option result = %T/%+v, want optional comment step", commentStep, commentStep)
	}

	request.Option = addComment.Option
	request.Message = "ordinary message report"
	finished, rpc := messagesReportCall(t, h, reporter.ID, request)
	if rpc != nil {
		t.Fatalf("complete report flow for ordinary message: %s", rpc.ErrorMessage)
	}
	if _, ok := finished.(*tg.ReportResultReported); !ok {
		t.Fatalf("completed ordinary-message report = %T, want reported", finished)
	}
	conn := reportDatabase(t, ctx, dsn)
	var reportCount int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM user_sticker_set_reports`).Scan(&reportCount); err != nil {
		t.Fatalf("count reports for an ordinary message: %v", err)
	}
	if reportCount != 0 {
		t.Fatalf("ordinary-message reports = %d, want none", reportCount)
	}
}

func TestMessagesReportDeduplicatesIdenticalAdminReport(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	blobs := testBlobs(t)
	dsn := pgtest.DSN(t)
	s := openStickerStore(t, blobs, dsn)
	owner, err := s.CreateUser(ctx, "+15559420011")
	if err != nil {
		t.Fatalf("create set owner: %v", err)
	}
	reporter, err := s.CreateUser(ctx, "+15559420012")
	if err != nil {
		t.Fatalf("create reporter: %v", err)
	}
	file := storedStickerDocument(t, s, blobs, owner.ID, validStickerTGS(t))
	h := fullChannelDispatcher(s)
	set := createStickerSet(t, h, owner.ID, createStickerRequest("report_cats", file, false, false))
	if _, _, _, _, err := s.SendStickerMessageWithEntities(ctx, owner.ID, reporter.ID, "cat", 9420011, file.ID, 0, nil); err != nil {
		t.Fatalf("send user sticker: %v", err)
	}
	history, err := s.History(ctx, reporter.ID, store.PeerTypeUser, owner.ID, 0, 10)
	if err != nil || len(history) != 1 {
		t.Fatalf("reporter sticker history = %d messages, err %v; want one", len(history), err)
	}
	if history[0].StickerDocumentID == nil || *history[0].StickerDocumentID != file.ID {
		t.Fatalf("reporter sticker document id = %v, want %d", history[0].StickerDocumentID, file.ID)
	}
	request := &tg.MessagesReportRequest{
		Peer: api.InputPeerUser(reporter.ID, owner.ID),
		ID:   []int{int(history[0].LocalID)},
	}
	for range 2 {
		completeMessageReport(t, h, reporter.ID, request, "copied artwork <script>")
	}

	var gotReporterID, gotSetID, gotPeerID, gotMessageID int64
	var gotPeerType int16
	var reason, comment, status string
	conn := reportDatabase(t, ctx, dsn)
	if err := conn.QueryRow(ctx, `
		SELECT reporter_id, set_id, message_peer_type, message_peer_id, message_id, reason, comment, status
		FROM user_sticker_set_reports
	`).Scan(&gotReporterID, &gotSetID, &gotPeerType, &gotPeerID, &gotMessageID, &reason, &comment, &status); err != nil {
		t.Fatalf("read admin sticker-set report: %v", err)
	}
	if gotReporterID != reporter.ID || gotSetID != set.Set.ID || gotPeerType != int16(store.PeerTypeUser) ||
		gotPeerID != owner.ID || gotMessageID != history[0].LocalID || reason != "copyright" ||
		comment != "copied artwork <script>" || status != "open" {
		t.Fatalf("stored report = reporter %d, set %d, peer %d/%d, message %d, reason %q, comment %q, status %q",
			gotReporterID, gotSetID, gotPeerType, gotPeerID, gotMessageID, reason, comment, status)
	}
	var reportCount int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM user_sticker_set_reports`).Scan(&reportCount); err != nil {
		t.Fatalf("count duplicate reports: %v", err)
	}
	if reportCount != 1 {
		t.Fatalf("duplicate reports = %d, want one", reportCount)
	}
}

func TestMessagesReportProcessesMultipleSelectedMessages(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	blobs := testBlobs(t)
	dsn := pgtest.DSN(t)
	s := openStickerStore(t, blobs, dsn)
	owner, err := s.CreateUser(ctx, "+15559420071")
	if err != nil {
		t.Fatalf("create set owner: %v", err)
	}
	reporter, err := s.CreateUser(ctx, "+15559420072")
	if err != nil {
		t.Fatalf("create reporter: %v", err)
	}
	file := storedStickerDocument(t, s, blobs, owner.ID, validStickerTGS(t))
	h := fullChannelDispatcher(s)
	set := createStickerSet(t, h, owner.ID, createStickerRequest("report_selected_messages", file, false, false))
	if _, _, _, duplicate, err := s.SendMessage(ctx, reporter.ID, owner.ID, "ordinary selected message", 9420071, 0, 0); err != nil || duplicate {
		t.Fatalf("send ordinary selected message: duplicate %t, err %v", duplicate, err)
	}
	for i := range 2 {
		if _, _, _, _, err := s.SendStickerMessageWithEntities(ctx, owner.ID, reporter.ID, "cat", int64(9420072+i), file.ID, 0, nil); err != nil {
			t.Fatalf("send selected sticker message %d: %v", i, err)
		}
	}
	history, err := s.History(ctx, reporter.ID, store.PeerTypeUser, owner.ID, 0, 10)
	if err != nil || len(history) != 3 {
		t.Fatalf("selected-message history = %d messages, err %v; want ordinary message plus two stickers", len(history), err)
	}
	messageIDs := make([]int, 0, len(history))
	wantReportMessageIDs := make(map[int64]struct{}, 2)
	for _, message := range history {
		messageIDs = append(messageIDs, int(message.LocalID))
		if message.StickerDocumentID != nil {
			wantReportMessageIDs[message.LocalID] = struct{}{}
		}
	}
	if len(wantReportMessageIDs) != 2 {
		t.Fatalf("selected sticker messages = %d, want two", len(wantReportMessageIDs))
	}

	request := &tg.MessagesReportRequest{
		Peer: api.InputPeerUser(reporter.ID, owner.ID),
		ID:   messageIDs,
	}
	completeMessageReport(t, h, reporter.ID, request, "selected messages report")

	rows, err := reportDatabase(t, ctx, dsn).Query(ctx, `SELECT message_id FROM user_sticker_set_reports WHERE set_id = $1`, set.Set.ID)
	if err != nil {
		t.Fatalf("query reports for selected messages: %v", err)
	}
	defer rows.Close()
	got := make(map[int64]struct{}, len(wantReportMessageIDs))
	for rows.Next() {
		var messageID int64
		if err := rows.Scan(&messageID); err != nil {
			t.Fatalf("scan selected-message report: %v", err)
		}
		got[messageID] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read selected-message reports: %v", err)
	}
	if len(got) != len(wantReportMessageIDs) {
		t.Fatalf("reports for selected stickers = %v, want both selected sticker messages %v", got, wantReportMessageIDs)
	}
	for messageID := range wantReportMessageIDs {
		if _, ok := got[messageID]; !ok {
			t.Errorf("selected sticker message %d has no report", messageID)
		}
	}
}

func TestMessagesReportRejectsMessageOutsideReportersMessageSpace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	reporter, err := s.CreateUser(ctx, "+15559420021")
	if err != nil {
		t.Fatalf("create reporter: %v", err)
	}
	owner, err := s.CreateUser(ctx, "+15559420022")
	if err != nil {
		t.Fatalf("create message owner: %v", err)
	}
	peer, err := s.CreateUser(ctx, "+15559420023")
	if err != nil {
		t.Fatalf("create message peer: %v", err)
	}
	message, _, _, duplicate, err := s.SendMessage(ctx, owner.ID, peer.ID, "private message", 9420021, 0, 0)
	if err != nil {
		t.Fatalf("send private message: %v", err)
	}
	if duplicate {
		t.Fatal("send private message unexpectedly deduplicated")
	}
	request := &tg.MessagesReportRequest{
		Peer: api.InputPeerUser(reporter.ID, peer.ID),
		ID:   []int{int(message.LocalID)},
	}
	_, rpc := messagesReportCall(t, fullChannelDispatcher(s), reporter.ID, request)
	if rpc == nil || rpc.ErrorMessage != "MESSAGE_ID_INVALID" {
		if rpc == nil {
			t.Fatal("report for a message outside the reporter's message space succeeded")
		}
		t.Fatalf("unowned message report = %q, want MESSAGE_ID_INVALID", rpc.ErrorMessage)
	}
	var count int
	if err := reportDatabase(t, ctx, dsn).QueryRow(ctx, `SELECT count(*) FROM user_sticker_set_reports`).Scan(&count); err != nil {
		t.Fatalf("count reports after unowned message: %v", err)
	}
	if count != 0 {
		t.Fatalf("reports after unowned message = %d, want none", count)
	}
}

func TestMessagesReportResolvesUserCustomEmojiFromMessageEntities(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	blobs := testBlobs(t)
	dsn := pgtest.DSN(t)
	s := openStickerStore(t, blobs, dsn)
	owner, err := s.CreateUser(ctx, "+15559420031")
	if err != nil {
		t.Fatalf("create emoji-set owner: %v", err)
	}
	reporter, err := s.CreateUser(ctx, "+15559420032")
	if err != nil {
		t.Fatalf("create reporter: %v", err)
	}
	file := storedStickerDocument(t, s, blobs, owner.ID, validStickerTGS(t))
	h := fullChannelDispatcher(s)
	_, fingerprint, err := s.ReadStickerFileForSetWithFingerprint(ctx, file)
	if err != nil {
		t.Fatalf("fingerprint custom emoji file: %v", err)
	}
	set, err := s.CreateUserStickerSet(ctx, store.UserStickerSet{
		CreatorID: owner.ID, Title: "Report custom emoji", ShortName: "report_custom_emoji", Kind: "emojis",
	}, []store.UserStickerSetItem{{
		FileID: file.ID, AccessHash: file.AccessHash, Size: file.Size,
		Emoji: "⭐", Format: "TGS", Width: 100, Height: 100,
	}}, []store.StickerFileFingerprint{fingerprint})
	if err != nil {
		t.Fatalf("create custom emoji set: %v", err)
	}
	entities := []store.PollDescriptionEntity{{
		Type: store.PollDescriptionEntityCustomEmoji, Offset: 0, Length: 1, Argument: file.ID,
	}}
	if _, _, _, _, err := s.SendMessageWithEntities(ctx, owner.ID, reporter.ID, "x", 9420031, 0, 0, entities); err != nil {
		t.Fatalf("send custom emoji message: %v", err)
	}
	history, err := s.History(ctx, reporter.ID, store.PeerTypeUser, owner.ID, 0, 10)
	if err != nil || len(history) != 1 {
		t.Fatalf("custom emoji history = %d messages, err %v; want one", len(history), err)
	}
	request := &tg.MessagesReportRequest{
		Peer: api.InputPeerUser(reporter.ID, owner.ID),
		ID:   []int{int(history[0].LocalID)},
	}
	completeMessageReport(t, h, reporter.ID, request, "custom emoji report")
	var gotSetID int64
	if err := reportDatabase(t, ctx, dsn).QueryRow(ctx, `SELECT set_id FROM user_sticker_set_reports`).Scan(&gotSetID); err != nil {
		t.Fatalf("read custom emoji report: %v", err)
	}
	if gotSetID != set.ID {
		t.Fatalf("custom emoji report set = %d, want %d", gotSetID, set.ID)
	}
}

func TestMessagesReportAcceptsReadableChannelStickerWithCustomEmojiEntity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	blobs := testBlobs(t)
	dsn := pgtest.DSN(t)
	s := openStickerStore(t, blobs, dsn)
	owner, err := s.CreateUser(ctx, "+15559420041")
	if err != nil {
		t.Fatalf("create set owner: %v", err)
	}
	reporter, err := s.CreateUser(ctx, "+15559420042")
	if err != nil {
		t.Fatalf("create reporter: %v", err)
	}
	file := storedStickerDocument(t, s, blobs, owner.ID, validStickerTGSSize(t, 100, 100))
	h := fullChannelDispatcher(s)
	setRequest := createStickerRequest("report_channel_sticker", file, false, true)
	setRequest.TextColor = true
	set := createStickerSet(t, h, owner.ID, setRequest)
	channel, err := s.CreateChannel(ctx, owner.ID, "Reportable stickers", "", true)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	if _, err := s.AddChannelMembers(ctx, channel.ID, owner.ID, []int64{reporter.ID}); err != nil {
		t.Fatalf("add reporter to channel: %v", err)
	}
	entities := []store.PollDescriptionEntity{{
		Type: store.PollDescriptionEntityCustomEmoji, Offset: 0, Length: 1, Argument: file.ID,
	}}
	post, _, _, err := s.PostChannelStickerAsWithEntities(ctx, channel.ID, owner.ID, "cat", 9420041, file.ID, 0, entities)
	if err != nil {
		t.Fatalf("post user sticker with custom emoji entity in channel: %v", err)
	}
	request := &tg.MessagesReportRequest{
		Peer: api.InputPeerChannel(reporter.ID, channel.ID),
		ID:   []int{int(post.LocalID)},
	}
	completeMessageReport(t, h, reporter.ID, request, "channel sticker report")
	var gotSetID, gotPeerID int64
	var gotPeerType int16
	if err := reportDatabase(t, ctx, dsn).QueryRow(ctx, `
		SELECT set_id, message_peer_type, message_peer_id FROM user_sticker_set_reports
	`).Scan(&gotSetID, &gotPeerType, &gotPeerID); err != nil {
		t.Fatalf("read channel report: %v", err)
	}
	if gotSetID != set.Set.ID || gotPeerType != int16(store.PeerTypeChannel) || gotPeerID != channel.ID {
		t.Fatalf("channel report set/peer = %d/%d/%d, want %d/%d/%d", gotSetID, gotPeerType, gotPeerID,
			set.Set.ID, store.PeerTypeChannel, channel.ID)
	}
}

func TestMessagesReportRejectsUnreadableChannelPost(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	owner, err := s.CreateUser(ctx, "+15559420051")
	if err != nil {
		t.Fatalf("create channel owner: %v", err)
	}
	reporter, err := s.CreateUser(ctx, "+15559420052")
	if err != nil {
		t.Fatalf("create outsider: %v", err)
	}
	channel, err := s.CreateChannel(ctx, owner.ID, "Private report test", "", false)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	post, _, _, err := s.PostChannelMessage(ctx, channel.ID, owner.ID, "private post", 9420051, nil, 0)
	if err != nil {
		t.Fatalf("create channel post: %v", err)
	}
	request := &tg.MessagesReportRequest{
		Peer: api.InputPeerChannel(reporter.ID, channel.ID),
		ID:   []int{int(post.LocalID)},
	}
	_, rpc := messagesReportCall(t, fullChannelDispatcher(s), reporter.ID, request)
	if rpc == nil || rpc.ErrorMessage != "MESSAGE_ID_INVALID" {
		if rpc == nil {
			t.Fatal("report for a post outside the reporter's readable channel history succeeded")
		}
		t.Fatalf("unreadable channel report = %q, want MESSAGE_ID_INVALID", rpc.ErrorMessage)
	}
	var count int
	if err := reportDatabase(t, ctx, dsn).QueryRow(ctx, `SELECT count(*) FROM user_sticker_set_reports`).Scan(&count); err != nil {
		t.Fatalf("count reports after unreadable channel post: %v", err)
	}
	if count != 0 {
		t.Fatalf("reports after unreadable channel post = %d, want none", count)
	}
}

func TestMessagesReportRejectsChatCopyAfterReporterLeaves(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	blobs := testBlobs(t)
	dsn := pgtest.DSN(t)
	s := openStickerStore(t, blobs, dsn)
	owner, err := s.CreateUser(ctx, "+15559420061")
	if err != nil {
		t.Fatalf("create set owner: %v", err)
	}
	reporter, err := s.CreateUser(ctx, "+15559420062")
	if err != nil {
		t.Fatalf("create reporter: %v", err)
	}
	h := fullChannelDispatcher(s)
	file, _ := seedReportableEmojiSet(t, ctx, s, blobs, h, owner.ID, "report_chat_leave")
	other, err := s.CreateUser(ctx, "+15559420063")
	if err != nil {
		t.Fatalf("create other member: %v", err)
	}
	chat, err := s.CreateChat(ctx, owner.ID, "Report leave", []int64{reporter.ID, other.ID})
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	if _, _, _, err := s.SendChatMessage(ctx, store.FanOut{
		ChatID: chat.ID, FromID: owner.ID, Text: "x", RandomID: 9420061,
		Entities: []store.PollDescriptionEntity{{
			Type: store.PollDescriptionEntityCustomEmoji, Offset: 0, Length: 1, Argument: file.ID,
		}},
	}); err != nil {
		t.Fatalf("send custom emoji message to chat: %v", err)
	}
	history, err := s.History(ctx, reporter.ID, store.PeerTypeChat, chat.ID, 0, 10)
	if err != nil {
		t.Fatalf("read reporter chat history: %v", err)
	}
	var copyID int64
	for _, message := range history {
		if message.Text == "x" {
			copyID = message.LocalID
		}
	}
	if copyID == 0 {
		t.Fatalf("reporter chat history = %+v, want the custom emoji copy", history)
	}
	request := &tg.MessagesReportRequest{
		Peer: api.InputPeerChat(reporter.ID, chat.ID),
		ID:   []int{int(copyID)},
	}
	requireReportMenu(t, h, reporter.ID, request)
	if removed, _, _, err := s.RemoveChatUser(ctx, chat.ID, reporter.ID, reporter.ID); err != nil || !removed {
		t.Fatalf("reporter leaves chat: removed %t, err %v", removed, err)
	}
	requireReportRejected(t, ctx, dsn, h, reporter.ID, request)
}

func TestMessagesReportRejectsBannedChannelMember(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	blobs := testBlobs(t)
	dsn := pgtest.DSN(t)
	s := openStickerStore(t, blobs, dsn)
	owner, err := s.CreateUser(ctx, "+15559420071")
	if err != nil {
		t.Fatalf("create set owner: %v", err)
	}
	reporter, err := s.CreateUser(ctx, "+15559420072")
	if err != nil {
		t.Fatalf("create reporter: %v", err)
	}
	h := fullChannelDispatcher(s)
	file, _ := seedReportableEmojiSet(t, ctx, s, blobs, h, owner.ID, "report_banned_member")
	channel, err := s.CreateChannel(ctx, owner.ID, "Report ban", "", false)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	if _, err := s.AddChannelMembers(ctx, channel.ID, owner.ID, []int64{reporter.ID}); err != nil {
		t.Fatalf("add reporter to channel: %v", err)
	}
	post := postReportableEmoji(t, ctx, s, channel.ID, owner.ID, file.ID, 9420071)
	request := &tg.MessagesReportRequest{
		Peer: api.InputPeerChannel(reporter.ID, channel.ID),
		ID:   []int{int(post.LocalID)},
	}
	requireReportMenu(t, h, reporter.ID, request)
	if err := s.SetChannelBan(ctx, channel.ID, owner.ID, reporter.ID, nil, true); err != nil {
		t.Fatalf("ban reporter: %v", err)
	}
	requireReportRejected(t, ctx, dsn, h, reporter.ID, request)
}

func TestMessagesReportRejectsChannelPostBelowHistoryFloor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	blobs := testBlobs(t)
	dsn := pgtest.DSN(t)
	s := openStickerStore(t, blobs, dsn)
	owner, err := s.CreateUser(ctx, "+15559420081")
	if err != nil {
		t.Fatalf("create set owner: %v", err)
	}
	reporter, err := s.CreateUser(ctx, "+15559420082")
	if err != nil {
		t.Fatalf("create reporter: %v", err)
	}
	h := fullChannelDispatcher(s)
	file, _ := seedReportableEmojiSet(t, ctx, s, blobs, h, owner.ID, "report_history_floor")
	channel, err := s.CreateChannel(ctx, owner.ID, "Report floor", "", true)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	before := postReportableEmoji(t, ctx, s, channel.ID, owner.ID, file.ID, 9420081)
	if _, _, changed, err := s.SetChannelPreHistoryHidden(ctx, channel.ID, owner.ID, true); err != nil || !changed {
		t.Fatalf("hide channel history: changed %t, err %v", changed, err)
	}
	if _, err := s.AddChannelMembers(ctx, channel.ID, owner.ID, []int64{reporter.ID}); err != nil {
		t.Fatalf("add reporter to channel: %v", err)
	}
	after := postReportableEmoji(t, ctx, s, channel.ID, owner.ID, file.ID, 9420082)

	requireReportMenu(t, h, reporter.ID, &tg.MessagesReportRequest{
		Peer: api.InputPeerChannel(reporter.ID, channel.ID),
		ID:   []int{int(after.LocalID)},
	})
	requireReportRejected(t, ctx, dsn, h, reporter.ID, &tg.MessagesReportRequest{
		Peer: api.InputPeerChannel(reporter.ID, channel.ID),
		ID:   []int{int(before.LocalID)},
	})
}

func seedReportableEmojiSet(
	t *testing.T, ctx context.Context, s *store.Store, blobs blob.Store, h mtproto.Handler, ownerID int64, shortName string,
) (store.File, int64) {
	t.Helper()
	file := storedStickerDocument(t, s, blobs, ownerID, validStickerTGSSize(t, 100, 100))
	request := createStickerRequest(shortName, file, false, true)
	request.TextColor = true
	set := createStickerSet(t, h, ownerID, request)
	return file, set.Set.ID
}

func postReportableEmoji(t *testing.T, ctx context.Context, s *store.Store, channelID, ownerID, fileID, randomID int64) store.ChannelMessage {
	t.Helper()
	post, _, _, err := s.PostChannelStickerAsWithEntities(ctx, channelID, ownerID, "cat", randomID, fileID, 0,
		[]store.PollDescriptionEntity{{
			Type: store.PollDescriptionEntityCustomEmoji, Offset: 0, Length: 1, Argument: fileID,
		}})
	if err != nil {
		t.Fatalf("post custom emoji to channel: %v", err)
	}
	return post
}

func requireReportMenu(t *testing.T, h mtproto.Handler, reporterID int64, request *tg.MessagesReportRequest) {
	t.Helper()
	request.Option = nil
	request.Message = ""
	result, rpc := messagesReportCall(t, h, reporterID, request)
	if rpc != nil {
		t.Fatalf("report on a visible message = %q, want the option menu", rpc.ErrorMessage)
	}
	if _, ok := result.(*tg.ReportResultChooseOption); !ok {
		t.Fatalf("report on a visible message = %T, want the option menu", result)
	}
}

// requireReportRejected walks the whole report flow and asserts every step is
// refused with MESSAGE_ID_INVALID and no report row exists afterwards.
func requireReportRejected(
	t *testing.T, ctx context.Context, dsn string, h mtproto.Handler, reporterID int64, request *tg.MessagesReportRequest,
) {
	t.Helper()
	for _, step := range []struct {
		option  string
		message string
	}{{"", ""}, {"reason:copyright", ""}, {"comment:copyright", "details"}} {
		request.Option = []byte(step.option)
		request.Message = step.message
		_, rpc := messagesReportCall(t, h, reporterID, request)
		if rpc == nil || rpc.ErrorMessage != "MESSAGE_ID_INVALID" {
			t.Fatalf("report step %q on a hidden message = %v, want MESSAGE_ID_INVALID", step.option, rpc)
		}
	}
	var count int
	if err := reportDatabase(t, ctx, dsn).QueryRow(ctx, `SELECT count(*) FROM user_sticker_set_reports`).Scan(&count); err != nil {
		t.Fatalf("count reports after rejected flow: %v", err)
	}
	if count != 0 {
		t.Fatalf("reports after rejected flow = %d, want none", count)
	}
}

func TestMessagesReportRejectsCommentOver512Bytes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	reporter, err := s.CreateUser(ctx, "+15559420061")
	if err != nil {
		t.Fatalf("create reporter: %v", err)
	}
	peer, err := s.CreateUser(ctx, "+15559420062")
	if err != nil {
		t.Fatalf("create peer: %v", err)
	}
	message, _, _, duplicate, err := s.SendMessage(ctx, reporter.ID, peer.ID, "ordinary message", 9420061, 0, 0)
	if err != nil {
		t.Fatalf("send test message: %v", err)
	}
	if duplicate {
		t.Fatal("send test message unexpectedly deduplicated")
	}
	request := &tg.MessagesReportRequest{
		Peer: api.InputPeerUser(reporter.ID, peer.ID),
		ID:   []int{int(message.LocalID)},
	}
	h := fullChannelDispatcher(s)
	choose, rpc := messagesReportCall(t, h, reporter.ID, request)
	if rpc != nil {
		t.Fatalf("start report flow: %s", rpc.ErrorMessage)
	}
	menu, ok := choose.(*tg.ReportResultChooseOption)
	if !ok || len(menu.Options) == 0 {
		t.Fatalf("report option menu = %T/%+v, want options", choose, choose)
	}
	request.Option = menu.Options[0].Option
	commentResult, rpc := messagesReportCall(t, h, reporter.ID, request)
	if rpc != nil {
		t.Fatalf("choose report reason: %s", rpc.ErrorMessage)
	}
	commentStep, ok := commentResult.(*tg.ReportResultAddComment)
	if !ok {
		t.Fatalf("report comment step = %T, want optional comment", commentResult)
	}
	request.Option = commentStep.Option
	request.Message = strings.Repeat("x", 513)
	_, rpc = messagesReportCall(t, h, reporter.ID, request)
	if rpc == nil || rpc.ErrorMessage != "MESSAGE_TOO_LONG" {
		if rpc == nil {
			t.Fatal("513-byte report comment succeeded")
		}
		t.Fatalf("513-byte comment error = %q, want MESSAGE_TOO_LONG", rpc.ErrorMessage)
	}
	var count int
	if err := reportDatabase(t, ctx, dsn).QueryRow(ctx, `SELECT count(*) FROM user_sticker_set_reports`).Scan(&count); err != nil {
		t.Fatalf("count reports after overlong comment: %v", err)
	}
	if count != 0 {
		t.Fatalf("reports after overlong comment = %d, want none", count)
	}
}

func messagesReportCall(t *testing.T, h mtproto.Handler, userID int64, request *tg.MessagesReportRequest) (tg.ReportResultClass, *mt.RPCError) {
	t.Helper()
	body := dispatchChatMethod(t, h, userID, "messages.report", request)
	var result tg.ReportResultBox
	if err := result.Decode(&bin.Buffer{Buf: body}); err == nil {
		return result.ReportResult, nil
	}
	var rpc mt.RPCError
	if err := rpc.Decode(&bin.Buffer{Buf: body}); err == nil {
		return nil, &rpc
	}
	t.Fatalf("decode messages.report response")
	return nil, nil
}

func completeMessageReport(t *testing.T, h mtproto.Handler, userID int64, request *tg.MessagesReportRequest, comment string) {
	t.Helper()
	request.Option = nil
	request.Message = ""
	menuResult, rpc := messagesReportCall(t, h, userID, request)
	if rpc != nil {
		t.Fatalf("start messages.report flow: %s", rpc.ErrorMessage)
	}
	menu, ok := menuResult.(*tg.ReportResultChooseOption)
	if !ok {
		t.Fatalf("report menu = %T, want choose-option", menuResult)
	}
	var choice []byte
	for _, option := range menu.Options {
		if option.Text == "Copyright" {
			choice = option.Option
			break
		}
	}
	if len(choice) == 0 {
		t.Fatalf("report options = %+v, want Copyright", menu.Options)
	}
	request.Option = choice
	commentStepResult, rpc := messagesReportCall(t, h, userID, request)
	if rpc != nil {
		t.Fatalf("choose messages.report option: %s", rpc.ErrorMessage)
	}
	commentStep, ok := commentStepResult.(*tg.ReportResultAddComment)
	if !ok || !commentStep.Optional {
		t.Fatalf("chosen report result = %T/%+v, want optional comment step", commentStepResult, commentStepResult)
	}
	request.Option = commentStep.Option
	request.Message = comment
	finished, rpc := messagesReportCall(t, h, userID, request)
	if rpc != nil {
		t.Fatalf("finish messages.report flow: %s", rpc.ErrorMessage)
	}
	if _, ok := finished.(*tg.ReportResultReported); !ok {
		t.Fatalf("completed messages.report result = %T, want reported", finished)
	}
}

func reportDatabase(t *testing.T, ctx context.Context, dsn string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to report database: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("close report database connection: %v", err)
		}
	})
	return conn
}
