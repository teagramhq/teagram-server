package main

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/teagramhq/teagram-server/internal/srp"
)

func TestModerationSmokeDoesNotChangeUsername(t *testing.T) {
	fset := token.NewFileSet()
	source, err := parser.ParseFile(fset, "general_chat_moderation_smoke.go", nil, 0)
	if err != nil {
		t.Fatalf("parse moderation smoke implementation: %v", err)
	}
	ast.Inspect(source, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		method, ok := call.Fun.(*ast.SelectorExpr)
		if ok && method.Sel.Name == "AccountUpdateUsername" {
			t.Errorf("moderation smoke calls account.updateUsername at %s", fset.Position(call.Pos()))
		}
		return true
	})
}

func TestModerationSmokeSetsPasswordBeforeProfileLookup(t *testing.T) {
	fset := token.NewFileSet()
	source, err := parser.ParseFile(fset, "general_chat_moderation_smoke.go", nil, 0)
	if err != nil {
		t.Fatalf("parse moderation smoke implementation: %v", err)
	}
	var signup *ast.FuncDecl
	for _, declaration := range source.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if ok && function.Name.Name == "signUpModerationSmokeGuest" {
			signup = function
			break
		}
	}
	if signup == nil {
		t.Fatal("signUpModerationSmokeGuest was not found")
	}
	var preparationCall, passwordCall, profileCall token.Pos
	ast.Inspect(signup.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		name, ok := call.Fun.(*ast.Ident)
		if !ok {
			return true
		}
		switch name.Name {
		case "prepareModerationSmokeGuest":
			preparationCall = call.Pos()
		case "setNewPassword":
			passwordCall = call.Pos()
		case "verifyModerationSmokeGuestProfile":
			profileCall = call.Pos()
		}
		return true
	})
	if preparationCall == token.NoPos || passwordCall == token.NoPos || profileCall == token.NoPos ||
		preparationCall >= passwordCall || passwordCall >= profileCall {
		t.Fatalf("signup calls at %s, password setup at %s, and profile lookup at %s must be ordered", fset.Position(preparationCall), fset.Position(passwordCall), fset.Position(profileCall))
	}
}

func TestPrepareModerationSmokeGuestSignsUpWithShortUsername(t *testing.T) {
	api := &fakeModerationSmokeSignupAPI{}
	user, err := prepareModerationSmokeGuest(context.Background(), api)
	if err != nil {
		t.Fatalf("sign up moderation smoke guest: %v", err)
	}
	if user.ID != 42 {
		t.Fatalf("sign-up user ID = %d, want 42", user.ID)
	}
	if got, want := fmt.Sprint(api.calls), "[send-code sign-up]"; got != want {
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

func TestSignUpModerationSmokeGuestSetsPasswordBeforeProfileLookup(t *testing.T) {
	api := &fakeModerationSmokeSignupAPI{}
	if err := signUpModerationSmokeGuest(context.Background(), api, "test-password"); err != nil {
		t.Fatalf("sign up moderation smoke guest: %v", err)
	}
	if got, want := fmt.Sprint(api.calls), "[send-code sign-up get-password set-password get-full-user]"; got != want {
		t.Fatalf("moderation smoke guest RPCs = %s, want %s", got, want)
	}
	if api.passwordRequest == nil || len(api.passwordRequest.NewSettings.NewPasswordHash) == 0 {
		t.Fatal("password setup did not submit a generated password hash")
	}
}

type fakeModerationSmokeSignupAPI struct {
	calls           []string
	sendCodeRequest *tg.AuthSendCodeRequest
	signUpRequest   *tg.AuthSignUpRequest
	passwordRequest *tg.AccountUpdatePasswordSettingsRequest
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

func (api *fakeModerationSmokeSignupAPI) AccountGetPassword(context.Context) (*tg.AccountPassword, error) {
	api.calls = append(api.calls, "get-password")
	return &tg.AccountPassword{
		NewAlgo: &tg.PasswordKdfAlgoSHA256SHA256PBKDF2HMACSHA512iter100000SHA256ModPow{
			Salt1: []byte("initial-salt"),
			Salt2: []byte("server-salt"),
			G:     3,
			P:     srp.PBytes(),
		},
	}, nil
}

func (api *fakeModerationSmokeSignupAPI) AccountUpdatePasswordSettings(_ context.Context, request *tg.AccountUpdatePasswordSettingsRequest) (bool, error) {
	api.calls = append(api.calls, "set-password")
	api.passwordRequest = request
	return true, nil
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
