package main

import (
	"context"
	"fmt"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

func TestPrepareModerationSmokeGuestSignsUpWithShortUsername(t *testing.T) {
	api := &fakeModerationSmokeSignupAPI{}
	if err := prepareModerationSmokeGuest(context.Background(), api); err != nil {
		t.Fatalf("sign up moderation smoke guest: %v", err)
	}
	if got, want := fmt.Sprint(api.calls), "[send-code sign-up get-full-user]"; got != want {
		t.Fatalf("moderation smoke guest RPCs = %s, want %s", got, want)
	}
	if api.sendCodeRequest == nil || api.sendCodeRequest.PhoneNumber != moderationSmokeShortUsername {
		t.Fatalf("auth.sendCode request = %+v, want handle %q", api.sendCodeRequest, moderationSmokeShortUsername)
	}
	if api.signUpRequest == nil {
		t.Fatal("auth.signUp was not called")
	}
	if api.signUpRequest.PhoneNumber != moderationSmokeShortUsername || api.signUpRequest.PhoneCodeHash != "smoke-code-hash" {
		t.Fatalf("auth.signUp request = %+v, want new account handle %q and the issued code", api.signUpRequest, moderationSmokeShortUsername)
	}
	if api.signUpRequest.FirstName == "" {
		t.Fatal("auth.signUp did not set a first name")
	}
}

type fakeModerationSmokeSignupAPI struct {
	calls           []string
	sendCodeRequest *tg.AuthSendCodeRequest
	signUpRequest   *tg.AuthSignUpRequest
}

func (api *fakeModerationSmokeSignupAPI) AuthSendCode(_ context.Context, request *tg.AuthSendCodeRequest) (tg.AuthSentCodeClass, error) {
	api.calls = append(api.calls, "send-code")
	api.sendCodeRequest = request
	return &tg.AuthSentCode{PhoneCodeHash: "smoke-code-hash"}, nil
}

func (api *fakeModerationSmokeSignupAPI) AuthSignUp(_ context.Context, request *tg.AuthSignUpRequest) (tg.AuthAuthorizationClass, error) {
	api.calls = append(api.calls, "sign-up")
	api.signUpRequest = request
	return &tg.AuthAuthorization{User: &tg.User{ID: 42}}, nil
}

func (api *fakeModerationSmokeSignupAPI) UsersGetFullUser(_ context.Context, _ tg.InputUserClass) (*tg.UsersUserFull, error) {
	api.calls = append(api.calls, "get-full-user")
	return &tg.UsersUserFull{Users: []tg.UserClass{&tg.User{ID: 42, Username: moderationSmokeShortUsername}}}, nil
}

func TestCleanupRestoresShortHandleGuestMembershipWithoutRenaming(t *testing.T) {
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
			guest := &tg.User{ID: 42, AccessHash: 43, Username: moderationSmokeShortUsername}
			if err := restoreModerationSmokeGuestMembership(context.Background(), tg.NewClient(rpc), remote, guest); err != nil {
				t.Fatalf("restore moderation smoke guest membership: %v", err)
			}
			if guest.Username != moderationSmokeShortUsername {
				t.Fatalf("cleanup changed smoke guest handle to %q", guest.Username)
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
