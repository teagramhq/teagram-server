package api

import (
	"unicode/utf16"

	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/store"
)

func (h *handlers) encodeMessageEntities(viewerID int64, text string, entities []tg.MessageEntityClass) ([]store.PollDescriptionEntity, error) {
	if len(entities) > store.MaxPollDescriptionEntities {
		return nil, errEntitiesTooLong
	}
	if len(entities) == 0 {
		return nil, nil
	}

	textLength := len(utf16.Encode([]rune(text)))
	encoded := make([]store.PollDescriptionEntity, 0, len(entities))
	for _, entity := range entities {
		if entity == nil || entity.Zero() {
			return nil, errEntityBoundsInvalid
		}
		offset, length := entity.GetOffset(), entity.GetLength()
		if offset < 0 || length <= 0 || offset > textLength || length > textLength-offset {
			return nil, errEntityBoundsInvalid
		}
		stored := store.PollDescriptionEntity{Offset: offset, Length: length}
		switch entity := entity.(type) {
		case *tg.MessageEntityMention:
			stored.Type = store.PollDescriptionEntityMention
		case *tg.MessageEntityHashtag:
			stored.Type = store.PollDescriptionEntityHashtag
		case *tg.MessageEntityBotCommand:
			stored.Type = store.PollDescriptionEntityBotCommand
		case *tg.MessageEntityURL:
			stored.Type = store.PollDescriptionEntityURL
		case *tg.MessageEntityEmail:
			stored.Type = store.PollDescriptionEntityEmail
		case *tg.MessageEntityBold:
			stored.Type = store.PollDescriptionEntityBold
		case *tg.MessageEntityItalic:
			stored.Type = store.PollDescriptionEntityItalic
		case *tg.MessageEntityCode:
			stored.Type = store.PollDescriptionEntityCode
		case *tg.MessageEntityPre:
			stored.Type = store.PollDescriptionEntityPre
			stored.Text = entity.Language
		case *tg.MessageEntityTextURL:
			stored.Type = store.PollDescriptionEntityTextURL
			stored.Text = entity.URL
		case *tg.InputMessageEntityMentionName:
			userID, err := h.inputUserID(entity.UserID, viewerID)
			if err != nil {
				return nil, err
			}
			stored.Type = store.PollDescriptionEntityMentionName
			stored.Argument = userID
		case *tg.MessageEntityPhone:
			stored.Type = store.PollDescriptionEntityPhone
		case *tg.MessageEntityCashtag:
			stored.Type = store.PollDescriptionEntityCashtag
		case *tg.MessageEntityUnderline:
			stored.Type = store.PollDescriptionEntityUnderline
		case *tg.MessageEntityStrike:
			stored.Type = store.PollDescriptionEntityStrike
		case *tg.MessageEntityBankCard:
			stored.Type = store.PollDescriptionEntityBankCard
		case *tg.MessageEntitySpoiler:
			stored.Type = store.PollDescriptionEntitySpoiler
		case *tg.MessageEntityCustomEmoji:
			stored.Type = store.PollDescriptionEntityCustomEmoji
			stored.Argument = entity.DocumentID
		case *tg.MessageEntityBlockquote:
			stored.Type = store.PollDescriptionEntityBlockquote
			stored.Collapsed = entity.GetCollapsed()
		case *tg.MessageEntityMentionName:
			// Output mentions carry no proof that the sender can reference this ID.
			return nil, errInputRequestInvalid
		default:
			return nil, errInputRequestInvalid
		}
		encoded = append(encoded, stored)
	}
	return encoded, nil
}

func decodeMessageEntities(entities []store.PollDescriptionEntity) []tg.MessageEntityClass {
	decoded := make([]tg.MessageEntityClass, 0, len(entities))
	for _, entity := range entities {
		switch entity.Type {
		case store.PollDescriptionEntityMention:
			decoded = append(decoded, &tg.MessageEntityMention{Offset: entity.Offset, Length: entity.Length})
		case store.PollDescriptionEntityHashtag:
			decoded = append(decoded, &tg.MessageEntityHashtag{Offset: entity.Offset, Length: entity.Length})
		case store.PollDescriptionEntityBotCommand:
			decoded = append(decoded, &tg.MessageEntityBotCommand{Offset: entity.Offset, Length: entity.Length})
		case store.PollDescriptionEntityURL:
			decoded = append(decoded, &tg.MessageEntityURL{Offset: entity.Offset, Length: entity.Length})
		case store.PollDescriptionEntityEmail:
			decoded = append(decoded, &tg.MessageEntityEmail{Offset: entity.Offset, Length: entity.Length})
		case store.PollDescriptionEntityBold:
			decoded = append(decoded, &tg.MessageEntityBold{Offset: entity.Offset, Length: entity.Length})
		case store.PollDescriptionEntityItalic:
			decoded = append(decoded, &tg.MessageEntityItalic{Offset: entity.Offset, Length: entity.Length})
		case store.PollDescriptionEntityCode:
			decoded = append(decoded, &tg.MessageEntityCode{Offset: entity.Offset, Length: entity.Length})
		case store.PollDescriptionEntityPre:
			decoded = append(decoded, &tg.MessageEntityPre{Offset: entity.Offset, Length: entity.Length, Language: entity.Text})
		case store.PollDescriptionEntityTextURL:
			decoded = append(decoded, &tg.MessageEntityTextURL{Offset: entity.Offset, Length: entity.Length, URL: entity.Text})
		case store.PollDescriptionEntityMentionName:
			decoded = append(decoded, &tg.MessageEntityMentionName{Offset: entity.Offset, Length: entity.Length, UserID: entity.Argument})
		case store.PollDescriptionEntityPhone:
			decoded = append(decoded, &tg.MessageEntityPhone{Offset: entity.Offset, Length: entity.Length})
		case store.PollDescriptionEntityCashtag:
			decoded = append(decoded, &tg.MessageEntityCashtag{Offset: entity.Offset, Length: entity.Length})
		case store.PollDescriptionEntityUnderline:
			decoded = append(decoded, &tg.MessageEntityUnderline{Offset: entity.Offset, Length: entity.Length})
		case store.PollDescriptionEntityStrike:
			decoded = append(decoded, &tg.MessageEntityStrike{Offset: entity.Offset, Length: entity.Length})
		case store.PollDescriptionEntityBankCard:
			decoded = append(decoded, &tg.MessageEntityBankCard{Offset: entity.Offset, Length: entity.Length})
		case store.PollDescriptionEntitySpoiler:
			decoded = append(decoded, &tg.MessageEntitySpoiler{Offset: entity.Offset, Length: entity.Length})
		case store.PollDescriptionEntityCustomEmoji:
			decoded = append(decoded, &tg.MessageEntityCustomEmoji{Offset: entity.Offset, Length: entity.Length, DocumentID: entity.Argument})
		case store.PollDescriptionEntityBlockquote:
			blockquote := &tg.MessageEntityBlockquote{Offset: entity.Offset, Length: entity.Length}
			blockquote.SetCollapsed(entity.Collapsed)
			decoded = append(decoded, blockquote)
		}
	}
	return decoded
}
