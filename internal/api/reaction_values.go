package api

import (
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/store"
)

func reactionValueFromTL(reaction tg.ReactionClass) (string, int64, bool) {
	switch value := reaction.(type) {
	case *tg.ReactionEmoji:
		if value == nil || !isStandardReactionEmoji(value.Emoticon) {
			return "", 0, false
		}
		return value.Emoticon, 0, true
	case *tg.ReactionCustomEmoji:
		if value == nil {
			return "", 0, false
		}
		encoded, ok := store.CustomEmojiReactionValue(value.DocumentID)
		return encoded, value.DocumentID, ok
	default:
		return "", 0, false
	}
}

func reactionToTL(reaction string) tg.ReactionClass {
	if documentID, ok := store.CustomEmojiDocumentID(reaction); ok {
		return &tg.ReactionCustomEmoji{DocumentID: documentID}
	}
	return &tg.ReactionEmoji{Emoticon: reaction}
}
