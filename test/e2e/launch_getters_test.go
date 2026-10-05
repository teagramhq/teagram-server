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
	for _, hash := range []int64{1, 0} {
		stickers, err := client.MessagesGetStickers(ctx, &tg.MessagesGetStickersRequest{Emoticon: "👍", Hash: hash})
		if err != nil {
			return fmt.Errorf("messages.getStickers: %w", err)
		}
		if err := assertGetterVariant("messages.getStickers", stickers, hash, &tg.MessagesStickers{}, &tg.MessagesStickersNotModified{}); err != nil {
			return err
		}
		if result, ok := stickers.(*tg.MessagesStickers); ok && (result.Hash != 0 || len(result.Stickers) != 0) {
			return fmt.Errorf("messages.getStickers = hash %d, %d stickers; want empty", result.Hash, len(result.Stickers))
		}

		allStickers, err := client.MessagesGetAllStickers(ctx, hash)
		if err != nil {
			return fmt.Errorf("messages.getAllStickers: %w", err)
		}
		if err := assertGetterVariant("messages.getAllStickers", allStickers, hash, &tg.MessagesAllStickers{}, &tg.MessagesAllStickersNotModified{}); err != nil {
			return err
		}
		if result, ok := allStickers.(*tg.MessagesAllStickers); ok && (result.Hash != 0 || len(result.Sets) != 0) {
			return fmt.Errorf("messages.getAllStickers = hash %d, %d sets; want empty", result.Hash, len(result.Sets))
		}

		recentStickers, err := client.MessagesGetRecentStickers(ctx, &tg.MessagesGetRecentStickersRequest{Hash: hash})
		if err != nil {
			return fmt.Errorf("messages.getRecentStickers: %w", err)
		}
		if err := assertGetterVariant("messages.getRecentStickers", recentStickers, hash, &tg.MessagesRecentStickers{}, &tg.MessagesRecentStickersNotModified{}); err != nil {
			return err
		}
		if result, ok := recentStickers.(*tg.MessagesRecentStickers); ok && (result.Hash != 0 || len(result.Packs) != 0 || len(result.Stickers) != 0 || len(result.Dates) != 0) {
			return errors.New("messages.getRecentStickers returned data for empty history")
		}

		favedStickers, err := client.MessagesGetFavedStickers(ctx, hash)
		if err != nil {
			return fmt.Errorf("messages.getFavedStickers: %w", err)
		}
		if err := assertGetterVariant("messages.getFavedStickers", favedStickers, hash, &tg.MessagesFavedStickers{}, &tg.MessagesFavedStickersNotModified{}); err != nil {
			return err
		}
		if result, ok := favedStickers.(*tg.MessagesFavedStickers); ok && (result.Hash != 0 || len(result.Packs) != 0 || len(result.Stickers) != 0) {
			return errors.New("messages.getFavedStickers returned data for empty favorites")
		}

		featuredStickers, err := client.MessagesGetFeaturedStickers(ctx, hash)
		if err != nil {
			return fmt.Errorf("messages.getFeaturedStickers: %w", err)
		}
		if err := assertGetterVariant("messages.getFeaturedStickers", featuredStickers, hash, &tg.MessagesFeaturedStickers{}, &tg.MessagesFeaturedStickersNotModified{}); err != nil {
			return err
		}
		if result, ok := featuredStickers.(*tg.MessagesFeaturedStickers); ok && (result.Hash != 0 || result.Count != 0 || len(result.Sets) != 0 || len(result.Unread) != 0) {
			return errors.New("messages.getFeaturedStickers returned data for empty featured stickers")
		}

		emojiStickers, err := client.MessagesGetEmojiStickers(ctx, hash)
		if err != nil {
			return fmt.Errorf("messages.getEmojiStickers: %w", err)
		}
		if err := assertGetterVariant("messages.getEmojiStickers", emojiStickers, hash, &tg.MessagesAllStickers{}, &tg.MessagesAllStickersNotModified{}); err != nil {
			return err
		}
		if result, ok := emojiStickers.(*tg.MessagesAllStickers); ok && (result.Hash != 0 || len(result.Sets) != 0) {
			return fmt.Errorf("messages.getEmojiStickers = hash %d, %d sets; want empty", result.Hash, len(result.Sets))
		}

		featuredEmoji, err := client.MessagesGetFeaturedEmojiStickers(ctx, hash)
		if err != nil {
			return fmt.Errorf("messages.getFeaturedEmojiStickers: %w", err)
		}
		if err := assertGetterVariant("messages.getFeaturedEmojiStickers", featuredEmoji, hash, &tg.MessagesFeaturedStickers{}, &tg.MessagesFeaturedStickersNotModified{}); err != nil {
			return err
		}
		if result, ok := featuredEmoji.(*tg.MessagesFeaturedStickers); ok && (result.Hash != 0 || result.Count != 0 || len(result.Sets) != 0 || len(result.Unread) != 0) {
			return errors.New("messages.getFeaturedEmojiStickers returned data for empty featured stickers")
		}

		savedGifs, err := client.MessagesGetSavedGifs(ctx, hash)
		if err != nil {
			return fmt.Errorf("messages.getSavedGifs: %w", err)
		}
		if err := assertGetterVariant("messages.getSavedGifs", savedGifs, hash, &tg.MessagesSavedGifs{}, &tg.MessagesSavedGifsNotModified{}); err != nil {
			return err
		}
		if result, ok := savedGifs.(*tg.MessagesSavedGifs); ok && (result.Hash != 0 || len(result.Gifs) != 0) {
			return fmt.Errorf("messages.getSavedGifs = hash %d, %d GIFs; want empty", result.Hash, len(result.Gifs))
		}

		emojiGroups, err := client.MessagesGetEmojiGroups(ctx, int(hash))
		if err != nil {
			return fmt.Errorf("messages.getEmojiGroups: %w", err)
		}
		if err := assertGetterVariant("messages.getEmojiGroups", emojiGroups, hash, &tg.MessagesEmojiGroups{}, &tg.MessagesEmojiGroupsNotModified{}); err != nil {
			return err
		}
		if result, ok := emojiGroups.(*tg.MessagesEmojiGroups); ok && (result.Hash != 0 || len(result.Groups) != 0) {
			return fmt.Errorf("messages.getEmojiGroups = hash %d, %d groups; want empty", result.Hash, len(result.Groups))
		}

		availableReactions, err := client.MessagesGetAvailableReactions(ctx, int(hash))
		if err != nil {
			return fmt.Errorf("messages.getAvailableReactions: %w", err)
		}
		if err := assertGetterVariant("messages.getAvailableReactions", availableReactions, hash, &tg.MessagesAvailableReactions{}, &tg.MessagesAvailableReactionsNotModified{}); err != nil {
			return err
		}
		if result, ok := availableReactions.(*tg.MessagesAvailableReactions); ok && (result.Hash != 0 || len(result.Reactions) != 0) {
			return fmt.Errorf("messages.getAvailableReactions = hash %d, %d reactions; want empty", result.Hash, len(result.Reactions))
		}

		quickReplies, err := client.MessagesGetQuickReplies(ctx, hash)
		if err != nil {
			return fmt.Errorf("messages.getQuickReplies: %w", err)
		}
		if err := assertGetterVariant("messages.getQuickReplies", quickReplies, hash, &tg.MessagesQuickReplies{}, &tg.MessagesQuickRepliesNotModified{}); err != nil {
			return err
		}
		if result, ok := quickReplies.(*tg.MessagesQuickReplies); ok && (len(result.QuickReplies) != 0 || len(result.Messages) != 0 || len(result.Chats) != 0 || len(result.Users) != 0) {
			return errors.New("messages.getQuickReplies returned data for empty shortcuts")
		}

		scheduled, err := client.MessagesGetScheduledHistory(ctx, &tg.MessagesGetScheduledHistoryRequest{Peer: &tg.InputPeerSelf{}, Hash: hash})
		if err != nil {
			return fmt.Errorf("messages.getScheduledHistory: %w", err)
		}
		if err := assertGetterVariant("messages.getScheduledHistory", scheduled, hash, &tg.MessagesMessages{}, &tg.MessagesMessagesNotModified{}); err != nil {
			return err
		}
		if result, ok := scheduled.(*tg.MessagesMessages); ok && (len(result.Messages) != 0 || len(result.Topics) != 0 || len(result.Chats) != 0 || len(result.Users) != 0) {
			return errors.New("messages.getScheduledHistory returned data for empty schedule")
		}

		auctions, err := client.PaymentsGetStarGiftActiveAuctions(ctx, hash)
		if err != nil {
			return fmt.Errorf("payments.getStarGiftActiveAuctions: %w", err)
		}
		if err := assertGetterVariant("payments.getStarGiftActiveAuctions", auctions, hash, &tg.PaymentsStarGiftActiveAuctions{}, &tg.PaymentsStarGiftActiveAuctionsNotModified{}); err != nil {
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

	return nil
}

func assertGetterVariant(name string, got any, hash int64, empty, notModified any) error {
	want := empty
	if hash == 0 {
		want = notModified
	}
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
}
