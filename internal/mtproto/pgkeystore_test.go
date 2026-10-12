package mtproto_test

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/crypto"
	"github.com/gotd/td/exchange"
	"github.com/gotd/td/mt"
	"github.com/gotd/td/proto/codec"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/transport"

	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestPersistedAuthKeyReconnectAndRevocation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	blobs, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}
	st, err := store.Open(ctx, pgtest.DSN(t), pgtest.EncKey(), store.WithBlobStore(blobs))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})

	user, err := st.CreateUser(ctx, "+15551239992")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	var rawKey crypto.Key
	for i := range rawKey {
		rawKey[i] = byte(i + 1)
	}
	key := rawKey.WithID()
	keys := mtproto.NewPgAuthKeyStore(st)
	if err := keys.Save(ctx, key); err != nil {
		t.Fatalf("save auth key: %v", err)
	}
	if err := st.BindAuthKeyUserWithMetadata(ctx, key.IntID(), user.ID, store.AuthorizationMetadata{
		DeviceModel: "Old device", Platform: "old-platform", SystemVersion: "Old OS",
		APIID: 1, AppVersion: "1.0", IP: "192.0.2.10",
	}); err != nil {
		t.Fatalf("bind auth key: %v", err)
	}
	initial, ok, err := st.AuthKeyByID(ctx, key.IntID())
	if err != nil || !ok {
		t.Fatalf("load auth key before connection: ok=%v err=%v", ok, err)
	}
	if initial.UserID != user.ID {
		t.Fatalf("auth key bound to user %d, want %d", initial.UserID, user.ID)
	}

	server, addr, stopServer := startPGAuthKeyServer(t, ctx, keys)

	// A lookup hit cannot advance last_seen_at unless the frame's MAC validates.
	invalidFrame := clientFrame(t, key, 42, 1<<32, &mt.PingRequest{PingID: 1})
	invalidFrame[len(invalidFrame)-1] ^= 1
	if err := sendFrameExpectClose(ctx, addr, invalidFrame); err != nil {
		t.Fatalf("invalid encrypted frame: %v", err)
	}
	afterInvalid, ok, err := st.AuthKeyByID(ctx, key.IntID())
	if err != nil || !ok {
		t.Fatalf("load auth key after invalid frame: ok=%v err=%v", ok, err)
	}
	if !afterInvalid.LastSeenAt.Equal(initial.LastSeenAt) {
		t.Fatalf("last_seen_at changed on an unauthenticated frame: before=%s after=%s", initial.LastSeenAt, afterInvalid.LastSeenAt)
	}

	var absentRaw crypto.Key
	for i := range absentRaw {
		absentRaw[i] = byte(255 - i)
	}
	absent := absentRaw.WithID()
	if absent.ID == key.ID {
		t.Fatal("test auth keys unexpectedly share an ID")
	}
	if err := sendAuthKeyIDAndExpect404(ctx, addr, absent.ID); err != nil {
		t.Fatalf("absent key response: %v", err)
	}
	if _, ok, err := st.AuthKeyByID(ctx, absent.IntID()); err != nil || ok {
		t.Fatalf("absent key was created: ok=%v err=%v", ok, err)
	}

	originalRaw, originalConn := dialAuthKeyTestClient(t, ctx, addr)
	sendPingExpectPong(t, ctx, originalConn, key, 1<<32, 1)
	waitForRegistryConnections(t, server, 1)
	unchangedSessions, err := st.AuthKeysByUser(ctx, user.ID)
	if err != nil || len(unchangedSessions) != 1 {
		t.Fatalf("load authorization before metadata reconnect: sessions=%d err=%v", len(unchangedSessions), err)
	}
	if session := unchangedSessions[0]; session.DeviceModel != "Old device" || session.Platform != "old-platform" ||
		session.SystemVersion != "Old OS" || session.APIID != 1 || session.AppVersion != "1.0" {
		t.Fatalf("ordinary RPC without initConnection changed metadata: %+v", session)
	}
	if err := originalRaw.Close(); err != nil {
		t.Fatalf("close original connection: %v", err)
	}
	waitForRegistryConnections(t, server, 0)
	stopServer()

	// The persisted key must remain usable after a process restart: this Server
	// instance has no connection-local state from the one that just closed.
	server, addr, _ = startPGAuthKeyServer(t, ctx, keys)
	reconnectedRaw, reconnectedConn := dialAuthKeyTestClient(t, ctx, addr)
	sendInitConnectionExpectResponse(t, ctx, reconnectedConn, key, 1<<32, &tg.InitConnectionRequest{
		APIID: 2, DeviceModel: "New device", SystemVersion: "New OS", AppVersion: "2.0",
		SystemLangCode: "en", LangPack: "android", Query: &tg.HelpGetConfigRequest{},
	})
	sendPingExpectPong(t, ctx, reconnectedConn, key, 2<<32, 2)
	waitForRegistryConnections(t, server, 1)
	afterReconnect, ok, err := st.AuthKeyByID(ctx, key.IntID())
	if err != nil || !ok {
		t.Fatalf("load auth key after valid reconnect: ok=%v err=%v", ok, err)
	}
	if afterReconnect.UserID != user.ID {
		t.Fatalf("valid reconnect changed auth key binding to %d, want %d", afterReconnect.UserID, user.ID)
	}
	sessions, err := st.AuthKeysByUser(ctx, user.ID)
	if err != nil || len(sessions) != 1 {
		t.Fatalf("load authorization metadata after reconnect: sessions=%d err=%v", len(sessions), err)
	}
	if session := sessions[0]; session.DeviceModel != "New device" || session.Platform != "android" || session.SystemVersion != "New OS" ||
		session.APIID != 2 || session.AppVersion != "2.0" || session.IP != "127.0.0.1" {
		t.Fatalf("authorization metadata after reconnect = %+v, want refreshed device metadata and socket IP", session)
	}

	if err := st.DeleteAuthKey(ctx, key.IntID()); err != nil {
		t.Fatalf("delete auth key: %v", err)
	}
	revokedFrame := clientFrame(t, key, 42, 2<<32, &mt.PingRequest{PingID: 3})
	if err := reconnectedConn.Send(ctx, &bin.Buffer{Buf: revokedFrame}); err != nil {
		t.Fatalf("send frame after revocation: %v", err)
	}
	if !closedByServer(t, reconnectedRaw, 2*time.Second) {
		t.Fatal("server kept the already-bound socket open after its auth key was deleted")
	}
	waitForRegistryConnections(t, server, 0)
	if err := sendAuthKeyIDAndExpect404(ctx, addr, key.ID); err != nil {
		t.Fatalf("deleted key response: %v", err)
	}
	if _, ok, err := st.AuthKeyByID(ctx, key.IntID()); err != nil || ok {
		t.Fatalf("deleted key was restored: ok=%v err=%v", ok, err)
	}
	userKeys, err := st.AuthKeysByUser(ctx, user.ID)
	if err != nil {
		t.Fatalf("auth keys by user: %v", err)
	}
	if len(userKeys) != 0 {
		t.Fatalf("deleted key was rebound or restored: got %d auth keys", len(userKeys))
	}
}

func startPGAuthKeyServer(t *testing.T, ctx context.Context, keys mtproto.AuthKeyStore) (*mtproto.Server, string, func()) {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	serverCtx, cancel := context.WithCancel(ctx)
	handler := mtproto.UnpackInvoke(mtproto.HandlerFunc(func(c *mtproto.Conn, req *mtproto.Request) error {
		return c.SendResult(req, &tg.BoolTrue{})
	}))
	server := mtproto.New(exchange.PrivateKey{}, 2, keys, handler, nil)
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(serverCtx, listener) }()

	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			cancel()
			if err := <-serveDone; err != nil {
				t.Errorf("serve: %v", err)
			}
		})
	}
	t.Cleanup(stop)
	return server, listener.Addr().String(), stop
}

func dialAuthKeyTestClient(t *testing.T, ctx context.Context, addr string) (net.Conn, transport.Conn) {
	t.Helper()
	raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() {
		if err := raw.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("close client: %v", err)
		}
	})
	conn, err := transport.Abridged.Handshake(raw)
	if err != nil {
		t.Fatalf("transport handshake: %v", err)
	}
	return raw, conn
}

func sendPingExpectPong(t *testing.T, ctx context.Context, conn transport.Conn, key crypto.AuthKey, msgID, pingID int64) {
	t.Helper()
	sendBodyExpectPong(t, ctx, conn, key, msgID, &mt.PingRequest{PingID: pingID}, pingID)
}

func sendInitConnectionExpectResponse(t *testing.T, ctx context.Context, conn transport.Conn, key crypto.AuthKey, msgID int64, request *tg.InitConnectionRequest) {
	t.Helper()
	frame := clientFrame(t, key, 42, msgID, request)
	if err := conn.Send(ctx, &bin.Buffer{Buf: frame}); err != nil {
		t.Fatalf("send initConnection request: %v", err)
	}
	cipher := crypto.NewClientCipher(crypto.DefaultRand())
	for {
		var response bin.Buffer
		if err := conn.Recv(ctx, &response); err != nil {
			t.Fatalf("receive initConnection response: %v", err)
		}
		message, err := cipher.DecryptFromBuffer(key, &response)
		if err != nil {
			t.Fatalf("decrypt initConnection response: %v", err)
		}
		body := &bin.Buffer{Buf: message.MessageDataWithPadding[:message.MessageDataLen]}
		id, err := body.PeekID()
		if err != nil {
			t.Fatalf("peek initConnection response ID: %v", err)
		}
		if id == mt.NewSessionCreatedTypeID {
			continue
		}
		if id != mt.RPCResultTypeID {
			t.Fatalf("initConnection response ID = %#x, want rpc_result", id)
		}
		return
	}
}

func sendBodyExpectPong(t *testing.T, ctx context.Context, conn transport.Conn, key crypto.AuthKey, msgID int64, body bin.Encoder, pingID int64) {
	t.Helper()
	frame := clientFrame(t, key, 42, msgID, body)
	if err := conn.Send(ctx, &bin.Buffer{Buf: frame}); err != nil {
		t.Fatalf("send ping: %v", err)
	}
	cipher := crypto.NewClientCipher(crypto.DefaultRand())
	for {
		var response bin.Buffer
		if err := conn.Recv(ctx, &response); err != nil {
			t.Fatalf("receive ping response: %v", err)
		}
		message, err := cipher.DecryptFromBuffer(key, &response)
		if err != nil {
			t.Fatalf("decrypt ping response: %v", err)
		}
		body := &bin.Buffer{Buf: message.MessageDataWithPadding[:message.MessageDataLen]}
		id, err := body.PeekID()
		if err != nil {
			t.Fatalf("peek ping response ID: %v", err)
		}
		if id == mt.NewSessionCreatedTypeID {
			continue
		}
		var pong mt.Pong
		if err := pong.Decode(body); err != nil {
			t.Fatalf("decode ping response: %v", err)
		}
		if pong.PingID != pingID {
			t.Fatalf("pong ping ID = %d, want %d", pong.PingID, pingID)
		}
		return
	}
}

func waitForRegistryConnections(t *testing.T, server *mtproto.Server, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := server.Registry().TotalConns(); got == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("registered connections = %d, want %d", server.Registry().TotalConns(), want)
}

func sendFrameExpectClose(ctx context.Context, addr string, frame []byte) error {
	raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer raw.Close() //nolint:errcheck // this test only inspects the server response.
	conn, err := transport.Abridged.Handshake(raw)
	if err != nil {
		return err
	}
	var request bin.Buffer
	request.ResetTo(frame)
	if err := conn.Send(ctx, &request); err != nil {
		return err
	}
	var response bin.Buffer
	err = conn.Recv(ctx, &response)
	var protocolErr *codec.ProtocolErr
	if err == nil || errors.As(err, &protocolErr) {
		return errors.New("server replied instead of closing after invalid encrypted frame")
	}
	return nil
}

func sendAuthKeyIDAndExpect404(ctx context.Context, addr string, id [8]byte) error {
	raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer raw.Close() //nolint:errcheck // the protocol error is the assertion.
	conn, err := transport.Abridged.Handshake(raw)
	if err != nil {
		return err
	}
	var request bin.Buffer
	request.Put(make([]byte, 16))
	copy(request.Buf[:8], id[:])
	if err := conn.Send(ctx, &request); err != nil {
		return err
	}
	var response bin.Buffer
	err = conn.Recv(ctx, &response)
	var protocolErr *codec.ProtocolErr
	if !errors.As(err, &protocolErr) || protocolErr.Code != codec.CodeAuthKeyNotFound {
		return errors.New("server did not return -404 for absent auth key")
	}
	return nil
}
