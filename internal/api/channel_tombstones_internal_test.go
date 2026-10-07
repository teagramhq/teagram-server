package api

import (
	"testing"
	"time"

	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/store"
)

func TestChannelMessageToTLRedactsTombstonePollAndFile(t *testing.T) {
	fileID := int64(42)
	got, err := channelMessageToTL(store.ChannelMessage{
		ChannelID: 7,
		LocalID:   9,
		FromID:    11,
		Message:   "deleted text",
		Deleted:   true,
		FileID:    &fileID,
		Poll:      &store.Poll{Question: []byte("deleted poll question")},
	}, 11, map[int64]*tg.Document{fileID: {ID: fileID, AccessHash: 99, MimeType: "secret/type"}})
	if err != nil {
		t.Fatal(err)
	}
	empty, ok := got.(*tg.MessageEmpty)
	if !ok || empty.ID != 9 {
		t.Fatalf("rendered tombstone = %#v, want MessageEmpty{id:9}", got)
	}
}

func TestChannelPollUpdateDateFallsBackForTombstone(t *testing.T) {
	now := time.Date(2026, time.October, 7, 15, 0, 0, 0, time.UTC)
	clockCalls := 0
	clock := func() time.Time {
		clockCalls++
		return now
	}
	if got := channelPollUpdateDate(time.Time{}, clock); got != int(now.Unix()) {
		t.Fatalf("zero message date = %d, want server date %d", got, now.Unix())
	}
	if clockCalls != 1 {
		t.Fatalf("server clock called %d times for zero message date, want once", clockCalls)
	}

	messageDate := now.Add(-time.Hour)
	if got := channelPollUpdateDate(messageDate, clock); got != int(messageDate.Unix()) {
		t.Fatalf("live message date = %d, want original date %d", got, messageDate.Unix())
	}
	if clockCalls != 1 {
		t.Fatalf("server clock called %d times for live message date, want no additional calls", clockCalls)
	}
}
