package api

import (
	"fmt"
	"unicode/utf16"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
)

const maxMessageEntities = 100

func encodeMessageEntities(text string, entities []tg.MessageEntityClass) ([]byte, error) {
	if len(entities) > maxMessageEntities {
		return nil, errEntitiesTooLong
	}
	if len(entities) == 0 {
		return nil, nil
	}

	textLength := len(utf16.Encode([]rune(text)))
	encoded := bin.Buffer{}
	encoded.PutVectorHeader(len(entities))
	for _, entity := range entities {
		if entity == nil {
			return nil, errEntityBoundsInvalid
		}
		offset, length := entity.GetOffset(), entity.GetLength()
		if offset < 0 || length <= 0 || offset > textLength || length > textLength-offset {
			return nil, errEntityBoundsInvalid
		}
		if err := entity.Encode(&encoded); err != nil {
			return nil, errInputRequestInvalid
		}
	}
	return encoded.Copy(), nil
}

func decodeMessageEntities(raw []byte) ([]tg.MessageEntityClass, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	encoded := bin.Buffer{Buf: append([]byte(nil), raw...)}
	count, err := encoded.VectorHeader()
	if err != nil {
		return nil, fmt.Errorf("read message entity vector: %w", err)
	}
	if count > maxMessageEntities {
		return nil, fmt.Errorf("message entity count %d exceeds limit", count)
	}
	entities := make([]tg.MessageEntityClass, count)
	for i := range entities {
		entity, decodeErr := tg.DecodeMessageEntity(&encoded)
		if decodeErr != nil {
			return nil, fmt.Errorf("decode message entity %d: %w", i, decodeErr)
		}
		entities[i] = entity
	}
	if encoded.Len() != 0 {
		return nil, fmt.Errorf("message entity payload has %d trailing bytes", encoded.Len())
	}
	return entities, nil
}
