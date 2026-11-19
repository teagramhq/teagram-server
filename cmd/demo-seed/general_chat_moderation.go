package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/gotd/td/tg"
)

func moderateGeneralChat(ctx context.Context, cfg apiConfig, options seedOptions, ownerPassword string) error {
	return withSeedOwner(ctx, cfg, options, ownerPassword, func(runCtx context.Context, api *tg.Client) error {
		if options.moderateDeleteMessageID != 0 {
			if err := deleteGeneralChatMessage(runCtx, api, int(options.moderateDeleteMessageID)); err != nil {
				return err
			}
			fmt.Printf("General message deleted id=%d\n", options.moderateDeleteMessageID)
			return nil
		}
		if err := banGeneralChatGuest(runCtx, api, options.moderateBanUsername); err != nil {
			return err
		}
		fmt.Printf("General guest banned username=%s\n", options.moderateBanUsername)
		return nil
	})
}

func deleteGeneralChatMessage(ctx context.Context, api *tg.Client, messageID int) error {
	if messageID <= 0 || int64(messageID) > math.MaxInt32 {
		return errors.New("general message ID must be positive and fit in signed 32-bit MTProto range")
	}
	remote, err := resolveModerationGeneralChat(ctx, api)
	if err != nil {
		return err
	}
	if _, err := api.ChannelsDeleteMessages(ctx, &tg.ChannelsDeleteMessagesRequest{
		Channel: remote.inputChannel(),
		ID:      []int{messageID},
	}); err != nil {
		return fmt.Errorf("delete General message %d: %w", messageID, err)
	}
	return nil
}

func banGeneralChatGuest(ctx context.Context, api *tg.Client, username string) error {
	if !validGeneralChatUsername(username) {
		return errors.New("general guest username is invalid")
	}
	username = strings.ToLower(username)
	remote, err := resolveModerationGeneralChat(ctx, api)
	if err != nil {
		return err
	}
	resolved, err := api.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{Username: username})
	if err != nil {
		return fmt.Errorf("resolve General guest %s: %w", username, err)
	}
	var user *tg.User
	for _, candidate := range resolved.Users {
		if found, ok := candidate.(*tg.User); ok && found.Username == username {
			user = found
			break
		}
	}
	if user == nil || user.ID <= 0 || user.AccessHash == 0 || user.Bot {
		return fmt.Errorf("general guest %s is not a resolvable user account", username)
	}
	userPeer := &tg.InputPeerUser{UserID: user.ID, AccessHash: user.AccessHash}
	participant, err := api.ChannelsGetParticipant(ctx, &tg.ChannelsGetParticipantRequest{
		Channel: remote.inputChannel(), Participant: userPeer,
	})
	if err != nil {
		return fmt.Errorf("check General membership for %s: %w", username, err)
	}
	switch member := participant.Participant.(type) {
	case *tg.ChannelParticipant:
		if member.UserID != user.ID {
			return errors.New("general membership target did not match the requested guest")
		}
	case *tg.ChannelParticipantBanned:
		bannedPeer, ok := member.Peer.(*tg.PeerUser)
		if ok && bannedPeer.UserID == user.ID {
			if member.BannedRights.ViewMessages && member.BannedRights.UntilDate == 0 {
				return nil
			}
		} else {
			return errors.New("general ban record did not match the requested guest")
		}
	default:
		return fmt.Errorf("general user %s is not an ordinary guest member", username)
	}
	rights := tg.ChatBannedRights{}
	rights.SetViewMessages(true)
	if _, err := api.ChannelsEditBanned(ctx, &tg.ChannelsEditBannedRequest{
		Channel:      remote.inputChannel(),
		Participant:  userPeer,
		BannedRights: rights,
	}); err != nil {
		return fmt.Errorf("ban General guest %s: %w", username, err)
	}
	return nil
}

func resolveModerationGeneralChat(ctx context.Context, api *tg.Client) (remoteSpace, error) {
	remote, _, err := resolveOwnedGeneralChat(ctx, api, demoGeneralChatSpace)
	if err != nil {
		return remoteSpace{}, fmt.Errorf("resolve General chat for moderation: %w", err)
	}
	if remote.channelID == 0 || !remote.creator {
		return remoteSpace{}, errors.New("demo_seed_owner does not own General chat")
	}
	return remote, nil
}
