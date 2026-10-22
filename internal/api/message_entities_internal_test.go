package api

import (
	"errors"
	"strings"
	"testing"
	"time"

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

func TestTrimMessageURLSuffixHandlesLongUnmatchedClosingBrackets(t *testing.T) {
	const url = "https://example.com/path"
	input := url + strings.Repeat(")", 100_000)

	started := time.Now()
	got := trimMessageURLSuffix(input)
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("trimMessageURLSuffix took %s for 100,000 unmatched closers, want under 250ms", elapsed)
	}
	if got != url {
		t.Fatalf("trimMessageURLSuffix = %q, want %q", got, url)
	}
}

func TestAddDetectedMessageURLsSeparatesUnicodeWhitespace(t *testing.T) {
	tests := []struct {
		name string
		text string
		urls []string
	}{
		{
			name: "scheme URLs",
			text: "https://t.me/+first\u00a0https://t.me/+second",
			urls: []string{"https://t.me/+first", "https://t.me/+second"},
		},
		{
			name: "bare domains",
			text: "t.me/+first\u00a0t.me/+second",
			urls: []string{"t.me/+first", "t.me/+second"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			entities, err := addDetectedMessageURLs(test.text, nil)
			if err != nil {
				t.Fatalf("addDetectedMessageURLs: %v", err)
			}
			if len(entities) != len(test.urls) {
				t.Fatalf("detected %d URLs, want %d: %#v", len(entities), len(test.urls), entities)
			}
			for i, url := range test.urls {
				byteOffset := strings.Index(test.text, url)
				if byteOffset < 0 {
					t.Fatalf("URL %q missing from test text %q", url, test.text)
				}
				wantOffset := utf16Length(test.text[:byteOffset])
				wantLength := utf16Length(url)
				if got := entities[i]; got.Type != store.PollDescriptionEntityURL || got.Offset != wantOffset || got.Length != wantLength {
					t.Errorf("entity %d = %#v, want URL at (%d, %d)", i, got, wantOffset, wantLength)
				}
			}
		})
	}
}
