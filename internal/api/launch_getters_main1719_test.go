package api_test

import (
	"log/slog"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/pgtest"
)

func TestMAIN1719EmptyGettersThroughDispatcher(t *testing.T) {
	h := api.New(
		nil,
		2,
		&tg.Config{},
		slog.New(slog.DiscardHandler),
		false,
		1,
		nil,
		1,
		pgtest.PeerDeriver(), pgtest.PhotoDeriver(),
		config.RateLimitsConfig{},
		config.RegistrationInvite,
	)

	methods := []struct {
		name          string
		constructorID uint32
		request       func() bin.Encoder
		response      func() bin.Decoder
		assert        func(*testing.T, bin.Decoder)
	}{
		{
			name:          "stories.getPeerStories",
			constructorID: 0x2c4ada50,
			request: func() bin.Encoder {
				return &tg.StoriesGetPeerStoriesRequest{Peer: &tg.InputPeerSelf{}}
			},
			response: func() bin.Decoder { return &tg.StoriesPeerStories{} },
			assert: func(t *testing.T, response bin.Decoder) {
				t.Helper()
				got, ok := response.(*tg.StoriesPeerStories)
				if !ok {
					t.Fatalf("peer stories response = %T, want *tg.StoriesPeerStories", response)
				}
				if got.Stories.Peer == nil {
					t.Fatal("peer stories omitted the requested peer")
				}
				peer, ok := got.Stories.Peer.(*tg.PeerUser)
				if !ok || peer.UserID != 1 {
					t.Fatalf("peer stories peer = %#v, want self user 1", got.Stories.Peer)
				}
				if len(got.Stories.Stories) != 0 || len(got.Chats) != 0 || len(got.Users) != 0 {
					t.Fatalf("peer stories = %d stories, %d chats, %d users; want empty", len(got.Stories.Stories), len(got.Chats), len(got.Users))
				}
			},
		},
		{
			name:          "contacts.getBirthdays",
			constructorID: 0xdaeda864,
			request:       func() bin.Encoder { return &tg.ContactsGetBirthdaysRequest{} },
			response:      func() bin.Decoder { return &tg.ContactsContactBirthdays{} },
			assert: func(t *testing.T, response bin.Decoder) {
				t.Helper()
				got, ok := response.(*tg.ContactsContactBirthdays)
				if !ok {
					t.Fatalf("birthdays response = %T, want *tg.ContactsContactBirthdays", response)
				}
				if len(got.Contacts) != 0 || len(got.Users) != 0 {
					t.Fatalf("birthdays = %d contacts, %d users; want empty", len(got.Contacts), len(got.Users))
				}
			},
		},
		{
			name:          "messages.getPaidReactionPrivacy",
			constructorID: 0x472455aa,
			request:       func() bin.Encoder { return &tg.MessagesGetPaidReactionPrivacyRequest{} },
			response:      func() bin.Decoder { return &tg.UpdatesBox{} },
			assert: func(t *testing.T, response bin.Decoder) {
				t.Helper()
				got, ok := response.(*tg.UpdatesBox)
				if !ok {
					t.Fatalf("paid reaction privacy response = %T, want *tg.UpdatesBox", response)
				}
				short, ok := got.Updates.(*tg.UpdateShort)
				if !ok {
					t.Fatalf("paid reaction privacy update = %T, want *tg.UpdateShort", got.Updates)
				}
				privacy, ok := short.Update.(*tg.UpdatePaidReactionPrivacy)
				if !ok {
					t.Fatalf("paid reaction privacy update payload = %T, want *tg.UpdatePaidReactionPrivacy", short.Update)
				}
				if _, ok := privacy.Private.(*tg.PaidReactionPrivacyDefault); !ok {
					t.Fatalf("paid reaction privacy = %T, want default", privacy.Private)
				}
			},
		},
		{
			name:          "payments.getStarsStatus",
			constructorID: 0x4ea9b3bf,
			request: func() bin.Encoder {
				return &tg.PaymentsGetStarsStatusRequest{Peer: &tg.InputPeerSelf{}}
			},
			response: func() bin.Decoder { return &tg.PaymentsStarsStatus{} },
			assert: func(t *testing.T, response bin.Decoder) {
				t.Helper()
				got, ok := response.(*tg.PaymentsStarsStatus)
				if !ok {
					t.Fatalf("stars status response = %T, want *tg.PaymentsStarsStatus", response)
				}
				balance, ok := got.Balance.(*tg.StarsAmount)
				if !ok || balance.Amount != 0 || balance.Nanos != 0 {
					t.Fatalf("stars balance = %#v, want zero", got.Balance)
				}
				if len(got.Subscriptions) != 0 || len(got.History) != 0 || len(got.Chats) != 0 || len(got.Users) != 0 {
					t.Fatalf("stars status = %d subscriptions, %d transactions, %d chats, %d users; want empty", len(got.Subscriptions), len(got.History), len(got.Chats), len(got.Users))
				}
			},
		},
		{
			name:          "messages.getSearchCounters",
			constructorID: 0x1bbcf300,
			request: func() bin.Encoder {
				return &tg.MessagesGetSearchCountersRequest{
					Peer:    &tg.InputPeerSelf{},
					Filters: []tg.MessagesFilterClass{&tg.InputMessagesFilterEmpty{}, &tg.InputMessagesFilterPhotos{}},
				}
			},
			response: func() bin.Decoder { return &tg.MessagesSearchCounterVector{} },
			assert: func(t *testing.T, response bin.Decoder) {
				t.Helper()
				got, ok := response.(*tg.MessagesSearchCounterVector)
				if !ok {
					t.Fatalf("search counters response = %T, want *tg.MessagesSearchCounterVector", response)
				}
				if len(got.Elems) != 2 || got.Elems[0].Count != 0 || got.Elems[1].Count != 0 {
					t.Fatalf("search counters = %#v, want two zero counts", got.Elems)
				}
				if _, ok := got.Elems[0].Filter.(*tg.InputMessagesFilterEmpty); !ok {
					t.Fatalf("first search counter filter = %T, want empty filter", got.Elems[0].Filter)
				}
				if _, ok := got.Elems[1].Filter.(*tg.InputMessagesFilterPhotos); !ok {
					t.Fatalf("second search counter filter = %T, want photo filter", got.Elems[1].Filter)
				}
			},
		},
		{
			name:          "messages.getSavedHistory",
			constructorID: 0x998ab009,
			request: func() bin.Encoder {
				return &tg.MessagesGetSavedHistoryRequest{Peer: &tg.InputPeerSelf{}, Limit: 10}
			},
			response: func() bin.Decoder { return &tg.MessagesMessagesBox{} },
			assert: func(t *testing.T, response bin.Decoder) {
				t.Helper()
				box, ok := response.(*tg.MessagesMessagesBox)
				if !ok {
					t.Fatalf("saved history response = %T, want *tg.MessagesMessagesBox", response)
				}
				got, ok := box.Messages.(*tg.MessagesMessages)
				if !ok {
					t.Fatalf("saved history = %T, want *tg.MessagesMessages", box.Messages)
				}
				if len(got.Messages) != 0 || len(got.Topics) != 0 || len(got.Chats) != 0 || len(got.Users) != 0 {
					t.Fatalf("saved history = %d messages, %d topics, %d chats, %d users; want empty", len(got.Messages), len(got.Topics), len(got.Chats), len(got.Users))
				}
			},
		},
		{
			name:          "account.getConnectedBots",
			constructorID: 0x4ea4c80f,
			request:       func() bin.Encoder { return &tg.AccountGetConnectedBotsRequest{} },
			response:      func() bin.Decoder { return &tg.AccountConnectedBots{} },
			assert: func(t *testing.T, response bin.Decoder) {
				t.Helper()
				got, ok := response.(*tg.AccountConnectedBots)
				if !ok {
					t.Fatalf("connected bots response = %T, want *tg.AccountConnectedBots", response)
				}
				if len(got.ConnectedBots) != 0 || len(got.Users) != 0 {
					t.Fatalf("connected bots = %d bots, %d users; want empty", len(got.ConnectedBots), len(got.Users))
				}
			},
		},
		{
			name:          "account.getPasskeys",
			constructorID: 0xea1f0c52,
			request:       func() bin.Encoder { return &tg.AccountGetPasskeysRequest{} },
			response:      func() bin.Decoder { return &tg.AccountPasskeys{} },
			assert: func(t *testing.T, response bin.Decoder) {
				t.Helper()
				got, ok := response.(*tg.AccountPasskeys)
				if !ok {
					t.Fatalf("passkeys response = %T, want *tg.AccountPasskeys", response)
				}
				if len(got.Passkeys) != 0 {
					t.Fatalf("passkeys = %d, want empty", len(got.Passkeys))
				}
			},
		},
		{
			name:          "account.getWebAuthorizations",
			constructorID: 0x182e6d6f,
			request:       func() bin.Encoder { return &tg.AccountGetWebAuthorizationsRequest{} },
			response:      func() bin.Decoder { return &tg.AccountWebAuthorizations{} },
			assert: func(t *testing.T, response bin.Decoder) {
				t.Helper()
				got, ok := response.(*tg.AccountWebAuthorizations)
				if !ok {
					t.Fatalf("web authorizations response = %T, want *tg.AccountWebAuthorizations", response)
				}
				if len(got.Authorizations) != 0 || len(got.Users) != 0 {
					t.Fatalf("web authorizations = %d authorizations, %d users; want empty", len(got.Authorizations), len(got.Users))
				}
			},
		},
	}

	for _, method := range methods {
		t.Run(method.name, func(t *testing.T) {
			request := method.request()
			constructor, ok := request.(interface{ TypeID() uint32 })
			if !ok {
				t.Fatalf("request %T does not expose its constructor ID", request)
			}
			if got := constructor.TypeID(); got != method.constructorID {
				t.Fatalf("constructor ID = %#08x, want %#08x", got, method.constructorID)
			}

			body := dispatchSettings(t, h, settingsHandler{
				name:    method.name,
				request: func() bin.Encoder { return request },
			}, 1, false)
			response := method.response()
			if err := response.Decode(&bin.Buffer{Buf: body}); err != nil {
				t.Fatalf("decode getter response: %v", err)
			}
			method.assert(t, response)
		})
	}
}
