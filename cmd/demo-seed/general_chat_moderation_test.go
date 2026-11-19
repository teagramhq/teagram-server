package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

func TestModerateGeneralChatDeletesRequestedMessage(t *testing.T) {
	rpc := &generalModerationRPC{targetRole: "member"}
	api := tg.NewClient(rpc)
	if err := deleteGeneralChatMessage(context.Background(), api, 81); err != nil {
		t.Fatalf("delete General message: %v", err)
	}
	if rpc.deletedMessageID != 81 || rpc.deleteCount != 1 {
		t.Fatalf("deleted General message IDs = %d after %d calls, want 81 once", rpc.deletedMessageID, rpc.deleteCount)
	}
}

func TestModerateGeneralChatPermanentlyBansMemberOnly(t *testing.T) {
	rpc := &generalModerationRPC{targetRole: "member"}
	api := tg.NewClient(rpc)
	if err := banGeneralChatGuest(context.Background(), api, "freshguest"); err != nil {
		t.Fatalf("ban General guest: %v", err)
	}
	if rpc.banCount != 1 || !rpc.bannedViewMessages || rpc.bannedUntilDate != 0 {
		t.Fatalf("General ban calls=%d view=%t until=%d, want one permanent view ban", rpc.banCount, rpc.bannedViewMessages, rpc.bannedUntilDate)
	}

	adminRPC := &generalModerationRPC{targetRole: "admin"}
	if err := banGeneralChatGuest(context.Background(), tg.NewClient(adminRPC), "freshguest"); err == nil {
		t.Fatal("moderation command banned a General administrator")
	}
	if adminRPC.banCount != 0 {
		t.Fatalf("General administrator ban calls = %d, want none", adminRPC.banCount)
	}
}

func TestModerateGeneralChatPermanentlyBansShortSignupUsername(t *testing.T) {
	rpc := &generalModerationRPC{targetRole: "member"}
	if err := banGeneralChatGuest(context.Background(), tg.NewClient(rpc), "ab"); err != nil {
		t.Fatalf("ban short-handle General guest: %v", err)
	}
	if rpc.banCount != 1 || !rpc.bannedViewMessages || rpc.bannedUntilDate != 0 {
		t.Fatalf("short-handle General ban calls=%d view=%t until=%d, want one permanent view ban", rpc.banCount, rpc.bannedViewMessages, rpc.bannedUntilDate)
	}
}

func TestModerateGeneralChatNormalizesUsernameBeforeResolve(t *testing.T) {
	rpc := &generalModerationRPC{targetRole: "member"}
	if err := banGeneralChatGuest(context.Background(), tg.NewClient(rpc), "AB"); err != nil {
		t.Fatalf("ban short-handle General guest with mixed-case argument: %v", err)
	}
	if rpc.resolvedUsername != "ab" {
		t.Fatalf("resolved username = %q, want normalized lowercase username", rpc.resolvedUsername)
	}
	if rpc.banCount != 1 {
		t.Fatalf("mixed-case short-handle ban calls = %d, want one", rpc.banCount)
	}
}

func TestModerateGeneralChatUpgradesTemporaryBanToPermanent(t *testing.T) {
	rights := tg.ChatBannedRights{ViewMessages: true, UntilDate: int(time.Now().Add(time.Hour).Unix())}
	rights.SetFlags()
	rpc := &generalModerationRPC{targetRole: "banned", bannedRights: rights}
	if err := banGeneralChatGuest(context.Background(), tg.NewClient(rpc), "freshguest"); err != nil {
		t.Fatalf("permanently ban temporarily banned General guest: %v", err)
	}
	if rpc.banCount != 1 || !rpc.bannedViewMessages || rpc.bannedUntilDate != 0 {
		t.Fatalf("General ban calls=%d view=%t until=%d, want one permanent view ban", rpc.banCount, rpc.bannedViewMessages, rpc.bannedUntilDate)
	}
}

func TestModerateGeneralChatKeepsPermanentBanIdempotent(t *testing.T) {
	rights := tg.ChatBannedRights{ViewMessages: true}
	rights.SetFlags()
	rpc := &generalModerationRPC{targetRole: "banned", bannedRights: rights}
	if err := banGeneralChatGuest(context.Background(), tg.NewClient(rpc), "freshguest"); err != nil {
		t.Fatalf("re-ban permanently banned General guest: %v", err)
	}
	if rpc.banCount != 0 {
		t.Fatalf("already-permanent General ban updates = %d, want none", rpc.banCount)
	}
}

type generalModerationRPC struct {
	targetRole         string
	bannedRights       tg.ChatBannedRights
	resolvedUsername   string
	deletedMessageID   int
	deleteCount        int
	banCount           int
	bannedViewMessages bool
	bannedUntilDate    int
}

func (rpc *generalModerationRPC) Invoke(_ context.Context, input bin.Encoder, output bin.Decoder) error {
	switch request := input.(type) {
	case *tg.ContactsResolveUsernameRequest:
		resolved, ok := output.(*tg.ContactsResolvedPeer)
		if !ok {
			return fmt.Errorf("unexpected resolved-peer output %T", output)
		}
		if request.Username == demoGeneralChatUsername {
			resolved.Chats = []tg.ChatClass{&tg.Channel{
				ID: 71, AccessHash: 93, Username: demoGeneralChatUsername, Title: "General", Megagroup: true,
			}}
			return nil
		}
		rpc.resolvedUsername = request.Username
		if request.Username != "freshguest" && request.Username != "ab" {
			return fmt.Errorf("unexpected moderation target %q", request.Username)
		}
		resolved.Users = []tg.UserClass{&tg.User{ID: 42, AccessHash: 43, Username: strings.ToLower(request.Username)}}
	case *tg.ChannelsGetParticipantRequest:
		participant, ok := output.(*tg.ChannelsChannelParticipant)
		if !ok {
			return fmt.Errorf("unexpected participant output %T", output)
		}
		if _, self := request.Participant.(*tg.InputPeerSelf); self {
			participant.Participant = &tg.ChannelParticipantCreator{}
			return nil
		}
		if rpc.targetRole == "missing" {
			return tgerr.New(400, "USER_NOT_PARTICIPANT")
		}
		if rpc.targetRole == "banned" {
			participant.Participant = &tg.ChannelParticipantBanned{
				Peer:         &tg.PeerUser{UserID: 42},
				BannedRights: rpc.bannedRights,
			}
			return nil
		}
		if rpc.targetRole == "admin" {
			participant.Participant = &tg.ChannelParticipantAdmin{UserID: 42}
		} else {
			participant.Participant = &tg.ChannelParticipant{UserID: 42}
		}
	case *tg.ChannelsDeleteMessagesRequest:
		channel, ok := request.Channel.(*tg.InputChannel)
		if !ok || channel.ChannelID != 71 || channel.AccessHash != 93 || len(request.ID) != 1 {
			return fmt.Errorf("unexpected General delete request: %+v", request)
		}
		rpc.deletedMessageID = request.ID[0]
		rpc.deleteCount++
		result, ok := output.(*tg.MessagesAffectedMessages)
		if !ok {
			return fmt.Errorf("unexpected delete output %T", output)
		}
		result.PtsCount = 1
	case *tg.ChannelsEditBannedRequest:
		channel, ok := request.Channel.(*tg.InputChannel)
		if !ok || channel.ChannelID != 71 || channel.AccessHash != 93 {
			return fmt.Errorf("unexpected General ban channel: %+v", request.Channel)
		}
		target, ok := request.Participant.(*tg.InputPeerUser)
		if !ok || target.UserID != 42 || target.AccessHash != 43 {
			return fmt.Errorf("unexpected General ban target: %+v", request.Participant)
		}
		rpc.bannedViewMessages = request.BannedRights.ViewMessages
		rpc.bannedUntilDate = request.BannedRights.UntilDate
		rpc.banCount++
		updates, ok := output.(*tg.UpdatesBox)
		if !ok {
			return fmt.Errorf("unexpected ban output %T", output)
		}
		updates.Updates = &tg.Updates{}
	default:
		return fmt.Errorf("unexpected General moderation request %T", input)
	}
	return nil
}

func TestParseOptionsGeneralChatModeration(t *testing.T) {
	t.Setenv("TG_DEMO_GENERAL_CHAT_ENABLED", "true")
	options, err := parseOptions([]string{"--moderate-delete-message", "81"})
	if err != nil || options.moderateDeleteMessageID != 81 {
		t.Fatalf("parse delete-message options = %+v, %v; want message id 81", options, err)
	}
	options, err = parseOptions([]string{"--moderate-ban-user", "freshguest"})
	if err != nil || options.moderateBanUsername != "freshguest" {
		t.Fatalf("parse ban-user options = %+v, %v; want username freshguest", options, err)
	}
	if _, err := parseOptions([]string{"--moderate-delete-message", "0"}); err == nil {
		t.Fatal("parseOptions accepted message ID zero")
	}
	options, err = parseOptions([]string{"--moderate-delete-message", "2147483647"})
	if err != nil || options.moderateDeleteMessageID != 2147483647 {
		t.Fatalf("parseOptions rejected MaxInt32 message ID: options=%+v err=%v", options, err)
	}
	if _, err := parseOptions([]string{"--moderate-delete-message", "2147483648"}); err == nil {
		t.Fatal("parseOptions accepted a message ID above MaxInt32")
	}
	if _, err := parseOptions([]string{"--moderate-ban-user", "@freshguest"}); err == nil {
		t.Fatal("parseOptions accepted a username with an @ prefix")
	}
}

func TestGeneralChatModerationIsDisabledWithFeatureFlag(t *testing.T) {
	t.Setenv("TG_DEMO_GENERAL_CHAT_ENABLED", "false")
	if _, err := parseOptions([]string{"--moderate-delete-message", "81"}); err == nil {
		t.Fatal("parseOptions accepted moderation while General chat is disabled")
	}
}

func TestBanGeneralChatGuestRequiresExistingMembership(t *testing.T) {
	rpc := &generalModerationRPC{targetRole: "missing"}
	if err := banGeneralChatGuest(context.Background(), tg.NewClient(rpc), "freshguest"); err == nil {
		t.Fatal("moderation command banned a user who is not a General member")
	}
	if rpc.banCount != 0 {
		t.Fatalf("missing-member ban calls = %d, want none", rpc.banCount)
	}
}
