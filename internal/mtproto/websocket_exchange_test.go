package mtproto_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/binary"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/gotd/td/bin"
	"github.com/gotd/td/crypto"
	"github.com/gotd/td/exchange"
	"github.com/gotd/td/mt"
	"github.com/gotd/td/mtproxy"
	"github.com/gotd/td/mtproxy/obfuscator"
	"github.com/gotd/td/proto"
	"github.com/gotd/td/proto/codec"
	"github.com/gotd/td/transport"

	"github.com/teagramhq/teagram-server/internal/mtproto"
)

const webSocketAbridgedLongFormThreshold = 508

var (
	webSocketExchangeKeyOnce sync.Once
	webSocketExchangeKey     *rsa.PrivateKey
	errWebSocketExchangeKey  error
)

func TestServeWebSocketEmitsPlainExchangeRepliesOverObfuscatedAbridged(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	rsaKey := webSocketTestExchangeKey(t)
	server := mtproto.New(
		exchange.PrivateKey{RSA: rsaKey},
		2,
		mtproto.NewMemoryAuthKeyStore(),
		nil,
		nil,
	)
	listener := mustListenTCP(t, ctx, "127.0.0.1:0")
	serverCtx, stopServer := context.WithCancel(ctx)
	served := make(chan error, 1)
	go func() { served <- server.ServeWebSocket(serverCtx, listener) }()
	t.Cleanup(func() {
		stopServer()
		if err := <-served; err != nil {
			t.Error("WebSocket server did not stop cleanly")
		}
	})

	ws, err := dialWebSocket(ctx, listener.Addr().String())
	if err != nil {
		t.Fatal("dial WebSocket endpoint")
	}
	t.Cleanup(func() { closeWebSocket(t, ws) })

	stream := websocket.NetConn(ctx, ws, websocket.MessageBinary)
	obfuscated := obfuscator.Obfuscated2(rand.Reader, stream)
	if err := obfuscated.Handshake(codec.Abridged{}.ObfuscatedTag(), 2, mtproxy.Secret{}); err != nil {
		t.Fatal("start obfuscated abridged transport")
	}
	deobfuscated := &deobfuscatedCaptureConn{Conn: obfuscated}
	protocol := transport.NewProtocol(func() transport.Codec {
		return codec.NoHeader{Codec: codec.Abridged{}}
	})
	client, err := protocol.Handshake(deobfuscated)
	if err != nil {
		t.Fatal("initialize abridged client transport")
	}

	var reqPQ mt.ReqPqMultiRequest
	nonce, err := crypto.RandInt128(rand.Reader)
	if err != nil {
		t.Fatal("generate client nonce")
	}
	reqPQ.Nonce = nonce
	if err := sendPlainExchangeRequest(ctx, client, time.Unix(1_700_000_000, 0), &reqPQ); err != nil {
		t.Fatal("send req_pq_multi")
	}

	resPQEnvelope, resPQFrame, err := receivePlainExchangeReply(ctx, client)
	if err != nil {
		t.Fatal("receive resPQ")
	}
	var resPQ mt.ResPQ
	if err := decodePlainExchangeReply(resPQEnvelope, &resPQ); err != nil {
		t.Fatal("decode resPQ")
	}
	const serverPQ = "17ed48941a08f981"
	wantPQ := []byte{0x17, 0xed, 0x48, 0x94, 0x1a, 0x08, 0xf9, 0x81}
	if !bytes.Equal(resPQ.Pq, wantPQ) {
		t.Fatalf("server test pq = %x, want %s", resPQ.Pq, serverPQ)
	}

	// gotd's test server always uses this documented pq fixture. Supplying its
	// factors directly keeps this transport framing test out of the randomized
	// factorization proof-of-work performed by exchange.Client.
	p := []byte{0x49, 0x4c, 0x55, 0x3b}
	q := []byte{0x53, 0x91, 0x10, 0x73}
	newNonce, err := crypto.RandInt256(rand.Reader)
	if err != nil {
		t.Fatal("generate new nonce")
	}
	innerData := &mt.PQInnerDataDC{
		Pq:          resPQ.Pq,
		P:           p,
		Q:           q,
		Nonce:       nonce,
		ServerNonce: resPQ.ServerNonce,
		NewNonce:    newNonce,
		DC:          2,
	}
	var innerDataBuffer bin.Buffer
	if err := innerData.Encode(&innerDataBuffer); err != nil {
		t.Fatal("encode p_q_inner_data_dc")
	}
	encryptedData, err := crypto.RSAPad(innerDataBuffer.Buf, &rsaKey.PublicKey, rand.Reader)
	if err != nil {
		t.Fatal("encrypt p_q_inner_data_dc")
	}
	publicKey := exchange.PrivateKey{RSA: rsaKey}.Public()
	reqDH := &mt.ReqDHParamsRequest{
		Nonce:                nonce,
		ServerNonce:          resPQ.ServerNonce,
		P:                    p,
		Q:                    q,
		PublicKeyFingerprint: publicKey.Fingerprint(),
		EncryptedData:        encryptedData,
	}
	if err := sendPlainExchangeRequest(ctx, client, time.Unix(1_700_000_004, 0), reqDH); err != nil {
		t.Fatal("send req_DH_params")
	}

	serverDHEnvelope, serverDHFrame, err := receivePlainExchangeReply(ctx, client)
	if err != nil {
		t.Fatal("receive server_DH_params")
	}
	var serverDH bin.Buffer
	serverDH.ResetTo(serverDHEnvelope.MessageData)
	serverDHParams, err := mt.DecodeServerDHParams(&serverDH)
	if err != nil {
		t.Fatal("decode server_DH_params")
	}
	if _, ok := serverDHParams.(*mt.ServerDHParamsOk); !ok {
		t.Fatalf("server_DH_params type = %T, want *mt.ServerDHParamsOk", serverDHParams)
	}

	wireFrames := parseCapturedAbridgedFrames(t, deobfuscated.snapshot())
	if len(wireFrames) != 2 {
		t.Fatal("WebSocket stream did not contain exactly two exchange replies")
	}
	responseFrames := [][]byte{resPQFrame, serverDHFrame}
	for i := range wireFrames {
		assertPlainExchangeEnvelope(t, responseFrames[i], []uint32{
			mt.ResPQTypeID,
			mt.ServerDHParamsOkTypeID,
		}[i])
		if len(wireFrames[i].payload) != len(responseFrames[i]) {
			t.Fatal("abridged frame length did not match its reassembled reply")
		}
	}
	if wireFrames[0].headerLength != 1 || len(wireFrames[0].payload) >= webSocketAbridgedLongFormThreshold {
		t.Fatal("resPQ control reply did not use short abridged framing")
	}
	if wireFrames[1].headerLength != 4 || len(wireFrames[1].payload) <= webSocketAbridgedLongFormThreshold {
		t.Fatal("server_DH_params_ok reply did not use long abridged framing")
	}
}

func webSocketTestExchangeKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	webSocketExchangeKeyOnce.Do(func() {
		webSocketExchangeKey, errWebSocketExchangeKey = rsa.GenerateKey(rand.Reader, crypto.RSAKeyBits)
	})
	if errWebSocketExchangeKey != nil {
		t.Fatal("generate synthetic RSA identity")
	}
	return webSocketExchangeKey
}

func sendPlainExchangeRequest(ctx context.Context, conn transport.Conn, at time.Time, request bin.Encoder) error {
	var requestData bin.Buffer
	if err := request.Encode(&requestData); err != nil {
		return err
	}
	message := proto.UnencryptedMessage{
		MessageID:   int64(proto.NewMessageID(at, proto.MessageFromClient)),
		MessageData: requestData.Copy(),
	}
	var frame bin.Buffer
	if err := message.Encode(&frame); err != nil {
		return err
	}
	return conn.Send(ctx, &frame)
}

func receivePlainExchangeReply(ctx context.Context, conn transport.Conn) (proto.UnencryptedMessage, []byte, error) {
	var frame bin.Buffer
	if err := conn.Recv(ctx, &frame); err != nil {
		return proto.UnencryptedMessage{}, nil, err
	}
	wireFrame := frame.Copy()
	var message proto.UnencryptedMessage
	if err := message.Decode(&frame); err != nil {
		return proto.UnencryptedMessage{}, nil, err
	}
	return message, wireFrame, nil
}

func decodePlainExchangeReply(message proto.UnencryptedMessage, response bin.Decoder) error {
	var body bin.Buffer
	body.ResetTo(message.MessageData)
	return response.Decode(&body)
}

type deobfuscatedCaptureConn struct {
	net.Conn

	mu      sync.Mutex
	readBuf []byte
}

func (c *deobfuscatedCaptureConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.mu.Lock()
		c.readBuf = append(c.readBuf, p[:n]...)
		c.mu.Unlock()
	}
	return n, err
}

func (c *deobfuscatedCaptureConn) snapshot() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.readBuf...)
}

type capturedAbridgedFrame struct {
	headerLength int
	payload      []byte
}

func parseCapturedAbridgedFrames(t *testing.T, stream []byte) []capturedAbridgedFrame {
	t.Helper()
	var frames []capturedAbridgedFrame
	for offset := 0; offset < len(stream); {
		headerLength := 1
		words := int(stream[offset])
		if words >= 127 {
			if len(stream)-offset < 4 {
				t.Fatal("truncated long abridged length")
			}
			headerLength = 4
			words = int(stream[offset+1]) | int(stream[offset+2])<<8 | int(stream[offset+3])<<16
		}
		payloadLength := words * 4
		if words == 0 || payloadLength > len(stream)-offset-headerLength {
			t.Fatal("invalid abridged frame length")
		}
		end := offset + headerLength + payloadLength
		frames = append(frames, capturedAbridgedFrame{
			headerLength: headerLength,
			payload:      append([]byte(nil), stream[offset+headerLength:end]...),
		})
		offset = end
	}
	return frames
}

func assertPlainExchangeEnvelope(t *testing.T, message []byte, constructor uint32) {
	t.Helper()
	if len(message) < 24 {
		t.Fatal("plaintext exchange envelope is too short")
	}
	if binary.LittleEndian.Uint64(message[:8]) != 0 {
		t.Fatal("plaintext exchange envelope has a nonzero auth_key_id")
	}
	messageLength := binary.LittleEndian.Uint32(message[16:20])
	if int64(messageLength) != int64(len(message)-20) {
		t.Fatal("plaintext exchange envelope msg_len does not match its body")
	}
	if binary.LittleEndian.Uint32(message[20:24]) != constructor {
		t.Fatal("plaintext exchange envelope has the wrong body constructor")
	}
}
