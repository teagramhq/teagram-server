package api_test

import (
	"context"
	"log/slog"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

func savedDialogTestHandler(s *store.Store) mtproto.Handler {
	return api.New(
		s,
		2,
		&tg.Config{},
		slog.New(slog.DiscardHandler),
		false,
		100<<20,
		nil,
		2<<30,
		pgtest.PeerDeriver(),
		pgtest.PhotoDeriver(),
		config.RateLimitsConfig{},
		config.RegistrationClosed,
	)
}

func TestGetSavedDialogsIncludesMyNotesAndLatestMessage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	user, err := s.CreateUser(ctx, "+15551232037")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	h := savedDialogTestHandler(s)
	get := func() *tg.MessagesSavedDialogs {
		t.Helper()
		method := settingsHandler{
			name: "messages.getSavedDialogs",
			request: func() bin.Encoder {
				return &tg.MessagesGetSavedDialogsRequest{
					OffsetPeer: &tg.InputPeerSelf{},
					Limit:      10,
				}
			},
		}
		body := dispatchSettings(t, h, method, user.ID, false)
		var response tg.MessagesSavedDialogs
		if err := response.Decode(&bin.Buffer{Buf: body}); err != nil {
			t.Fatalf("decode messages.getSavedDialogs response: %v", err)
		}
		return &response
	}

	assertSelfDialog := func(response *tg.MessagesSavedDialogs, topMessage int) {
		t.Helper()
		if len(response.Dialogs) != 1 {
			t.Fatalf("saved dialogs = %d, want only My Notes", len(response.Dialogs))
		}
		dialog, ok := response.Dialogs[0].(*tg.SavedDialog)
		if !ok {
			t.Fatalf("saved dialog = %T, want *tg.SavedDialog", response.Dialogs[0])
		}
		peer, ok := dialog.Peer.(*tg.PeerUser)
		if !ok || peer.UserID != user.ID || dialog.TopMessage != topMessage || dialog.GetPinned() {
			t.Fatalf("saved dialog = %+v, want unpinned self peer %d with top message %d", dialog, user.ID, topMessage)
		}
		if len(response.Chats) != 0 || len(response.Users) != 1 {
			t.Fatalf("saved dialogs entities = %d chats, %d users; want self user only", len(response.Chats), len(response.Users))
		}
		self, ok := response.Users[0].(*tg.User)
		if !ok || !self.Self || self.ID != user.ID {
			t.Fatalf("saved dialog user = %#v, want self user %d", response.Users[0], user.ID)
		}
	}

	empty := get()
	assertSelfDialog(empty, 0)
	if len(empty.Messages) != 0 {
		t.Fatalf("empty My Notes messages = %d, want none", len(empty.Messages))
	}

	if _, err := api.SendMessageForTest(s, user.ID, &tg.MessagesSendMessageRequest{
		Peer: &tg.InputPeerSelf{}, Message: "saved note", RandomID: 2037001,
	}); err != nil {
		t.Fatalf("send self message: %v", err)
	}
	populated := get()
	assertSelfDialog(populated, 1)
	if len(populated.Messages) != 1 {
		t.Fatalf("My Notes messages = %d, want latest message", len(populated.Messages))
	}
	message, ok := populated.Messages[0].(*tg.Message)
	if !ok || message.ID != 1 || message.Message != "saved note" || !message.Out {
		t.Fatalf("My Notes top message = %#v, want outgoing saved note #1", populated.Messages[0])
	}
}

func TestGetPinnedSavedDialogsReturnsEmptyList(t *testing.T) {
	t.Parallel()
	h := savedDialogTestHandler(nil)
	method := settingsHandler{
		name:    "messages.getPinnedSavedDialogs",
		request: func() bin.Encoder { return &tg.MessagesGetPinnedSavedDialogsRequest{} },
	}
	body := dispatchSettings(t, h, method, 1, false)
	var response tg.MessagesSavedDialogs
	if err := response.Decode(&bin.Buffer{Buf: body}); err != nil {
		t.Fatalf("decode messages.getPinnedSavedDialogs response: %v", err)
	}
	if len(response.Dialogs) != 0 || len(response.Messages) != 0 || len(response.Chats) != 0 || len(response.Users) != 0 {
		t.Fatalf("pinned saved dialogs = %+v, want empty vectors", response)
	}
}

func TestGetWebPageReturnsEmptyPreviewWithoutStoredPage(t *testing.T) {
	t.Parallel()
	h := savedDialogTestHandler(nil)
	for _, tc := range []struct {
		name string
		hash int
	}{
		{name: "first request", hash: 0},
		{name: "unknown cached page hash", hash: 12345},
	} {
		t.Run(tc.name, func(t *testing.T) {
			method := settingsHandler{
				name: "messages.getWebPage",
				request: func() bin.Encoder {
					return &tg.MessagesGetWebPageRequest{URL: "https://example.invalid/page", Hash: tc.hash}
				},
			}
			body := dispatchSettings(t, h, method, 1, false)
			var response tg.MessagesWebPage
			if err := response.Decode(&bin.Buffer{Buf: body}); err != nil {
				t.Fatalf("decode messages.getWebPage response: %v", err)
			}
			if _, ok := response.Webpage.(*tg.WebPageEmpty); !ok {
				t.Fatalf("webpage = %T, want webPageEmpty when no preview is stored (hash %d)", response.Webpage, tc.hash)
			}
			if len(response.Chats) != 0 || len(response.Users) != 0 {
				t.Fatalf("webpage entities = %d chats, %d users; want empty", len(response.Chats), len(response.Users))
			}
		})
	}
}
