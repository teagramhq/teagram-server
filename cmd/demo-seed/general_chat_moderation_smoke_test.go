package main

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/teagramhq/teagram-server/internal/demo"
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

func TestFindOwnedGeneralChatForModerationSmokeSkipsNonOwnedAndIgnoresDescription(t *testing.T) {
	rpc := &moderationSmokeDialogsRPC{}
	general, err := findOwnedGeneralChatForModerationSmoke(context.Background(), tg.NewClient(rpc))
	if err != nil {
		t.Fatalf("find owned General from dialogs: %v", err)
	}
	if general.channelID != 72 || general.accessHash != 94 || !general.creator {
		t.Fatalf("resolved General = %+v, want owned fallback handle channel 72", general)
	}
	if rpc.fullInfoCalls != 0 {
		t.Fatalf("full-channel description lookups = %d, want 0", rpc.fullInfoCalls)
	}
}

type moderationSmokeDialogsRPC struct {
	fullInfoCalls int
}

func (rpc *moderationSmokeDialogsRPC) Invoke(_ context.Context, input bin.Encoder, output bin.Decoder) error {
	switch input.(type) {
	case *tg.MessagesGetDialogsRequest:
		response, ok := output.(*tg.MessagesDialogsBox)
		if !ok {
			return fmt.Errorf("unexpected dialogs output %T", output)
		}
		response.Dialogs = &tg.MessagesDialogs{Chats: []tg.ChatClass{
			&tg.Channel{
				ID: 71, AccessHash: 93, Username: demo.GeneralChatHandle,
				Title: demoGeneralChatSpace.title, Megagroup: true,
			},
			&tg.Channel{
				ID: 72, AccessHash: 94, Username: demo.GeneralChatHandles()[1],
				Title: demoGeneralChatSpace.title, Megagroup: true, Creator: true,
			},
		}}
	case *tg.ChannelsGetFullChannelRequest:
		rpc.fullInfoCalls++
		return errors.New("unexpected full-channel description lookup")
	default:
		return fmt.Errorf("unexpected General dialog lookup request %T", input)
	}
	return nil
}

func TestCleanupGeneralModerationSmokeRestoresMembershipAndRemovesArtifactsWithLookupQuotaExhausted(t *testing.T) {
	sessionFile := filepath.Join(t.TempDir(), "moderation-smoke.session")
	if err := os.WriteFile(sessionFile, []byte("session"), 0600); err != nil {
		t.Fatalf("create smoke guest session: %v", err)
	}
	rpc := &moderationSmokeCleanupRPC{}
	guestCalls := 0
	ownerCalls := 0
	dependencies := moderationSmokeCleanupDependencies{
		withGuest: func(ctx context.Context, _ apiConfig, _ string, action func(context.Context, *tg.Client, *tg.User) error) error {
			guestCalls++
			return action(ctx, tg.NewClient(rpc), &tg.User{ID: 42, AccessHash: 43, Username: moderationSmokeShortUsername})
		},
		withOwner: func(ctx context.Context, _ apiConfig, _ seedOptions, _ string, action apiAction) error {
			ownerCalls++
			return action(ctx, tg.NewClient(rpc))
		},
		sessionFile: sessionFile,
	}
	options := seedOptions{moderationSmokeMarker: strings.Repeat("a", 32)}
	if err := cleanupGeneralModerationSmoke(context.Background(), apiConfig{}, options, "unused", dependencies); err != nil {
		t.Fatalf("cleanup General moderation smoke: %v", err)
	}
	if guestCalls != 1 || ownerCalls != 1 {
		t.Fatalf("cleanup runners guest=%d owner=%d, want one each", guestCalls, ownerCalls)
	}
	if rpc.usernameLookupCalls != 0 {
		t.Fatalf("quota-charged username lookups = %d, want 0", rpc.usernameLookupCalls)
	}
	if rpc.unbanCalls != 1 || rpc.inviteCalls != 1 || rpc.participantChecks != 1 || !rpc.memberRestored {
		t.Fatalf("membership cleanup unban=%d invite=%d verify=%d, want one each", rpc.unbanCalls, rpc.inviteCalls, rpc.participantChecks)
	}
	if rpc.deletedMessageID != 81 {
		t.Fatalf("deleted moderation marker post ID = %d, want 81", rpc.deletedMessageID)
	}
	if _, err := os.Stat(sessionFile); !os.IsNotExist(err) {
		t.Fatalf("moderation smoke session stat error = %v, want removed session", err)
	}
}

type moderationSmokeCleanupRPC struct {
	usernameLookupCalls int
	unbanCalls          int
	inviteCalls         int
	participantChecks   int
	memberRestored      bool
	deletedMessageID    int
}

func (rpc *moderationSmokeCleanupRPC) Invoke(_ context.Context, input bin.Encoder, output bin.Decoder) error {
	const channelID int64 = 72
	const channelHash int64 = 94
	switch request := input.(type) {
	case *tg.ContactsResolveUsernameRequest:
		rpc.usernameLookupCalls++
		return tgerr.New(420, "FLOOD_WAIT_86400")
	case *tg.ContactsSearchRequest:
		rpc.usernameLookupCalls++
		return tgerr.New(420, "FLOOD_WAIT_86400")
	case *tg.MessagesGetDialogsRequest:
		response, ok := output.(*tg.MessagesDialogsBox)
		if !ok {
			return fmt.Errorf("unexpected dialogs output %T", output)
		}
		response.Dialogs = &tg.MessagesDialogs{Chats: []tg.ChatClass{
			&tg.Channel{ID: 71, AccessHash: 93, Username: demo.GeneralChatHandle, Title: demoGeneralChatSpace.title, Megagroup: true},
			&tg.Channel{ID: channelID, AccessHash: channelHash, Username: demo.GeneralChatHandles()[1], Title: demoGeneralChatSpace.title, Megagroup: true, Creator: true},
		}}
	case *tg.ChannelsGetFullChannelRequest:
		return fmt.Errorf("cleanup must not fetch General description: %+v", request.Channel)
	case *tg.ChannelsGetParticipantsRequest:
		channel, ok := request.Channel.(*tg.InputChannel)
		if !ok || channel.ChannelID != channelID || channel.AccessHash != channelHash || request.Limit != 200 {
			return fmt.Errorf("unexpected moderation guest roster request: %+v", request)
		}
		var includeGuest bool
		switch filter := request.Filter.(type) {
		case *tg.ChannelParticipantsSearch:
			if filter.Q != moderationSmokeShortUsername {
				return fmt.Errorf("member filter = %+v, want smoke username", filter)
			}
		case *tg.ChannelParticipantsBanned:
			if filter.Q != moderationSmokeShortUsername {
				return fmt.Errorf("banned filter = %+v, want smoke username", filter)
			}
			includeGuest = true
		default:
			return fmt.Errorf("unexpected moderation guest roster filter: %T %+v", request.Filter, request.Filter)
		}
		response, ok := output.(*tg.ChannelsChannelParticipantsBox)
		if !ok {
			return fmt.Errorf("unexpected participant roster output %T", output)
		}
		roster := &tg.ChannelsChannelParticipants{}
		if includeGuest {
			roster.Count = 1
			roster.Users = []tg.UserClass{&tg.User{ID: 42, AccessHash: 43, Username: moderationSmokeShortUsername}}
			roster.Participants = []tg.ChannelParticipantClass{&tg.ChannelParticipantBanned{Peer: &tg.PeerUser{UserID: 42}}}
		}
		response.ChannelParticipants = roster
	case *tg.ChannelsEditBannedRequest:
		channel, ok := request.Channel.(*tg.InputChannel)
		if !ok || channel.ChannelID != channelID || channel.AccessHash != channelHash {
			return fmt.Errorf("unexpected unban channel: %+v", request.Channel)
		}
		target, ok := request.Participant.(*tg.InputPeerUser)
		if !ok || target.UserID != 42 || target.AccessHash != 43 || request.BannedRights.ViewMessages || request.BannedRights.UntilDate != 0 {
			return fmt.Errorf("unexpected unban target or rights: %+v", request)
		}
		rpc.unbanCalls++
		updates, ok := output.(*tg.UpdatesBox)
		if !ok {
			return fmt.Errorf("unexpected unban output %T", output)
		}
		updates.Updates = &tg.Updates{}
	case *tg.ChannelsInviteToChannelRequest:
		channel, ok := request.Channel.(*tg.InputChannel)
		if !ok || channel.ChannelID != channelID || channel.AccessHash != channelHash || len(request.Users) != 1 {
			return fmt.Errorf("unexpected invite request: %+v", request)
		}
		user, ok := request.Users[0].(*tg.InputUser)
		if !ok || user.UserID != 42 || user.AccessHash != 43 {
			return fmt.Errorf("unexpected invite target: %+v", request.Users)
		}
		rpc.inviteCalls++
		if rpc.unbanCalls != 1 {
			return errors.New("smoke guest was invited before its ban was cleared")
		}
		rpc.memberRestored = true
		if _, ok := output.(*tg.MessagesInvitedUsers); !ok {
			return fmt.Errorf("unexpected invite output %T", output)
		}
	case *tg.ChannelsGetParticipantRequest:
		channel, ok := request.Channel.(*tg.InputChannel)
		if !ok || channel.ChannelID != channelID || channel.AccessHash != channelHash {
			return fmt.Errorf("unexpected membership check channel: %+v", request.Channel)
		}
		target, ok := request.Participant.(*tg.InputPeerUser)
		if !ok || target.UserID != 42 || target.AccessHash != 43 {
			return fmt.Errorf("unexpected membership check target: %+v", request.Participant)
		}
		participant, ok := output.(*tg.ChannelsChannelParticipant)
		if !ok {
			return fmt.Errorf("unexpected participant output %T", output)
		}
		rpc.participantChecks++
		if !rpc.memberRestored {
			return tgerr.New(400, "USER_NOT_PARTICIPANT")
		}
		participant.Participant = &tg.ChannelParticipant{UserID: 42}
	case *tg.MessagesGetHistoryRequest:
		channel, ok := request.Peer.(*tg.InputPeerChannel)
		if !ok || channel.ChannelID != channelID || channel.AccessHash != channelHash {
			return fmt.Errorf("unexpected General history peer: %+v", request.Peer)
		}
		response, ok := output.(*tg.MessagesMessagesBox)
		if !ok {
			return fmt.Errorf("unexpected history output %T", output)
		}
		response.Messages = &tg.MessagesChannelMessages{Messages: []tg.MessageClass{
			&tg.Message{ID: 81, Message: moderationSmokeMessage(strings.Repeat("a", 32)).text},
		}}
	case *tg.ChannelsDeleteMessagesRequest:
		channel, ok := request.Channel.(*tg.InputChannel)
		if !ok || channel.ChannelID != channelID || channel.AccessHash != channelHash || len(request.ID) != 1 || request.ID[0] != 81 {
			return fmt.Errorf("unexpected moderation marker delete request: %+v", request)
		}
		result, ok := output.(*tg.MessagesAffectedMessages)
		if !ok {
			return fmt.Errorf("unexpected delete output %T", output)
		}
		result.PtsCount = 1
		rpc.deletedMessageID = request.ID[0]
	default:
		return fmt.Errorf("unexpected moderation cleanup request %T", input)
	}
	return nil
}
