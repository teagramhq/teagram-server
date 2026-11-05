package api

import (
	"errors"
	"fmt"
	"slices"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/store"
)

var chatReactionsAll = []string{
	"👍", "👎", "❤", "🔥", "🥰", "👏", "😁", "🤔", "🤯", "😱", "🤬", "😢", "🎉", "🤩", "🤮", "💩", "🙏", "👌", "🕊", "🤡", "🥱", "🥴", "😍", "🐳", "❤‍🔥", "🌚", "🌭", "💯", "🤣", "⚡", "🍌", "🏆", "💔", "🤨", "😐", "🍓", "🍾", "💋", "🖕", "😈", "😴", "😭", "🤓", "👻", "👨‍💻", "👀", "🎃", "🙈", "😇", "😨", "🤝", "✍", "🤗", "🫡", "🎅", "🎄", "☃", "💅", "🤪", "🗿", "🆒", "💘", "🙉", "🦄", "😘", "💊", "🙊", "😎", "👾", "🤷‍♂", "🤷", "🤷‍♀", "😡",
}

func isStandardReactionEmoji(emoticon string) bool {
	return slices.Contains(chatReactionsAll, emoticon)
}

func chatAvailableReactionsFromTL(value tg.ChatReactionsClass) (store.ChatAvailableReactions, error) {
	switch reactions := value.(type) {
	case *tg.ChatReactionsAll:
		if reactions == nil {
			return store.ChatAvailableReactions{}, errReactionInvalid
		}
		return store.ChatAvailableReactions{Mode: store.ChatAvailableReactionsAll, AllowCustom: reactions.GetAllowCustom()}, nil
	case *tg.ChatReactionsSome:
		if reactions == nil || len(reactions.Reactions) > len(chatReactionsAll)+maxCustomEmojiDocumentIDs {
			return store.ChatAvailableReactions{}, errReactionInvalid
		}
		allowed := make([]string, 0, len(reactions.Reactions))
		customAllowed := make([]int64, 0, len(reactions.Reactions))
		seen := make(map[string]struct{}, len(reactions.Reactions))
		seenCustom := make(map[int64]struct{}, len(reactions.Reactions))
		for _, reaction := range reactions.Reactions {
			switch value := reaction.(type) {
			case *tg.ReactionEmoji:
				if value == nil || !isStandardReactionEmoji(value.Emoticon) {
					return store.ChatAvailableReactions{}, errReactionInvalid
				}
				if _, ok := seen[value.Emoticon]; ok {
					continue
				}
				seen[value.Emoticon] = struct{}{}
				allowed = append(allowed, value.Emoticon)
			case *tg.ReactionCustomEmoji:
				if value == nil || value.DocumentID == 0 {
					return store.ChatAvailableReactions{}, errReactionInvalid
				}
				if _, ok := seenCustom[value.DocumentID]; ok {
					continue
				}
				seenCustom[value.DocumentID] = struct{}{}
				customAllowed = append(customAllowed, value.DocumentID)
			default:
				return store.ChatAvailableReactions{}, errReactionInvalid
			}
		}
		return store.ChatAvailableReactions{Mode: store.ChatAvailableReactionsSome, Emoticons: allowed, CustomEmojiIDs: customAllowed}, nil
	case *tg.ChatReactionsNone:
		if reactions == nil {
			return store.ChatAvailableReactions{}, errReactionInvalid
		}
		return store.ChatAvailableReactions{Mode: store.ChatAvailableReactionsNone}, nil
	default:
		return store.ChatAvailableReactions{}, errReactionInvalid
	}
}

func chatAvailableReactionsToTL(available store.ChatAvailableReactions) (tg.ChatReactionsClass, error) {
	switch available.Mode {
	case store.ChatAvailableReactionsAll:
		return &tg.ChatReactionsAll{AllowCustom: available.AllowCustom}, nil
	case store.ChatAvailableReactionsSome:
		reactions := make([]tg.ReactionClass, 0, len(available.Emoticons)+len(available.CustomEmojiIDs))
		for _, emoticon := range available.Emoticons {
			reactions = append(reactions, &tg.ReactionEmoji{Emoticon: emoticon})
		}
		for _, documentID := range available.CustomEmojiIDs {
			reactions = append(reactions, &tg.ReactionCustomEmoji{DocumentID: documentID})
		}
		return &tg.ChatReactionsSome{Reactions: reactions}, nil
	case store.ChatAvailableReactionsNone:
		return &tg.ChatReactionsNone{}, nil
	default:
		return nil, fmt.Errorf("unknown stored chat reaction mode %q", available.Mode)
	}
}

func (h *handlers) handleSetChatAvailableReactions(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesSetChatAvailableReactionsRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	peerType, peerID, err := h.inputPeer(req.Peer, r.UserID)
	if err != nil {
		return nil, err
	}
	if (peerType != store.PeerTypeChat && peerType != store.PeerTypeChannel) || peerID <= 0 {
		return nil, errPeerIDInvalid
	}
	if _, ok := req.GetReactionsLimit(); ok {
		return nil, errReactionInvalid
	}
	if paidEnabled, ok := req.GetPaidEnabled(); ok && paidEnabled {
		return nil, errReactionInvalid
	}
	available, err := chatAvailableReactionsFromTL(req.AvailableReactions)
	if err != nil {
		return nil, err
	}
	live, err := h.liveCustomEmojiDocuments(r.Ctx, r.UserID, available.CustomEmojiIDs)
	if err != nil {
		return nil, err
	}
	if !live {
		return nil, errReactionInvalid
	}
	if peerType == store.PeerTypeChat {
		err = h.store.SetChatAvailableReactions(r.Ctx, peerID, r.UserID, available)
	} else {
		err = h.store.SetChannelAvailableReactions(r.Ctx, peerID, r.UserID, available)
	}
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotMember):
			return nil, errPeerIDInvalid
		case errors.Is(err, store.ErrChatAdminRequired):
			return nil, errChatAdminRequired
		case errors.Is(err, store.ErrReactionInvalid):
			return nil, errReactionInvalid
		default:
			h.log.Error("set chat available reactions", "chat_id", peerID, "user_id", r.UserID, "err", err)
			return nil, errInternal
		}
	}
	return &tg.Updates{Updates: []tg.UpdateClass{}, Date: int(h.now().Unix())}, nil
}
