package e2e_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/gotd/td/tg"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/store"
)

func TestSearchResultsCalendarKeepsDateJumpSessionAndHistory(t *testing.T) {
	f := newSmokeFixture(t)
	const phoneA, phoneB = "+15552888001", "+15552888002"
	seedSmokeUsers(t, f, phoneA, phoneB)
	a := newSmokeClient(t, f, "calendar sender", phoneA)
	b := newSmokeClient(t, f, "calendar peer", phoneB)
	peer := peerUser(a.id, b.id)

	texts := []string{
		"calendar history before one",
		"calendar history before two",
		"calendar history before three",
		"calendar history after",
	}
	dates := []time.Time{
		time.Date(2024, time.May, 1, 12, 0, 0, 0, time.UTC),
		time.Date(2024, time.May, 2, 12, 0, 0, 0, time.UTC),
		time.Date(2024, time.May, 3, 10, 0, 0, 0, time.UTC),
		time.Date(2024, time.May, 3, 14, 0, 0, 0, time.UTC),
	}
	for i, message := range texts {
		if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
			_, err := client.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
				Peer: peer, Message: message, RandomID: int64(2888001 + i),
			})
			return err
		}); err != nil {
			t.Fatalf("send history message %q: %v", message, err)
		}
	}

	conn, err := pgx.Connect(f.ctx, f.dsn)
	if err != nil {
		t.Fatalf("connect to seed history dates: %v", err)
	}
	defer func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("close date seed connection: %v", err)
		}
	}()
	for i, message := range texts {
		tag, err := conn.Exec(f.ctx, `
			UPDATE messages SET date = $1
			WHERE owner_id = $2 AND peer_type = $3 AND peer_id = $4 AND message = $5
		`, dates[i], a.id, int16(store.PeerTypeUser), b.id, message)
		if err != nil {
			t.Fatalf("seed date for %q: %v", message, err)
		}
		if tag.RowsAffected() != 1 {
			t.Fatalf("seed date for %q changed %d rows, want 1", message, tag.RowsAffected())
		}
	}

	var calendar *tg.MessagesSearchResultsCalendar
	if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		var err error
		calendar, err = client.MessagesGetSearchResultsCalendar(ctx, &tg.MessagesGetSearchResultsCalendarRequest{
			Peer: peer, Filter: &tg.InputMessagesFilterPhotoVideo{},
			OffsetDate: int(time.Date(2024, time.May, 31, 23, 59, 59, 0, time.UTC).Unix()),
		})
		return err
	}); err != nil {
		t.Fatalf("messages.getSearchResultsCalendar: %v", err)
	}
	if calendar.Count != 0 || len(calendar.Periods) != 0 || len(calendar.Messages) != 0 {
		t.Fatalf("photo calendar for text-only history = count %d, periods %d, messages %d; want empty", calendar.Count, len(calendar.Periods), len(calendar.Messages))
	}

	var history tg.MessagesMessagesClass
	date := time.Date(2024, time.May, 3, 12, 0, 0, 0, time.UTC)
	if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		var err error
		history, err = client.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
			Peer: peer, OffsetDate: int(date.Unix()), AddOffset: -2, Limit: 4,
		})
		return err
	}); err != nil {
		t.Fatalf("messages.getHistory around chosen date: %v", err)
	}
	page, ok := history.(*tg.MessagesMessages)
	if !ok {
		t.Fatalf("date history result = %T, want *tg.MessagesMessages", history)
	}
	if len(page.Messages) != len(texts) {
		t.Fatalf("date history page = %d messages, want %d", len(page.Messages), len(texts))
	}
	for i, want := range []string{texts[3], texts[2], texts[1], texts[0]} {
		message, ok := page.Messages[i].(*tg.Message)
		if !ok || message.Message != want || message.Date != int(dates[len(texts)-i-1].Unix()) {
			t.Fatalf("date history message %d = %T/%+v, want %q at %s", i, page.Messages[i], page.Messages[i], want, dates[len(texts)-i-1].Format(time.RFC3339))
		}
	}
	first, ok := page.Messages[0].(*tg.Message)
	if !ok {
		t.Fatalf("newest date history message = %T, want *tg.Message", page.Messages[0])
	}
	last, ok := page.Messages[len(page.Messages)-1].(*tg.Message)
	if !ok {
		t.Fatalf("oldest date history message = %T, want *tg.Message", page.Messages[len(page.Messages)-1])
	}
	if first.Date <= int(date.Unix()) || last.Date >= int(date.Unix()) {
		t.Fatalf("date history page does not straddle %s: %s", date.Format(time.RFC3339), fmt.Sprint(page.Messages))
	}
}
