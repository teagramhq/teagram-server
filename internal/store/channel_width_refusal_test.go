package store_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

const channelWireMax int64 = 1<<31 - 1

type channelAllocatorState struct {
	pts         int64
	nextLocalID int64
}

func setChannelAllocatorState(t *testing.T, s *store.Store, channelID int64, state channelAllocatorState) {
	t.Helper()
	ctx := context.Background()
	if err := store.SetChannelPts(ctx, s, channelID, state.pts); err != nil {
		t.Fatalf("set channel %d pts: %v", channelID, err)
	}
	if err := store.SetChannelStateNextLocalID(ctx, s, channelID, state.nextLocalID); err != nil {
		t.Fatalf("set channel %d next local_id: %v", channelID, err)
	}
}

func getChannelAllocatorState(t *testing.T, s *store.Store, channelID int64) channelAllocatorState {
	t.Helper()
	ctx := context.Background()
	var state channelAllocatorState
	if err := store.ReadChannelAllocatorStateForTest(ctx, s, channelID, &state.pts, &state.nextLocalID); err != nil {
		t.Fatalf("read channel %d allocator state: %v", channelID, err)
	}
	return state
}

func channelWidthSnapshot(t *testing.T, s *store.Store, channelID int64) [7]string {
	t.Helper()
	snapshot, err := store.ChannelWidthMutationFingerprints(context.Background(), s, channelID)
	if err != nil {
		t.Fatalf("snapshot channel %d: %v", channelID, err)
	}
	return snapshot
}

func TestChannelWidthRefusalRejectsEveryPostKindAtomically(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		post func(*testing.T, context.Context, *store.Store, store.Channel, store.User) (store.ChannelMessage, int, bool, error)
	}{
		{
			name: "text",
			post: func(_ *testing.T, ctx context.Context, s *store.Store, channel store.Channel, creator store.User) (store.ChannelMessage, int, bool, error) {
				return s.PostChannelMessageAs(ctx, channel.ID, creator.ID, "refused text", 1498001, nil, 0)
			},
		},
		{
			name: "photo",
			post: func(t *testing.T, ctx context.Context, s *store.Store, channel store.Channel, creator store.User) (store.ChannelMessage, int, bool, error) {
				t.Helper()
				photo := storedPhotoFile(t, s, creator.ID)
				message, pts, duplicate, err := s.PostChannelPhotoAs(ctx, channel.ID, creator.ID, 1498002, "refused photo", photo.ID, 0)
				return message, pts, duplicate, err
			},
		},
		{
			name: "poll",
			post: func(_ *testing.T, ctx context.Context, s *store.Store, channel store.Channel, creator store.User) (store.ChannelMessage, int, bool, error) {
				message, _, pts, duplicate, err := s.PostChannelPollAs(ctx, channel.ID, creator.ID, 1498003, "refused poll", ordinaryPollDraft())
				return message, pts, duplicate, err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := open(t)
			ctx := context.Background()
			creator := mustUser(t, s, "+15551498001")
			channel := mustChannel(t, s, creator.ID, "width refusal")
			setChannelAllocatorState(t, s, channel.ID, channelAllocatorState{pts: channelWireMax, nextLocalID: 2})
			before := channelWidthSnapshot(t, s, channel.ID)

			message, pts, duplicate, err := tc.post(t, ctx, s, channel, creator)
			if err == nil {
				t.Fatal("post succeeded when resulting channel pts would exceed int32")
			}
			if !errors.Is(err, store.ErrChannelStateExhausted) {
				t.Fatalf("post error = %v, want ErrChannelStateExhausted", err)
			}
			if !reflect.DeepEqual(message, store.ChannelMessage{}) || pts != 0 || duplicate {
				t.Errorf("refused post exposed message=%+v pts=%d duplicate=%v", message, pts, duplicate)
			}
			if after := channelWidthSnapshot(t, s, channel.ID); after != before {
				t.Errorf("channel state changed after refusal: before=%v after=%v", before, after)
			}
			pollIDs, pollErr := s.ChannelPollMessageLocalIDs(ctx, channel.ID, []int64{2})
			if pollErr != nil {
				t.Fatalf("read poll links after refusal: %v", pollErr)
			}
			if len(pollIDs) != 0 {
				t.Errorf("refused post left poll message links %v", pollIDs)
			}
		})
	}
}

func TestChannelWidthRefusalRejectsBeyondWidthLocalIDAtomically(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	creator := mustUser(t, s, "+15551498002")
	channel := mustChannel(t, s, creator.ID, "local id refusal")
	setChannelAllocatorState(t, s, channel.ID, channelAllocatorState{pts: 10, nextLocalID: channelWireMax + 1})
	before := channelWidthSnapshot(t, s, channel.ID)

	message, pts, duplicate, err := s.PostChannelMessageAs(ctx, channel.ID, creator.ID, "refused", 1498010, nil, 0)
	if err == nil {
		t.Fatal("post succeeded with next local_id beyond int32")
	}
	if !errors.Is(err, store.ErrChannelStateExhausted) {
		t.Fatalf("post error = %v, want ErrChannelStateExhausted", err)
	}
	if !reflect.DeepEqual(message, store.ChannelMessage{}) || pts != 0 || duplicate {
		t.Errorf("refused post exposed message=%+v pts=%d duplicate=%v", message, pts, duplicate)
	}
	if after := channelWidthSnapshot(t, s, channel.ID); after != before {
		t.Errorf("channel state changed after refusal: before=%v after=%v", before, after)
	}
}

func TestChannelWidthRefusalAllowsLastLegalLocalIDAndPts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("last local_id", func(t *testing.T) {
		s := open(t)
		creator := mustUser(t, s, "+15551498003")
		channel := mustChannel(t, s, creator.ID, "last local id")
		setChannelAllocatorState(t, s, channel.ID, channelAllocatorState{pts: 10, nextLocalID: channelWireMax})

		message, pts, duplicate, err := s.PostChannelMessageAs(ctx, channel.ID, creator.ID, "last legal id", 1498020, nil, 0)
		if err != nil || duplicate || message.LocalID != channelWireMax || pts != 11 {
			t.Fatalf("last local_id post = message %d pts %d duplicate %v err %v", message.LocalID, pts, duplicate, err)
		}
		if got, want := getChannelAllocatorState(t, s, channel.ID), (channelAllocatorState{pts: 11, nextLocalID: channelWireMax + 1}); got != want {
			t.Fatalf("state after last legal local_id = %+v, want %+v", got, want)
		}

		retry, retryPts, duplicate, err := s.PostChannelMessageAs(ctx, channel.ID, creator.ID, "retry", 1498020, nil, 0)
		if err != nil || !duplicate || retry.LocalID != channelWireMax || retryPts != 11 {
			t.Fatalf("idempotent retry = message %d pts %d duplicate %v err %v", retry.LocalID, retryPts, duplicate, err)
		}
		before := channelWidthSnapshot(t, s, channel.ID)
		if _, _, _, err = s.PostChannelMessageAs(ctx, channel.ID, creator.ID, "past last id", 1498021, nil, 0); err == nil {
			t.Fatal("post succeeded after the allocator cursor passed the wire maximum")
		}
		if !errors.Is(err, store.ErrChannelStateExhausted) {
			t.Fatalf("exhausted-cursor error = %v, want ErrChannelStateExhausted", err)
		}
		if after := channelWidthSnapshot(t, s, channel.ID); after != before {
			t.Errorf("state changed after exhausted-cursor refusal: before=%v after=%v", before, after)
		}
	})

	t.Run("last pts", func(t *testing.T) {
		s := open(t)
		creator := mustUser(t, s, "+15551498004")
		channel := mustChannel(t, s, creator.ID, "last pts")
		setChannelAllocatorState(t, s, channel.ID, channelAllocatorState{pts: channelWireMax - 1, nextLocalID: 40})

		message, pts, duplicate, err := s.PostChannelMessageAs(ctx, channel.ID, creator.ID, "last legal pts", 1498030, nil, 0)
		if err != nil || duplicate || message.LocalID != 40 || pts != int(channelWireMax) {
			t.Fatalf("last pts post = message %d pts %d duplicate %v err %v", message.LocalID, pts, duplicate, err)
		}
		if got, want := getChannelAllocatorState(t, s, channel.ID), (channelAllocatorState{pts: channelWireMax, nextLocalID: 41}); got != want {
			t.Fatalf("state after last legal pts = %+v, want %+v", got, want)
		}
		before := channelWidthSnapshot(t, s, channel.ID)
		if _, _, _, err = s.PostChannelMessageAs(ctx, channel.ID, creator.ID, "past last pts", 1498031, nil, 0); err == nil {
			t.Fatal("post succeeded after channel pts reached the wire maximum")
		}
		if !errors.Is(err, store.ErrChannelStateExhausted) {
			t.Fatalf("exhausted-pts error = %v, want ErrChannelStateExhausted", err)
		}
		if after := channelWidthSnapshot(t, s, channel.ID); after != before {
			t.Errorf("state changed after exhausted-pts refusal: before=%v after=%v", before, after)
		}
	})
}

func TestChannelWidthRefusalPtsOnlyDeleteIsAtomic(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	creator := mustUser(t, s, "+15551498005")
	channel := mustChannel(t, s, creator.ID, "delete refusal")
	post, _ := post(t, s, channel.ID, creator.ID, "keep me", 1498040)
	setChannelAllocatorState(t, s, channel.ID, channelAllocatorState{pts: channelWireMax, nextLocalID: 3})
	before := channelWidthSnapshot(t, s, channel.ID)

	pts, count, err := s.DeleteChannelMessages(ctx, channel.ID, creator.ID, []int64{post.LocalID})
	if err == nil {
		t.Fatal("delete succeeded when resulting channel pts would exceed int32")
	}
	if !errors.Is(err, store.ErrChannelStateExhausted) {
		t.Fatalf("delete error = %v, want ErrChannelStateExhausted", err)
	}
	if pts != 0 || count != 0 {
		t.Errorf("refused delete returned pts=%d count=%d", pts, count)
	}
	if after := channelWidthSnapshot(t, s, channel.ID); after != before {
		t.Errorf("channel state changed after delete refusal: before=%v after=%v", before, after)
	}
}

func TestChannelWidthRefusalPtsOnlyPollCloseIsAtomic(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	creator := mustUser(t, s, "+15551498006")
	channel := mustChannel(t, s, creator.ID, "poll close refusal")
	message, poll, pts, duplicate, err := s.PostChannelPollAs(ctx, channel.ID, creator.ID, 1498050, "poll", ordinaryPollDraft())
	if err != nil {
		t.Fatalf("create channel poll: %v", err)
	}
	if poll.ID <= 0 || pts <= 0 || duplicate {
		t.Fatalf("seed channel poll = poll %d pts %d duplicate %v", poll.ID, pts, duplicate)
	}
	ref := store.PollMessageRef{PeerType: store.PeerTypeChannel, PeerID: channel.ID, LocalID: message.LocalID}
	pollBefore, err := s.PollForMessage(ctx, creator.ID, ref)
	if err != nil {
		t.Fatalf("read channel poll before refusal: %v", err)
	}
	setChannelAllocatorState(t, s, channel.ID, channelAllocatorState{pts: channelWireMax, nextLocalID: 3})
	before := channelWidthSnapshot(t, s, channel.ID)

	changed, updates, err := s.ClosePollWithUpdates(ctx, creator.ID, ref)
	if err == nil {
		t.Fatal("poll close succeeded when resulting channel pts would exceed int32")
	}
	if !errors.Is(err, store.ErrChannelStateExhausted) {
		t.Fatalf("poll close error = %v, want ErrChannelStateExhausted", err)
	}
	if changed || len(updates) != 0 {
		t.Errorf("refused close returned changed=%v updates=%v", changed, updates)
	}
	if after := channelWidthSnapshot(t, s, channel.ID); after != before {
		t.Errorf("channel state changed after close refusal: before=%v after=%v", before, after)
	}
	pollAfter, err := s.PollForMessage(ctx, creator.ID, ref)
	if err != nil {
		t.Fatalf("read channel poll after refusal: %v", err)
	}
	if !reflect.DeepEqual(pollAfter, pollBefore) {
		t.Errorf("poll changed after close refusal: before=%+v after=%+v", pollBefore, pollAfter)
	}
}

func TestChannelWidthRefusalCreationServicePostIsAtomic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	s := openStore(t, dsn)
	creator := mustUser(t, s, "+15551498007")
	const channelID = store.ChannelIDMin + 1498
	store.SetChannelIDSource(s, func() (int64, error) { return channelID, nil })

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to install channel-state trigger: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) }) //nolint:errcheck // teardown
	_, err = conn.Exec(ctx, `
CREATE FUNCTION test_channel_width_exhaustion() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  NEW.pts := 2147483647;
  NEW.next_local_id := 2147483648;
  RETURN NEW;
END
$$;
CREATE TRIGGER test_channel_width_exhaustion BEFORE INSERT ON channel_state
FOR EACH ROW EXECUTE FUNCTION test_channel_width_exhaustion();`)
	if err != nil {
		t.Fatalf("install channel-state exhaustion trigger: %v", err)
	}

	before := channelWidthSnapshot(t, s, channelID)
	channel, message, pts, err := s.CreateChannelWithServiceMessage(ctx, creator.ID, "refused creation", "", false)
	if err == nil {
		t.Fatal("channel creation succeeded when its service post identifiers exceeded int32")
	}
	if !errors.Is(err, store.ErrChannelStateExhausted) {
		t.Fatalf("channel creation error = %v, want ErrChannelStateExhausted", err)
	}
	if !reflect.DeepEqual(channel, store.Channel{}) || !reflect.DeepEqual(message, store.ChannelMessage{}) || pts != 0 {
		t.Errorf("refused creation exposed channel=%+v message=%+v pts=%d", channel, message, pts)
	}
	if after := channelWidthSnapshot(t, s, channelID); after != before {
		t.Errorf("creation left channel rows behind: before=%v after=%v", before, after)
	}
	if _, ok, lookupErr := s.ChannelByID(ctx, channelID); lookupErr != nil || ok {
		t.Errorf("refused channel lookup: exists=%v err=%v, want absent", ok, lookupErr)
	}
	channels, err := s.ChannelsForUser(ctx, creator.ID)
	if err != nil {
		t.Fatalf("list creator channels after refusal: %v", err)
	}
	if len(channels) != 0 {
		t.Errorf("creator has channels after refused creation: %+v", channels)
	}
}
