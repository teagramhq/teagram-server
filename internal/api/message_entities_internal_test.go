package api

import (
	"errors"
	"testing"

	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/peerhash"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestPollDescriptionEntitiesCanonicalizeMentionNames(t *testing.T) {
	t.Parallel()
	h := testHandlers(nil)
	const viewerID int64 = 100
	const mentionedUserID int64 = 200
	entities, err := h.encodeMessageEntities(viewerID, "A @Bob", []tg.MessageEntityClass{
		&tg.MessageEntityBold{Offset: 0, Length: 1},
		&tg.InputMessageEntityMentionName{
			Offset: 2,
			Length: 4,
			UserID: &tg.InputUser{
				UserID:     mentionedUserID,
				AccessHash: h.peers.Derive(viewerID, peerhash.KindUser, mentionedUserID),
			},
		},
	})
	if err != nil {
		t.Fatalf("encode allowlisted entities: %v", err)
	}
	if len(entities) != 2 || entities[1].Type != store.PollDescriptionEntityMentionName || entities[1].Argument != mentionedUserID {
		t.Fatalf("stored mention = %#v, want resolved user ID %d", entities, mentionedUserID)
	}
	decoded := decodeMessageEntities(entities)
	if len(decoded) != 2 {
		t.Fatalf("decoded entity count = %d, want 2", len(decoded))
	}
	mention, ok := decoded[1].(*tg.MessageEntityMentionName)
	if !ok || mention.UserID != mentionedUserID || mention.Offset != 2 || mention.Length != 4 {
		t.Fatalf("decoded mention = %#v, want messageEntityMentionName for user %d", decoded[1], mentionedUserID)
	}
}

func TestPollDescriptionEntitiesRejectUntrustedConstructorsAndMentions(t *testing.T) {
	t.Parallel()
	h := testHandlers(nil)
	const viewerID int64 = 101
	const mentionedUserID int64 = 201
	tests := []struct {
		name    string
		entity  tg.MessageEntityClass
		wantErr error
	}{
		{
			name:    "unchecked output mention ID",
			entity:  &tg.MessageEntityMentionName{Offset: 0, Length: 3, UserID: mentionedUserID},
			wantErr: errInputRequestInvalid,
		},
		{
			name:    "input mention with invalid access hash",
			entity:  &tg.InputMessageEntityMentionName{Offset: 0, Length: 3, UserID: &tg.InputUser{UserID: mentionedUserID, AccessHash: h.peers.Derive(viewerID, peerhash.KindUser, mentionedUserID) + 1}},
			wantErr: errPeerIDInvalid,
		},
		{
			name:    "server-only entity type",
			entity:  &tg.MessageEntityFormattedDate{Offset: 0, Length: 3},
			wantErr: errInputRequestInvalid,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := h.encodeMessageEntities(viewerID, "Bob", []tg.MessageEntityClass{test.entity})
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("encode error = %v, want %v", err, test.wantErr)
			}
		})
	}
}
