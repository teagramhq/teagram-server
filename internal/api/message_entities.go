package api

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/gotd/td/tg"
	"golang.org/x/net/publicsuffix"

	"github.com/teagramhq/teagram-server/internal/store"
)

var messageURLPattern = regexp.MustCompile(`(?i)(?:https?://|tg://)[^\s\v\x{85}\p{Z}<>"']+|(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,}(?::[0-9]{1,5})?(?:[/?#][^\s\v\x{85}\p{Z}<>"']*)?`)

func (h *handlers) encodeMessageEntitiesWithURLs(viewerID int64, text string, entities []tg.MessageEntityClass) ([]store.PollDescriptionEntity, error) {
	encoded, err := h.encodeMessageEntities(viewerID, text, entities)
	if err != nil {
		return nil, err
	}
	return addDetectedMessageURLs(text, encoded)
}

func addDetectedMessageURLs(text string, entities []store.PollDescriptionEntity) ([]store.PollDescriptionEntity, error) {
	merged := append([]store.PollDescriptionEntity(nil), entities...)
	if len(merged) >= store.MaxPollDescriptionEntities {
		return merged, nil
	}
	for searchFrom := 0; searchFrom < len(text); {
		match := messageURLPattern.FindStringIndex(text[searchFrom:])
		if match == nil {
			break
		}
		start, end := searchFrom+match[0], searchFrom+match[1]
		searchFrom = end
		if start > 0 {
			previous, _ := utf8.DecodeLastRuneInString(text[:start])
			if isMessageURLWordRune(previous) || previous == '@' || previous == '.' {
				continue
			}
		}
		candidate := trimMessageURLSuffix(text[start:end])
		if candidate == "" {
			continue
		}
		if !hasExplicitMessageURLScheme(candidate) && !hasKnownMessageURLSuffix(candidate) {
			continue
		}
		end = start + len(candidate)
		offset := utf16Length(text[:start])
		length := utf16Length(text[start:end])
		if offset > store.MaxMessageEntityOffset || length > store.MaxMessageEntityOffset-offset {
			continue
		}
		if overlapsMessageEntity(offset, length, merged) {
			continue
		}
		merged = append(merged, store.PollDescriptionEntity{
			Type: store.PollDescriptionEntityURL, Offset: offset, Length: length,
		})
		if len(merged) >= store.MaxPollDescriptionEntities {
			break
		}
	}
	return merged, nil
}

func hasExplicitMessageURLScheme(candidate string) bool {
	candidate = strings.ToLower(candidate)
	return strings.HasPrefix(candidate, "http://") || strings.HasPrefix(candidate, "https://") || strings.HasPrefix(candidate, "tg://")
}

func hasKnownMessageURLSuffix(candidate string) bool {
	if hostEnd := strings.IndexAny(candidate, ":/?#"); hostEnd >= 0 {
		candidate = candidate[:hostEnd]
	}
	if candidate == "" {
		return false
	}
	candidate = strings.ToLower(candidate)
	suffix, icann := publicsuffix.PublicSuffix(candidate)
	// Telegram also accepts the special-use .onion and TON .ton suffixes.
	return icann || strings.Contains(suffix, ".") || suffix == "onion" || suffix == "ton"
}

func isMessageURLWordRune(r rune) bool {
	return r == '_' || r == '-' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

func trimMessageURLSuffix(value string) string {
	value = strings.TrimRight(value, ".,!?;:")
	var openParentheses, closeParentheses int
	var openBrackets, closeBrackets int
	var openBraces, closeBraces int
	for i := range len(value) {
		switch value[i] {
		case '(':
			openParentheses++
		case ')':
			closeParentheses++
		case '[':
			openBrackets++
		case ']':
			closeBrackets++
		case '{':
			openBraces++
		case '}':
			closeBraces++
		}
	}
	for len(value) > 0 {
		switch value[len(value)-1] {
		case ')':
			if closeParentheses <= openParentheses {
				return value
			}
			closeParentheses--
		case ']':
			if closeBrackets <= openBrackets {
				return value
			}
			closeBrackets--
		case '}':
			if closeBraces <= openBraces {
				return value
			}
			closeBraces--
		default:
			return value
		}
		value = value[:len(value)-1]
	}
	return value
}

func overlapsMessageEntity(offset, length int, entities []store.PollDescriptionEntity) bool {
	end := offset + length
	for _, entity := range entities {
		if offset < entity.Offset+entity.Length && entity.Offset < end {
			return true
		}
	}
	return false
}

func utf16Length(text string) int {
	return len(utf16.Encode([]rune(text)))
}

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
		if offset < 0 || length <= 0 || offset > textLength || length > textLength-offset ||
			offset > store.MaxMessageEntityOffset || length > store.MaxMessageEntityOffset-offset {
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
