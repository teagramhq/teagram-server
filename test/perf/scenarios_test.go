//go:build performance

package perf_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/test/perf/perfreport"
)

const (
	readSamples            = 20
	writeSamples           = 10
	authKeyExchangeSamples = 20
	pageLimit              = 100
)

// randomIDs keeps every send in every run distinct from the seeded ones.
var randomIDs atomic.Int64

func init() { randomIDs.Store(time.Now().UnixNano()) }

func TestAuthKeyExchangeP95AllowsOneOutlier(t *testing.T) {
	for _, sc := range scenarios() {
		if sc.name != "auth key exchange" {
			continue
		}
		samples := make([]float64, sc.samples)
		for i := range samples {
			samples[i] = 10
		}
		samples[len(samples)-1] = 100
		if got := percentile(samples, 95); got >= 50 {
			t.Fatalf("p95 with one outlier = %.1f ms, want below 50 ms", got)
		}
		return
	}
	t.Fatal("auth key exchange scenario missing")
}

func TestContactsGetContactsReportsServerSideTimingBoundary(t *testing.T) {
	for _, sc := range scenarios() {
		if sc.name != "contacts.getContacts" {
			continue
		}
		for _, boundary := range []string{"server-side", "decrypted contacts RPC", "full response write", "no-reply frames excluded"} {
			if !strings.Contains(sc.note, boundary) {
				t.Errorf("contacts.getContacts note %q does not describe %q", sc.note, boundary)
			}
		}
		return
	}
	t.Fatal("contacts.getContacts scenario missing")
}

type call = func(ctx context.Context) error

// fixed is a setup for a request that needs nothing from the fixture beyond
// the env itself.
func fixed(build func(e *env) call) func(context.Context, *env) (call, string, error) {
	return func(_ context.Context, e *env) (call, string, error) { return build(e), "", nil }
}

func read(name string, build func(e *env) call) scenario {
	return scenario{name: name, kind: perfreport.Read, samples: readSamples, setup: fixed(build)}
}

// scenarios lists every RPC desktop and web send at startup and in normal
// use, in the order a session meets them. Reads come first: the writes change
// the account a little, and the reads must see the fixture as seeded.
func scenarios() []scenario {
	return []scenario{
		{
			name: "auth key exchange", kind: perfreport.Write, samples: authKeyExchangeSamples,
			note: "server time across a new connection's key exchange and first RPC; gotd's client-side DH prime check is excluded",
			setup: fixed(func(e *env) call {
				return func(ctx context.Context) error {
					d, err := e.srv.exchangeKey(ctx)
					e.elapsed = d
					return err
				}
			}),
		},
		read("help.getConfig", func(e *env) call {
			return func(ctx context.Context) error {
				_, err := e.owner.api.HelpGetConfig(ctx)
				return err
			}
		}),
		read("messages.getAvailableReactions", func(e *env) call {
			return func(ctx context.Context) error {
				_, err := e.owner.api.MessagesGetAvailableReactions(ctx, 0)
				return err
			}
		}),
		read("messages.getDialogs", func(e *env) call {
			return func(ctx context.Context) error {
				_, err := e.owner.api.MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{
					OffsetPeer: &tg.InputPeerEmpty{}, Limit: pageLimit,
				})
				return err
			}
		}),
		read("messages.getHistory newest page", func(e *env) call {
			return history(e, &tg.MessagesGetHistoryRequest{Limit: pageLimit})
		}),
		read("messages.getHistory oldest page", func(e *env) call {
			return history(e, &tg.MessagesGetHistoryRequest{OffsetID: e.f.OldestChatID, AddOffset: -pageLimit, Limit: pageLimit})
		}),
		{
			name: "messages.getHistory jump to date", kind: perfreport.Read, samples: readSamples,
			setup: jumpToDate,
		},
		read("messages.getSearchResultsCalendar photo video", func(e *env) call {
			peer := userPeer(e.f.Owner, e.f.Partner)
			middle := time.Unix(int64(e.f.MiddleDate), 0).UTC()
			monthEnd := time.Date(middle.Year(), middle.Month()+1, 0, 23, 59, 59, 0, time.UTC)
			return func(ctx context.Context) error {
				_, err := e.owner.api.MessagesGetSearchResultsCalendar(ctx, &tg.MessagesGetSearchResultsCalendarRequest{
					Peer: peer, Filter: &tg.InputMessagesFilterPhotoVideo{},
					OffsetDate: int(monthEnd.Unix()),
				})
				return err
			}
		}),
		read("messages.getHistory group", func(e *env) call {
			peer := channelPeer(e.f.Owner, e.f.Group)
			return func(ctx context.Context) error {
				_, err := e.owner.api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{Peer: peer, Limit: pageLimit})
				return err
			}
		}),
		read("messages.search", func(e *env) call {
			peer := userPeer(e.f.Owner, e.f.Partner)
			return func(ctx context.Context) error {
				_, err := e.owner.api.MessagesSearch(ctx, &tg.MessagesSearchRequest{
					Peer: peer, Q: "needle", Filter: &tg.InputMessagesFilterEmpty{}, Limit: pageLimit,
				})
				return err
			}
		}),
		read("contacts.search", func(e *env) call {
			return func(ctx context.Context) error {
				_, err := e.owner.api.ContactsSearch(ctx, &tg.ContactsSearchRequest{Q: "k00042", Limit: 50})
				return err
			}
		}),
		{
			name: "contacts.getContacts", kind: perfreport.Read, samples: readSamples, fullSet: true,
			note: "server-side time from the decrypted contacts RPC dispatch through completion of the full response write (no-reply frames excluded, client decoding excluded); answers every contact by protocol, so only its query count is held to scale invariance",
			setup: fixed(func(e *env) call {
				return func(ctx context.Context) error {
					if err := e.srv.rpcTimer.start(); err != nil {
						return err
					}
					_, err := e.owner.api.ContactsGetContacts(ctx, 0)
					elapsed, timingErr := e.srv.rpcTimer.finish(ctx)
					if err != nil {
						return err
					}
					if timingErr != nil {
						return timingErr
					}
					e.elapsed = elapsed
					return nil
				}
			}),
		},
		{name: "updates.getDifference 1 day offline", kind: perfreport.Read, samples: readSamples, setup: difference(1)},
		{name: "updates.getDifference 30 days offline", kind: perfreport.Read, samples: readSamples, setup: difference(30)},
		read("channels.getParticipants", func(e *env) call {
			channel := inputChannel(e.f.Owner, e.f.Channel)
			return func(ctx context.Context) error {
				_, err := e.owner.api.ChannelsGetParticipants(ctx, &tg.ChannelsGetParticipantsRequest{
					Channel: channel, Filter: &tg.ChannelParticipantsRecent{}, Limit: 200,
				})
				return err
			}
		}),
		{
			name: "upload.getFile first part", kind: perfreport.Read, samples: readSamples,
			setup: firstPhotoPart,
		},
		{
			name: "messages.sendMessage private", kind: perfreport.Write, samples: writeSamples,
			setup: fixed(func(e *env) call {
				peer := userPeer(e.f.Owner, e.f.Partner)
				return func(ctx context.Context) error {
					_, err := e.owner.api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
						Peer: peer, Message: "perf send", RandomID: randomIDs.Add(1),
					})
					return err
				}
			}),
		},
		{
			name: "messages.sendMessage channel", kind: perfreport.Write, samples: writeSamples,
			setup: fixed(func(e *env) call {
				peer := channelPeer(e.f.Owner, e.f.Channel)
				return func(ctx context.Context) error {
					_, err := e.owner.api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
						Peer: peer, Message: "perf post", RandomID: randomIDs.Add(1),
					})
					return err
				}
			}),
		},
		{
			name: "channel post fan-out", kind: perfreport.FanOut, samples: 5,
			note:    "from sendMessage until an online subscriber holds the post",
			setup:   fanOut,
			prepare: func(_ context.Context, e *env) error { drain(e.subscriber); return nil },
		},
		{
			name: "messages.readHistory", kind: perfreport.Write, samples: writeSamples,
			setup: fixed(func(e *env) call {
				peer := userPeer(e.f.Owner, e.f.Partner)
				return func(ctx context.Context) error {
					_, err := e.owner.api.MessagesReadHistory(ctx, &tg.MessagesReadHistoryRequest{Peer: peer})
					return err
				}
			}),
			// Each sample reads one new incoming message, as an open chat does.
			prepare: func(ctx context.Context, e *env) error {
				_, _, _, _, err := e.srv.store.SendMessage(ctx, e.f.Partner, e.f.Owner, "unread", randomIDs.Add(1), 0, 0)
				return err
			},
		},
	}
}

func history(e *env, req *tg.MessagesGetHistoryRequest) call {
	req.Peer = userPeer(e.f.Owner, e.f.Partner)
	return func(ctx context.Context) error {
		_, err := e.owner.api.MessagesGetHistory(ctx, req)
		return err
	}
}

// jumpToDate asks for the page around a date half way through the long chat,
// the request a client sends from the calendar.
func jumpToDate(ctx context.Context, e *env) (call, string, error) {
	req := &tg.MessagesGetHistoryRequest{
		Peer: userPeer(e.f.Owner, e.f.Partner), OffsetDate: e.f.MiddleDate, AddOffset: -pageLimit / 2, Limit: pageLimit,
	}
	page, err := historyPage(ctx, e.owner.api, req)
	if err != nil {
		return nil, "", err
	}
	newest, err := historyPage(ctx, e.owner.api, &tg.MessagesGetHistoryRequest{Peer: req.Peer, Limit: pageLimit})
	if err != nil {
		return nil, "", err
	}
	note := ""
	if len(page) > 0 && len(newest) > 0 && page[0].ID == newest[0].ID {
		note = "the server ignores offset_date and answers the newest page"
	}
	return func(ctx context.Context) error {
		_, err := e.owner.api.MessagesGetHistory(ctx, req)
		return err
	}, note, nil
}

// difference replays what a client that was offline for days asks first.
func difference(days int) func(context.Context, *env) (call, string, error) {
	return func(ctx context.Context, e *env) (call, string, error) {
		state, err := e.owner.api.UpdatesGetState(ctx)
		if err != nil {
			return nil, "", err
		}
		req := &tg.UpdatesGetDifferenceRequest{
			Pts:  max(1, state.Pts-days*e.f.Spec.DayEvents),
			Date: state.Date - days*86400,
			Qts:  state.Qts,
		}
		return func(ctx context.Context) error {
			_, err := e.owner.api.UpdatesGetDifference(ctx, req)
			return err
		}, "", nil
	}
}

func firstPhotoPart(ctx context.Context, e *env) (call, string, error) {
	res, err := e.owner.api.MessagesSearch(ctx, &tg.MessagesSearchRequest{
		Peer: channelPeer(e.f.Owner, e.f.Group), Filter: &tg.InputMessagesFilterPhotos{}, Limit: 1,
	})
	if err != nil {
		return nil, "", err
	}
	modified, ok := res.AsModified()
	if !ok || len(modified.GetMessages()) == 0 {
		return nil, "", errors.New("no photo in the group")
	}
	msg, ok := modified.GetMessages()[0].(*tg.Message)
	if !ok {
		return nil, "", errors.New("photo search returned a service message")
	}
	media, ok := msg.Media.(*tg.MessageMediaPhoto)
	if !ok {
		return nil, "", fmt.Errorf("photo message media = %T", msg.Media)
	}
	photo, ok := media.Photo.(*tg.Photo)
	if !ok || len(photo.Sizes) == 0 {
		return nil, "", errors.New("photo without sizes")
	}
	req := &tg.UploadGetFileRequest{
		Location: &tg.InputPhotoFileLocation{
			ID: photo.ID, AccessHash: photo.AccessHash, FileReference: photo.FileReference,
			ThumbSize: photo.Sizes[len(photo.Sizes)-1].GetType(),
		},
		Limit: 128 << 10,
	}
	return func(ctx context.Context) error {
		_, err := e.owner.api.UploadGetFile(ctx, req)
		return err
	}, "", nil
}

func fanOut(_ context.Context, e *env) (call, string, error) {
	peer := channelPeer(e.f.Owner, e.f.Channel)
	return func(ctx context.Context) error {
		text := fmt.Sprintf("fan-out %d", randomIDs.Add(1))
		if _, err := e.owner.api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: peer, Message: text, RandomID: randomIDs.Add(1),
		}); err != nil {
			return err
		}
		return awaitPost(ctx, e.subscriber, e.f.Channel, text)
	}, "", nil
}

func drain(c *client) {
	for {
		select {
		case <-c.updates:
		default:
			return
		}
	}
}

func awaitPost(ctx context.Context, c *client, channelID int64, text string) error {
	timeout := time.After(10 * time.Second)
	for {
		select {
		case u := <-c.updates:
			if carriesPost(u, channelID, text) {
				return nil
			}
		case <-timeout:
			return errors.New("subscriber did not receive the post within 10 s")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func carriesPost(u tg.UpdatesClass, channelID int64, text string) bool {
	var updates []tg.UpdateClass
	switch v := u.(type) {
	case *tg.Updates:
		updates = v.Updates
	case *tg.UpdatesCombined:
		updates = v.Updates
	case *tg.UpdateShort:
		updates = []tg.UpdateClass{v.Update}
	}
	for _, update := range updates {
		n, ok := update.(*tg.UpdateNewChannelMessage)
		if !ok {
			continue
		}
		msg, ok := n.Message.(*tg.Message)
		if !ok || msg.Message != text {
			continue
		}
		if peer, ok := msg.PeerID.(*tg.PeerChannel); ok && peer.ChannelID == channelID {
			return true
		}
	}
	return false
}
