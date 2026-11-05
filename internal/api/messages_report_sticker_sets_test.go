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

func TestMessagesReportStoresOneAdminReportPerReporterAndSet(t *testing.T) {
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
	setIDs, visible, err := s.UserStickerSetsInMessageForReport(ctx, reporter.ID, store.PeerTypeUser, owner.ID, history[0].LocalID)
	if err != nil || !visible || len(setIDs) != 1 || setIDs[0] != set.Set.ID {
		t.Fatalf("resolve reportable sticker sets = %v, visible %t, err %v; want set %d", setIDs, visible, err, set.Set.ID)
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

func TestMessagesReportAcceptsReadableChannelStickerPost(t *testing.T) {
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
	file := storedStickerDocument(t, s, blobs, owner.ID, validStickerTGS(t))
	h := fullChannelDispatcher(s)
	set := createStickerSet(t, h, owner.ID, createStickerRequest("report_channel_sticker", file, false, false))
	channel, err := s.CreateChannel(ctx, owner.ID, "Reportable stickers", "", true)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	if _, err := s.AddChannelMembers(ctx, channel.ID, owner.ID, []int64{reporter.ID}); err != nil {
		t.Fatalf("add reporter to channel: %v", err)
	}
	post, _, _, err := s.PostChannelStickerAsWithEntities(ctx, channel.ID, owner.ID, "cat", 9420041, file.ID, 0, nil)
	if err != nil {
		t.Fatalf("post user sticker in channel: %v", err)
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
