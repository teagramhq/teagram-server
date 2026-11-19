package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/demo"
)

const (
	moderationSmokeShortUsername      = "ab"
	defaultModerationSmokeSessionFile = "/run/secrets/demo-accounts/moderation-smoke.session"
)

func validModerationSmokeMarker(marker string) bool {
	if len(marker) != 32 {
		return false
	}
	for _, char := range marker {
		if char < '0' || char > '9' {
			if char < 'a' || char > 'f' {
				return false
			}
		}
	}
	return true
}

func moderationSmokeMessage(marker string) seedMessage {
	markerText := "[[teagram-demo-moderation-smoke:" + marker + "]]"
	return seedMessage{marker: markerText, text: "General moderation smoke " + markerText}
}

func prepareGeneralModerationSmoke(
	ctx context.Context,
	cfg apiConfig,
	options seedOptions,
	ownerPassword string,
) error {
	if !validModerationSmokeMarker(options.moderationSmokeMarker) {
		return errors.New("demo General moderation smoke marker is invalid")
	}
	if _, err := os.Lstat(defaultModerationSmokeSessionFile); err == nil {
		return errors.New("stale General moderation smoke session already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect General moderation smoke session: %w", err)
	}

	if err := createModerationSmokeGuest(ctx, cfg, options.endpoint); err != nil {
		cleanupOptions := options
		cleanupOptions.moderationSmokeCleanup = true
		cleanupOptions.moderationSmokePrepare = false
		cleanupOptions.moderationSmokeMarker = options.moderationSmokeMarker
		cleanupErr := cleanupGeneralModerationSmoke(ctx, cfg, cleanupOptions, ownerPassword)
		return errors.Join(fmt.Errorf("prepare short-handle General guest: %w", err), cleanupErr)
	}
	if err := verifyModerationSmokeSessionFile(); err != nil {
		cleanupOptions := options
		cleanupOptions.moderationSmokeCleanup = true
		cleanupOptions.moderationSmokePrepare = false
		cleanupErr := cleanupGeneralModerationSmoke(ctx, cfg, cleanupOptions, ownerPassword)
		return errors.Join(err, cleanupErr)
	}
	if err := withModerationSmokeGuest(ctx, cfg, options.endpoint, func(_ context.Context, _ *tg.Client, _ *tg.User) error {
		return nil
	}); err != nil {
		cleanupOptions := options
		cleanupOptions.moderationSmokeCleanup = true
		cleanupOptions.moderationSmokePrepare = false
		cleanupErr := cleanupGeneralModerationSmoke(ctx, cfg, cleanupOptions, ownerPassword)
		return errors.Join(fmt.Errorf("verify new General moderation smoke guest: %w", err), cleanupErr)
	}

	var messageID int64
	err := withSeedOwner(ctx, cfg, options, ownerPassword, func(runCtx context.Context, api *tg.Client) error {
		remote, _, err := resolveOwnedGeneralChat(runCtx, api, demoGeneralChatSpace)
		if err != nil {
			return err
		}
		if !remote.creator {
			return errors.New("seed owner does not own General during moderation smoke preparation")
		}
		messageID, err = sendText(runCtx, api, remote.peer(), moderationSmokeMessage(options.moderationSmokeMarker), 0)
		if err != nil {
			return fmt.Errorf("create General moderation smoke post: %w", err)
		}
		return nil
	})
	if err != nil {
		cleanupOptions := options
		cleanupOptions.moderationSmokeCleanup = true
		cleanupOptions.moderationSmokePrepare = false
		cleanupOptions.moderationSmokeMarker = options.moderationSmokeMarker
		cleanupErr := cleanupGeneralModerationSmoke(ctx, cfg, cleanupOptions, ownerPassword)
		return errors.Join(err, cleanupErr)
	}
	fmt.Printf("general moderation smoke prepared message_id=%d\n", messageID)
	return nil
}

func verifyGeneralModerationSmoke(ctx context.Context, cfg apiConfig, options seedOptions, ownerPassword string) error {
	if !validModerationSmokeMarker(options.moderationSmokeMarker) || options.moderationSmokeMessageID <= 0 {
		return errors.New("demo General moderation smoke verification arguments are invalid")
	}
	var guest *tg.User
	if err := withModerationSmokeGuest(ctx, cfg, options.endpoint, func(_ context.Context, _ *tg.Client, user *tg.User) error {
		guest = user
		return nil
	}); err != nil {
		return err
	}

	var general remoteSpace
	if err := withSeedOwner(ctx, cfg, options, ownerPassword, func(runCtx context.Context, api *tg.Client) error {
		var err error
		general, _, err = resolveOwnedGeneralChat(runCtx, api, demoGeneralChatSpace)
		if err != nil {
			return err
		}
		if !general.creator {
			return errors.New("seed owner does not own General during moderation smoke verification")
		}
		messages, err := historyMessages(runCtx, api, general.peer())
		if err != nil {
			return err
		}
		for _, candidate := range messages {
			message, ok := candidate.(*tg.Message)
			if ok && (int64(message.ID) == options.moderationSmokeMessageID || message.Message == moderationSmokeMessage(options.moderationSmokeMarker).text) {
				return errors.New("demo General moderation smoke post remains visible after delete-message")
			}
		}

		resolvedGuest, err := resolveModerationSmokeGuest(runCtx, api, guest.ID, moderationSmokeShortUsername)
		if err != nil {
			return err
		}
		participant, err := api.ChannelsGetParticipant(runCtx, &tg.ChannelsGetParticipantRequest{
			Channel: general.inputChannel(), Participant: &tg.InputPeerUser{UserID: resolvedGuest.ID, AccessHash: resolvedGuest.AccessHash},
		})
		if err != nil {
			return fmt.Errorf("verify General smoke guest ban: %w", err)
		}
		banned, ok := participant.Participant.(*tg.ChannelParticipantBanned)
		if !ok || !banned.BannedRights.ViewMessages || banned.BannedRights.UntilDate != 0 {
			return errors.New("short-handle General smoke guest does not have a permanent view ban")
		}
		peer, ok := banned.Peer.(*tg.PeerUser)
		if !ok || peer.UserID != guest.ID {
			return errors.New("demo General smoke ban belongs to a different user")
		}
		return nil
	}); err != nil {
		return err
	}

	err := withModerationSmokeGuest(ctx, cfg, options.endpoint, func(runCtx context.Context, api *tg.Client, user *tg.User) error {
		if user.ID != guest.ID || !strings.EqualFold(user.Username, moderationSmokeShortUsername) {
			return errors.New("demo General moderation smoke guest identity changed before rejoin check")
		}
		channel, err := resolveGeneralChatForGuest(runCtx, api)
		if err != nil {
			return err
		}
		_, err = api.ChannelsJoinChannel(runCtx, channel.inputChannel())
		if !isRPCMessage(err, "PEER_ID_INVALID") {
			if err == nil {
				return errors.New("permanently banned short-handle guest rejoined General")
			}
			return fmt.Errorf("short-handle guest rejoin returned %w, want PEER_ID_INVALID", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	fmt.Println("general moderation smoke verified deleted post and refused banned guest rejoin")
	return nil
}

func cleanupGeneralModerationSmoke(ctx context.Context, cfg apiConfig, options seedOptions, ownerPassword string) error {
	if !validModerationSmokeMarker(options.moderationSmokeMarker) {
		return errors.New("demo General moderation smoke marker is invalid")
	}
	var guest *tg.User
	guestErr := withModerationSmokeGuest(ctx, cfg, options.endpoint, func(_ context.Context, _ *tg.Client, user *tg.User) error {
		guest = user
		return nil
	})
	if errors.Is(guestErr, os.ErrNotExist) {
		guestErr = nil
	}

	ownerErr := withSeedOwner(ctx, cfg, options, ownerPassword, func(runCtx context.Context, api *tg.Client) error {
		remote, _, err := resolveOwnedGeneralChat(runCtx, api, demoGeneralChatSpace)
		if err != nil {
			return err
		}
		if !remote.creator {
			return errors.New("seed owner does not own General during moderation smoke cleanup")
		}
		if guest != nil {
			resolved, err := resolveModerationSmokeGuest(runCtx, api, guest.ID, guest.Username)
			if err != nil {
				return err
			}
			if err := restoreModerationSmokeGuestMembership(runCtx, api, remote, resolved); err != nil {
				return err
			}
		}
		messages, err := historyMessages(runCtx, api, remote.peer())
		if err != nil {
			return err
		}
		var ids []int
		for _, candidate := range messages {
			message, ok := candidate.(*tg.Message)
			if ok && message.Message == moderationSmokeMessage(options.moderationSmokeMarker).text {
				ids = append(ids, message.ID)
			}
		}
		if len(ids) != 0 {
			if _, err := api.ChannelsDeleteMessages(runCtx, &tg.ChannelsDeleteMessagesRequest{
				Channel: remote.inputChannel(), ID: ids,
			}); err != nil {
				return fmt.Errorf("delete leftover General moderation smoke post: %w", err)
			}
		}
		return nil
	})

	if err := errors.Join(guestErr, ownerErr); err != nil {
		return err
	}
	if guest != nil {
		if err := removeModerationSmokeSessionFile(); err != nil {
			return err
		}
	}
	fmt.Println("general moderation smoke post removed and guest membership restored")
	return nil
}

func restoreModerationSmokeGuestMembership(ctx context.Context, api *tg.Client, remote remoteSpace, guest *tg.User) error {
	if remote.channelID <= 0 || remote.accessHash == 0 || !remote.creator {
		return errors.New("refuse to restore a smoke guest in a General chat not owned by the seed account")
	}
	if guest == nil || guest.ID <= 0 || guest.AccessHash == 0 || guest.Bot {
		return errors.New("refuse to restore an invalid General moderation smoke guest")
	}
	channel := remote.inputChannel()
	participant := &tg.InputPeerUser{UserID: guest.ID, AccessHash: guest.AccessHash}
	if _, err := api.ChannelsEditBanned(ctx, &tg.ChannelsEditBannedRequest{
		Channel: channel, Participant: participant, BannedRights: tg.ChatBannedRights{},
	}); err != nil {
		return fmt.Errorf("clear General moderation smoke ban: %w", err)
	}
	if _, err := api.ChannelsInviteToChannel(ctx, &tg.ChannelsInviteToChannelRequest{
		Channel: channel, Users: []tg.InputUserClass{&tg.InputUser{UserID: guest.ID, AccessHash: guest.AccessHash}},
	}); err != nil && !isRPCMessage(err, "USER_ALREADY_PARTICIPANT") && !isRPCMessage(err, "USER_ALREADY_IN_CHANNEL") {
		return fmt.Errorf("restore moderation smoke guest to General after cleanup: %w", err)
	}
	result, err := api.ChannelsGetParticipant(ctx, &tg.ChannelsGetParticipantRequest{
		Channel: channel, Participant: participant,
	})
	if err != nil {
		return fmt.Errorf("verify moderation smoke guest restored to General after cleanup: %w", err)
	}
	member, ok := result.Participant.(*tg.ChannelParticipant)
	if !ok || member.UserID != guest.ID {
		return errors.New("moderation smoke guest is not a General member after cleanup")
	}
	return nil
}

type moderationSmokeSignupAPI interface {
	authSendCodeAPI
	AuthSignUp(context.Context, *tg.AuthSignUpRequest) (tg.AuthAuthorizationClass, error)
}

type moderationSmokeProfileAPI interface {
	UsersGetFullUser(context.Context, tg.InputUserClass) (*tg.UsersUserFull, error)
}

type moderationSmokeGuestAPI interface {
	moderationSmokeSignupAPI
	moderationSmokeProfileAPI
	passwordSetupAPI
}

func prepareModerationSmokeGuest(ctx context.Context, api moderationSmokeSignupAPI) (*tg.User, error) {
	sent, err := sendAuthCode(ctx, api, moderationSmokeShortUsername)
	if err != nil {
		return nil, fmt.Errorf("request General moderation smoke guest sign-up code: %w", err)
	}
	sentCode, ok := sent.(*tg.AuthSentCode)
	if !ok || sentCode.PhoneCodeHash == "" {
		return nil, fmt.Errorf("unexpected General moderation smoke sign-up code response %T", sent)
	}
	authorization, err := api.AuthSignUp(ctx, &tg.AuthSignUpRequest{
		PhoneNumber:   moderationSmokeShortUsername,
		PhoneCodeHash: sentCode.PhoneCodeHash,
		FirstName:     "Moderation",
		LastName:      "Smoke",
	})
	if err != nil {
		return nil, fmt.Errorf("create General moderation smoke guest through auth.signUp: %w", err)
	}
	result, ok := authorization.(*tg.AuthAuthorization)
	if !ok {
		return nil, fmt.Errorf("unexpected General moderation smoke sign-up response %T", authorization)
	}
	user, ok := result.User.(*tg.User)
	if !ok || user.ID <= 0 || user.Bot {
		return nil, errors.New("auth.signUp did not create an ordinary General moderation smoke guest")
	}
	return user, nil
}

func verifyModerationSmokeGuestProfile(ctx context.Context, api moderationSmokeProfileAPI, userID int64) error {
	full, err := api.UsersGetFullUser(ctx, &tg.InputUserSelf{})
	if err != nil {
		return fmt.Errorf("inspect new General moderation smoke guest: %w", err)
	}
	if len(full.Users) != 1 {
		return fmt.Errorf("demo General moderation smoke guest profile count = %d, want 1", len(full.Users))
	}
	profile, ok := full.Users[0].(*tg.User)
	if !ok || profile.ID != userID || profile.Bot || !strings.EqualFold(profile.Username, moderationSmokeShortUsername) {
		return errors.New("auth.signUp did not create the expected short-handle ordinary guest")
	}
	return nil
}

func signUpModerationSmokeGuest(ctx context.Context, api moderationSmokeGuestAPI, password string) error {
	user, err := prepareModerationSmokeGuest(ctx, api)
	if err != nil {
		return err
	}
	if err := setNewPassword(ctx, api, password); err != nil {
		return fmt.Errorf("secure General moderation smoke guest: %w", err)
	}
	return verifyModerationSmokeGuestProfile(ctx, api, user.ID)
}

func createModerationSmokeGuest(ctx context.Context, cfg apiConfig, endpoint string) error {
	clientOptions, err := accountClientOptions(cfg, endpoint, &session.FileStorage{Path: defaultModerationSmokeSessionFile})
	if err != nil {
		return err
	}
	return runTelegramClient(ctx, clientOptions, func(runCtx context.Context, client *telegram.Client) error {
		password, err := randomPassword()
		if err != nil {
			return fmt.Errorf("generate General moderation smoke guest password: %w", err)
		}
		return signUpModerationSmokeGuest(runCtx, client.API(), password)
	})
}

func withModerationSmokeGuest(
	ctx context.Context,
	cfg apiConfig,
	endpoint string,
	action func(context.Context, *tg.Client, *tg.User) error,
) error {
	if err := verifyModerationSmokeSessionFile(); err != nil {
		return err
	}
	clientOptions, err := accountClientOptions(cfg, endpoint, &session.FileStorage{Path: defaultModerationSmokeSessionFile})
	if err != nil {
		return err
	}
	return runTelegramClient(ctx, clientOptions, func(runCtx context.Context, client *telegram.Client) error {
		status, err := client.Auth().Status(runCtx)
		if err != nil {
			return fmt.Errorf("check General moderation smoke guest session: %w", err)
		}
		if !status.Authorized || status.User == nil || status.User.ID <= 0 || status.User.Bot {
			return errors.New("demo General moderation smoke guest session is not authorized")
		}
		username := strings.ToLower(status.User.Username)
		if username != moderationSmokeShortUsername {
			return fmt.Errorf("unexpected General moderation smoke guest username %q", status.User.Username)
		}
		return action(runCtx, client.API(), status.User)
	})
}

func verifyModerationSmokeSessionFile() error {
	info, err := os.Lstat(defaultModerationSmokeSessionFile)
	if err != nil {
		return fmt.Errorf("inspect General moderation smoke guest session: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return errors.New("demo General moderation smoke guest session must be a regular mode 0600 file")
	}
	return nil
}

func removeModerationSmokeSessionFile() error {
	if err := verifyModerationSmokeSessionFile(); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := os.Remove(defaultModerationSmokeSessionFile); err != nil {
		return fmt.Errorf("remove General moderation smoke guest session: %w", err)
	}
	return nil
}

func resolveModerationSmokeGuest(ctx context.Context, api *tg.Client, userID int64, username string) (*tg.User, error) {
	resolved, err := api.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{Username: username})
	if err != nil {
		return nil, fmt.Errorf("resolve General moderation smoke guest: %w", err)
	}
	for _, candidate := range resolved.Users {
		user, ok := candidate.(*tg.User)
		if ok && user.ID == userID && strings.EqualFold(user.Username, username) && user.AccessHash != 0 && !user.Bot {
			return user, nil
		}
	}
	return nil, errors.New("demo General moderation smoke handle does not resolve to the smoke guest")
}

func resolveGeneralChatForGuest(ctx context.Context, api *tg.Client) (remoteSpace, error) {
	for _, username := range demo.GeneralChatHandles() {
		resolved, err := api.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{Username: username})
		if isRPCMessage(err, "USERNAME_NOT_OCCUPIED") {
			continue
		}
		if err != nil {
			return remoteSpace{}, fmt.Errorf("resolve General handle %s for guest rejoin test: %w", username, err)
		}
		channel, ok := resolvedChannel(resolved)
		if !ok || channel.Username != username || channel.Title != demoGeneralChatSpace.title || !channel.Megagroup {
			return remoteSpace{}, fmt.Errorf("reserved General handle %s resolved to a different channel", username)
		}
		return remoteSpace{spec: demoGeneralChatSpace, channelID: channel.ID, accessHash: channel.AccessHash}, nil
	}
	return remoteSpace{}, errors.New("no reserved General handle resolves during the guest rejoin test")
}
