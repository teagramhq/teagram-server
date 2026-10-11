package mtproto_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/binary"
	"errors"
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
	"github.com/gotd/td/proto/codec"
	"github.com/gotd/td/transport"

	"github.com/teagramhq/teagram-server/internal/mtproto"
)

const webSocketAbridgedLongFormThreshold = 508

var errStopAfterSecondExchangeReply = errors.New("stop after second exchange reply")

func TestServeWebSocketEmitsPlainExchangeRepliesOverObfuscatedAbridged(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	rsaKey, err := rsa.GenerateKey(rand.Reader, crypto.RSAKeyBits)
	if err != nil {
		t.Fatal("generate synthetic RSA identity")
	}
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
	responses := &stopAfterSecondExchangeReply{Conn: client}
	_, err = exchange.NewExchanger(responses, 2).
		Client([]exchange.PublicKey{exchange.PrivateKey{RSA: rsaKey}.Public()}).
		Run(ctx)
	if !errors.Is(err, errStopAfterSecondExchangeReply) {
		t.Fatal("client exchange did not reach the second server reply")
	}
	if len(responses.frames) != 2 {
		t.Fatal("client transport did not reassemble both server replies")
	}

	wireFrames := parseCapturedAbridgedFrames(t, deobfuscated.snapshot())
	if len(wireFrames) != 2 {
		t.Fatal("WebSocket stream did not contain exactly two exchange replies")
	}
	for i := range wireFrames {
		assertPlainExchangeEnvelope(t, responses.frames[i], []uint32{
			mt.ResPQTypeID,
			mt.ServerDHParamsOkTypeID,
		}[i])
		if len(wireFrames[i].payload) != len(responses.frames[i]) {
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

type stopAfterSecondExchangeReply struct {
	transport.Conn

	frames [][]byte
}

func (c *stopAfterSecondExchangeReply) Recv(ctx context.Context, b *bin.Buffer) error {
	if err := c.Conn.Recv(ctx, b); err != nil {
		return err
	}
	c.frames = append(c.frames, b.Copy())
	if len(c.frames) == 2 {
		return errStopAfterSecondExchangeReply
	}
	return nil
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
