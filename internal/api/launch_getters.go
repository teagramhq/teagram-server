package api

import (
	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/mtproto"
)

func decodeLaunchGetterRequest(r *mtproto.Request, req bin.Decoder) error {
	if err := req.Decode(r.Buf); err != nil {
		return errMethodNotImpl
	}
	if r.UserID == 0 {
		return errAuthKeyUnreg
	}
	return nil
}

func (h *handlers) handleGetStickers(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesGetStickersRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return &tg.MessagesStickers{Hash: 0, Stickers: []tg.DocumentClass{}}, nil
}

func (h *handlers) handleGetAllStickers(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesGetAllStickersRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return &tg.MessagesAllStickers{Hash: 0, Sets: []tg.StickerSet{}}, nil
}

func (h *handlers) handleGetRecentStickers(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesGetRecentStickersRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return &tg.MessagesRecentStickers{
		Hash:     0,
		Packs:    []tg.StickerPack{},
		Stickers: []tg.DocumentClass{},
		Dates:    []int{},
	}, nil
}

func (h *handlers) handleGetFavedStickers(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesGetFavedStickersRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return &tg.MessagesFavedStickers{
		Hash:     0,
		Packs:    []tg.StickerPack{},
		Stickers: []tg.DocumentClass{},
	}, nil
}

func (h *handlers) handleGetFeaturedStickers(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesGetFeaturedStickersRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return &tg.MessagesFeaturedStickers{
		Hash:   0,
		Count:  0,
		Sets:   []tg.StickerSetCoveredClass{},
		Unread: []int64{},
	}, nil
}

func (h *handlers) handleGetEmojiStickers(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesGetEmojiStickersRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return &tg.MessagesAllStickers{Hash: 0, Sets: []tg.StickerSet{}}, nil
}

func (h *handlers) handleGetFeaturedEmojiStickers(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesGetFeaturedEmojiStickersRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return &tg.MessagesFeaturedStickers{
		Hash:   0,
		Count:  0,
		Sets:   []tg.StickerSetCoveredClass{},
		Unread: []int64{},
	}, nil
}

func (h *handlers) handleGetSavedGifs(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesGetSavedGifsRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return &tg.MessagesSavedGifs{Hash: 0, Gifs: []tg.DocumentClass{}}, nil
}

func (h *handlers) handleGetEmojiGroups(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesGetEmojiGroupsRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return &tg.MessagesEmojiGroups{Hash: 0, Groups: []tg.EmojiGroupClass{}}, nil
}

func (h *handlers) handleGetEmojiKeywordsLanguages(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesGetEmojiKeywordsLanguagesRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return &tg.EmojiLanguageVector{Elems: []tg.EmojiLanguage{}}, nil
}

func (h *handlers) handleGetAvailableReactions(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesGetAvailableReactionsRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return &tg.MessagesAvailableReactions{Hash: 0, Reactions: []tg.AvailableReaction{}}, nil
}

func (h *handlers) handleGetReactionsNotifySettings(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.AccountGetReactionsNotifySettingsRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	settings := &tg.ReactionsNotifySettings{Sound: &tg.NotificationSoundDefault{}}
	settings.SetMessagesNotifyFrom(&tg.ReactionNotificationsFromContacts{})
	settings.SetStoriesNotifyFrom(&tg.ReactionNotificationsFromContacts{})
	return settings, nil
}

func (h *handlers) handleGetTopPeers(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.ContactsGetTopPeersRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return &tg.ContactsTopPeersDisabled{}, nil
}

func (h *handlers) handleGetQuickReplies(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesGetQuickRepliesRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return &tg.MessagesQuickReplies{
		QuickReplies: []tg.QuickReply{},
		Messages:     []tg.MessageClass{},
		Chats:        []tg.ChatClass{},
		Users:        []tg.UserClass{},
	}, nil
}

func (h *handlers) handleGetScheduledHistory(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesGetScheduledHistoryRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return &tg.MessagesMessages{
		Messages: []tg.MessageClass{},
		Topics:   []tg.ForumTopicClass{},
		Chats:    []tg.ChatClass{},
		Users:    []tg.UserClass{},
	}, nil
}

func (h *handlers) handleGetContactSignUpNotification(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.AccountGetContactSignUpNotificationRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return &tg.BoolFalse{}, nil
}

func (h *handlers) handleGetPremiumPromo(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.HelpGetPremiumPromoRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return &tg.HelpPremiumPromo{
		StatusEntities: []tg.MessageEntityClass{},
		VideoSections:  []string{},
		Videos:         []tg.DocumentClass{},
		PeriodOptions:  []tg.PremiumSubscriptionOption{},
		Users:          []tg.UserClass{},
	}, nil
}

func (h *handlers) handleGetAllStories(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.StoriesGetAllStoriesRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return &tg.StoriesAllStories{
		Count:       0,
		State:       "",
		PeerStories: []tg.PeerStories{},
		Chats:       []tg.ChatClass{},
		Users:       []tg.UserClass{},
	}, nil
}

func (h *handlers) handleGetStarGiftActiveAuctions(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.PaymentsGetStarGiftActiveAuctionsRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return &tg.PaymentsStarGiftActiveAuctions{
		Auctions: []tg.StarGiftActiveAuctionState{},
		Users:    []tg.UserClass{},
		Chats:    []tg.ChatClass{},
	}, nil
}

func (h *handlers) handleGetTimezonesList(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.HelpGetTimezonesListRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return &tg.HelpTimezonesList{Timezones: []tg.Timezone{}}, nil
}

func (h *handlers) handleGetSavedMusic(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.UsersGetSavedMusicRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return &tg.UsersSavedMusic{
		Documents: []tg.DocumentClass{},
	}, nil
}

func (h *handlers) handleGetPinnedStories(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.StoriesGetPinnedStoriesRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return &tg.StoriesStories{
		Stories: []tg.StoryItemClass{},
		Chats:   []tg.ChatClass{},
		Users:   []tg.UserClass{},
	}, nil
}

func (h *handlers) handleGetSavedStarGifts(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.PaymentsGetSavedStarGiftsRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return &tg.PaymentsSavedStarGifts{
		Gifts: []tg.SavedStarGift{},
		Chats: []tg.ChatClass{},
		Users: []tg.UserClass{},
	}, nil
}

func (h *handlers) handleGetAccountTTL(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.AccountGetAccountTTLRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return &tg.AccountDaysTTL{Days: 365}, nil
}

func (h *handlers) handleGetDefaultHistoryTTL(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesGetDefaultHistoryTTLRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return &tg.DefaultHistoryTTL{Period: 0}, nil
}

func (h *handlers) handleGetSendAs(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.ChannelsGetSendAsRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return &tg.ChannelsSendAsPeers{
		Peers: []tg.SendAsPeer{},
		Chats: []tg.ChatClass{},
		Users: []tg.UserClass{},
	}, nil
}

func (h *handlers) handleGetStoriesArchive(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.StoriesGetStoriesArchiveRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return &tg.StoriesStories{
		Stories: []tg.StoryItemClass{},
		Chats:   []tg.ChatClass{},
		Users:   []tg.UserClass{},
	}, nil
}

func (h *handlers) handleGetSponsoredMessages(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesGetSponsoredMessagesRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return &tg.MessagesSponsoredMessagesEmpty{}, nil
}

func (h *handlers) handleGetMessagesViews(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesGetMessagesViewsRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	views := make([]tg.MessageViews, len(req.ID))
	for i := range views {
		views[i].SetViews(0)
	}
	return &tg.MessagesMessageViews{
		Views: views,
		Chats: []tg.ChatClass{},
		Users: []tg.UserClass{},
	}, nil
}
