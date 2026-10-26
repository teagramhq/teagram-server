package api

import (
	"context"
	"log/slog"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

type countingTracer struct{ n atomic.Int64 }

func (c *countingTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.n.Add(1)
	return ctx
}

func (c *countingTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// perEventMessages is the per-event lookup batchMessages replaced, kept as the
// reference the batched load must reproduce.
func perEventMessages(ctx context.Context, s *store.Store, userID int64, events []store.Event) (map[int64]store.Message, error) {
	msgs := make(map[int64]store.Message, len(events))
	for _, ev := range events {
		switch ev.Type {
		case store.EventNewMessage, store.EventEdit, store.EventReadIn, store.EventReadOut, store.EventPin, store.EventUnpin:
		default:
			continue
		}
		if ev.LocalID == 0 {
			continue
		}
		if _, done := msgs[ev.LocalID]; done {
			continue
		}
		m, ok, err := s.MessageByOwnerLocal(ctx, userID, ev.LocalID)
		if err != nil {
			return nil, err
		}
		if ok {
			msgs[ev.LocalID] = m
		}
	}
	return msgs, nil
}

// TestDifferenceMessagesLoadInOneStatement replays a 1-day and a 30-day offline
// gap: the batch must name the same rows the per-event lookup found (vanished
// and expired rows absent, an edited or read message once) in one statement.
func TestDifferenceMessagesLoadInOneStatement(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	tracer := &countingTracer{}
	s, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithoutBlobStore(), store.WithQueryTracer(tracer))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})

	owner, err := s.CreateUser(ctx, "+15551298511")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	peer, err := s.CreateUser(ctx, "+15551298512")
	if err != nil {
		t.Fatalf("create peer: %v", err)
	}

	const dayEvents, days = 8, 31
	var ownerSent []int64
	for i := range dayEvents * days / 2 {
		if i%2 == 0 {
			m, _, _, _, err := s.SendMessage(ctx, owner.ID, peer.ID, "out", int64(2*i+1), 0, 0)
			if err != nil {
				t.Fatalf("send %d: %v", i, err)
			}
			ownerSent = append(ownerSent, m.LocalID)
		} else if _, _, _, _, err := s.SendMessage(ctx, peer.ID, owner.ID, "in", int64(2*i+2), 0, 0); err != nil {
			t.Fatalf("receive %d: %v", i, err)
		}
		switch i % 6 {
		case 2:
			if _, _, err := s.EditMessage(ctx, owner.ID, ownerSent[len(ownerSent)-1], "edited"); err != nil {
				t.Fatalf("edit %d: %v", i, err)
			}
		case 3:
			if _, _, err := s.ReadHistory(ctx, owner.ID, peer.ID, 1<<40); err != nil {
				t.Fatalf("read %d: %v", i, err)
			}
		case 5:
			if _, err := s.DeleteMessages(ctx, owner.ID, ownerSent[len(ownerSent)-1:], false); err != nil {
				t.Fatalf("delete %d: %v", i, err)
			}
		}
	}
	setGroupedIDForPushTest(t, dsn,
		`UPDATE messages SET expires_at = now() - interval '1 minute' WHERE owner_id = $1 AND local_id = $2`,
		owner.ID, ownerSent[len(ownerSent)-2])

	h := &handlers{store: s, log: slog.New(slog.DiscardHandler), peers: pgtest.PeerDeriver()}
	state, err := s.StateWithoutUnread(ctx, owner.ID)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	for _, offline := range []int{1, 30} {
		events, err := s.EventsWindow(ctx, owner.ID, max(0, state.Pts-offline*dayEvents), state.Pts, maxDiffEvents)
		if err != nil {
			t.Fatalf("%d days: events: %v", offline, err)
		}
		want, err := perEventMessages(ctx, s, owner.ID, events)
		if err != nil {
			t.Fatalf("%d days: reference: %v", offline, err)
		}
		if len(want) == 0 {
			t.Fatalf("%d days: window of %d events names no message", offline, len(events))
		}
		before := tracer.n.Load()
		got, err := h.batchMessages(ctx, owner.ID, events)
		if err != nil {
			t.Fatalf("%d days: batch: %v", offline, err)
		}
		if n := tracer.n.Load() - before; n != 1 {
			t.Errorf("%d days: %d events loaded in %d statements, want 1", offline, len(events), n)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%d days: batch loaded %d messages, per-event lookup %d; contents differ", offline, len(got), len(want))
		}
	}
}
