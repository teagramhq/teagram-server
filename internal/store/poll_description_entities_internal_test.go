package store

import (
	"errors"
	"testing"
)

func TestPollEntityRangeLimitRemainsSeparateFromMessageEntities(t *testing.T) {
	entity := PollDescriptionEntity{Type: PollDescriptionEntityBold, Offset: maxPollTextBytes, Length: 1}
	if _, err := normalizePollDescriptionEntities([]PollDescriptionEntity{entity}); !errors.Is(err, ErrPollInvalid) {
		t.Fatalf("poll entity past poll text limit error = %v, want ErrPollInvalid", err)
	}

	encoded, err := EncodeMessageEntities([]PollDescriptionEntity{entity})
	if err != nil {
		t.Fatalf("encode message entity past poll text limit: %v", err)
	}
	if got := DecodeMessageEntities([]byte(encoded)); len(got) != 1 || got[0] != entity {
		t.Fatalf("decoded message entities = %#v, want %#v", got, entity)
	}
	if got := decodePollDescriptionEntities([]byte(encoded)); len(got) != 0 {
		t.Fatalf("decoded poll entities = %#v, want none past poll text limit", got)
	}
}
