package api

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/store"
)

func decodeLaunchGetterRequest(r *mtproto.Request, req bin.Decoder) error {
	if r.UserID == 0 {
		return errAuthKeyUnreg
	}
	if err := req.Decode(r.Buf); err != nil {
		return errMethodNotImpl
	}
	return nil
}

func (h *handlers) handleGetWebPagePreview(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesGetWebPagePreviewRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return &tg.MessagesWebPagePreview{
		Media: &tg.MessageMediaEmpty{},
		Chats: []tg.ChatClass{},
		Users: []tg.UserClass{},
	}, nil
}

func (h *handlers) handleGetWebPage(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesGetWebPageRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	// Web page previews are not stored or fetched. With no cached page there is
	// no hash to match, so even a non-zero client hash must receive an empty page.
	return &tg.MessagesWebPage{
		Webpage: &tg.WebPageEmpty{},
		Chats:   []tg.ChatClass{},
		Users:   []tg.UserClass{},
	}, nil
}

func (h *handlers) handleGetStickers(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesGetStickersRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return &tg.MessagesStickers{Hash: 0, Stickers: []tg.DocumentClass{}}, nil
}

func (h *handlers) handleGetArchivedStickers(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesGetArchivedStickersRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return &tg.MessagesArchivedStickers{Count: 0, Sets: []tg.StickerSetCoveredClass{}}, nil
}

func (h *handlers) handleSearchStickers(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesSearchStickersRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return &tg.MessagesFoundStickers{Hash: 0, Stickers: []tg.DocumentClass{}}, nil
}

func (h *handlers) handleSearchEmojiStickerSets(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesSearchEmojiStickerSetsRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return &tg.MessagesFoundStickerSets{Hash: 0, Sets: []tg.StickerSetCoveredClass{}}, nil
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
	catalog, err := h.stickerSetCatalog()
	if err != nil {
		h.log.Error("load featured sticker sets", "err", err)
		return nil, errInternal
	}
	installed, err := h.store.InstalledStickerSets(r.Ctx, r.UserID)
	if err != nil {
		h.log.Error("load featured sticker set state", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	installedAt := make(map[int64]time.Time, len(installed))
	for _, item := range installed {
		if _, ok := catalog.byID[item.ID]; ok {
			installedAt[item.ID] = item.InstalledAt
		}
	}
	if len(catalog.featured) == 0 {
		return nil, errInternal
	}
	hash := featuredStickerSetsHashList(catalog.featured, installedAt)
	if req.Hash != 0 && req.Hash == hash {
		return &tg.MessagesFeaturedStickersNotModified{}, nil
	}
	coveredSets := make([]tg.StickerSetCoveredClass, 0, len(catalog.featured))
	for _, set := range catalog.featured {
		documents := set.responseDocuments(h.dcID)
		if len(documents) == 0 {
			return nil, errInternal
		}
		var setInstalledAt *time.Time
		if value, ok := installedAt[set.set.ID]; ok {
			setInstalledAt = &value
		}
		coveredSets = append(coveredSets, &tg.StickerSetCovered{
			Set:   set.responseSet(setInstalledAt),
			Cover: documents[0],
		})
	}
	return &tg.MessagesFeaturedStickers{
		Hash:   hash,
		Count:  len(coveredSets),
		Sets:   coveredSets,
		Unread: []int64{},
	}, nil
}

func featuredStickerSetsHashList(sets []*builtInStickerSet, installedAt map[int64]time.Time) int64 {
	if len(sets) == 1 {
		var installed *time.Time
		if value, ok := installedAt[sets[0].set.ID]; ok {
			installed = &value
		}
		return featuredStickerSetsHash(sets[0].set, installed, sets[0].documents[0].ID)
	}
	input := make([]byte, 0, len(sets)*40)
	var long [8]byte
	for _, set := range sets {
		binary.BigEndian.PutUint64(long[:], uint64(set.set.ID)) //nolint:gosec // Catalogue ids use a reserved negative namespace.
		input = append(input, long[:]...)
		binary.BigEndian.PutUint64(long[:], uint64(set.set.AccessHash)) //nolint:gosec // Hashes are serialized as their two's-complement bit pattern.
		input = append(input, long[:]...)
		binary.BigEndian.PutUint64(long[:], uint64(int64(set.set.Hash))) //nolint:gosec // Set hashes are constrained to signed 31-bit values.
		input = append(input, long[:]...)
		binary.BigEndian.PutUint64(long[:], uint64(set.documents[0].ID)) //nolint:gosec // Catalogue document ids use a reserved negative namespace.
		input = append(input, long[:]...)
		var installed int64
		if value, ok := installedAt[set.set.ID]; ok {
			installed = value.Unix()
		}
		binary.BigEndian.PutUint64(long[:], uint64(installed))
		input = append(input, long[:]...)
	}
	hash := int64(crc32.ChecksumIEEE(input) & 0x7fffffff)
	if hash == 0 {
		return 1
	}
	return hash
}

func featuredStickerSetsHash(set tg.StickerSet, installedAt *time.Time, coverID int64) int64 {
	input := make([]byte, 0, 36)
	var long [8]byte
	binary.BigEndian.PutUint64(long[:], uint64(set.ID)) //nolint:gosec // Catalogue ids use a reserved negative namespace.
	input = append(input, long[:]...)
	binary.BigEndian.PutUint64(long[:], uint64(set.AccessHash)) //nolint:gosec // Hashes are serialized as their two's-complement bit pattern.
	input = append(input, long[:]...)
	binary.BigEndian.PutUint64(long[:], uint64(int64(set.Hash))) //nolint:gosec // Set hashes are constrained to signed 31-bit values.
	input = append(input, long[:]...)
	binary.BigEndian.PutUint64(long[:], uint64(coverID)) //nolint:gosec // Catalogue document ids use a reserved negative namespace.
	input = append(input, long[:]...)
	var installed int64
	if installedAt != nil {
		installed = installedAt.Unix()
	}
	binary.BigEndian.PutUint64(long[:], uint64(installed))
	input = append(input, long[:]...)
	hash := int64(crc32.ChecksumIEEE(input) & 0x7fffffff)
	if hash == 0 {
		return 1
	}
	return hash
}

func (h *handlers) handleGetEmojiStickers(r *mtproto.Request) (bin.Encoder, error) {
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	var req tg.MessagesGetEmojiStickersRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	catalog, err := h.stickerSetCatalog()
	if err != nil {
		h.log.Error("load installed emoji sticker catalogue", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	installed, err := h.store.InstalledStickerSets(r.Ctx, r.UserID)
	if err != nil {
		h.log.Error("load installed emoji sticker sets", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	userSetIDs := make([]int64, 0, len(installed))
	for _, item := range installed {
		if item.ID > 0 {
			userSetIDs = append(userSetIDs, item.ID)
		}
	}
	userSets, err := h.store.UserStickerSetSummariesByInstalledIDs(r.Ctx, userSetIDs)
	if err != nil {
		h.log.Error("load installed emoji sticker sets", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	sets := make([]tg.StickerSet, 0, len(installed))
	for _, item := range installed {
		if item.ID < 0 {
			if set, ok := catalog.byID[item.ID]; ok && set.set.GetEmojis() {
				sets = append(sets, set.responseSet(&item.InstalledAt))
			}
			continue
		}
		if item.ID == 0 {
			continue
		}
		set, ok := userSets[item.ID]
		if !ok {
			continue
		}
		if set.Kind == "emojis" {
			sets = append(sets, userStickerSetSummaryTL(set, r.UserID, &item.InstalledAt))
		}
	}
	hash := installedStickerSetsHash(sets)
	if req.Hash != 0 && req.Hash == hash {
		return &tg.MessagesAllStickersNotModified{}, nil
	}
	return &tg.MessagesAllStickers{Hash: hash, Sets: sets}, nil
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

func (h *handlers) handleGetEmojiKeywordsDifference(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesGetEmojiKeywordsDifferenceRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return &tg.EmojiKeywordsDifference{
		LangCode:    req.LangCode,
		FromVersion: req.FromVersion,
		Version:     req.FromVersion,
		Keywords:    []tg.EmojiKeywordClass{},
	}, nil
}

func (h *handlers) handleGetAvailableReactions(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesGetAvailableReactionsRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	catalog, err := getReactionDocumentCatalog()
	if err != nil {
		h.log.Error("load reaction documents", "err", err)
		return nil, errInternal
	}
	if req.Hash == catalog.hash {
		return &tg.MessagesAvailableReactionsNotModified{}, nil
	}
	reactions, err := catalog.responseReactions(h.dcID)
	if err != nil {
		h.log.Error("build available reactions response", "err", err)
		return nil, errInternal
	}
	return &tg.MessagesAvailableReactions{
		Hash:      catalog.hash,
		Reactions: reactions,
	}, nil
}

func (h *handlers) handleGetTopReactions(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesGetTopReactionsRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return emptyReactions(), nil
}

func (h *handlers) handleGetRecentReactions(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesGetRecentReactionsRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return emptyReactions(), nil
}

func (h *handlers) handleGetDefaultTagReactions(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesGetDefaultTagReactionsRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return emptyReactions(), nil
}

func emptyReactions() tg.MessagesReactionsClass {
	return &tg.MessagesReactions{Hash: 0, Reactions: []tg.ReactionClass{}}
}

func (h *handlers) handleGetAvailableEffects(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesGetAvailableEffectsRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return &tg.MessagesAvailableEffects{
		Hash:      0,
		Effects:   []tg.AvailableEffect{},
		Documents: []tg.DocumentClass{},
	}, nil
}

func (h *handlers) handleGetEmojiStickerGroups(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesGetEmojiStickerGroupsRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return &tg.MessagesEmojiGroups{Hash: 0, Groups: []tg.EmojiGroupClass{}}, nil
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

func (h *handlers) handleGetConnectedBots(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.AccountGetConnectedBotsRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return &tg.AccountConnectedBots{
		ConnectedBots: []tg.ConnectedBot{},
		Users:         []tg.UserClass{},
	}, nil
}

func (h *handlers) handleGetPasskeys(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.AccountGetPasskeysRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return &tg.AccountPasskeys{Passkeys: []tg.Passkey{}}, nil
}

func (h *handlers) handleGetWebAuthorizations(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.AccountGetWebAuthorizationsRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return &tg.AccountWebAuthorizations{
		Authorizations: []tg.WebAuthorization{},
		Users:          []tg.UserClass{},
	}, nil
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

func (h *handlers) handleGetPeerStories(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.StoriesGetPeerStoriesRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	peerType, peerID, err := h.inputPeer(req.Peer, r.UserID)
	if err != nil {
		return nil, err
	}
	return &tg.StoriesPeerStories{
		Stories: tg.PeerStories{
			Peer:    peerToTL(peerType, peerID),
			Stories: []tg.StoryItemClass{},
		},
		Chats: []tg.ChatClass{},
		Users: []tg.UserClass{},
	}, nil
}

func (h *handlers) handleGetBirthdays(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.ContactsGetBirthdaysRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return &tg.ContactsContactBirthdays{
		Contacts: []tg.ContactBirthday{},
		Users:    []tg.UserClass{},
	}, nil
}

func (h *handlers) handleGetPaidReactionPrivacy(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesGetPaidReactionPrivacyRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return &tg.UpdateShort{
		Update: &tg.UpdatePaidReactionPrivacy{Private: &tg.PaidReactionPrivacyDefault{}},
		Date:   int(h.now().Unix()),
	}, nil
}

func (h *handlers) handleGetStarsStatus(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.PaymentsGetStarsStatusRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	if req.Peer != nil {
		if _, _, err := h.inputPeer(req.Peer, r.UserID); err != nil {
			return nil, err
		}
	}
	return &tg.PaymentsStarsStatus{
		Balance:       &tg.StarsAmount{},
		Subscriptions: []tg.StarsSubscription{},
		History:       []tg.StarsTransaction{},
		Chats:         []tg.ChatClass{},
		Users:         []tg.UserClass{},
	}, nil
}

func (h *handlers) handleGetSearchCounters(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesGetSearchCountersRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	peerType, peerID, err := h.inputPeer(req.Peer, r.UserID)
	if err != nil {
		return nil, err
	}
	if savedPeer, ok := req.GetSavedPeerID(); ok {
		if _, _, err := h.inputPeer(savedPeer, r.UserID); err != nil {
			return nil, err
		}
	}
	uniqueFilters := make([]store.MediaSearchFilter, 0)
	seenFilters := make(map[store.MediaSearchFilter]struct{})
	for _, filter := range req.Filters {
		mediaFilter, supported := mediaSearchFilterFromTL(filter)
		if !supported {
			continue
		}
		if _, seen := seenFilters[mediaFilter]; seen {
			continue
		}
		seenFilters[mediaFilter] = struct{}{}
		uniqueFilters = append(uniqueFilters, mediaFilter)
	}
	rateLimitCost := len(uniqueFilters)
	if rateLimitCost == 0 {
		rateLimitCost = 1
	}
	// Charge before any membership lookup. Each distinct supported filter opens
	// one count query; unsupported-only requests still pay for the RPC checks.
	if h.rateLimitSearchMessages.Enabled() {
		if err := h.checkRateLimitCost(r, "messages_search", h.rateLimitSearchMessages, rateLimitCost); err != nil {
			return nil, err
		}
	}
	if peerType == store.PeerTypeChat {
		if err := h.requireMember(r.Ctx, peerID, r.UserID); err != nil {
			return nil, err
		}
	}
	if peerType == store.PeerTypeChannel {
		if _, err := h.requireChannelMember(r.Ctx, peerID, r.UserID); err != nil {
			return nil, err
		}
	}
	counts := make(map[store.MediaSearchFilter]int, len(uniqueFilters))
	for _, mediaFilter := range uniqueFilters {
		var count int
		if peerType == store.PeerTypeChannel {
			_, count, err = h.store.SearchFilteredChannelPosts(r.Ctx, r.UserID, peerID, "", mediaFilter, 0, 0)
			if errors.Is(err, store.ErrNotMember) {
				return nil, errPeerIDInvalid
			}
			if err != nil {
				h.log.Error("count filtered channel posts", "user_id", r.UserID, "channel_id", peerID, "err", err)
				return nil, errInternal
			}
		} else {
			_, count, err = h.store.SearchFilteredMessages(r.Ctx, r.UserID, peerType, peerID, "", mediaFilter, 0, 0, 0, 0, 0)
			if errors.Is(err, store.ErrNotMember) {
				return nil, errPeerIDInvalid
			}
			if err != nil {
				h.log.Error("count filtered messages", "user_id", r.UserID, "err", err)
				return nil, errInternal
			}
		}
		counts[mediaFilter] = count
	}
	counters := make([]tg.MessagesSearchCounter, len(req.Filters))
	for i, filter := range req.Filters {
		count := 0
		if mediaFilter, supported := mediaSearchFilterFromTL(filter); supported {
			count = counts[mediaFilter]
		}
		counters[i] = tg.MessagesSearchCounter{Filter: filter, Count: count}
	}
	return &tg.MessagesSearchCounterVector{Elems: counters}, nil
}

func (h *handlers) handleGetSavedHistory(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesGetSavedHistoryRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	if _, _, err := h.inputPeer(req.Peer, r.UserID); err != nil {
		return nil, err
	}
	if parentPeer, ok := req.GetParentPeer(); ok {
		if _, _, err := h.inputPeer(parentPeer, r.UserID); err != nil {
			return nil, err
		}
	}
	return &tg.MessagesMessages{
		Messages: []tg.MessageClass{},
		Topics:   []tg.ForumTopicClass{},
		Chats:    []tg.ChatClass{},
		Users:    []tg.UserClass{},
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

func (h *handlers) handleGetStoriesArchive(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.StoriesGetStoriesArchiveRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return &tg.StoriesStories{
		Count:   0,
		Stories: []tg.StoryItemClass{},
		Chats:   []tg.ChatClass{},
		Users:   []tg.UserClass{},
	}, nil
}

func (h *handlers) handleGetPeerColors(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.HelpGetPeerColorsRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return &tg.HelpPeerColors{Hash: 0, Colors: []tg.HelpPeerColorOption{}}, nil
}

func (h *handlers) handleGetPeerProfileColors(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.HelpGetPeerProfileColorsRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return &tg.HelpPeerColors{Hash: 0, Colors: []tg.HelpPeerColorOption{}}, nil
}

func (h *handlers) handleGetTones(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.AicomposeGetTonesRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return &tg.AicomposeTones{
		Hash:  0,
		Tones: []tg.AiComposeToneClass{},
		Users: []tg.UserClass{},
	}, nil
}

func (h *handlers) handleGetDefaultEmojiStatuses(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.AccountGetDefaultEmojiStatusesRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return &tg.AccountEmojiStatuses{Hash: 0, Statuses: []tg.EmojiStatusClass{}}, nil
}

func (h *handlers) handleGetCollectibleEmojiStatuses(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.AccountGetCollectibleEmojiStatusesRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	if req.Hash == 0 {
		return &tg.AccountEmojiStatusesNotModified{}, nil
	}
	return &tg.AccountEmojiStatuses{Hash: 0, Statuses: []tg.EmojiStatusClass{}}, nil
}

func (h *handlers) handleGetChannelRecommendations(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.ChannelsGetChannelRecommendationsRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return &tg.MessagesChats{Chats: []tg.ChatClass{}}, nil
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

func (h *handlers) handleGetSponsoredMessages(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesGetSponsoredMessagesRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return &tg.MessagesSponsoredMessagesEmpty{}, nil
}

func (h *handlers) handleGetSponsoredPeers(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.ContactsGetSponsoredPeersRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return &tg.ContactsSponsoredPeersEmpty{}, nil
}

func (h *handlers) handleGetPremiumGiftCodeOptions(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.PaymentsGetPremiumGiftCodeOptionsRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return &tg.PremiumGiftCodeOptionVector{Elems: []tg.PremiumGiftCodeOption{}}, nil
}

func (h *handlers) handleGetTimezonesList(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.HelpGetTimezonesListRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return &tg.HelpTimezonesList{Hash: 0, Timezones: []tg.Timezone{}}, nil
}

func (h *handlers) handleGetSavedMusic(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.UsersGetSavedMusicRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return &tg.UsersSavedMusic{Count: 0, Documents: []tg.DocumentClass{}}, nil
}

func (h *handlers) handleGetSavedMusicIDs(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.AccountGetSavedMusicIDsRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return &tg.AccountSavedMusicIDs{IDs: []int64{}}, nil
}

func (h *handlers) handleGetPinnedStories(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.StoriesGetPinnedStoriesRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return &tg.StoriesStories{
		Count:       0,
		Stories:     []tg.StoryItemClass{},
		PinnedToTop: []int{},
		Chats:       []tg.ChatClass{},
		Users:       []tg.UserClass{},
	}, nil
}

func (h *handlers) handleGetSavedStarGifts(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.PaymentsGetSavedStarGiftsRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	return &tg.PaymentsSavedStarGifts{
		Count:      0,
		Gifts:      []tg.SavedStarGift{},
		NextOffset: "",
		Chats:      []tg.ChatClass{},
		Users:      []tg.UserClass{},
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
