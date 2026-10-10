package mtproto

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/gotd/td/bin"
	"github.com/gotd/td/crypto"
	"github.com/gotd/td/exchange"
	"github.com/gotd/td/mt"
	"github.com/gotd/td/mtproxy/obfuscated2"
	"github.com/gotd/td/proto"
	"github.com/gotd/td/proto/codec"
)

const (
	webClientD1Revision               = "c88211e3985942343bf40dcbbcb8e8f5b4b7d364"
	webClientD1Origin                 = "https://d1.synthetic.invalid"
	webClientD1MaxResponseBytes       = 1 << 20
	webClientD1MaxResponseMessages    = 16
	webClientD1MaxResponseFrameLength = 16 << 20
)

type webClientD1Vector struct {
	Format      string `json:"format"`
	WebRevision string `json:"web_revision"`
	Seed        string `json:"seed"`
	Nonce       string `json:"nonce"`
	Messages    struct {
		Init       string `json:"init"`
		ReqPQMulti string `json:"req_pq_multi"`
	} `json:"messages"`
}

type webClientD1Report struct {
	Stage                      string `json:"stage"`
	Category                   string `json:"category"`
	ResponseMessageCount       int    `json:"response_message_count"`
	FirstMessageCompletePacket bool   `json:"first_message_complete_packet"`
}

func TestWebClientD1(t *testing.T) {
	outputDir := os.Getenv("D1_OUTPUT_DIR")
	if outputDir == "" {
		t.Skip("D1 diagnostic runs only in its dedicated CI job")
	}
	outputRoot, err := openWebClientD1OutputRoot(outputDir)
	if err != nil {
		t.Fatal("D1 output directory is unavailable")
	}
	t.Cleanup(func() {
		if err := outputRoot.Close(); err != nil {
			t.Error("D1 output cleanup failed")
		}
	})
	initMessage, requestMessage, expectedNonce := readWebClientD1Vector(t)

	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	rsaKey, err := rsa.GenerateKey(rand.Reader, crypto.RSAKeyBits)
	if err != nil {
		t.Fatal("D1 test key generation failed")
	}
	server := New(exchange.PrivateKey{RSA: rsaKey}, 2, NewMemoryAuthKeyStore(), nil, nil)
	server.SetWebSocketOriginPatterns([]string{webClientD1Origin})
	server.SetHandshakeTimeout(5 * time.Second)
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("D1 listener setup failed")
	}
	serverCtx, stopServer := context.WithCancel(ctx)
	served := make(chan error, 1)
	go func() { served <- server.ServeWebSocket(serverCtx, listener) }()
	t.Cleanup(func() {
		stopServer()
		if err := <-served; err != nil {
			t.Error("D1 server cleanup failed")
		}
	})

	ws, response, err := websocket.Dial(ctx, "ws://"+listener.Addr().String()+websocketPath, &websocket.DialOptions{
		HTTPHeader:   http.Header{"Origin": []string{webClientD1Origin}},
		Subprotocols: []string{"binary"},
	})
	if response != nil && response.Body != nil {
		if closeErr := response.Body.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}
	if err != nil || response == nil || response.StatusCode != http.StatusSwitchingProtocols ||
		response.Header.Get("Sec-WebSocket-Protocol") != "binary" {
		if ws != nil {
			if closeErr := ws.CloseNow(); closeErr != nil {
				t.Error("D1 WebSocket cleanup failed")
			}
		}
		writeWebClientD1Report(t, outputRoot, "upgrade", 0, false)
		return
	}
	t.Cleanup(func() {
		if err := ws.CloseNow(); err != nil {
			t.Error("D1 WebSocket cleanup failed")
		}
	})

	responseMessageCount := 0
	firstMessageCompletePacket := false
	writeFailed := ws.Write(ctx, websocket.MessageBinary, initMessage) != nil
	if !writeFailed {
		writeFailed = ws.Write(ctx, websocket.MessageBinary, requestMessage) != nil
	}
	framingAccepted := webClientD1FramingAccepted(initMessage)
	codecAccepted := webClientD1CodecAccepted(server, initMessage, requestMessage)
	stage := "exchange"
	switch {
	case !framingAccepted:
		stage = "framing"
	case !codecAccepted:
		stage = "codec"
	case writeFailed:
		stage = "framing"
	default:
		var responseMessages [][]byte
		responseBytes := 0
		for len(responseMessages) < webClientD1MaxResponseMessages {
			messageType, message, readErr := ws.Read(ctx)
			if readErr != nil {
				break
			}
			if messageType != websocket.MessageBinary {
				stage = "framing"
				break
			}
			responseBytes += len(message)
			if responseBytes > webClientD1MaxResponseBytes {
				break
			}
			responseMessages = append(responseMessages, message)
			complete, valid := webClientD1InspectResponse(initMessage, responseMessages, expectedNonce)
			if !complete {
				continue
			}
			if valid {
				firstMessageCompletePacket, _ = webClientD1InspectResponse(initMessage, responseMessages[:1], expectedNonce)
				responseMessageCount = len(responseMessages)
				responseCapture, err := webClientD1EncodeResponseMessages(responseMessages)
				if err != nil {
					t.Fatal("D1 response capture failed")
				}
				if err := outputRoot.WriteFile("server-response.json", responseCapture, 0o600); err != nil {
					t.Fatal("D1 response capture failed")
				}
				stage = "client_decode"
			}
			break
		}
	}
	writeWebClientD1Report(t, outputRoot, stage, responseMessageCount, firstMessageCompletePacket)
}

func openWebClientD1OutputRoot(outputDir string) (*os.Root, error) {
	tempDir := os.TempDir()
	relativeDir, err := filepath.Rel(tempDir, outputDir)
	if err != nil {
		return nil, err
	}
	if relativeDir == "." || !filepath.IsLocal(relativeDir) {
		return nil, errors.New("D1 output directory must be within the temp directory")
	}
	tempRoot, err := os.OpenRoot(tempDir)
	if err != nil {
		return nil, err
	}
	outputRoot, err := tempRoot.OpenRoot(relativeDir)
	if err != nil {
		return nil, errors.Join(err, tempRoot.Close())
	}
	if err := tempRoot.Close(); err != nil {
		return nil, errors.Join(err, outputRoot.Close())
	}
	return outputRoot, nil
}

func readWebClientD1Vector(t *testing.T) ([]byte, []byte, bin.Int128) {
	t.Helper()
	path := filepath.Join("testdata", "web-client-req-pq.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("D1 vector is unavailable")
	}
	var vector webClientD1Vector
	if err := json.Unmarshal(data, &vector); err != nil {
		t.Fatal("D1 vector is invalid")
	}
	if vector.Format != "teagram-web-obfuscated2-abridged-v1" ||
		vector.WebRevision != webClientD1Revision ||
		vector.Seed != "lcg32:1597" ||
		vector.Nonce != "0102030405060708090a0b0c0d0e0f10" {
		t.Fatal("D1 vector metadata is invalid")
	}
	initMessage, err := hex.DecodeString(vector.Messages.Init)
	if err != nil || len(initMessage) != 64 {
		t.Fatal("D1 init vector is invalid")
	}
	requestMessage, err := hex.DecodeString(vector.Messages.ReqPQMulti)
	if err != nil || len(requestMessage) == 0 {
		t.Fatal("D1 request vector is invalid")
	}
	nonceBytes, err := hex.DecodeString(vector.Nonce)
	if err != nil || len(nonceBytes) != len(bin.Int128{}) {
		t.Fatal("D1 nonce vector is invalid")
	}
	var nonce bin.Int128
	copy(nonce[:], nonceBytes)
	return initMessage, requestMessage, nonce
}

func TestWebClientD1ResponseValidationRequiresCompleteResPQ(t *testing.T) {
	initMessage, _, expectedNonce := readWebClientD1Vector(t)
	response := webClientD1EncryptedResponse(t, initMessage, expectedNonce)
	segments := [][]byte{response[:1], response[1:4], response[4:]}

	complete, valid := webClientD1InspectResponse(initMessage, segments, expectedNonce)
	if !complete || !valid {
		t.Fatalf("valid segmented resPQ = (%v, %v), want (true, true)", complete, valid)
	}

	wrongNonce := expectedNonce
	wrongNonce[0] ^= 0xff
	complete, valid = webClientD1InspectResponse(initMessage, segments, wrongNonce)
	if !complete || valid {
		t.Fatalf("wrong-nonce resPQ = (%v, %v), want (true, false)", complete, valid)
	}

	truncated := response[:len(response)-1]
	complete, valid = webClientD1InspectResponse(initMessage, [][]byte{truncated}, expectedNonce)
	if complete || valid {
		t.Fatalf("truncated resPQ = (%v, %v), want (false, false)", complete, valid)
	}
}

type webClientD1CipherFixture struct {
	reader *bytes.Reader
	writes bytes.Buffer
}

func (c *webClientD1CipherFixture) Read(p []byte) (int, error) { return c.reader.Read(p) }

func (c *webClientD1CipherFixture) Write(p []byte) (int, error) { return c.writes.Write(p) }

func webClientD1EncryptedResponse(t *testing.T, initMessage []byte, nonce bin.Int128) []byte {
	t.Helper()
	var responseBody bin.Buffer
	resPQ := &mt.ResPQ{
		Nonce:                       nonce,
		ServerNonce:                 bin.Int128{16, 15, 14, 13, 12, 11, 10, 9, 8, 7, 6, 5, 4, 3, 2, 1},
		Pq:                          []byte{0x17, 0x0f},
		ServerPublicKeyFingerprints: []int64{1},
	}
	if err := resPQ.Encode(&responseBody); err != nil {
		t.Fatalf("encode synthetic resPQ: %v", err)
	}
	var message bin.Buffer
	if err := (proto.UnencryptedMessage{MessageID: 1 << 32, MessageData: responseBody.Buf}).Encode(&message); err != nil {
		t.Fatalf("encode synthetic unencrypted response: %v", err)
	}
	var framed bytes.Buffer
	if err := (codec.Abridged{}).Write(&framed, &message); err != nil {
		t.Fatalf("frame synthetic response: %v", err)
	}
	fixture := &webClientD1CipherFixture{reader: bytes.NewReader(initMessage)}
	obfuscated, metadata, err := obfuscated2.Accept(fixture, nil)
	if err != nil {
		t.Fatalf("initialize synthetic response cipher: %v", err)
	}
	if metadata.Protocol != [4]byte{0xef, 0xef, 0xef, 0xef} {
		t.Fatalf("synthetic request protocol = %x", metadata.Protocol)
	}
	if _, err := obfuscated.Write(framed.Bytes()); err != nil {
		t.Fatalf("encrypt synthetic response: %v", err)
	}
	return bytes.Clone(fixture.writes.Bytes())
}

func webClientD1InspectResponse(initMessage []byte, responseMessages [][]byte, expectedNonce bin.Int128) (complete, valid bool) {
	wire := bytes.NewBuffer(bytes.Clone(initMessage))
	obfuscated, metadata, err := obfuscated2.Accept(wire, nil)
	if err != nil || metadata.Protocol != [4]byte{0xef, 0xef, 0xef, 0xef} {
		return false, false
	}
	wire.Reset()
	var plaintext []byte
	for _, message := range responseMessages {
		if _, err := obfuscated.Write(message); err != nil {
			return false, false
		}
		plaintext = append(plaintext, wire.Bytes()...)
		wire.Reset()
	}
	if len(plaintext) == 0 {
		return false, false
	}

	headerLength := 1
	words := int(plaintext[0])
	if words >= 127 {
		if len(plaintext) < 4 {
			return false, false
		}
		headerLength = 4
		words = int(plaintext[1]) | int(plaintext[2])<<8 | int(plaintext[3])<<16
	}
	if words == 0 || words > webClientD1MaxResponseFrameLength/4 {
		return true, false
	}
	frameLength := headerLength + words*4
	if len(plaintext) < frameLength {
		return false, false
	}
	if len(plaintext) != frameLength {
		return true, false
	}

	reader := bytes.NewReader(plaintext)
	var packet bin.Buffer
	if err := (codec.Abridged{}).Read(reader, &packet); err != nil || reader.Len() != 0 {
		return true, false
	}
	var message proto.UnencryptedMessage
	if err := message.Decode(&packet); err != nil || packet.Len() != 0 || message.MessageID == 0 {
		return true, false
	}
	responseBody := bin.Buffer{Buf: message.MessageData}
	var response mt.ResPQ
	if err := response.Decode(&responseBody); err != nil || responseBody.Len() != 0 {
		return true, false
	}
	return true, response.Nonce == expectedNonce
}

func webClientD1EncodeResponseMessages(messages [][]byte) ([]byte, error) {
	encoded := make([]string, 0, len(messages))
	for _, message := range messages {
		encoded = append(encoded, hex.EncodeToString(message))
	}
	data, err := json.Marshal(encoded)
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func webClientD1FramingAccepted(initMessage []byte) bool {
	client, server := net.Pipe()
	deadline := time.Now().Add(3 * time.Second)
	if err := server.SetDeadline(deadline); err != nil {
		if closeErr := closeWebClientD1Pipe(client, server); closeErr != nil {
			return false
		}
		return false
	}
	writeDone := make(chan error, 1)
	go func() {
		_, err := client.Write(initMessage[:framingPrefixLen])
		writeDone <- err
	}()
	obfuscated, stream, err := sniffFraming(server)
	var streamCloseErr error
	if stream != nil {
		streamCloseErr = stream.Close()
	}
	clientCloseErr := client.Close()
	writeErr := <-writeDone
	return err == nil && obfuscated && streamCloseErr == nil && clientCloseErr == nil && writeErr == nil
}

func webClientD1CodecAccepted(server *Server, initMessage, requestMessage []byte) bool {
	client, serverSide := net.Pipe()
	deadline := time.Now().Add(3 * time.Second)
	if err := client.SetDeadline(deadline); err != nil {
		if closeErr := closeWebClientD1Pipe(client, serverSide); closeErr != nil {
			return false
		}
		return false
	}
	if err := serverSide.SetDeadline(deadline); err != nil {
		if closeErr := closeWebClientD1Pipe(client, serverSide); closeErr != nil {
			return false
		}
		return false
	}
	writeDone := make(chan error, 1)
	go func() {
		if _, err := client.Write(initMessage); err != nil {
			writeDone <- err
			return
		}
		_, err := client.Write(requestMessage)
		writeDone <- err
	}()
	conn, err := server.detectCodec(serverSide)
	if err != nil {
		if closeErr := closeWebClientD1Pipe(client, serverSide); closeErr != nil {
			return false
		}
		<-writeDone
		return false
	}
	var packet bin.Buffer
	recvCtx, cancel := context.WithDeadline(context.Background(), deadline)
	recvErr := conn.Recv(recvCtx, &packet)
	cancel()
	connCloseErr := conn.Close()
	clientCloseErr := client.Close()
	writeErr := <-writeDone
	return recvErr == nil && len(packet.Buf) > 0 && connCloseErr == nil && clientCloseErr == nil && writeErr == nil
}

func closeWebClientD1Pipe(client, server net.Conn) error {
	clientErr := client.Close()
	serverErr := server.Close()
	if clientErr != nil {
		return clientErr
	}
	return serverErr
}

func writeWebClientD1Report(t *testing.T, outputRoot *os.Root, stage string, responseMessageCount int, firstMessageCompletePacket bool) {
	t.Helper()
	data, err := json.Marshal(webClientD1Report{
		Stage:                      stage,
		Category:                   stage,
		ResponseMessageCount:       responseMessageCount,
		FirstMessageCompletePacket: firstMessageCompletePacket,
	})
	if err != nil {
		t.Fatal("D1 stage report failed")
	}
	if err := outputRoot.WriteFile("result.json", append(data, '\n'), 0o600); err != nil {
		t.Fatal("D1 stage report failed")
	}
}
