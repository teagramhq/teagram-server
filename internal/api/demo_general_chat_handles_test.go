package api_test

import (
	"context"
	"testing"

	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/demo"
)

func TestDemoGeneralHandlesAreReservedForSeedOwner(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	seedOwner, err := s.CreateUser(ctx, demo.SeedOwnerUsername)
	if err != nil {
		t.Fatalf("create seed owner: %v", err)
	}
	if _, err := api.UpdateUsernameForTestWithDemoGeneralChat(s, seedOwner.ID, demo.SeedOwnerUsername); err != nil {
		t.Fatalf("seed owner claims its reserved account handle: %v", err)
	}
	guest, err := s.CreateUser(ctx, "+15550000135")
	if err != nil {
		t.Fatalf("create ordinary account: %v", err)
	}
	for _, handle := range demo.GeneralChatHandles() {
		if _, err := api.UpdateUsernameForTestWithDemoGeneralChat(s, guest.ID, handle); err == nil {
			t.Errorf("ordinary account claimed reserved General handle %q", handle)
		} else {
			rpcError(t, err, "USERNAME_INVALID")
		}
	}

	ownerChannel, _, _, err := s.CreateChannelWithServiceMessage(ctx, seedOwner.ID, "General", "", true)
	if err != nil {
		t.Fatalf("create owner channel: %v", err)
	}
	if _, err := api.EditChannelUsernameForTestWithDemoGeneralChat(s, seedOwner.ID, &tg.ChannelsUpdateUsernameRequest{
		Channel:  &tg.InputChannel{ChannelID: ownerChannel.ID, AccessHash: api.DeriveChannelHash(seedOwner.ID, ownerChannel.ID)},
		Username: demo.GeneralChatHandles()[0],
	}); err != nil {
		t.Fatalf("seed owner claims reserved channel handle: %v", err)
	}

	guestChannel, _, _, err := s.CreateChannelWithServiceMessage(ctx, guest.ID, "Guest channel", "", true)
	if err != nil {
		t.Fatalf("create ordinary channel: %v", err)
	}
	if _, err := api.EditChannelUsernameForTestWithDemoGeneralChat(s, guest.ID, &tg.ChannelsUpdateUsernameRequest{
		Channel:  &tg.InputChannel{ChannelID: guestChannel.ID, AccessHash: api.DeriveChannelHash(guest.ID, guestChannel.ID)},
		Username: demo.GeneralChatHandles()[1],
	}); err == nil {
		t.Fatal("ordinary channel claimed a reserved General handle")
	} else {
		rpcError(t, err, "USERNAME_INVALID")
	}

	nonDemoChannel, _, _, err := s.CreateChannelWithServiceMessage(ctx, guest.ID, "Non-demo channel", "", true)
	if err != nil {
		t.Fatalf("create non-demo channel: %v", err)
	}
	if _, err := api.EditChannelUsernameForTest(s, guest.ID, &tg.ChannelsUpdateUsernameRequest{
		Channel:  &tg.InputChannel{ChannelID: nonDemoChannel.ID, AccessHash: api.DeriveChannelHash(guest.ID, nonDemoChannel.ID)},
		Username: demo.GeneralChatHandles()[2],
	}); err != nil {
		t.Fatalf("non-demo channel username behavior changed: %v", err)
	}
}
