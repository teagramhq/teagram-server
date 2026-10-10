package mtproto_test

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/crypto"
	"github.com/gotd/td/exchange"
	"github.com/gotd/td/mt"
	"github.com/gotd/td/proto"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/teagramhq/teagram-server/internal/mtproto"
)

func TestMsgsStateReqInContainerReturnsMsgsStateInfo(t *testing.T) {
	t.Parallel()

	key := rebindTestKey()
	const sessionID = 42
	const getDifferenceID = int64(1 << 32)
	const stateReqID = getDifferenceID + 4
	const containerID = stateReqID + 4
	container := mtprotoMessageContainer(t, getDifferenceID, containerID,
		&tg.UpdatesGetDifferenceRequest{},
		&mt.MsgsStateReq{MsgIDs: []int64{getDifferenceID}},
	)
	conn := &recordingFrameConn{frames: [][]byte{clientFrame(t, key, sessionID, containerID, container)}}
	if err := serveServiceMessageFrame(t, key, conn); !errors.Is(err, io.EOF) {
		t.Fatalf("ServeConn = %v, want io.EOF", err)
	}

	replies := conn.replies(t, key)
	var got mt.MsgsStateInfo
	if !decodeServiceReply(replies, &got) {
		t.Fatalf("server replies contain no msgs_state_info: %x", replies)
	}
	if got.ReqMsgID != stateReqID {
		t.Fatalf("msgs_state_info.req_msg_id = %d, want container message id %d", got.ReqMsgID, stateReqID)
	}
	if len(got.Info) != 1 {
		t.Fatalf("msgs_state_info.info has %d bytes, want 1 for one requested id", len(got.Info))
	}
	if got.Info[0] != 236 {
		t.Fatalf("msgs_state_info.info[0] = %d, want received and answered status 236", got.Info[0])
	}
	if hasRPCError(replies) {
		t.Fatalf("service message produced an RPC error: %x", replies)
	}
}

func TestMsgResendReqReturnsMsgsStateInfo(t *testing.T) {
	t.Parallel()

	key := rebindTestKey()
	const requestID = int64(1 << 32)
	conn := &recordingFrameConn{frames: [][]byte{clientFrame(t, key, 42, requestID,
		&mt.MsgResendReq{MsgIDs: []int64{requestID - 4, requestID + 4}},
	)}}
	if err := serveServiceMessageFrame(t, key, conn); !errors.Is(err, io.EOF) {
		t.Fatalf("ServeConn = %v, want io.EOF", err)
	}

	replies := conn.replies(t, key)
	var got mt.MsgsStateInfo
	if !decodeServiceReply(replies, &got) {
		t.Fatalf("server replies contain no msgs_state_info: %x", replies)
	}
	if got.ReqMsgID != requestID {
		t.Fatalf("msgs_state_info.req_msg_id = %d, want %d", got.ReqMsgID, requestID)
	}
	if len(got.Info) != 2 {
		t.Fatalf("msgs_state_info.info has %d bytes, want 2 for two requested ids", len(got.Info))
	}
	if got.Info[0] != 1 || got.Info[1] != 3 {
		t.Fatalf("msgs_state_info.info = %v, want unknown and too-high status bytes", got.Info)
	}
	if hasRPCError(replies) {
		t.Fatalf("service message produced an RPC error: %x", replies)
	}
}

func TestMsgsAllInfoIsNotRoutedAsRPC(t *testing.T) {
	t.Parallel()

	key := rebindTestKey()
	conn := &recordingFrameConn{frames: [][]byte{clientFrame(t, key, 42, 1<<32,
		&mt.MsgsAllInfo{MsgIDs: []int64{1<<32 - 4}, Info: []byte{1}},
	)}}
	if err := serveServiceMessageFrame(t, key, conn); !errors.Is(err, io.EOF) {
		t.Fatalf("ServeConn = %v, want io.EOF", err)
	}

	replies := conn.replies(t, key)
	if hasRPCError(replies) {
		t.Fatalf("service message produced an RPC error: %x", replies)
	}
	if len(replies) != 1 {
		t.Fatalf("server emitted %d replies, want only new_session_created", len(replies))
	}
	var created mt.NewSessionCreated
	if err := created.Decode(&bin.Buffer{Buf: replies[0]}); err != nil {
		t.Fatalf("server reply = %x, want new_session_created: %v", replies[0], err)
	}
}

func mtprotoMessageContainer(t *testing.T, firstID, containerID int64, messages ...bin.Encoder) *proto.MessageContainer {
	t.Helper()
	container := &proto.MessageContainer{Messages: make([]proto.Message, 0, len(messages))}
	for i, message := range messages {
		var body bin.Buffer
		if err := message.Encode(&body); err != nil {
			t.Fatalf("encode container message %d: %v", i, err)
		}
		container.Messages = append(container.Messages, proto.Message{
			ID:    firstID + int64(i*4),
			SeqNo: i * 2,
			Bytes: body.Len(),
			Body:  body.Copy(),
		})
	}
	if lastID := firstID + int64((len(messages)-1)*4); containerID <= lastID {
		t.Fatalf("container id %d must be after its final inner message %d", containerID, lastID)
	}
	return container
}

func serveServiceMessageFrame(t *testing.T, key crypto.AuthKey, conn *recordingFrameConn) error {
	t.Helper()
	keys := mtproto.NewMemoryAuthKeyStore()
	if err := keys.Save(context.Background(), key); err != nil {
		t.Fatalf("save auth key: %v", err)
	}
	dispatcher := mtproto.NewDispatcher().
		HandleFunc(tg.UpdatesGetDifferenceRequestTypeID, func(c *mtproto.Conn, req *mtproto.Request) error {
			return c.SendResult(req, &tg.BoolTrue{})
		}).
		Fallback(mtproto.HandlerFunc(func(c *mtproto.Conn, req *mtproto.Request) error {
			return c.SendErr(req, tgerr.New(400, "INPUT_METHOD_INVALID"))
		}))
	return mtproto.New(exchange.PrivateKey{}, 2, keys, dispatcher, nil).ServeConn(context.Background(), conn)
}

func decodeServiceReply(replies [][]byte, response *mt.MsgsStateInfo) bool {
	for _, reply := range replies {
		if err := response.Decode(&bin.Buffer{Buf: reply}); err == nil {
			return true
		}
	}
	return false
}

func hasRPCError(replies [][]byte) bool {
	for _, reply := range replies {
		var response mt.RPCError
		if err := response.Decode(&bin.Buffer{Buf: reply}); err == nil {
			return true
		}
	}
	return false
}
