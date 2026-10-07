package store

import (
	"encoding/json"
	"unicode/utf8"
)

const (
	MaxPollDescriptionEntities    = 100
	maxPollDescriptionEntityBytes = 1 << 20
	pollDescriptionEntityVersion  = 1
)

// PollDescriptionEntityType is a stable server-owned entity kind. It is
// deliberately independent of gotd's layer-specific TL constructor IDs.
type PollDescriptionEntityType int16

const (
	PollDescriptionEntityMention PollDescriptionEntityType = iota + 1
	PollDescriptionEntityHashtag
	PollDescriptionEntityBotCommand
	PollDescriptionEntityURL
	PollDescriptionEntityEmail
	PollDescriptionEntityBold
	PollDescriptionEntityItalic
	PollDescriptionEntityCode
	PollDescriptionEntityPre
	PollDescriptionEntityTextURL
	PollDescriptionEntityMentionName
	PollDescriptionEntityPhone
	PollDescriptionEntityCashtag
	PollDescriptionEntityUnderline
	PollDescriptionEntityStrike
	PollDescriptionEntityBankCard
	PollDescriptionEntitySpoiler
	PollDescriptionEntityCustomEmoji
	PollDescriptionEntityBlockquote
)

// PollDescriptionEntity stores the allowlisted fields needed to reconstruct a
// message entity. Argument carries a user or document ID; Text carries a URL
// or preformatted-code language; Collapsed is used by blockquotes.
type PollDescriptionEntity struct {
	Type      PollDescriptionEntityType `json:"type"`
	Offset    int                       `json:"offset"`
	Length    int                       `json:"length"`
	Argument  int64                     `json:"argument,omitempty"`
	Text      string                    `json:"text,omitempty"`
	Collapsed bool                      `json:"collapsed,omitempty"`
}

type storedPollDescriptionEntities struct {
	Version  int                     `json:"version"`
	Entities []PollDescriptionEntity `json:"entities"`
}

func normalizePollDescriptionEntities(entities []PollDescriptionEntity) ([]PollDescriptionEntity, error) {
	if len(entities) > MaxPollDescriptionEntities {
		return nil, ErrPollInvalid
	}
	if len(entities) == 0 {
		return nil, nil
	}
	canonical := append([]PollDescriptionEntity(nil), entities...)
	for _, entity := range canonical {
		if !validPollDescriptionEntity(entity) {
			return nil, ErrPollInvalid
		}
	}
	return canonical, nil
}

func encodePollDescriptionEntities(entities []PollDescriptionEntity) (string, error) {
	encoded, err := json.Marshal(storedPollDescriptionEntities{
		Version:  pollDescriptionEntityVersion,
		Entities: entities,
	})
	if err != nil {
		return "", err
	}
	if len(encoded) > maxPollDescriptionEntityBytes {
		return "", ErrPollInvalid
	}
	return string(encoded), nil
}

// decodePollDescriptionEntities treats invalid stored metadata as absent
// formatting. A bad entity must not prevent its containing message or poll
// from being delivered through history, differences, dialogs, or search.
func decodePollDescriptionEntities(encoded []byte) []PollDescriptionEntity {
	if len(encoded) == 0 || len(encoded) > maxPollDescriptionEntityBytes {
		return nil
	}
	var stored storedPollDescriptionEntities
	if err := json.Unmarshal(encoded, &stored); err != nil || stored.Version != pollDescriptionEntityVersion || len(stored.Entities) > MaxPollDescriptionEntities {
		return nil
	}
	entities := make([]PollDescriptionEntity, 0, len(stored.Entities))
	for _, entity := range stored.Entities {
		if validPollDescriptionEntity(entity) {
			entities = append(entities, entity)
		}
	}
	return entities
}

func validPollDescriptionEntity(entity PollDescriptionEntity) bool {
	if entity.Offset < 0 || entity.Length <= 0 || entity.Offset > maxPollTextBytes || entity.Length > maxPollTextBytes-entity.Offset {
		return false
	}
	if len(entity.Text) > maxPollTextBytes || !utf8.ValidString(entity.Text) {
		return false
	}
	switch entity.Type {
	case PollDescriptionEntityMention,
		PollDescriptionEntityHashtag,
		PollDescriptionEntityBotCommand,
		PollDescriptionEntityURL,
		PollDescriptionEntityEmail,
		PollDescriptionEntityBold,
		PollDescriptionEntityItalic,
		PollDescriptionEntityCode,
		PollDescriptionEntityPhone,
		PollDescriptionEntityCashtag,
		PollDescriptionEntityUnderline,
		PollDescriptionEntityStrike,
		PollDescriptionEntityBankCard,
		PollDescriptionEntitySpoiler:
		return entity.Argument == 0 && entity.Text == "" && !entity.Collapsed
	case PollDescriptionEntityPre:
		return entity.Argument == 0 && !entity.Collapsed
	case PollDescriptionEntityTextURL:
		return entity.Argument == 0 && entity.Text != "" && !entity.Collapsed
	case PollDescriptionEntityMentionName, PollDescriptionEntityCustomEmoji:
		return entity.Argument > 0 && entity.Text == "" && !entity.Collapsed
	case PollDescriptionEntityBlockquote:
		return entity.Argument == 0 && entity.Text == ""
	default:
		return false
	}
}
