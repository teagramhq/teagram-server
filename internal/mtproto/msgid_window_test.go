package mtproto_test

import (
	"context"
	"errors"
	"io"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/crypto"
	"github.com/gotd/td/exchange"
	"github.com/gotd/td/mt"
	"github.com/gotd/td/proto"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/mtproto"
)

// clientMsgIDAt returns a client msg_id dated at.
func clientMsgIDAt(at time.Time) int64 {
	id := int64(proto.NewMessageID(at, proto.MessageFromClient))
	return id - id%4
}

// serverReply is one decrypted message the server wrote, with its envelope id.
type serverReply struct {
	msgID int64
	body  []byte
}

func envelopeReplies(t *testing.T, conn *recordingFrameConn, key crypto.AuthKey) []serverReply {
	t.Helper()
	conn.mu.Lock()
	frames := slices.Clone(conn.sent)
	conn.mu.Unlock()
	cipher := crypto.NewClientCipher(crypto.DefaultRand())
	out := make([]serverReply, 0, len(frames))
	for _, f := range frames {
		m := &crypto.EncryptedMessage{}
		if err := m.DecodeWithoutCopy(&bin.Buffer{Buf: f}); err != nil {
			t.Fatalf("decode server frame: %v", err)
		}
		msg, err := cipher.Decrypt(key, m)
		if err != nil {
			t.Fatalf("decrypt server frame: %v", err)
		}
		out = append(out, serverReply{msgID: msg.MessageID, body: slices.Clone(msg.Data())})
	}
	return out
}

func badMsgNotifications(replies []serverReply) map[int64]serverReply {
	out := map[int64]serverReply{}
	for _, r := range replies {
		var bad mt.BadMsgNotification
		if err := bad.Decode(&bin.Buffer{Buf: r.body}); err == nil {
			out[bad.BadMsgID] = r
		}
	}
	return out
}

func badMsgCode(t *testing.T, r serverReply) int {
	t.Helper()
	var bad mt.BadMsgNotification
	if err := bad.Decode(&bin.Buffer{Buf: r.body}); err != nil {
		t.Fatalf("decode bad_msg_notification: %v", err)
	}
	return bad.ErrorCode
}

// serveMsgIDWindow serves frames on a server with the msg_id time window on, as
// in production, and returns the msg_ids the handler executed.
func serveMsgIDWindow(t *testing.T, key crypto.AuthKey, conn *recordingFrameConn) []int64 {
	t.Helper()
	keys := mtproto.NewMemoryAuthKeyStore()
	if err := keys.Save(context.Background(), key); err != nil {
		t.Fatalf("save auth key: %v", err)
	}
	var (
		mu       sync.Mutex
		executed []int64
	)
	dispatcher := mtproto.NewDispatcher().
		HandleFunc(tg.HelpGetConfigRequestTypeID, func(c *mtproto.Conn, req *mtproto.Request) error {
			mu.Lock()
			executed = append(executed, req.MsgID)
			mu.Unlock()
			return c.SendResult(req, &tg.BoolTrue{})
		})
	server := mtproto.New(exchange.PrivateKey{}, 2, keys, dispatcher, nil)
	mtproto.EnableMsgIDTimeCheck(server)
	if err := server.ServeConn(context.Background(), conn); !errors.Is(err, io.EOF) {
		t.Fatalf("ServeConn = %v, want io.EOF", err)
	}
	mu.Lock()
	defer mu.Unlock()
	return executed
}

func TestMsgIDOutsideTimeWindowIsRefusedUnexecuted(t *testing.T) {
	t.Parallel()

	// Offsets, not times: a parallel subtest can start well after the parent.
	for _, tc := range []struct {
		name   string
		offset time.Duration
		code   int
	}{
		{name: "ten minutes old", offset: -10 * time.Minute, code: 16},
		{name: "just past the old bound", offset: -310 * time.Second, code: 16},
		{name: "one minute ahead", offset: time.Minute, code: 17},
		{name: "just past the future bound", offset: 40 * time.Second, code: 17},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			key := rebindTestKey()
			skewed := clientMsgIDAt(time.Now().Add(tc.offset))
			current := clientMsgIDAt(time.Now())
			conn := &recordingFrameConn{frames: [][]byte{
				clientFrame(t, key, 42, skewed, &tg.HelpGetConfigRequest{}),
				clientFrame(t, key, 42, current, &tg.HelpGetConfigRequest{}),
			}}
			sent := time.Now()
			executed := serveMsgIDWindow(t, key, conn)

			if !slices.Equal(executed, []int64{current}) {
				t.Fatalf("executed msg_ids = %v, want only the current %d", executed, current)
			}
			replies := envelopeReplies(t, conn, key)
			bad := badMsgNotifications(replies)
			r, ok := bad[skewed]
			if !ok || len(bad) != 1 {
				t.Fatalf("got %d bad_msg_notifications, want one for the skewed request %d", len(bad), skewed)
			}
			if got := badMsgCode(t, r); got != tc.code {
				t.Fatalf("bad_msg_notification error_code = %d, want %d", got, tc.code)
			}
			// The envelope id is the server's clock, which a client resyncs from.
			if drift := proto.MessageID(r.msgID).Time().Sub(sent).Abs(); drift > 5*time.Second {
				t.Fatalf("bad_msg_notification envelope time is %v from server time, want within 5s", drift)
			}
			if proto.MessageID(r.msgID).Type() != proto.MessageServerResponse {
				t.Fatalf("bad_msg_notification envelope type = %v, want server response", proto.MessageID(r.msgID).Type())
			}
			if findResult(t, bodies(replies), current) == nil {
				t.Fatalf("no rpc_result for the current request %d", current)
			}
		})
	}
}

func TestMsgIDInsideTimeWindowIsExecuted(t *testing.T) {
	t.Parallel()

	key := rebindTestKey()
	now := time.Now()
	ids := []int64{
		clientMsgIDAt(now.Add(-290 * time.Second)),
		clientMsgIDAt(now),
		clientMsgIDAt(now.Add(20 * time.Second)),
	}
	frames := make([][]byte, 0, len(ids))
	for _, id := range ids {
		frames = append(frames, clientFrame(t, key, 42, id, &tg.HelpGetConfigRequest{}))
	}
	conn := &recordingFrameConn{frames: frames}
	executed := serveMsgIDWindow(t, key, conn)

	if !slices.Equal(executed, ids) {
		t.Fatalf("executed msg_ids = %v, want %v", executed, ids)
	}
	if bad := badMsgNotifications(envelopeReplies(t, conn, key)); len(bad) != 0 {
		t.Fatalf("got %d bad_msg_notifications inside the window, want none", len(bad))
	}
}

func TestMsgIDWindowRefusesOnlyTheStaleEntryOfAContainer(t *testing.T) {
	t.Parallel()

	key := rebindTestKey()
	now := time.Now()
	stale := clientMsgIDAt(now.Add(-10 * time.Minute))
	first := clientMsgIDAt(now)
	last := first + 8
	containerID := last + 4
	entries := []int64{first, stale, last}
	container := &proto.MessageContainer{}
	for i, id := range entries {
		var body bin.Buffer
		if err := (&tg.HelpGetConfigRequest{}).Encode(&body); err != nil {
			t.Fatalf("encode entry %d: %v", i, err)
		}
		container.Messages = append(container.Messages, proto.Message{ID: id, SeqNo: i*2 + 1, Bytes: body.Len(), Body: body.Copy()})
	}
	conn := &recordingFrameConn{frames: [][]byte{clientFrame(t, key, 42, containerID, container)}}
	executed := serveMsgIDWindow(t, key, conn)

	if !slices.Equal(executed, []int64{first, last}) {
		t.Fatalf("executed msg_ids = %v, want %v", executed, []int64{first, last})
	}
	bad := badMsgNotifications(envelopeReplies(t, conn, key))
	r, ok := bad[stale]
	if !ok || len(bad) != 1 {
		t.Fatalf("got %d bad_msg_notifications, want one for the stale entry %d", len(bad), stale)
	}
	if got := badMsgCode(t, r); got != 16 {
		t.Fatalf("bad_msg_notification error_code = %d, want 16", got)
	}
	var notice mt.BadMsgNotification
	if err := notice.Decode(&bin.Buffer{Buf: r.body}); err != nil {
		t.Fatalf("decode bad_msg_notification: %v", err)
	}
	if notice.BadMsgSeqno != 3 {
		t.Fatalf("bad_msg_notification bad_msg_seqno = %d, want the entry's seqno 3", notice.BadMsgSeqno)
	}
}

func TestMsgIDWindowLeavesAcknowledgementsAlone(t *testing.T) {
	t.Parallel()

	key := rebindTestKey()
	stale := clientMsgIDAt(time.Now().Add(-10 * time.Minute))
	conn := &recordingFrameConn{frames: [][]byte{clientFrame(t, key, 42, stale, &mt.MsgsAck{MsgIDs: []int64{1}})}}
	serveMsgIDWindow(t, key, conn)

	if bad := badMsgNotifications(envelopeReplies(t, conn, key)); len(bad) != 0 {
		t.Fatalf("got %d bad_msg_notifications for msgs_ack, want none", len(bad))
	}
}

func bodies(replies []serverReply) [][]byte {
	out := make([][]byte, 0, len(replies))
	for _, r := range replies {
		out = append(out, r.body)
	}
	return out
}
