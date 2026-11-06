//go:build performance

package mtproto

import (
	"log"
	"os"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/proto"
	"github.com/gotd/td/tg"
)

func perfRPCFrame(data []byte, messageID int64, emit func(int64, string)) {
	b := &bin.Buffer{Buf: data}
	id, err := b.PeekID()
	if err != nil {
		return
	}
	if id == proto.MessageContainerTypeID {
		var container proto.MessageContainer
		if container.Decode(b) != nil {
			return
		}
		for _, message := range container.Messages {
			perfRPCFrame(message.Body, message.ID, emit)
		}
		return
	}
	id, err = enclosedInvokeMethodID(b, id)
	if err != nil {
		return
	}
	switch id {
	case tg.MessagesGetDialogsRequestTypeID:
		emit(messageID, "messages.getDialogs")
	case tg.MessagesGetHistoryRequestTypeID:
		emit(messageID, "messages.getHistory")
	}
}

func perfRPCEvent(c *Conn, id int64, method, event string) {
	if os.Getenv("TG_PERF_PROFILE_GETDIALOGS") == "" || (method != "messages.getDialogs" && method != "messages.getHistory") {
		return
	}
	fixture := "unknown"
	switch os.Getenv("TG_PERF_FIXTURE") {
	case "1x":
		fixture = "1x"
	case "100x":
		fixture = "100x"
	}
	switch event {
	case "receive", "dispatch-frame", "handler-start", "write-wait", "write-start", "write-end":
	default:
		return
	}
	log.Printf("perf rpc time=%s fixture=%s conn=%p id=%d method=%s event=%s", time.Now().UTC().Format(time.RFC3339Nano), fixture, c, id, method, event)
}

func perfRPCFrameEvent(c *Conn, data []byte, id int64, event string) {
	if os.Getenv("TG_PERF_PROFILE_GETDIALOGS") == "" {
		return
	}
	perfRPCFrame(data, id, func(messageID int64, method string) { perfRPCEvent(c, messageID, method, event) })
}
