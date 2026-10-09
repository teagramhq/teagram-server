package e2e_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"
)

func TestLaunchGetterResults(t *testing.T) {
	t.Parallel()
	f := newHelpPollingFixture(t)
	const phone = "+15551239982"
	seedPhoneUsers(t, f.ctx, f.store, phone)
	flow := auth.NewFlow(
		auth.Constant(phone, "", auth.CodeAuthenticatorFunc(func(ctx context.Context, _ *tg.AuthSentCode) (string, error) {
			return f.codes.wait(ctx)
		})),
		auth.SendCodeOptions{},
	)
	client := f.newClient(&session.StorageMemory{})
	if err := client.Run(f.ctx, func(ctx context.Context) error {
		if err := client.Auth().IfNecessary(ctx, flow); err != nil {
			return fmt.Errorf("login: %w", err)
		}
		return checkLaunchGetterResults(ctx, client.API())
	}); err != nil {
		t.Fatalf("launch getter calls: %v", err)
	}
}

func checkLaunchGetterResults(ctx context.Context, client *tg.Client) error {
	for _, hash := range []int64{0, 1} {
		topReactions, err := client.MessagesGetTopReactions(ctx, &tg.MessagesGetTopReactionsRequest{Limit: 20, Hash: hash})
		if err != nil {
			return fmt.Errorf("messages.getTopReactions: %w", err)
		}
		if err := assertEmptyReactionsGetter("messages.getTopReactions", hash, topReactions); err != nil {
			return err
		}

		recentReactions, err := client.MessagesGetRecentReactions(ctx, &tg.MessagesGetRecentReactionsRequest{Limit: 20, Hash: hash})
		if err != nil {
			return fmt.Errorf("messages.getRecentReactions: %w", err)
		}
		if err := assertEmptyReactionsGetter("messages.getRecentReactions", hash, recentReactions); err != nil {
			return err
		}

		defaultTagReactions, err := client.MessagesGetDefaultTagReactions(ctx, hash)
		if err != nil {
			return fmt.Errorf("messages.getDefaultTagReactions: %w", err)
		}
		if err := assertEmptyReactionsGetter("messages.getDefaultTagReactions", hash, defaultTagReactions); err != nil {
			return err
		}

		availableEffects, err := client.MessagesGetAvailableEffects(ctx, int(hash))
		if err != nil {
			return fmt.Errorf("messages.getAvailableEffects: %w", err)
		}
		if err := assertEmptyAvailableEffects("messages.getAvailableEffects", int(hash), availableEffects); err != nil {
			return err
		}

		emojiStickerGroups, err := client.MessagesGetEmojiStickerGroups(ctx, int(hash))
		if err != nil {
			return fmt.Errorf("messages.getEmojiStickerGroups: %w", err)
		}
		if err := assertEmptyEmojiStickerGroups("messages.getEmojiStickerGroups", int(hash), emojiStickerGroups); err != nil {
			return err
		}

		stickers, err := client.MessagesGetStickers(ctx, &tg.MessagesGetStickersRequest{Emoticon: "👍", Hash: hash})
		if err != nil {
			return fmt.Errorf("messages.getStickers: %w", err)
		}
		if err := assertFullGetterVariant("messages.getStickers", stickers, &tg.MessagesStickers{}); err != nil {
			return err
		}
		if result, ok := stickers.(*tg.MessagesStickers); ok && (result.Hash != 0 || len(result.Stickers) != 0) {
			return fmt.Errorf("messages.getStickers = hash %d, %d stickers; want empty", result.Hash, len(result.Stickers))
		}

		allStickers, err := client.MessagesGetAllStickers(ctx, hash)
		if err != nil {
			return fmt.Errorf("messages.getAllStickers: %w", err)
		}
		if err := assertFullGetterVariant("messages.getAllStickers", allStickers, &tg.MessagesAllStickers{}); err != nil {
			return err
		}
		if result, ok := allStickers.(*tg.MessagesAllStickers); ok && (result.Hash != 0 || len(result.Sets) != 0) {
			return fmt.Errorf("messages.getAllStickers = hash %d, %d sets; want empty", result.Hash, len(result.Sets))
		}

		recentStickers, err := client.MessagesGetRecentStickers(ctx, &tg.MessagesGetRecentStickersRequest{Hash: hash})
		if err != nil {
			return fmt.Errorf("messages.getRecentStickers: %w", err)
		}
		if err := assertFullGetterVariant("messages.getRecentStickers", recentStickers, &tg.MessagesRecentStickers{}); err != nil {
			return err
		}
		if result, ok := recentStickers.(*tg.MessagesRecentStickers); ok && (result.Hash != 0 || len(result.Packs) != 0 || len(result.Stickers) != 0 || len(result.Dates) != 0) {
			return errors.New("messages.getRecentStickers returned data for empty history")
		}

		favedStickers, err := client.MessagesGetFavedStickers(ctx, hash)
		if err != nil {
			return fmt.Errorf("messages.getFavedStickers: %w", err)
		}
		if err := assertFullGetterVariant("messages.getFavedStickers", favedStickers, &tg.MessagesFavedStickers{}); err != nil {
			return err
		}
		if result, ok := favedStickers.(*tg.MessagesFavedStickers); ok && (result.Hash != 0 || len(result.Packs) != 0 || len(result.Stickers) != 0) {
			return errors.New("messages.getFavedStickers returned data for empty favorites")
		}

		featuredStickers, err := client.MessagesGetFeaturedStickers(ctx, hash)
		if err != nil {
			return fmt.Errorf("messages.getFeaturedStickers: %w", err)
		}
		if err := assertFullGetterVariant("messages.getFeaturedStickers", featuredStickers, &tg.MessagesFeaturedStickers{}); err != nil {
			return err
		}
		if result, ok := featuredStickers.(*tg.MessagesFeaturedStickers); ok && (result.Hash != 0 || result.Count != 0 || len(result.Sets) != 0 || len(result.Unread) != 0) {
			return errors.New("messages.getFeaturedStickers returned data for empty featured stickers")
		}

		emojiStickers, err := client.MessagesGetEmojiStickers(ctx, hash)
		if err != nil {
			return fmt.Errorf("messages.getEmojiStickers: %w", err)
		}
		if err := assertFullGetterVariant("messages.getEmojiStickers", emojiStickers, &tg.MessagesAllStickers{}); err != nil {
			return err
		}
		if result, ok := emojiStickers.(*tg.MessagesAllStickers); ok && (result.Hash != 0 || len(result.Sets) != 0) {
			return fmt.Errorf("messages.getEmojiStickers = hash %d, %d sets; want empty", result.Hash, len(result.Sets))
		}

		featuredEmoji, err := client.MessagesGetFeaturedEmojiStickers(ctx, hash)
		if err != nil {
			return fmt.Errorf("messages.getFeaturedEmojiStickers: %w", err)
		}
		if err := assertFullGetterVariant("messages.getFeaturedEmojiStickers", featuredEmoji, &tg.MessagesFeaturedStickers{}); err != nil {
			return err
		}
		if result, ok := featuredEmoji.(*tg.MessagesFeaturedStickers); ok && (result.Hash != 0 || result.Count != 0 || len(result.Sets) != 0 || len(result.Unread) != 0) {
			return errors.New("messages.getFeaturedEmojiStickers returned data for empty featured stickers")
		}

		savedGifs, err := client.MessagesGetSavedGifs(ctx, hash)
		if err != nil {
			return fmt.Errorf("messages.getSavedGifs: %w", err)
		}
		if err := assertFullGetterVariant("messages.getSavedGifs", savedGifs, &tg.MessagesSavedGifs{}); err != nil {
			return err
		}
		if result, ok := savedGifs.(*tg.MessagesSavedGifs); ok && (result.Hash != 0 || len(result.Gifs) != 0) {
			return fmt.Errorf("messages.getSavedGifs = hash %d, %d GIFs; want empty", result.Hash, len(result.Gifs))
		}

		emojiGroups, err := client.MessagesGetEmojiGroups(ctx, int(hash))
		if err != nil {
			return fmt.Errorf("messages.getEmojiGroups: %w", err)
		}
		if err := assertFullGetterVariant("messages.getEmojiGroups", emojiGroups, &tg.MessagesEmojiGroups{}); err != nil {
			return err
		}
		if result, ok := emojiGroups.(*tg.MessagesEmojiGroups); ok && (result.Hash != 0 || len(result.Groups) != 0) {
			return fmt.Errorf("messages.getEmojiGroups = hash %d, %d groups; want empty", result.Hash, len(result.Groups))
		}

		availableReactions, err := client.MessagesGetAvailableReactions(ctx, int(hash))
		if err != nil {
			return fmt.Errorf("messages.getAvailableReactions: %w", err)
		}
		if err := assertFullGetterVariant("messages.getAvailableReactions", availableReactions, &tg.MessagesAvailableReactions{}); err != nil {
			return err
		}
		if result, ok := availableReactions.(*tg.MessagesAvailableReactions); ok && (result.Hash != 0 || len(result.Reactions) != 0) {
			return fmt.Errorf("messages.getAvailableReactions = hash %d, %d reactions; want empty", result.Hash, len(result.Reactions))
		}

		quickReplies, err := client.MessagesGetQuickReplies(ctx, hash)
		if err != nil {
			return fmt.Errorf("messages.getQuickReplies: %w", err)
		}
		if err := assertFullGetterVariant("messages.getQuickReplies", quickReplies, &tg.MessagesQuickReplies{}); err != nil {
			return err
		}
		if result, ok := quickReplies.(*tg.MessagesQuickReplies); ok && (len(result.QuickReplies) != 0 || len(result.Messages) != 0 || len(result.Chats) != 0 || len(result.Users) != 0) {
			return errors.New("messages.getQuickReplies returned data for empty shortcuts")
		}

		scheduled, err := client.MessagesGetScheduledHistory(ctx, &tg.MessagesGetScheduledHistoryRequest{Peer: &tg.InputPeerSelf{}, Hash: hash})
		if err != nil {
			return fmt.Errorf("messages.getScheduledHistory: %w", err)
		}
		if err := assertFullGetterVariant("messages.getScheduledHistory", scheduled, &tg.MessagesMessages{}); err != nil {
			return err
		}
		if result, ok := scheduled.(*tg.MessagesMessages); ok && (len(result.Messages) != 0 || len(result.Topics) != 0 || len(result.Chats) != 0 || len(result.Users) != 0) {
			return errors.New("messages.getScheduledHistory returned data for empty schedule")
		}

		auctions, err := client.PaymentsGetStarGiftActiveAuctions(ctx, hash)
		if err != nil {
			return fmt.Errorf("payments.getStarGiftActiveAuctions: %w", err)
		}
		if err := assertFullGetterVariant("payments.getStarGiftActiveAuctions", auctions, &tg.PaymentsStarGiftActiveAuctions{}); err != nil {
			return err
		}
		if result, ok := auctions.(*tg.PaymentsStarGiftActiveAuctions); ok && (len(result.Auctions) != 0 || len(result.Users) != 0 || len(result.Chats) != 0) {
			return errors.New("payments.getStarGiftActiveAuctions returned data for empty auctions")
		}
	}

	languages, err := client.MessagesGetEmojiKeywordsLanguages(ctx, []string{"en"})
	if err != nil {
		return fmt.Errorf("messages.getEmojiKeywordsLanguages: %w", err)
	}
	if len(languages) != 0 {
		return fmt.Errorf("messages.getEmojiKeywordsLanguages = %d languages, want empty", len(languages))
	}

	notify, err := client.AccountGetReactionsNotifySettings(ctx)
	if err != nil {
		return fmt.Errorf("account.getReactionsNotifySettings: %w", err)
	}
	if _, ok := notify.MessagesNotifyFrom.(*tg.ReactionNotificationsFromContacts); !ok {
		return fmt.Errorf("account.getReactionsNotifySettings messages source = %T, want contacts", notify.MessagesNotifyFrom)
	}
	if _, ok := notify.StoriesNotifyFrom.(*tg.ReactionNotificationsFromContacts); !ok {
		return fmt.Errorf("account.getReactionsNotifySettings stories source = %T, want contacts", notify.StoriesNotifyFrom)
	}
	if notify.PollVotesNotifyFrom != nil || notify.ShowPreviews {
		return fmt.Errorf("account.getReactionsNotifySettings = poll source %T, previews %v; want defaults", notify.PollVotesNotifyFrom, notify.ShowPreviews)
	}
	if _, ok := notify.Sound.(*tg.NotificationSoundDefault); !ok {
		return fmt.Errorf("account.getReactionsNotifySettings sound = %T, want default", notify.Sound)
	}

	topPeers, err := client.ContactsGetTopPeers(ctx, &tg.ContactsGetTopPeersRequest{})
	if err != nil {
		return fmt.Errorf("contacts.getTopPeers: %w", err)
	}
	if _, ok := topPeers.(*tg.ContactsTopPeersDisabled); !ok {
		return fmt.Errorf("contacts.getTopPeers = %T, want disabled", topPeers)
	}

	contactSignup, err := client.AccountGetContactSignUpNotification(ctx)
	if err != nil {
		return fmt.Errorf("account.getContactSignUpNotification: %w", err)
	}
	if contactSignup {
		return errors.New("account.getContactSignUpNotification = true, want false")
	}

	promo, err := client.HelpGetPremiumPromo(ctx)
	if err != nil {
		return fmt.Errorf("help.getPremiumPromo: %w", err)
	}
	if promo.StatusText != "" || len(promo.StatusEntities) != 0 || len(promo.VideoSections) != 0 || len(promo.Videos) != 0 || len(promo.PeriodOptions) != 0 || len(promo.Users) != 0 {
		return errors.New("help.getPremiumPromo returned data in empty promo")
	}

	stories, err := client.StoriesGetAllStories(ctx, &tg.StoriesGetAllStoriesRequest{})
	if err != nil {
		return fmt.Errorf("stories.getAllStories: %w", err)
	}
	if result, ok := stories.(*tg.StoriesAllStories); !ok {
		return fmt.Errorf("stories.getAllStories = %T, want empty stories", stories)
	} else if result.HasMore || result.Count != 0 || result.State != "" || len(result.PeerStories) != 0 || len(result.Chats) != 0 || len(result.Users) != 0 {
		return errors.New("stories.getAllStories returned data for empty story list")
	}
	if err := checkMAIN1505StaticGetters(ctx, client); err != nil {
		return err
	}

	return nil
}

func checkMAIN1505StaticGetters(ctx context.Context, client *tg.Client) error {
	archive, err := client.StoriesGetStoriesArchive(ctx, &tg.StoriesGetStoriesArchiveRequest{
		Peer: &tg.InputPeerSelf{}, OffsetID: 0, Limit: 20,
	})
	if err != nil {
		return fmt.Errorf("stories.getStoriesArchive: %w", err)
	}
	if archive.Count != 0 || len(archive.Stories) != 0 || len(archive.Chats) != 0 || len(archive.Users) != 0 {
		return fmt.Errorf("stories.getStoriesArchive returned %d stories, want empty", archive.Count)
	}

	for _, hash := range []int64{0, 1} {
		colors, err := client.HelpGetPeerColors(ctx, int(hash))
		if err != nil {
			return fmt.Errorf("help.getPeerColors: %w", err)
		}
		if err := assertEmptyPeerColorsResult("help.getPeerColors", colors); err != nil {
			return err
		}

		profileColors, err := client.HelpGetPeerProfileColors(ctx, int(hash))
		if err != nil {
			return fmt.Errorf("help.getPeerProfileColors: %w", err)
		}
		if err := assertEmptyPeerColorsResult("help.getPeerProfileColors", profileColors); err != nil {
			return err
		}

		tones, err := client.AicomposeGetTones(ctx, hash)
		if err != nil {
			return fmt.Errorf("aicompose.getTones: %w", err)
		}
		result, ok := tones.(*tg.AicomposeTones)
		if !ok || result.Hash != 0 || len(result.Tones) != 0 || len(result.Users) != 0 {
			return fmt.Errorf("aicompose.getTones = %#v, want full empty tones", tones)
		}

		statuses, err := client.AccountGetDefaultEmojiStatuses(ctx, hash)
		if err != nil {
			return fmt.Errorf("account.getDefaultEmojiStatuses: %w", err)
		}
		statusResult, ok := statuses.(*tg.AccountEmojiStatuses)
		if !ok || statusResult.Hash != 0 || len(statusResult.Statuses) != 0 {
			return fmt.Errorf("account.getDefaultEmojiStatuses = %#v, want full empty statuses", statuses)
		}
	}
	return nil
}

func assertEmptyPeerColorsResult(name string, got tg.HelpPeerColorsClass) error {
	result, ok := got.(*tg.HelpPeerColors)
	if !ok || result.Hash != 0 || len(result.Colors) != 0 {
		return fmt.Errorf("%s = %#v, want full empty colors", name, got)
	}
	return nil
}

func testMAIN1505PeerGetters(ctx context.Context, client *tg.Client, userID, chatID, channelID int64) error {
	recommendationRequest := &tg.ChannelsGetChannelRecommendationsRequest{}
	recommendationRequest.SetChannel(inputChannel(userID, channelID))
	recommendations, err := client.ChannelsGetChannelRecommendations(ctx, recommendationRequest)
	if err != nil {
		return fmt.Errorf("channels.getChannelRecommendations: %w", err)
	}
	chats, ok := recommendations.(*tg.MessagesChats)
	if !ok || len(chats.Chats) != 0 {
		return fmt.Errorf("channels.getChannelRecommendations = %#v, want empty chats", recommendations)
	}

	inviteRequest := &tg.MessagesGetExportedChatInvitesRequest{
		Peer: &tg.InputPeerChat{ChatID: chatID}, AdminID: &tg.InputUserSelf{},
		OffsetDate: 123, OffsetLink: "cursor", Limit: 20,
	}
	inviteRequest.SetRevoked(true)
	inviteRequest.SetOffsetDate(123)
	inviteRequest.SetOffsetLink("cursor")
	invites, err := client.MessagesGetExportedChatInvites(ctx, inviteRequest)
	if err != nil {
		return fmt.Errorf("messages.getExportedChatInvites: %w", err)
	}
	if invites.Count != 0 || len(invites.Invites) != 0 || len(invites.Users) != 0 {
		return fmt.Errorf("messages.getExportedChatInvites = count %d, %d invites, %d users; want empty", invites.Count, len(invites.Invites), len(invites.Users))
	}
	return nil
}

func assertEmptyReactionsGetter(name string, hash int64, got tg.MessagesReactionsClass) error {
	if hash == 0 {
		if _, ok := got.(*tg.MessagesReactionsNotModified); !ok {
			return fmt.Errorf("%s with matching hash = %T, want not modified", name, got)
		}
		return nil
	}
	result, ok := got.(*tg.MessagesReactions)
	if !ok {
		return fmt.Errorf("%s with stale hash = %T, want empty reactions", name, got)
	}
	if result.Hash != 0 || len(result.Reactions) != 0 {
		return fmt.Errorf("%s = hash %d, %d reactions; want empty", name, result.Hash, len(result.Reactions))
	}
	return nil
}

func assertEmptyAvailableEffects(name string, hash int, got tg.MessagesAvailableEffectsClass) error {
	if hash == 0 {
		if _, ok := got.(*tg.MessagesAvailableEffectsNotModified); !ok {
			return fmt.Errorf("%s with matching hash = %T, want not modified", name, got)
		}
		return nil
	}
	result, ok := got.(*tg.MessagesAvailableEffects)
	if !ok {
		return fmt.Errorf("%s with stale hash = %T, want empty effects", name, got)
	}
	if result.Hash != 0 || len(result.Effects) != 0 || len(result.Documents) != 0 {
		return fmt.Errorf("%s = hash %d, %d effects and %d documents; want empty", name, result.Hash, len(result.Effects), len(result.Documents))
	}
	return nil
}

func assertEmptyEmojiStickerGroups(name string, hash int, got tg.MessagesEmojiGroupsClass) error {
	if hash == 0 {
		if _, ok := got.(*tg.MessagesEmojiGroupsNotModified); !ok {
			return fmt.Errorf("%s with matching hash = %T, want not modified", name, got)
		}
		return nil
	}
	result, ok := got.(*tg.MessagesEmojiGroups)
	if !ok {
		return fmt.Errorf("%s with stale hash = %T, want empty groups", name, got)
	}
	if result.Hash != 0 || len(result.Groups) != 0 {
		return fmt.Errorf("%s = hash %d, %d groups; want empty", name, result.Hash, len(result.Groups))
	}
	return nil
}

func assertFullGetterVariant(name string, got, want any) error {
	if reflect.TypeOf(got) != reflect.TypeOf(want) {
		return fmt.Errorf("%s = %T, want %T", name, got, want)
	}
	return nil
}

func testSmokeLaunchGetters(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	const phone = "+15551046003"
	seedPhoneUsers(t, f.ctx, f.store, phone)
	client := newSmokeClient(t, f, "launch-getters", phone)
	if err := client.call(f.ctx, checkLaunchGetterResults); err != nil {
		t.Fatalf("launch getter calls: %v", err)
	}
	chat, err := f.store.CreateChat(f.ctx, client.id, "Smoke manage", nil)
	if err != nil {
		t.Fatalf("create getter group fixture: %v", err)
	}
	channelID := createBroadcastChannel(t, f.ctx, client.cmds, "Smoke recommendations")
	if err := client.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		return testMAIN1505PeerGetters(ctx, api, client.id, chat.ID, channelID)
	}); err != nil {
		t.Fatalf("group manage and channel getter calls: %v", err)
	}
}
