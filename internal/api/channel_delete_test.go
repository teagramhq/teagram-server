package api_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/mt"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/store"
)

func deleteChannelRPC(t *testing.T, h mtproto.Handler, userID int64, channelID int64) (*tg.Updates, *mt.RPCError) {
	t.Helper()
	body := dispatchChatMethod(t, h, userID, "channels.deleteChannel", &tg.ChannelsDeleteChannelRequest{
		Channel: api.InputChannel(userID, channelID),
	})
	var updates tg.Updates
	if err := updates.Decode(&bin.Buffer{Buf: body}); err == nil {
		return &updates, nil
	}
	var rpc mt.RPCError
	if err := rpc.Decode(&bin.Buffer{Buf: body}); err != nil {
		t.Fatalf("decode channels.deleteChannel response: %v", err)
	}
	return nil, &rpc
}

func TestChannelsDeleteChannelCreatorOnlyAndIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	creator := mustUser(t, s, "+15551499501")
	admin := mustUser(t, s, "+15551499502")
	channel, err := s.CreateChannel(ctx, creator.ID, "Delete channel", "", false)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	added, err := s.AddChannelMembers(ctx, channel.ID, creator.ID, []int64{admin.ID})
	if err != nil || len(added) != 1 {
		t.Fatalf("add admin: added=%v err=%v", added, err)
	}
	if err := s.SetChannelRole(ctx, channel.ID, creator.ID, admin.ID, 1); err != nil {
		t.Fatalf("promote admin: %v", err)
	}

	h := fullChannelDispatcher(s)
	if affected, rpc := deleteChannelRPC(t, h, admin.ID, channel.ID); rpc == nil || rpc.ErrorMessage != "CHAT_ADMIN_REQUIRED" {
		t.Fatalf("admin delete result = updates %v rpc %v, want CHAT_ADMIN_REQUIRED", affected, rpc)
	}
	if _, found, err := s.ChannelMemberOf(ctx, channel.ID, creator.ID); err != nil || !found {
		t.Fatalf("creator membership after refused delete: found=%v err=%v", found, err)
	}

	if affected, rpc := deleteChannelRPC(t, h, creator.ID, channel.ID); rpc != nil || affected == nil {
		t.Fatalf("creator delete result = updates %v rpc %v, want updates", affected, rpc)
	}
	if affected, rpc := deleteChannelRPC(t, h, creator.ID, channel.ID); rpc != nil || affected == nil {
		t.Fatalf("repeated creator delete result = updates %v rpc %v, want harmless success", affected, rpc)
	}
	if _, found, err := s.ChannelMemberOf(ctx, channel.ID, creator.ID); err != nil || found {
		t.Fatalf("creator membership after delete: found=%v err=%v, want absent", found, err)
	}
}

func TestDeletedPublicChannelHoldsUsernameForCreator(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	creator := mustUser(t, s, "+15551499511")
	stranger := mustUser(t, s, "+15551499512")
	otherCreator := mustUser(t, s, "+15551499513")
	channel, err := s.CreateChannel(ctx, creator.ID, "Held channel", "", false)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	oldChannel, err := s.CreateChannel(ctx, creator.ID, "Pre-existing channel", "", false)
	if err != nil {
		t.Fatalf("create pre-existing channel: %v", err)
	}
	if err := s.EditChannelUsername(ctx, channel.ID, creator.ID, "delme"); err != nil {
		t.Fatalf("set channel username: %v", err)
	}
	post, _, _, err := s.PostChannelMessage(ctx, channel.ID, creator.ID, "private post", 1149951, nil, 0)
	if err != nil {
		t.Fatalf("post before deletion: %v", err)
	}
	invite, err := s.CreateChannelInvite(ctx, channel.ID, creator.ID)
	if err != nil {
		t.Fatalf("create invite: %v", err)
	}

	if affected, rpc := deleteChannelRPC(t, fullChannelDispatcher(s), creator.ID, channel.ID); rpc != nil || affected == nil {
		t.Fatalf("creator delete result = affected %v rpc %v", affected, rpc)
	}
	if available, rpc := checkChannelUsername(t, s, stranger.ID, false, &tg.ChannelsCheckUsernameRequest{
		Channel:  &tg.InputChannelEmpty{},
		Username: "delme",
	}); rpc != nil || available {
		t.Fatalf("held username availability = %t rpc %v, want taken", available, rpc)
	}
	if err := s.ClaimUsername(ctx, stranger.ID, "delme"); !errors.Is(err, store.ErrUsernameOccupied) {
		t.Fatalf("stranger username claim error = %v, want ErrUsernameOccupied", err)
	}
	if resolution, found, err := s.UsernameByHandle(ctx, "delme"); err != nil || found {
		t.Fatalf("deleted channel username resolved as %+v found=%v err=%v, want absent", resolution, found, err)
	}
	if _, err := s.ChannelByInvite(ctx, invite); !errors.Is(err, store.ErrInviteInvalid) {
		t.Fatalf("deleted channel invite preview error = %v, want ErrInviteInvalid", err)
	}
	if err := s.EditChannelUsername(ctx, oldChannel.ID, creator.ID, "delme"); !errors.Is(err, store.ErrUsernameOccupied) {
		t.Fatalf("pre-existing channel claim error = %v, want ErrUsernameOccupied", err)
	}
	if history, err := s.ChannelHistory(ctx, channel.ID, 0, 10); err != nil || len(history) != 0 {
		t.Fatalf("deleted channel history = %v err=%v, want no content", history, err)
	}
	if messages, err := s.ChannelMessages(ctx, channel.ID, []int64{post.LocalID}); err != nil || len(messages) != 0 {
		t.Fatalf("deleted channel message lookup = %v err=%v, want no content", messages, err)
	}
	deleted, found, err := s.ChannelByID(ctx, channel.ID)
	if err != nil || !found || deleted.DeletedAt == nil || deleted.Username != nil {
		t.Fatalf("deleted channel row = %+v found=%v err=%v, want retained tombstone without username", deleted, found, err)
	}
	rendered := api.ChannelToTL(deleted, store.ChannelMember{Role: 2}, true, creator.ID)
	forbidden, ok := rendered.(*tg.ChannelForbidden)
	if !ok || forbidden.ID != channel.ID || forbidden.Title != "" {
		t.Fatalf("deleted channel rendered as %T (%+v), want metadata-free ChannelForbidden", rendered, rendered)
	}

	newChannel, err := s.CreateChannel(ctx, creator.ID, "Replacement channel", "", false)
	if err != nil {
		t.Fatalf("create replacement channel: %v", err)
	}
	foreignChannel, err := s.CreateChannel(ctx, otherCreator.ID, "Other creator channel", "", false)
	if err != nil {
		t.Fatalf("create other creator channel: %v", err)
	}
	if added, err := s.AddChannelMembers(ctx, foreignChannel.ID, otherCreator.ID, []int64{creator.ID}); err != nil || len(added) != 1 {
		t.Fatalf("add original creator to other channel: added=%v err=%v", added, err)
	}
	if err := s.SetChannelRole(ctx, foreignChannel.ID, otherCreator.ID, creator.ID, 1); err != nil {
		t.Fatalf("promote original creator in other channel: %v", err)
	}
	if err := s.EditChannelUsername(ctx, foreignChannel.ID, creator.ID, "delme"); !errors.Is(err, store.ErrUsernameOccupied) {
		t.Fatalf("other creator channel claim error = %v, want ErrUsernameOccupied", err)
	}
	if err := s.EditChannelUsername(ctx, newChannel.ID, creator.ID, "delme"); err != nil {
		t.Fatalf("same creator reclaim held username: %v", err)
	}
	resolution, found, err := s.UsernameByHandle(ctx, "delme")
	if err != nil || !found || resolution.Kind != store.UsernameKindChannel || resolution.Channel.ID != newChannel.ID {
		t.Fatalf("reclaimed username resolution = %+v found=%v err=%v", resolution, found, err)
	}
}
