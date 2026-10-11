package mtproto

import (
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/crypto"
	"github.com/gotd/td/exchange"
)

const (
	webPermanentExchangeTestOnlyMarker = "teagram-web-single-permanent-exchange-server-test-only"
	webPermanentExchangeOrigin         = "https://permanent-exchange.synthetic.invalid"
	webPermanentExchangeTestName       = "runs one shipped-Web permanent exchange against the real server"
)

type webPermanentExchangeConfig struct {
	Origin    string                     `json:"origin"`
	Endpoint  string                     `json:"endpoint"`
	PublicKey webPermanentExchangePublic `json:"public_key"`
}

type webPermanentExchangePublic struct {
	Modulus     string `json:"modulus"`
	Exponent    string `json:"exponent"`
	Fingerprint string `json:"fingerprint"`
}

type webPermanentExchangeClientResult struct {
	ClientStage string `json:"client_stage"`
}

func TestWebClientPermanentExchange(t *testing.T) {
	if os.Getenv("WEB_PERMANENT_CI_ONLY") != "1" {
		t.Skip("synthetic Web exchange runs in its dedicated CI job")
	}
	webRoot := os.Getenv("WEB_PERMANENT_WEB_ROOT")
	if !filepath.IsAbs(webRoot) {
		t.Fatal("synthetic Web checkout is unavailable")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()

	rsaKey, err := rsa.GenerateKey(newWebPermanentExchangeReader("rsa-key"), crypto.RSAKeyBits)
	if err != nil {
		t.Fatal("synthetic RSA identity generation failed")
	}
	privateKey := exchange.PrivateKey{RSA: rsaKey}
	publicKey := webPermanentExchangePublic{
		Modulus:     fmt.Sprintf("%x", rsaKey.N),
		Exponent:    fmt.Sprintf("%x", rsaKey.E),
		Fingerprint: formatWebPermanentExchangeFingerprint(privateKey.Fingerprint()),
	}
	if len(publicKey.Modulus) != 512 {
		t.Fatal("synthetic RSA identity has an invalid size")
	}
	if publicKey.Exponent == "3" {
		publicKey.Exponent = "03"
	} else if len(publicKey.Exponent)%2 != 0 {
		publicKey.Exponent = "0" + publicKey.Exponent
	}

	store := &webPermanentExchangeStore{AuthKeyStore: NewMemoryAuthKeyStore(), saved: make(chan struct{}, 2)}
	logs := &webPermanentExchangeLogHandler{}
	server := New(privateKey, 2, store, nil, slog.New(logs))
	server.cipher = crypto.NewServerCipher(newWebPermanentExchangeReader("server"))
	server.SetWebSocketOriginPatterns([]string{webPermanentExchangeOrigin})
	server.SetHandshakeTimeout(5 * time.Second)
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("synthetic WebSocket listener setup failed")
	}
	serverCtx, stopServer := context.WithCancel(ctx)
	served := make(chan error, 1)
	go func() { served <- server.ServeWebSocket(serverCtx, listener) }()
	t.Cleanup(func() {
		stopServer()
		select {
		case serveErr := <-served:
			if serveErr != nil && !errors.Is(serveErr, net.ErrClosed) {
				t.Error("synthetic WebSocket server did not stop cleanly")
			}
		case <-time.After(5 * time.Second):
			if closeErr := listener.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
				t.Error("synthetic WebSocket listener cleanup failed")
			}
			select {
			case serveErr := <-served:
				if serveErr != nil && !errors.Is(serveErr, net.ErrClosed) {
					t.Error("synthetic WebSocket server did not stop cleanly")
				}
			case <-time.After(time.Second):
				t.Error("synthetic WebSocket server did not stop")
			}
		}
	})

	tempDir := t.TempDir()
	endpoint := "ws://" + listener.Addr().String() + "/apiws"
	configPath := filepath.Join(tempDir, "config.json")
	resultPath := filepath.Join(tempDir, "client-result.json")
	config := webPermanentExchangeConfig{
		Origin:    webPermanentExchangeOrigin,
		Endpoint:  endpoint,
		PublicKey: publicKey,
	}
	encodedConfig, err := json.Marshal(config)
	if err != nil {
		t.Fatal("synthetic Web configuration could not be encoded")
	}
	if err := os.WriteFile(configPath, encodedConfig, 0o600); err != nil {
		t.Fatal("synthetic Web configuration could not be written")
	}

	clientCtx, stopClient := context.WithTimeout(ctx, 30*time.Second)
	defer stopClient()
	command := exec.CommandContext(
		clientCtx,
		"node",
		"node_modules/vitest/vitest.mjs",
		"run",
		"--pool=threads",
		"--maxWorkers=1",
		"--testNamePattern=^"+webPermanentExchangeTestName+"$",
		"src/tests/websocketPermanentExchange.test.ts",
	)
	command.Dir = webRoot
	command.Env = append(os.Environ(),
		"WEB_PERMANENT_CI_ONLY=1",
		"WEB_PERMANENT_CONFIG_PATH="+configPath,
		"WEB_PERMANENT_CLIENT_RESULT_PATH="+resultPath,
	)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	clientRunErr := command.Run()

	clientStage := readWebPermanentExchangeClientStage(resultPath)
	serverStage, serverCause := logs.snapshot()
	keyStored := store.waitForOneStoredKey(ctx, 2*time.Second)
	t.Logf("synthetic_web_exchange client_stage=%s server_stage=%s server_cause=%s", clientStage, serverStage, serverCause)
	if clientRunErr != nil || clientStage != "dh_gen_verified" || !keyStored {
		t.Fatalf("single permanent exchange failed: client_stage=%s server_stage=%s server_cause=%s", clientStage, serverStage, serverCause)
	}
}

func readWebPermanentExchangeClientStage(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "unknown"
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var result webPermanentExchangeClientResult
	if decoder.Decode(&result) != nil || !allowedWebPermanentExchangeClientStage(result.ClientStage) {
		return "unknown"
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return "unknown"
	}
	return result.ClientStage
}

func allowedWebPermanentExchangeClientStage(value string) bool {
	switch value {
	case "req_pq_sent", "res_pq_ok", "req_dh_sent", "server_dh_received", "server_dh_validated", "set_client_dh_sent", "dh_gen_verified", "unknown":
		return true
	default:
		return false
	}
}

func formatWebPermanentExchangeFingerprint(fingerprint int64) string {
	const digits = "0123456789abcdef"
	var output [16]byte
	for position := range output {
		shift := 4 * (len(output) - 1 - position)
		output[position] = digits[(fingerprint>>shift)&0xf]
	}
	return string(output[:])
}

type webPermanentExchangeStore struct {
	AuthKeyStore

	mu    sync.Mutex
	id    [8]byte
	saves int
	saved chan struct{}
}

func (s *webPermanentExchangeStore) Save(ctx context.Context, key crypto.AuthKey) error {
	if err := s.AuthKeyStore.Save(ctx, key); err != nil {
		return err
	}
	s.mu.Lock()
	s.id = key.ID
	s.saves++
	s.mu.Unlock()
	select {
	case s.saved <- struct{}{}:
	default:
	}
	return nil
}

func (s *webPermanentExchangeStore) waitForOneStoredKey(ctx context.Context, timeout time.Duration) bool {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		s.mu.Lock()
		saves, id := s.saves, s.id
		s.mu.Unlock()
		if saves > 0 {
			if saves != 1 {
				return false
			}
			key, _, _, _, ok, err := s.Get(ctx, id, 0)
			return err == nil && ok && key.ID == id
		}
		select {
		case <-s.saved:
		case <-deadline.C:
			return false
		case <-ctx.Done():
			return false
		}
	}
}

type webPermanentExchangeLogHandler struct {
	mu    sync.Mutex
	stage string
	cause string
}

func (h *webPermanentExchangeLogHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *webPermanentExchangeLogHandler) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *webPermanentExchangeLogHandler) WithGroup(string) slog.Handler            { return h }

func (h *webPermanentExchangeLogHandler) Handle(_ context.Context, record slog.Record) error {
	var category, stage, cause string
	record.Attrs(func(attr slog.Attr) bool {
		switch attr.Key {
		case "category":
			category = attr.Value.String()
		case "stage":
			stage = attr.Value.String()
		case "cause":
			cause = attr.Value.String()
		}
		return true
	})
	if category != string(serverFailureExchange) || !allowedWebPermanentExchangeServerStage(stage) || !allowedWebPermanentExchangeServerCause(cause) {
		return nil
	}
	h.mu.Lock()
	if h.stage == "" {
		h.stage, h.cause = stage, cause
	}
	h.mu.Unlock()
	return nil
}

func (h *webPermanentExchangeLogHandler) snapshot() (string, string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stage == "" {
		return "unknown", "unknown"
	}
	return h.stage, h.cause
}

func allowedWebPermanentExchangeServerStage(value string) bool {
	switch value {
	case "read_req_pq", "write_res_pq", "read_req_dh", "decode_inner", "write_server_dh", "read_client_dh", "decode_client_dh", "write_dh_gen", "unknown":
		return true
	default:
		return false
	}
}

func allowedWebPermanentExchangeServerCause(value string) bool {
	switch value {
	case "timeout", "peer_close", "unexpected_type", "decode", "write", "other", "unknown":
		return true
	default:
		return false
	}
}

type webPermanentExchangeReader struct {
	mu      sync.Mutex
	seed    []byte
	counter uint64
	block   []byte
	offset  int
}

func newWebPermanentExchangeReader(seed string) *webPermanentExchangeReader {
	return &webPermanentExchangeReader{seed: []byte(seed)}
}

func (r *webPermanentExchangeReader) Read(out []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for written := 0; written < len(out); {
		if r.offset == len(r.block) {
			input := make([]byte, len(r.seed)+8)
			copy(input, r.seed)
			binary.LittleEndian.PutUint64(input[len(r.seed):], r.counter)
			r.counter++
			block := sha256.Sum256(input)
			r.block = block[:]
			r.offset = 0
		}
		copied := copy(out[written:], r.block[r.offset:])
		written += copied
		r.offset += copied
	}
	return len(out), nil
}
