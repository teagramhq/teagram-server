package mtproto

import (
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
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
	"sync/atomic"
	"testing"
	"time"

	"github.com/gotd/td/crypto"
	"github.com/gotd/td/exchange"
	"github.com/gotd/td/transport"
)

const (
	webExchangeOrigin = "https://exchange.synthetic.invalid"
)

type webExchangeClientRow struct {
	Case              string `json:"case"`
	ClientStage       string `json:"client_stage"`
	ClientAbortReason string `json:"client_abort_reason"`
	PlainRequests     int    `json:"plain_requests_on_socket"`
	RetryReusedSocket bool   `json:"retry_reused_socket"`
	ThirdConstructor  string `json:"third_constructor"`
	ProtoError404     bool   `json:"proto_error_404"`
}

type webExchangeReportRow struct {
	Case              string `json:"case"`
	ClientStage       string `json:"client_stage"`
	ClientAbortReason string `json:"client_abort_reason"`
	PlainRequests     int    `json:"plain_requests_on_socket"`
	RetryReusedSocket bool   `json:"retry_reused_socket"`
	ThirdConstructor  string `json:"third_constructor"`
	ServerStage       string `json:"server_stage"`
	ServerCause       string `json:"server_cause"`
	ProtoError404     bool   `json:"proto_error_404"`
	KeySaved          bool   `json:"key_saved"`
}

type webExchangeReport struct {
	Format string                 `json:"format"`
	Cases  []webExchangeReportRow `json:"cases"`
}

type webExchangeConfig struct {
	Origin    string                     `json:"origin"`
	PublicKey webExchangePublicKey       `json:"public_key"`
	Cases     map[string]webExchangeCase `json:"cases"`
}

type webExchangePublicKey struct {
	Modulus     string `json:"modulus"`
	Exponent    string `json:"exponent"`
	Fingerprint string `json:"fingerprint"`
}

type webExchangeCase struct {
	Endpoint string `json:"endpoint"`
}

// TestWebClientExchangeExperiment is enabled only by the dedicated CI runner.
// It joins the shipped Web authorizer to real, isolated Server instances and
// writes only Security's fixed observation fields to the CI artifact.
func TestWebClientExchangeExperiment(t *testing.T) {
	if os.Getenv("WEB_EXCHANGE_CI_ONLY") != "1" {
		t.Skip("synthetic Web exchange experiment runs only in its dedicated CI job")
	}
	webRoot := os.Getenv("WEB_EXCHANGE_WEB_ROOT")
	artifactPath := os.Getenv("WEB_EXCHANGE_SANITIZED_ARTIFACT_PATH")
	if !filepath.IsAbs(webRoot) || !filepath.IsAbs(artifactPath) {
		t.Fatal("synthetic Web exchange runner configuration is unavailable")
	}
	artifactRoot, err := os.OpenRoot(filepath.Dir(artifactPath))
	if err != nil {
		t.Fatal("synthetic Web exchange report directory could not be opened")
	}
	defer func() {
		if err := artifactRoot.Close(); err != nil {
			t.Error("synthetic Web exchange report directory could not be closed")
		}
	}()
	artifactName := filepath.Base(artifactPath)
	if err := writeWebExchangeReport(artifactRoot, artifactName, unknownWebExchangeReport()); err != nil {
		t.Fatal("synthetic Web exchange report could not be initialized")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Second)
	defer cancel()

	rsaKey, err := rsa.GenerateKey(newWebExchangeReader("rsa-key"), crypto.RSAKeyBits)
	if err != nil {
		t.Fatal("synthetic Web exchange identity generation failed")
	}
	privateKey := exchange.PrivateKey{RSA: rsaKey}
	keyInfo := webExchangePublicKey{
		Modulus:     fmt.Sprintf("%x", rsaKey.N),
		Exponent:    fmt.Sprintf("%x", rsaKey.E),
		Fingerprint: formatWebExchangeFingerprint(privateKey.Fingerprint()),
	}
	if len(keyInfo.Modulus) != 512 {
		t.Fatal("synthetic Web exchange identity has an invalid size")
	}
	if _, err := decodeWebExchangeFingerprint(keyInfo.Fingerprint); err != nil {
		t.Fatal("synthetic Web exchange identity has an invalid fingerprint")
	}
	if keyInfo.Exponent == "3" {
		keyInfo.Exponent = "03"
	} else if len(keyInfo.Exponent)%2 != 0 {
		keyInfo.Exponent = "0" + keyInfo.Exponent
	}

	clientZero, clientZeroLog, clientZeroStore := runWebExchangeGotdControl(t, ctx, privateKey)
	if clientZero != 1 || clientZeroLog != "" {
		t.Fatal("gotd control did not complete with one saved key and no exchange stage log")
	}

	servers := make(map[string]*webExchangeServer, 6)
	for _, caseName := range []string{"A", "B", "C", "N1", "N2", "N3"} {
		servers[caseName] = startWebExchangeServer(t, ctx, privateKey, caseName)
	}

	tempRoot := t.TempDir()
	configRoot, err := os.OpenRoot(tempRoot)
	if err != nil {
		t.Fatal("synthetic Web exchange configuration directory could not be opened")
	}
	defer func() {
		if err := configRoot.Close(); err != nil {
			t.Error("synthetic Web exchange configuration directory could not be closed")
		}
	}()
	clientRowsPath := filepath.Join(tempRoot, "client-observations.json")
	config := webExchangeConfig{Origin: webExchangeOrigin, PublicKey: keyInfo, Cases: make(map[string]webExchangeCase, len(servers))}
	for name, server := range servers {
		config.Cases[name] = webExchangeCase{Endpoint: server.endpoint}
	}
	if err := writeWebExchangePrivateFile(configRoot, "config.json", config); err != nil {
		t.Fatal("synthetic Web exchange configuration could not be written")
	}
	clientCtx, stopClient := context.WithTimeout(ctx, 75*time.Second)
	defer stopClient()
	command := exec.CommandContext(clientCtx, "node", "node_modules/vitest/vitest.mjs", "run", "--pool=threads", "--maxWorkers=1", "src/tests/websocketExchangeExperiment.test.ts")
	command.Dir = webRoot
	command.Env = append(os.Environ(),
		"WEB_EXCHANGE_CONFIG_PATH="+filepath.Join(tempRoot, "config.json"),
		"WEB_EXCHANGE_CLIENT_ROWS_PATH="+clientRowsPath,
	)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	runErr := command.Run()

	clientRows, rowsErr := readWebExchangeClientRows(clientRowsPath)
	clientRowsByCase := make(map[string]webExchangeClientRow, len(clientRows))
	for _, row := range clientRows {
		clientRowsByCase[row.Case] = row
	}
	if row := clientRowsByCase["A"]; row.ClientStage == "dh_gen_verified" {
		waitWebExchangeSaves(t, servers["A"].store, 1)
	}
	if row := clientRowsByCase["B"]; row.ClientStage == "dh_gen_verified" {
		waitWebExchangeSaves(t, servers["B"].store, 1)
	}
	if row := clientRowsByCase["C"]; row.ClientStage == "dh_gen_verified" {
		waitWebExchangeSaves(t, servers["C"].store, 2)
	}
	report := combineWebExchangeRows(clientRows, servers, clientZeroStore)
	if err := writeWebExchangeReport(artifactRoot, artifactName, report); err != nil {
		t.Fatal("synthetic Web exchange report could not be written")
	}
	if runErr != nil || rowsErr != nil {
		t.Fatal("shipped Web exchange cases did not produce a complete sanitized observation")
	}
	if err := validateWebExchangeReport(report); err != nil {
		t.Fatal("synthetic Web exchange observations violated the fixed experiment contract")
	}
	if report.Cases[0].Case != "C0" || !report.Cases[0].KeySaved || report.Cases[0].ServerStage != "unknown" || report.Cases[0].ServerCause != "unknown" {
		t.Fatal("gotd control observation did not match its completion contract")
	}
	if caseRow(report, "B").ServerStage != "read_client_dh" || caseRow(report, "B").ServerCause != "unexpected_type" || caseRow(report, "B").ProtoError404 || !caseRow(report, "B").RetryReusedSocket || caseRow(report, "B").ThirdConstructor != "req_pq_multi" {
		t.Fatal("injected shipped-Web retry did not produce the expected retry control tuple")
	}
	if caseRow(report, "N1").ServerStage != "read_client_dh" || caseRow(report, "N1").ServerCause != "unexpected_type" || caseRow(report, "N1").ProtoError404 || caseRow(report, "N1").KeySaved || caseRow(report, "N1").ThirdConstructor != "req_pq_multi" {
		t.Fatal("next req_pq control did not produce the expected server tuple")
	}
	for _, name := range []string{"N2", "N3"} {
		row := caseRow(report, name)
		if row.ProtoError404 != true || row.ServerStage != "unknown" || row.ServerCause != "unknown" || row.KeySaved {
			t.Fatal("malformed client-DH control did not stay in the protocol-error path")
		}
	}
}

type webExchangeServer struct {
	endpoint string
	store    *webExchangeStore
	logs     *webExchangeLogHandler
}

func startWebExchangeServer(t *testing.T, parent context.Context, key exchange.PrivateKey, caseName string) *webExchangeServer {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(parent, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("synthetic WebSocket listener could not start")
	}
	store := &webExchangeStore{AuthKeyStore: NewMemoryAuthKeyStore(), saves: make(chan struct{}, 8)}
	logs := &webExchangeLogHandler{}
	server := New(key, 2, store, nil, slog.New(logs))
	server.cipher = crypto.NewServerCipher(newWebExchangeReader("server-" + caseName))
	server.SetWebSocketOriginPatterns([]string{webExchangeOrigin})
	server.SetHandshakeTimeout(5 * time.Second)
	ctx, stop := context.WithCancel(parent)
	served := make(chan error, 1)
	go func() { served <- server.ServeWebSocket(ctx, listener) }()
	t.Cleanup(func() {
		stop()
		select {
		case err := <-served:
			if err != nil {
				t.Error("synthetic WebSocket server did not stop cleanly")
			}
		case <-time.After(5 * time.Second):
			if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				t.Error("synthetic WebSocket listener cleanup failed")
			}
			select {
			case err := <-served:
				if err != nil && !errors.Is(err, net.ErrClosed) {
					t.Error("synthetic WebSocket server did not stop cleanly")
				}
			case <-time.After(1 * time.Second):
				t.Error("synthetic WebSocket server did not stop")
			}
		}
	})
	return &webExchangeServer{endpoint: "ws://" + listener.Addr().String() + "/apiws", store: store, logs: logs}
}

func runWebExchangeGotdControl(t *testing.T, ctx context.Context, key exchange.PrivateKey) (int32, string, *webExchangeStore) {
	t.Helper()
	store := &webExchangeStore{AuthKeyStore: NewMemoryAuthKeyStore(), saves: make(chan struct{}, 2)}
	logs := &webExchangeLogHandler{}
	server := New(key, 2, store, nil, slog.New(logs))
	server.cipher = crypto.NewServerCipher(newWebExchangeReader("server-c0"))
	client, serverConn := transport.Intermediate.Pipe()
	served := make(chan error, 1)
	go func() { served <- server.ServeConn(ctx, serverConn) }()
	_, err := exchange.NewExchanger(client, 2).
		WithRand(newWebExchangeReader("gotd-c0-client")).
		Client([]exchange.PublicKey{{RSA: &key.RSA.PublicKey}}).
		Run(ctx)
	if err != nil {
		if closeErr := client.Close(); closeErr != nil {
			t.Error("gotd synthetic client cleanup failed")
		}
		t.Fatal("gotd synthetic exchange did not complete")
	}
	waitWebExchangeSaves(t, store, 1)
	if err := client.Close(); err != nil {
		t.Fatal("gotd synthetic client cleanup failed")
	}
	select {
	case err := <-served:
		if err != nil {
			t.Fatal("gotd synthetic server did not stop cleanly")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("gotd synthetic server did not stop")
	}
	stage, cause := logs.snapshot()
	if stage != "unknown" || cause != "unknown" {
		return store.saved.Load(), stage + "/" + cause, store
	}
	return store.saved.Load(), "", store
}

type webExchangeStore struct {
	AuthKeyStore

	saves chan struct{}
	saved atomic.Int32
}

func (s *webExchangeStore) Save(ctx context.Context, key crypto.AuthKey) error {
	if err := s.AuthKeyStore.Save(ctx, key); err != nil {
		return err
	}
	s.saved.Add(1)
	select {
	case s.saves <- struct{}{}:
	default:
	}
	return nil
}

func waitWebExchangeSaves(t *testing.T, store *webExchangeStore, count int32) {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for store.saved.Load() < count {
		select {
		case <-store.saves:
		case <-deadline.C:
			t.Fatal("synthetic exchange did not save the expected auth key")
		}
	}
}

type webExchangeLogHandler struct {
	mu    sync.Mutex
	stage string
	cause string
}

func (h *webExchangeLogHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *webExchangeLogHandler) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *webExchangeLogHandler) WithGroup(string) slog.Handler            { return h }
func (h *webExchangeLogHandler) Handle(_ context.Context, record slog.Record) error {
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
	if category == string(serverFailureExchange) && allowedWebExchangeStage(stage) && allowedWebExchangeCause(cause) {
		h.mu.Lock()
		if h.stage == "" {
			h.stage, h.cause = stage, cause
		}
		h.mu.Unlock()
	}
	return nil
}

func (h *webExchangeLogHandler) snapshot() (string, string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stage == "" {
		return "unknown", "unknown"
	}
	return h.stage, h.cause
}

type webExchangeReader struct {
	mu      sync.Mutex
	seed    []byte
	counter uint64
	block   []byte
	offset  int
}

func newWebExchangeReader(seed string) *webExchangeReader {
	return &webExchangeReader{seed: []byte(seed)}
}

func (r *webExchangeReader) Read(out []byte) (int, error) {
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

func unknownWebExchangeReport() webExchangeReport {
	rows := make([]webExchangeReportRow, 0, 7)
	rows = append(rows, unknownWebExchangeRow("C0"))
	for _, name := range []string{"A", "B", "C", "N1", "N2", "N3"} {
		rows = append(rows, unknownWebExchangeRow(name))
	}
	return webExchangeReport{Format: "teagram-web-authorizer-exchange-v1", Cases: rows}
}

func unknownWebExchangeRow(name string) webExchangeReportRow {
	return webExchangeReportRow{
		Case: name, ClientStage: "unknown", ClientAbortReason: "unknown", ThirdConstructor: "unknown",
		ServerStage: "unknown", ServerCause: "unknown",
	}
}

func readWebExchangeClientRows(path string) ([]webExchangeClientRow, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var rows []webExchangeClientRow
	if err := decoder.Decode(&rows); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("extra report data")
	}
	return rows, nil
}

func combineWebExchangeRows(clientRows []webExchangeClientRow, servers map[string]*webExchangeServer, c0 *webExchangeStore) webExchangeReport {
	rowsByCase := make(map[string]webExchangeClientRow, len(clientRows))
	for _, row := range clientRows {
		rowsByCase[row.Case] = row
	}
	rows := []webExchangeReportRow{unknownWebExchangeRow("C0")}
	rows[0].ClientStage = "dh_gen_verified"
	rows[0].ClientAbortReason = "none"
	rows[0].PlainRequests = 3
	rows[0].ThirdConstructor = "set_client_dh_params"
	rows[0].KeySaved = c0.saved.Load() > 0
	for _, name := range []string{"A", "B", "C", "N1", "N2", "N3"} {
		row, ok := rowsByCase[name]
		if !ok {
			rows = append(rows, unknownWebExchangeRow(name))
			continue
		}
		stage, cause := servers[name].logs.snapshot()
		rows = append(rows, webExchangeReportRow{
			Case: name, ClientStage: row.ClientStage, ClientAbortReason: row.ClientAbortReason,
			PlainRequests: row.PlainRequests, RetryReusedSocket: row.RetryReusedSocket,
			ThirdConstructor: row.ThirdConstructor, ServerStage: stage, ServerCause: cause,
			ProtoError404: row.ProtoError404, KeySaved: servers[name].store.saved.Load() > 0,
		})
	}
	return webExchangeReport{Format: "teagram-web-authorizer-exchange-v1", Cases: rows}
}

func validateWebExchangeReport(report webExchangeReport) error {
	if report.Format != "teagram-web-authorizer-exchange-v1" || len(report.Cases) != 7 {
		return errors.New("invalid report shape")
	}
	want := []string{"C0", "A", "B", "C", "N1", "N2", "N3"}
	for i, row := range report.Cases {
		if row.Case != want[i] || !allowedWebExchangeClientStage(row.ClientStage) || !allowedWebExchangeAbort(row.ClientAbortReason) || !allowedWebExchangeConstructor(row.ThirdConstructor) || !allowedWebExchangeStage(row.ServerStage) || !allowedWebExchangeCause(row.ServerCause) || row.PlainRequests < 0 || row.PlainRequests > 12 {
			return errors.New("invalid report value")
		}
	}
	return nil
}

func writeWebExchangePrivateFile(root *os.Root, name string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return root.WriteFile(name, append(data, '\n'), 0o600)
}

func writeWebExchangeReport(root *os.Root, name string, report webExchangeReport) error {
	if err := validateWebExchangeReport(report); err != nil {
		return err
	}
	data, err := json.Marshal(report)
	if err != nil {
		return err
	}
	return root.WriteFile(name, append(data, '\n'), 0o600)
}

func formatWebExchangeFingerprint(fingerprint int64) string {
	const digits = "0123456789abcdef"
	var output [16]byte
	for position := range output {
		shift := 4 * (len(output) - 1 - position)
		output[position] = digits[(fingerprint>>shift)&0xf]
	}
	return string(output[:])
}

func caseRow(report webExchangeReport, name string) webExchangeReportRow {
	for _, row := range report.Cases {
		if row.Case == name {
			return row
		}
	}
	return unknownWebExchangeRow(name)
}

func allowedWebExchangeClientStage(value string) bool {
	switch value {
	case "req_pq_sent", "res_pq_ok", "req_dh_sent", "server_dh_received", "server_dh_validated", "set_client_dh_sent", "dh_gen_verified", "unknown":
		return true
	default:
		return false
	}
}

func allowedWebExchangeAbort(value string) bool {
	switch value {
	case "server_dh_validation", "response_mismatch", "transport_closed", "timeout", "none", "unknown":
		return true
	default:
		return false
	}
}

func allowedWebExchangeConstructor(value string) bool {
	switch value {
	case "set_client_dh_params", "req_pq_multi", "req_pq", "req_dh_params", "other", "unknown":
		return true
	default:
		return false
	}
}

func allowedWebExchangeStage(value string) bool {
	switch value {
	case "read_req_pq", "write_res_pq", "read_req_dh", "decode_inner", "write_server_dh", "read_client_dh", "decode_client_dh", "write_dh_gen", "unknown":
		return true
	default:
		return false
	}
}

func allowedWebExchangeCause(value string) bool {
	switch value {
	case "timeout", "peer_close", "unexpected_type", "decode", "write", "other", "unknown":
		return true
	default:
		return false
	}
}

func decodeWebExchangeFingerprint(value string) (uint64, error) {
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != 8 {
		return 0, errors.New("invalid synthetic fingerprint")
	}
	var out uint64
	for _, b := range decoded {
		out = out<<8 | uint64(b)
	}
	return out, nil
}
