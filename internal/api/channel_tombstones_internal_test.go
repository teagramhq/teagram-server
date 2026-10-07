package api

import (
	"testing"

	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/store"
)

func TestChannelMessageToTLRedactsTombstonePollAndFile(t *testing.T) {
	fileID := int64(42)
	got := channelMessageToTL(store.ChannelMessage{
		ChannelID: 7,
		LocalID:   9,
		FromID:    11,
		Message:   "deleted text",
		Deleted:   true,
		FileID:    &fileID,
		Poll:      &store.Poll{Question: []byte("deleted poll question")},
	}, 11, map[int64]*tg.Document{fileID: {ID: fileID, AccessHash: 99, MimeType: "secret/type"}})
	empty, ok := got.(*tg.MessageEmpty)
	if !ok || empty.ID != 9 {
		t.Fatalf("rendered tombstone = %#v, want MessageEmpty{id:9}", got)
	}
}
