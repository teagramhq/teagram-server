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

type privateChatMemberSnapshot struct {
	ownerID     int64
	peerID      int64
	nextLocalID int64
	topMessage  int64
	unreadCount int32
}

type privateChatSnapshot struct {
	members [2]privateChatMemberSnapshot
}

func privateChatLifecycle() (prepare func(context.Context, *env) error, cleanup func(context.Context, *env) error) {
	var snapshot *privateChatSnapshot
	prepare = func(ctx context.Context, e *env) error {
		var err error
		snapshot, err = capturePrivateChatSnapshot(ctx, e)
		return err
	}
	cleanup = func(ctx context.Context, e *env) error {
		if snapshot == nil {
			return errors.New("private chat state was not captured before sample")
		}
		previous := snapshot
		snapshot = nil
		return cleanupPrivateChatSample(ctx, e, previous)
	}
	return prepare, cleanup
}

func capturePrivateChatSnapshot(ctx context.Context, e *env) (*privateChatSnapshot, error) {
	if e.cleanupDB == nil {
		return nil, errors.New("private chat cleanup connection is not configured")
	}
	snapshot := &privateChatSnapshot{}
	participants := [2][2]int64{{e.f.Owner, e.f.Partner}, {e.f.Partner, e.f.Owner}}
	for i, participant := range participants {
		member := privateChatMemberSnapshot{ownerID: participant[0], peerID: participant[1]}
		if err := e.cleanupDB.QueryRow(ctx, `
			SELECT state.next_local_id, dialog.top_message, dialog.unread_count
			FROM update_state AS state
			JOIN dialogs AS dialog
			  ON dialog.owner_id = state.user_id
			 AND dialog.peer_type = 1
			 AND dialog.peer_id = $2
			WHERE state.user_id = $1`, member.ownerID, member.peerID).Scan(
			&member.nextLocalID, &member.topMessage, &member.unreadCount,
		); err != nil {
			return nil, fmt.Errorf("snapshot private dialog %d/%d: %w", member.ownerID, member.peerID, err)
		}
		snapshot.members[i] = member
	}
	return snapshot, nil
}

func cleanupPrivateChatSample(ctx context.Context, e *env, snapshot *privateChatSnapshot) error {
	tx, err := e.cleanupDB.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin private chat cleanup: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	// Connected perf clients can already have observed these events, so keep
	// update_state and message_events monotonic while removing the history rows.
	for _, member := range snapshot.members {
		deleted, err := tx.Exec(ctx, `
			DELETE FROM messages
			WHERE owner_id = $1 AND local_id = $2 AND peer_type = 1 AND peer_id = $3`,
			member.ownerID, member.nextLocalID, member.peerID,
		)
		if err != nil {
			return fmt.Errorf("delete private sample message %d/%d/%d: %w", member.ownerID, member.peerID, member.nextLocalID, err)
		}
		if deleted.RowsAffected() != 1 {
			return fmt.Errorf("private sample messages deleted for %d/%d: got %d, want 1", member.ownerID, member.peerID, deleted.RowsAffected())
		}
		updated, err := tx.Exec(ctx, `
			UPDATE dialogs SET top_message = $3, unread_count = $4
			WHERE owner_id = $1 AND peer_type = 1 AND peer_id = $2`,
			member.ownerID, member.peerID, member.topMessage, member.unreadCount,
		)
		if err != nil {
			return fmt.Errorf("restore private dialog %d/%d: %w", member.ownerID, member.peerID, err)
		}
		if updated.RowsAffected() != 1 {
			return fmt.Errorf("private dialog restored for %d/%d: got %d rows, want 1", member.ownerID, member.peerID, updated.RowsAffected())
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit private chat cleanup: %w", err)
	}
	return nil
}

func TestMutatingPrivateScenariosUseLongChatAndCleanSamples(t *testing.T) {
	e := &env{f: fixture{Owner: 1, Partner: 2, Contacts: []int64{3}}}
	seen := map[string]bool{}
	for _, sc := range scenarios() {
		switch sc.name {
		case "messages.sendMessage private", "messages.readHistory":
			seen[sc.name] = true
			if sc.privatePeer == nil {
				t.Errorf("%s has no private chat target", sc.name)
				continue
			}
			peer := sc.privatePeer(e)
			if peer.UserID != e.f.Partner {
				t.Errorf("%s targets %d, want long-chat partner %d", sc.name, peer.UserID, e.f.Partner)
			}
			if sc.prepare == nil {
				t.Errorf("%s does not snapshot state before each sample", sc.name)
			}
			if sc.cleanup == nil {
				t.Errorf("%s does not clean up after each sample", sc.name)
			}
		}
	}
	for _, name := range []string{"messages.sendMessage private", "messages.readHistory"} {
		if !seen[name] {
			t.Errorf("scenario %q missing", name)
		}
	}
}

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
	privateSend := scenario{
		name: "messages.sendMessage private", kind: perfreport.Write, samples: writeSamples,
		privatePeer: longChatPeer,
	}
	privateSend.prepare, privateSend.cleanup = privateChatLifecycle()
	privateSend.setup = fixed(func(e *env) call {
		peer := privateSend.privatePeer(e)
		return func(ctx context.Context) error {
			_, err := e.owner.api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
				Peer: peer, Message: "perf send", RandomID: randomIDs.Add(1),
			})
			return err
		}
	})
	readHistory := scenario{
		name: "messages.readHistory", kind: perfreport.Write, samples: writeSamples,
		privatePeer: longChatPeer,
	}
	captureReadHistoryState, cleanupReadHistorySample := privateChatLifecycle()
	readHistory.cleanup = cleanupReadHistorySample
	readHistory.setup = fixed(func(e *env) call {
		peer := readHistory.privatePeer(e)
		return func(ctx context.Context) error {
			_, err := e.owner.api.MessagesReadHistory(ctx, &tg.MessagesReadHistoryRequest{Peer: peer})
			return err
		}
	})
	// Each sample reads one new incoming message, as an open chat does.
	readHistory.prepare = func(ctx context.Context, e *env) error {
		if err := captureReadHistoryState(ctx, e); err != nil {
			return err
		}
		peer := readHistory.privatePeer(e)
		_, _, _, _, err := e.srv.store.SendMessage(ctx, peer.UserID, e.f.Owner, "unread", randomIDs.Add(1), 0, 0)
		return err
	}

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
		privateSend,
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
		readHistory,
	}
}

func history(e *env, req *tg.MessagesGetHistoryRequest) call {
	req.Peer = userPeer(e.f.Owner, e.f.Partner)
	return func(ctx context.Context) error {
		_, err := e.owner.api.MessagesGetHistory(ctx, req)
		return err
	}
}

func longChatPeer(e *env) *tg.InputPeerUser {
	return userPeer(e.f.Owner, e.f.Partner)
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
