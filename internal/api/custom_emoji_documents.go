package api

import (
	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/mtproto"
)

const maxCustomEmojiDocumentIDs = 200

func (h *handlers) handleGetCustomEmojiDocuments(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesGetCustomEmojiDocumentsRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	if len(req.DocumentID) > maxCustomEmojiDocumentIDs {
		return nil, errCustomEmojiDocumentsTooMuch
	}

	catalog, err := h.stickerSetCatalog()
	if err != nil {
		h.log.Error("load custom emoji catalogue", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}

	userDocuments, err := h.store.UserStickerSetsByFileIDs(r.Ctx, req.DocumentID)
	if err != nil {
		h.log.Error("resolve custom emoji documents", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	historicalIDs := make([]int64, 0, len(userDocuments))
	for id, document := range userDocuments {
		if document.Set.Kind == "emojis" && (!document.Set.IsLive || !document.IsActive) {
			historicalIDs = append(historicalIDs, id)
		}
	}
	messageReferencedIDs, err := h.store.UserStickerSetMessageReferencedFileIDs(r.Ctx, r.UserID, historicalIDs)
	if err != nil {
		h.log.Error("check historical custom emoji message references", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}

	documents := make([]tg.DocumentClass, 0, len(req.DocumentID))
	for _, id := range req.DocumentID {
		if customEmoji, ok := userDocuments[id]; ok && customEmoji.Set.Kind == "emojis" {
			_, messageReferenced := messageReferencedIDs[id]
			if (customEmoji.Set.IsLive && customEmoji.IsActive) || messageReferenced {
				documents = append(documents, userStickerDocumentTL(customEmoji.Set, customEmoji.Item, h.dcID))
				continue
			}
		}
		if document, ok := catalog.documentsByID[id]; ok && isCustomEmojiDocument(document) {
			documents = append(documents, cloneStickerDocument(document, h.dcID))
		}
	}
	return &tg.DocumentClassVector{Elems: documents}, nil
}

func isCustomEmojiDocument(document *tg.Document) bool {
	if document == nil {
		return false
	}
	for _, attribute := range document.Attributes {
		if _, ok := attribute.(*tg.DocumentAttributeCustomEmoji); ok {
			return true
		}
	}
	return false
}
