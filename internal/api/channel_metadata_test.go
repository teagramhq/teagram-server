package api_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/mt"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/api"
)

func TestChannelsEditTitleStoresAndAnnouncesTitleChange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	creator := chatUser(t, s, 202201)
	member := chatUser(t, s, 202202)
	channel, err := s.CreateChannel(ctx, creator.ID, "Ops", "", false)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	joined := joinChannelByInvite(t, s, channel, member.ID)

	body := dispatchChatMethod(t, fullChannelDispatcher(s), creator.ID, "channels.editTitle", &tg.ChannelsEditTitleRequest{
		Channel: api.InputChannel(creator.ID, channel.ID),
		Title:   "Ops team",
	})
	var updates tg.Updates
	if err := updates.Decode(&bin.Buffer{Buf: body}); err != nil {
		var rpc mt.RPCError
		if rpcErr := rpc.Decode(&bin.Buffer{Buf: body}); rpcErr == nil {
			t.Fatalf("edit title returned %s, want updates", rpc.ErrorMessage)
		}
		t.Fatalf("decode edit title result: %v", err)
	}

	var renamed *tg.UpdateNewChannelMessage
	var channelUpdated bool
	for _, update := range updates.Updates {
		switch update := update.(type) {
		case *tg.UpdateNewChannelMessage:
			renamed = update
		case *tg.UpdateChannel:
			channelUpdated = update.ChannelID == channel.ID
		}
	}
	if renamed == nil || !channelUpdated {
		t.Fatalf("updates = %#v, want a title service message and UpdateChannel", updates.Updates)
	}
	service, ok := renamed.Message.(*tg.MessageService)
	if !ok {
		t.Fatalf("title update message = %T, want *tg.MessageService", renamed.Message)
	}
	action, ok := service.Action.(*tg.MessageActionChatEditTitle)
	if !ok || action.Title != "Ops team" {
		t.Fatalf("title action = %#v, want Ops team", service.Action)
	}
	if renamed.Pts != 2 || renamed.PtsCount != 1 {
		t.Fatalf("title event pts/count = %d/%d, want 2/1", renamed.Pts, renamed.PtsCount)
	}

	stored, found, err := s.ChannelByID(ctx, channel.ID)
	if err != nil || !found {
		t.Fatalf("channel after rename: found=%v err=%v", found, err)
	}
	if stored.Title != "Ops team" {
		t.Fatalf("stored title = %q, want Ops team", stored.Title)
	}

	enc, err := api.GetChannelDifferenceForTest(s, member.ID, &tg.UpdatesGetChannelDifferenceRequest{
		Channel: api.InputChannel(member.ID, channel.ID),
		Filter:  &tg.ChannelMessagesFilterEmpty{},
		Pts:     joined.JoinPts,
		Limit:   10,
	})
	if err != nil {
		t.Fatalf("offline channel difference: %v", err)
	}
	difference, ok := enc.(*tg.UpdatesChannelDifference)
	if !ok {
		t.Fatalf("channel difference = %T, want *tg.UpdatesChannelDifference", enc)
	}
	if len(difference.NewMessages) != 1 {
		t.Fatalf("offline new messages = %d, want title service message", len(difference.NewMessages))
	}
	offlineService, ok := difference.NewMessages[0].(*tg.MessageService)
	if !ok {
		t.Fatalf("offline title message = %T, want *tg.MessageService", difference.NewMessages[0])
	}
	offlineAction, ok := offlineService.Action.(*tg.MessageActionChatEditTitle)
	if !ok || offlineAction.Title != "Ops team" {
		t.Fatalf("offline title action = %#v, want Ops team", offlineService.Action)
	}
}

func TestMessagesEditChatAboutStoresAndReadsDescription(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	creator := chatUser(t, s, 202211)
	channel, err := s.CreateChannel(ctx, creator.ID, "Notes", "", false)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	beforePts, err := s.ChannelState(ctx, channel.ID)
	if err != nil {
		t.Fatalf("channel state before edit: %v", err)
	}

	body := dispatchChatMethod(t, fullChannelDispatcher(s), creator.ID, "messages.editChatAbout", &tg.MessagesEditChatAboutRequest{
		Peer:  api.InputPeerChannel(creator.ID, channel.ID),
		About: "Daily notes",
	})
	var result tg.BoolTrue
	if err := result.Decode(&bin.Buffer{Buf: body}); err != nil {
		var rpc mt.RPCError
		if rpcErr := rpc.Decode(&bin.Buffer{Buf: body}); rpcErr == nil {
			t.Fatalf("edit about returned %s, want boolTrue", rpc.ErrorMessage)
		}
		t.Fatalf("decode edit about result: %v", err)
	}

	differenceEnc, err := api.GetChannelDifferenceForTest(s, creator.ID, &tg.UpdatesGetChannelDifferenceRequest{
		Channel: api.InputChannel(creator.ID, channel.ID),
		Filter:  &tg.ChannelMessagesFilterEmpty{},
		Pts:     beforePts,
		Limit:   10,
	})
	if err != nil {
		t.Fatalf("channel difference after edit about: %v", err)
	}
	difference, ok := differenceEnc.(*tg.UpdatesChannelDifference)
	if !ok {
		t.Fatalf("channel difference = %T, want *tg.UpdatesChannelDifference", differenceEnc)
	}
	if difference.Pts != beforePts+1 || len(difference.OtherUpdates) != 1 {
		t.Fatalf("channel difference pts/updates = %d/%d, want %d/1", difference.Pts, len(difference.OtherUpdates), beforePts+1)
	}
	if update, ok := difference.OtherUpdates[0].(*tg.UpdateChannel); !ok || update.ChannelID != channel.ID {
		t.Fatalf("channel difference update = %#v, want UpdateChannel for %d", difference.OtherUpdates[0], channel.ID)
	}

	fullBody := dispatchChatMethod(t, fullChannelDispatcher(s), creator.ID, "channels.getFullChannel", &tg.ChannelsGetFullChannelRequest{
		Channel: api.InputChannel(creator.ID, channel.ID),
	})
	var full tg.MessagesChatFull
	if err := full.Decode(&bin.Buffer{Buf: fullBody}); err != nil {
		t.Fatalf("decode getFullChannel: %v", err)
	}
	channelFull, ok := full.FullChat.(*tg.ChannelFull)
	if !ok || channelFull.About != "Daily notes" {
		t.Fatalf("full channel = %#v, want about Daily notes", full.FullChat)
	}
}

func TestMessagesEditChatAboutRejectsTooLongDescription(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	creator := chatUser(t, s, 202221)
	channel, err := s.CreateChannel(ctx, creator.ID, "Notes", "before", false)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}

	body := dispatchChatMethod(t, fullChannelDispatcher(s), creator.ID, "messages.editChatAbout", &tg.MessagesEditChatAboutRequest{
		Peer:  api.InputPeerChannel(creator.ID, channel.ID),
		About: strings.Repeat("x", 256),
	})
	var rpc mt.RPCError
	if err := rpc.Decode(&bin.Buffer{Buf: body}); err != nil {
		t.Fatalf("decode edit about error: %v", err)
	}
	if rpc.ErrorMessage != "CHAT_ABOUT_TOO_LONG" {
		t.Fatalf("edit about error = %s, want CHAT_ABOUT_TOO_LONG", rpc.ErrorMessage)
	}

	stored, found, err := s.ChannelByID(ctx, channel.ID)
	if err != nil || !found {
		t.Fatalf("channel after rejected about: found=%v err=%v", found, err)
	}
	if stored.About != "before" {
		t.Fatalf("stored about = %q, want unchanged before", stored.About)
	}
}

func TestChannelMetadataNoOpsReturnNotModified(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	creator := chatUser(t, s, 202231)
	channel, err := s.CreateChannel(ctx, creator.ID, "Ops", "Daily notes", false)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	h := fullChannelDispatcher(s)

	titleBody := dispatchChatMethod(t, h, creator.ID, "channels.editTitle", &tg.ChannelsEditTitleRequest{
		Channel: api.InputChannel(creator.ID, channel.ID), Title: "Ops",
	})
	var titleRPC mt.RPCError
	if err := titleRPC.Decode(&bin.Buffer{Buf: titleBody}); err != nil {
		t.Fatalf("decode unchanged title error: %v", err)
	}
	if titleRPC.ErrorMessage != "CHAT_NOT_MODIFIED" {
		t.Fatalf("unchanged title error = %s, want CHAT_NOT_MODIFIED", titleRPC.ErrorMessage)
	}

	aboutBody := dispatchChatMethod(t, h, creator.ID, "messages.editChatAbout", &tg.MessagesEditChatAboutRequest{
		Peer: api.InputPeerChannel(creator.ID, channel.ID), About: "Daily notes",
	})
	var aboutRPC mt.RPCError
	if err := aboutRPC.Decode(&bin.Buffer{Buf: aboutBody}); err != nil {
		t.Fatalf("decode unchanged about error: %v", err)
	}
	if aboutRPC.ErrorMessage != "CHAT_ABOUT_NOT_MODIFIED" {
		t.Fatalf("unchanged about error = %s, want CHAT_ABOUT_NOT_MODIFIED", aboutRPC.ErrorMessage)
	}
}

func TestChannelMetadataRequiresCurrentMembership(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	creator := chatUser(t, s, 202241)
	outsider := chatUser(t, s, 202242)
	banned := chatUser(t, s, 202243)
	left := chatUser(t, s, 202244)
	channel, err := s.CreateChannel(ctx, creator.ID, "Private", "secret", false)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	beforePts, err := s.ChannelState(ctx, channel.ID)
	if err != nil {
		t.Fatalf("channel state before rejected edits: %v", err)
	}
	joinChannelByInvite(t, s, channel, banned.ID)
	joinChannelByInvite(t, s, channel, left.ID)
	banChannelMember(t, ctx, dsn, channel.ID, banned.ID, time.Now().Add(time.Hour))
	if _, err := s.LeaveChannel(ctx, channel.ID, left.ID); err != nil {
		t.Fatalf("leave channel: %v", err)
	}
	h := fullChannelDispatcher(s)

	for _, caller := range []struct {
		name string
		id   int64
	}{
		{name: "non-member", id: outsider.ID},
		{name: "banned member", id: banned.ID},
		{name: "former member", id: left.ID},
	} {
		for _, method := range []string{"title", "about", "getFullChannel"} {
			var body []byte
			switch method {
			case "title":
				body = dispatchChatMethod(t, h, caller.id, "channels.editTitle", &tg.ChannelsEditTitleRequest{
					Channel: api.InputChannel(caller.id, channel.ID), Title: "Leaked",
				})
			case "about":
				body = dispatchChatMethod(t, h, caller.id, "messages.editChatAbout", &tg.MessagesEditChatAboutRequest{
					Peer: api.InputPeerChannel(caller.id, channel.ID), About: "Leaked",
				})
			case "getFullChannel":
				body = dispatchChatMethod(t, h, caller.id, "channels.getFullChannel", &tg.ChannelsGetFullChannelRequest{
					Channel: api.InputChannel(caller.id, channel.ID),
				})
			}
			var rpc mt.RPCError
			if err := rpc.Decode(&bin.Buffer{Buf: body}); err != nil {
				t.Fatalf("%s %s error decode: %v", caller.name, method, err)
			}
			if rpc.ErrorMessage != "PEER_ID_INVALID" {
				t.Errorf("%s %s error = %s, want PEER_ID_INVALID", caller.name, method, rpc.ErrorMessage)
			}
		}
	}

	stored, found, err := s.ChannelByID(ctx, channel.ID)
	if err != nil || !found {
		t.Fatalf("channel after outsider edits: found=%v err=%v", found, err)
	}
	if stored.Title != "Private" || stored.About != "secret" {
		t.Fatalf("outsider changed metadata: title=%q about=%q", stored.Title, stored.About)
	}
	if afterPts, err := s.ChannelState(ctx, channel.ID); err != nil || afterPts != beforePts {
		t.Fatalf("rejected metadata edits changed channel pts from %d to %d (err %v)", beforePts, afterPts, err)
	}
}

func TestChannelAdminWithoutChangeInfoCannotEditMetadata(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	creator := chatUser(t, s, 202251)
	admin := chatUser(t, s, 202252)
	channel, err := s.CreateChannel(ctx, creator.ID, "Group", "before", true)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	joinChannelByInvite(t, s, channel, admin.ID)
	adminRightsBody := dispatchChatMethod(t, fullChannelDispatcher(s), creator.ID, "channels.editAdmin", &tg.ChannelsEditAdminRequest{
		Channel:     api.InputChannel(creator.ID, channel.ID),
		UserID:      api.InputUser(creator.ID, admin.ID),
		AdminRights: tg.ChatAdminRights{BanUsers: true},
	})
	var adminUpdates tg.Updates
	if err := adminUpdates.Decode(&bin.Buffer{Buf: adminRightsBody}); err != nil {
		t.Fatalf("promote admin without change_info: %v", err)
	}

	body := dispatchChatMethod(t, fullChannelDispatcher(s), admin.ID, "channels.editTitle", &tg.ChannelsEditTitleRequest{
		Channel: api.InputChannel(admin.ID, channel.ID), Title: "Denied",
	})
	var rpc mt.RPCError
	if err := rpc.Decode(&bin.Buffer{Buf: body}); err != nil {
		t.Fatalf("decode denied admin rename: %v", err)
	}
	if rpc.ErrorMessage != "CHAT_ADMIN_REQUIRED" {
		t.Fatalf("admin without change_info error = %s, want CHAT_ADMIN_REQUIRED", rpc.ErrorMessage)
	}
	aboutBody := dispatchChatMethod(t, fullChannelDispatcher(s), admin.ID, "messages.editChatAbout", &tg.MessagesEditChatAboutRequest{
		Peer: api.InputPeerChannel(admin.ID, channel.ID), About: "Denied",
	})
	var aboutRPC mt.RPCError
	if err := aboutRPC.Decode(&bin.Buffer{Buf: aboutBody}); err != nil {
		t.Fatalf("decode denied admin about edit: %v", err)
	}
	if aboutRPC.ErrorMessage != "CHAT_ADMIN_REQUIRED" {
		t.Fatalf("admin without change_info about error = %s, want CHAT_ADMIN_REQUIRED", aboutRPC.ErrorMessage)
	}

	stored, found, err := s.ChannelByID(ctx, channel.ID)
	if err != nil || !found {
		t.Fatalf("channel after denied rename: found=%v err=%v", found, err)
	}
	if stored.Title != "Group" || stored.About != "before" {
		t.Fatalf("denied admin changed metadata: title=%q about=%q", stored.Title, stored.About)
	}
}

func TestChannelAdminWithChangeInfoCanEditMetadata(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	creator := chatUser(t, s, 202255)
	admin := chatUser(t, s, 202256)
	channel, err := s.CreateChannel(ctx, creator.ID, "Group", "before", false)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	joinChannelByInvite(t, s, channel, admin.ID)
	adminRightsBody := dispatchChatMethod(t, fullChannelDispatcher(s), creator.ID, "channels.editAdmin", &tg.ChannelsEditAdminRequest{
		Channel:     api.InputChannel(creator.ID, channel.ID),
		UserID:      api.InputUser(creator.ID, admin.ID),
		AdminRights: tg.ChatAdminRights{ChangeInfo: true},
	})
	var adminUpdates tg.Updates
	if err := adminUpdates.Decode(&bin.Buffer{Buf: adminRightsBody}); err != nil {
		t.Fatalf("promote admin with change_info: %v", err)
	}

	body := dispatchChatMethod(t, fullChannelDispatcher(s), admin.ID, "messages.editChatAbout", &tg.MessagesEditChatAboutRequest{
		Peer: api.InputPeerChannel(admin.ID, channel.ID), About: "admin notes",
	})
	var result tg.BoolTrue
	if err := result.Decode(&bin.Buffer{Buf: body}); err != nil {
		t.Fatalf("admin about edit: %v", err)
	}
	titleBody := dispatchChatMethod(t, fullChannelDispatcher(s), admin.ID, "channels.editTitle", &tg.ChannelsEditTitleRequest{
		Channel: api.InputChannel(admin.ID, channel.ID), Title: "Admin title",
	})
	var titleUpdates tg.Updates
	if err := titleUpdates.Decode(&bin.Buffer{Buf: titleBody}); err != nil {
		t.Fatalf("admin title edit: %v", err)
	}
	member, found, err := s.ChannelMemberOf(ctx, channel.ID, admin.ID)
	if err != nil || !found || !member.AdminChangeInfo {
		t.Fatalf("admin member = %+v found=%v err=%v, want persisted change_info", member, found, err)
	}
	stored, found, err := s.ChannelByID(ctx, channel.ID)
	if err != nil || !found || stored.Title != "Admin title" || stored.About != "admin notes" {
		t.Fatalf("admin edited metadata = %+v found=%v err=%v", stored, found, err)
	}
}

func TestMegagroupMemberCanEditInfoUnlessDefaultRightsBanIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	creator := chatUser(t, s, 202261)
	member := chatUser(t, s, 202262)
	channel, err := s.CreateChannel(ctx, creator.ID, "Team", "", true)
	if err != nil {
		t.Fatalf("create megagroup: %v", err)
	}
	joinChannelByInvite(t, s, channel, member.ID)
	h := fullChannelDispatcher(s)

	allowedBody := dispatchChatMethod(t, h, member.ID, "channels.editTitle", &tg.ChannelsEditTitleRequest{
		Channel: api.InputChannel(member.ID, channel.ID), Title: "Team Ops",
	})
	var allowedUpdates tg.Updates
	if err := allowedUpdates.Decode(&bin.Buffer{Buf: allowedBody}); err != nil {
		var rpc mt.RPCError
		if rpcErr := rpc.Decode(&bin.Buffer{Buf: allowedBody}); rpcErr == nil {
			t.Fatalf("unrestricted member rename returned %s, want updates", rpc.ErrorMessage)
		}
		t.Fatalf("decode unrestricted member rename: %v", err)
	}
	allowedAboutBody := dispatchChatMethod(t, h, member.ID, "messages.editChatAbout", &tg.MessagesEditChatAboutRequest{
		Peer: api.InputPeerChannel(member.ID, channel.ID), About: "Member description",
	})
	var allowedAbout tg.BoolTrue
	if err := allowedAbout.Decode(&bin.Buffer{Buf: allowedAboutBody}); err != nil {
		t.Fatalf("unrestricted member description edit: %v", err)
	}

	if _, _, err = s.SetChannelDefaultBannedRights(ctx, channel.ID, creator.ID, []string{"change_info"}); err != nil {
		t.Fatalf("restrict change_info: %v", err)
	}
	deniedBody := dispatchChatMethod(t, h, member.ID, "channels.editTitle", &tg.ChannelsEditTitleRequest{
		Channel: api.InputChannel(member.ID, channel.ID), Title: "Denied",
	})
	var rpc mt.RPCError
	if err := rpc.Decode(&bin.Buffer{Buf: deniedBody}); err != nil {
		t.Fatalf("decode restricted member rename: %v", err)
	}
	if rpc.ErrorMessage != "CHAT_ADMIN_REQUIRED" {
		t.Fatalf("restricted member error = %s, want CHAT_ADMIN_REQUIRED", rpc.ErrorMessage)
	}
	deniedAboutBody := dispatchChatMethod(t, h, member.ID, "messages.editChatAbout", &tg.MessagesEditChatAboutRequest{
		Peer: api.InputPeerChannel(member.ID, channel.ID), About: "Denied",
	})
	var deniedAboutRPC mt.RPCError
	if err := deniedAboutRPC.Decode(&bin.Buffer{Buf: deniedAboutBody}); err != nil {
		t.Fatalf("decode restricted member description: %v", err)
	}
	if deniedAboutRPC.ErrorMessage != "CHAT_ADMIN_REQUIRED" {
		t.Fatalf("restricted member description error = %s, want CHAT_ADMIN_REQUIRED", deniedAboutRPC.ErrorMessage)
	}
	stored, found, err := s.ChannelByID(ctx, channel.ID)
	if err != nil || !found {
		t.Fatalf("channel after restriction: found=%v err=%v", found, err)
	}
	if stored.Title != "Team Ops" || stored.About != "Member description" {
		t.Fatalf("restricted member changed metadata: title=%q about=%q", stored.Title, stored.About)
	}
}
