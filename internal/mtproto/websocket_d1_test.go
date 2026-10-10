package mtproto

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/hex"
	"encoding/json"
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
)

const (
	webClientD1Revision = "c88211e3985942343bf40dcbbcb8e8f5b4b7d364"
	webClientD1Origin   = "https://d1.synthetic.invalid"
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
	Stage    string `json:"stage"`
	Category string `json:"category"`
}

func TestWebClientD1(t *testing.T) {
	initMessage, requestMessage := readWebClientD1Vector(t)
	resultPath := os.Getenv("D1_RESULT_PATH")
	responsePath := os.Getenv("D1_RESPONSE_PATH")
	if resultPath == "" || responsePath == "" {
		t.Fatal("D1 output paths are required")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	rsaKey, err := rsa.GenerateKey(rand.Reader, crypto.RSAKeyBits)
	if err != nil {
		t.Fatal("D1 test key generation failed")
	}
	server := New(exchange.PrivateKey{RSA: rsaKey}, 2, NewMemoryAuthKeyStore(), nil, nil)
	server.SetWebSocketOriginPatterns([]string{webClientD1Origin})
	server.SetHandshakeTimeout(5 * time.Second)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
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
		writeWebClientD1Report(t, resultPath, "upgrade")
		return
	}
	t.Cleanup(func() {
		if err := ws.CloseNow(); err != nil {
			t.Error("D1 WebSocket cleanup failed")
		}
	})

	writeFailed := ws.Write(ctx, websocket.MessageBinary, initMessage) != nil
	if !writeFailed {
		writeFailed = ws.Write(ctx, websocket.MessageBinary, requestMessage) != nil
	}
	framingAccepted := webClientD1FramingAccepted(initMessage)
	codecAccepted := webClientD1CodecAccepted(server, initMessage, requestMessage)
	var (
		responseType  websocket.MessageType
		responseBytes []byte
		responseErr   error
	)
	if !writeFailed {
		responseType, responseBytes, responseErr = ws.Read(ctx)
	}
	gotMessage := responseErr == nil
	if gotMessage && (!framingAccepted || !codecAccepted) {
		t.Fatal("D1 stage instrumentation is inconsistent")
	}

	stage := "exchange"
	switch {
	case !framingAccepted:
		stage = "framing"
	case !codecAccepted:
		stage = "codec"
	case writeFailed:
		stage = "framing"
	case gotMessage && responseType != websocket.MessageBinary:
		stage = "framing"
	case gotMessage:
		stage = "client_decode"
	}
	if gotMessage && responseType == websocket.MessageBinary && stage == "client_decode" {
		if err := os.WriteFile(responsePath, responseBytes, 0o600); err != nil {
			t.Fatal("D1 response capture failed")
		}
	}
	writeWebClientD1Report(t, resultPath, stage)
}

func readWebClientD1Vector(t *testing.T) ([]byte, []byte) {
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
	return initMessage, requestMessage
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

func writeWebClientD1Report(t *testing.T, path, stage string) {
	t.Helper()
	data, err := json.Marshal(webClientD1Report{Stage: stage, Category: stage})
	if err != nil {
		t.Fatal("D1 stage report failed")
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal("D1 stage report failed")
	}
}
