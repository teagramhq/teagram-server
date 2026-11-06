//go:build performance

//nolint:testpackage // The diagnostic decoder is intentionally package-private.
package mtproto

import (
	"fmt"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/proto"
	"github.com/gotd/td/tg"
)

func TestPerfRPCFrameFindsDialogAndHistoryEntries(t *testing.T) {
	var dialogs, history, unrelated, wrapped bin.Buffer
	if err := (&tg.MessagesGetDialogsRequest{OffsetPeer: &tg.InputPeerEmpty{}}).Encode(&dialogs); err != nil {
		t.Fatal(err)
	}
	if err := (&tg.MessagesGetHistoryRequest{Peer: &tg.InputPeerSelf{}}).Encode(&history); err != nil {
		t.Fatal(err)
	}
	if err := (&tg.HelpGetConfigRequest{}).Encode(&unrelated); err != nil {
		t.Fatal(err)
	}
	if err := (&tg.InvokeWithoutUpdatesRequest{Query: &tg.MessagesGetHistoryRequest{Peer: &tg.InputPeerSelf{}}}).Encode(&wrapped); err != nil {
		t.Fatal(err)
	}
	container := proto.MessageContainer{Messages: []proto.Message{
		{ID: 11, Bytes: dialogs.Len(), Body: dialogs.Buf},
		{ID: 12, Bytes: history.Len(), Body: history.Buf},
		{ID: 13, Bytes: unrelated.Len(), Body: unrelated.Buf},
		{ID: 14, Bytes: wrapped.Len(), Body: wrapped.Buf},
	}}
	var encoded bin.Buffer
	if err := container.Encode(&encoded); err != nil {
		t.Fatal(err)
	}
	var got []string
	perfRPCFrame(encoded.Buf, 10, func(id int64, method string) { got = append(got, fmt.Sprintf("%d:%s", id, method)) })
	if len(got) != 4 || got[0] != "11:messages.getDialogs" || got[1] != "12:messages.getHistory" || got[2] != "13:help.getConfig" || got[3] != "14:messages.getHistory" {
		t.Fatalf("frame entries = %v", got)
	}
}
