package main

import (
	"context"
	"fmt"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

func TestRestoreModerationSmokeGuestRestoresMembership(t *testing.T) {
	for _, test := range []struct {
		name      string
		inviteErr error
	}{
		{name: "left after unban"},
		{name: "already a member", inviteErr: tgerr.New(400, "USER_ALREADY_PARTICIPANT")},
	} {
		t.Run(test.name, func(t *testing.T) {
			rpc := &moderationSmokeMembershipRPC{inviteErr: test.inviteErr}
			remote := remoteSpace{channelID: 71, accessHash: 93, creator: true}
			guest := &tg.User{ID: 42, AccessHash: 43}
			if err := restoreModerationSmokeGuestMembership(context.Background(), tg.NewClient(rpc), remote, guest); err != nil {
				t.Fatalf("restore moderation smoke guest membership: %v", err)
			}
			if rpc.unbanCount != 1 || rpc.unbanViewMessages || rpc.unbanUntilDate != 0 {
				t.Fatalf("unban calls=%d view=%t until=%d, want one empty unban", rpc.unbanCount, rpc.unbanViewMessages, rpc.unbanUntilDate)
			}
			if rpc.inviteCount != 1 || rpc.invitedUserID != guest.ID {
				t.Fatalf("invite calls=%d user=%d, want one invite for %d", rpc.inviteCount, rpc.invitedUserID, guest.ID)
			}
			if rpc.participantChecks != 1 {
				t.Fatalf("membership checks = %d, want one", rpc.participantChecks)
			}
		})
	}
}

type moderationSmokeMembershipRPC struct {
	inviteErr         error
	unbanCount        int
	unbanViewMessages bool
	unbanUntilDate    int
	inviteCount       int
	invitedUserID     int64
	participantChecks int
}

func (rpc *moderationSmokeMembershipRPC) Invoke(_ context.Context, input bin.Encoder, output bin.Decoder) error {
	switch request := input.(type) {
	case *tg.ChannelsEditBannedRequest:
		channel, ok := request.Channel.(*tg.InputChannel)
		if !ok || channel.ChannelID != 71 || channel.AccessHash != 93 {
			return fmt.Errorf("unexpected smoke cleanup channel: %+v", request.Channel)
		}
		target, ok := request.Participant.(*tg.InputPeerUser)
		if !ok || target.UserID != 42 || target.AccessHash != 43 {
			return fmt.Errorf("unexpected smoke cleanup participant: %+v", request.Participant)
		}
		rpc.unbanCount++
		rpc.unbanViewMessages = request.BannedRights.ViewMessages
		rpc.unbanUntilDate = request.BannedRights.UntilDate
		updates, ok := output.(*tg.UpdatesBox)
		if !ok {
			return fmt.Errorf("unexpected unban output %T", output)
		}
		updates.Updates = &tg.Updates{}
	case *tg.ChannelsInviteToChannelRequest:
		if request.Channel == nil || len(request.Users) != 1 {
			return fmt.Errorf("unexpected smoke cleanup invite: %+v", request)
		}
		user, ok := request.Users[0].(*tg.InputUser)
		if !ok || user.UserID != 42 || user.AccessHash != 43 {
			return fmt.Errorf("unexpected smoke cleanup invite target: %+v", request.Users[0])
		}
		rpc.inviteCount++
		rpc.invitedUserID = user.UserID
		if rpc.inviteErr != nil {
			return rpc.inviteErr
		}
		if _, ok := output.(*tg.MessagesInvitedUsers); !ok {
			return fmt.Errorf("unexpected invite output %T", output)
		}
	case *tg.ChannelsGetParticipantRequest:
		if request.Channel == nil || request.Participant == nil {
			return fmt.Errorf("unexpected smoke cleanup participant query: %+v", request)
		}
		participant, ok := output.(*tg.ChannelsChannelParticipant)
		if !ok {
			return fmt.Errorf("unexpected participant output %T", output)
		}
		rpc.participantChecks++
		participant.Participant = &tg.ChannelParticipant{UserID: 42}
	default:
		return fmt.Errorf("unexpected smoke cleanup request %T", input)
	}
	return nil
}
