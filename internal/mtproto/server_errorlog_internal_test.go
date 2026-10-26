package mtproto

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/clock"
	tdcrypto "github.com/gotd/td/crypto"
	"github.com/gotd/td/exchange"
	"github.com/gotd/td/mt"
	"github.com/gotd/td/proto"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/gotd/td/transport"
)

type serverErrorLogRecord struct {
	message string
	level   slog.Level
	attrs   map[string]any
}

type serverErrorLogSink struct {
	records []serverErrorLogRecord
}

type serverErrorLogEncoder struct{ err error }

func (e serverErrorLogEncoder) Encode(*bin.Buffer) error { return e.err }

func (*serverErrorLogSink) Enabled(context.Context, slog.Level) bool { return true }

func (s *serverErrorLogSink) Handle(_ context.Context, record slog.Record) error {
	attrs := make(map[string]any)
	record.Attrs(func(attr slog.Attr) bool {
		attrs[attr.Key] = attr.Value.Any()
		return true
	})
	s.records = append(s.records, serverErrorLogRecord{message: record.Message, level: record.Level, attrs: attrs})
	return nil
}

func (s *serverErrorLogSink) WithAttrs([]slog.Attr) slog.Handler { return s }
func (s *serverErrorLogSink) WithGroup(string) slog.Handler      { return s }

func TestExchangeFailureDiagnostics(t *testing.T) {
	const rawDetail = "ATTACKER-CONTROLLED-EXCHANGE-SENTINEL"
	for _, test := range []struct {
		name       string
		sendErrAt  int
		repeatReqP bool
	}{
		{name: "first resPQ send", sendErrAt: 1},
		{name: "repeated req_pq resPQ send", sendErrAt: 2, repeatReqP: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				t.Fatalf("generate RSA key: %v", err)
			}
			sink := &serverErrorLogSink{}
			server := New(exchange.PrivateKey{RSA: rsaKey}, 2, NewMemoryAuthKeyStore(), nil, slog.New(sink))
			reqPQ := exchangeRequestFrame(t)
			conn := &exchangeScriptConn{
				sendErrAt: test.sendErrAt,
				sendErr:   errors.New(rawDetail),
			}
			if test.repeatReqP {
				conn.recv = append(conn.recv, reqPQ)
			}

			err = server.runExchange(context.Background(), conn, &bin.Buffer{Buf: reqPQ}, netip.Addr{})
			if err == nil {
				t.Fatal("runExchange returned nil after failed resPQ send")
			}
			if isDisconnect(err) {
				t.Fatalf("synthetic send failure %v was filtered as a disconnect", err)
			}
			server.logConnectionFailure(err)
			server.logConnectionFailure(err)
			server.exchangeErrorLog.last.Store(time.Now().Add(-preAuthLogInterval).UnixNano())
			server.logConnectionFailure(err)

			var exchangeRecords []serverErrorLogRecord
			for _, record := range sink.records {
				if record.message == "server operation failed" {
					exchangeRecords = append(exchangeRecords, record)
				}
			}
			if len(exchangeRecords) != 2 {
				t.Fatalf("sampled exchange records = %d, want 2", len(exchangeRecords))
			}
			for _, record := range exchangeRecords {
				if record.message != "server operation failed" || record.level != slog.LevelWarn {
					t.Errorf("exchange record metadata = (%q, %s), want WARN server operation failed", record.message, record.level)
				}
				if got, want := record.attrs["category"], "exchange"; got != want {
					t.Errorf("category = %v, want %q", got, want)
				}
				if got, want := record.attrs["stage"], "write_res_pq"; got != want {
					t.Errorf("stage = %v, want %q", got, want)
				}
				if got, want := record.attrs["cause"], "write"; got != want {
					t.Errorf("cause = %v, want %q", got, want)
				}
				if strings.Contains(record.message+fmt.Sprint(record.attrs), rawDetail) {
					t.Errorf("exchange record exposed raw error details: %+v", record)
				}
				for key := range record.attrs {
					if key != "category" && key != "stage" && key != "cause" && key != "suppressed" {
						t.Errorf("exchange record contains unexpected attribute %q: %+v", key, record.attrs)
					}
				}
			}
			if got := exchangeRecords[0].attrs["suppressed"]; got != int64(0) {
				t.Errorf("first suppressed count = %v, want 0", got)
			}
			if got := exchangeRecords[1].attrs["suppressed"]; got != int64(1) {
				t.Errorf("second suppressed count = %v, want 1", got)
			}
		})
	}

	t.Run("lookup miss write failure keeps the exchange phase", func(t *testing.T) {
		rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("generate RSA key: %v", err)
		}
		sink := &serverErrorLogSink{}
		server := New(exchange.PrivateKey{RSA: rsaKey}, 2, NewMemoryAuthKeyStore(), nil, slog.New(sink))
		conn := &exchangeScriptConn{
			recv:      [][]byte{{1, 0, 0, 0, 0, 0, 0, 0}},
			sendErrAt: 2, // resPQ succeeds; the lookup-miss -404 fails.
			sendErr:   errors.New("lookup miss response write failed"),
		}
		err = server.runExchange(context.Background(), conn, &bin.Buffer{Buf: exchangeRequestFrame(t)}, netip.Addr{})
		if err == nil {
			t.Fatal("runExchange returned nil after failed lookup-miss response")
		}
		server.logConnectionFailure(err)
		for _, record := range sink.records {
			if record.message != "server operation failed" {
				continue
			}
			if record.attrs["stage"] != "read_req_dh" || record.attrs["cause"] != "write" {
				t.Fatalf("lookup-miss write diagnostics = %v, want read_req_dh/write", record.attrs)
			}
			return
		}
		t.Fatalf("no exchange failure record in %+v", sink.records)
	})

	t.Run("repeat requests and lookup misses preserve phase", func(t *testing.T) {
		const sendFailure = "SERVER-DH-SEND-SENTINEL"
		reqPQ := exchangeConstructorFrame(t, mt.ReqPqMultiRequestTypeID, proto.MessageFromClient)
		reqDH := exchangeConstructorFrame(t, mt.ReqDHParamsRequestTypeID, proto.MessageFromClient)
		unknownKeyFrame := []byte{1, 0, 0, 0, 0, 0, 0, 0}
		conn := &exchangeScriptConn{
			recv:      [][]byte{reqPQ, reqPQ, unknownKeyFrame, reqDH},
			sendErrAt: 4, // Two resPQ replies, the exchange -404, then server DH.
			sendErr:   errors.New(sendFailure),
		}
		diagnostics := newExchangeDiagnostics()
		observed := exchangeDiagnosticConn{Conn: conn, diagnostics: diagnostics}
		wrapped := exchangeConn{Conn: observed, keys: NewMemoryAuthKeyStore()}
		var frame bin.Buffer
		for _, wantStage := range []exchangeStage{exchangeStageReadReqPQ, exchangeStageReadReqDH} {
			if err := wrapped.Recv(context.Background(), &frame); err != nil {
				t.Fatalf("receive req_pq: %v", err)
			}
			if diagnostics.stage != wantStage {
				t.Fatalf("stage after req_pq = %q, want %q", diagnostics.stage, wantStage)
			}
			if err := observed.Send(context.Background(), &bin.Buffer{Buf: exchangeConstructorFrame(t, mt.ResPQTypeID, proto.MessageFromServer)}); err != nil {
				t.Fatalf("send resPQ: %v", err)
			}
		}
		if err := wrapped.Recv(context.Background(), &frame); err != nil {
			t.Fatalf("receive req_DH_params after lookup miss: %v", err)
		}
		if diagnostics.stage != exchangeStageDecodeInner {
			t.Fatalf("stage after identifiable req_DH_params = %q, want %q", diagnostics.stage, exchangeStageDecodeInner)
		}
		if got := len(conn.sent); got != 3 {
			t.Fatalf("sends before server-DH attempt = %d, want two resPQ replies and one -404", got)
		}
		if err := observed.Send(context.Background(), &bin.Buffer{Buf: exchangeConstructorFrame(t, mt.ServerDHParamsOkTypeID, proto.MessageFromServer)}); err == nil {
			t.Fatal("server-DH send succeeded, want synthetic failure")
		}
		if diagnostics.stage != exchangeStageWriteServerDH {
			t.Fatalf("stage after failed server-DH send = %q, want %q", diagnostics.stage, exchangeStageWriteServerDH)
		}
	})

	t.Run("later exchange phases", func(t *testing.T) {
		conn := &exchangeScriptConn{recv: [][]byte{
			exchangeConstructorFrame(t, mt.ReqPqMultiRequestTypeID, proto.MessageFromClient),
			exchangeConstructorFrame(t, mt.ReqDHParamsRequestTypeID, proto.MessageFromClient),
			exchangeConstructorFrame(t, mt.SetClientDHParamsRequestTypeID, proto.MessageFromClient),
		}}
		diagnostics := newExchangeDiagnostics()
		observed := exchangeDiagnosticConn{Conn: conn, diagnostics: diagnostics}
		var frame bin.Buffer
		if err := observed.Recv(context.Background(), &frame); err != nil {
			t.Fatalf("receive req_pq: %v", err)
		}
		if err := observed.Send(context.Background(), &bin.Buffer{Buf: exchangeConstructorFrame(t, mt.ResPQTypeID, proto.MessageFromServer)}); err != nil {
			t.Fatalf("send resPQ: %v", err)
		}
		if err := observed.Recv(context.Background(), &frame); err != nil {
			t.Fatalf("receive req_DH_params: %v", err)
		}
		if diagnostics.stage != exchangeStageDecodeInner {
			t.Fatalf("stage after req_DH_params = %q, want %q", diagnostics.stage, exchangeStageDecodeInner)
		}
		if err := observed.Send(context.Background(), &bin.Buffer{Buf: exchangeConstructorFrame(t, mt.ServerDHParamsOkTypeID, proto.MessageFromServer)}); err != nil {
			t.Fatalf("send server_DH_params_ok: %v", err)
		}
		if diagnostics.stage != exchangeStageReadClientDH {
			t.Fatalf("stage after successful server-DH send = %q, want %q", diagnostics.stage, exchangeStageReadClientDH)
		}
		if err := observed.Recv(context.Background(), &frame); err != nil {
			t.Fatalf("receive set_client_DH_params: %v", err)
		}
		if diagnostics.stage != exchangeStageDecodeClientDH {
			t.Fatalf("stage after set_client_DH_params = %q, want %q", diagnostics.stage, exchangeStageDecodeClientDH)
		}
		conn.sendErrAt = len(conn.sent) + 1
		conn.sendErr = errors.New("dh_gen send failed")
		if err := observed.Send(context.Background(), &bin.Buffer{Buf: exchangeConstructorFrame(t, mt.DhGenOkTypeID, proto.MessageFromServer)}); err == nil {
			t.Fatal("dh_gen send succeeded, want synthetic failure")
		}
		if diagnostics.stage != exchangeStageWriteDHGen {
			t.Fatalf("stage after failed dh_gen send = %q, want %q", diagnostics.stage, exchangeStageWriteDHGen)
		}
	})

	t.Run("malformed observations retain established phase", func(t *testing.T) {
		malformed := malformedExchangeEnvelope(t, mt.ReqDHParamsRequestTypeID)
		outOfOrder := exchangeConstructorFrame(t, mt.SetClientDHParamsRequestTypeID, proto.MessageFromClient)
		conn := &exchangeScriptConn{recv: [][]byte{outOfOrder}}
		diagnostics := newExchangeDiagnostics()
		observed := exchangeDiagnosticConn{Conn: conn, diagnostics: diagnostics}
		var frame bin.Buffer
		if err := observed.Recv(context.Background(), &frame); err != nil {
			t.Fatalf("receive out-of-order set_client_DH_params: %v", err)
		}
		if diagnostics.stage != exchangeStageReadReqPQ {
			t.Fatalf("out-of-order observation advanced stage to %q", diagnostics.stage)
		}
		if err := observed.Send(context.Background(), &bin.Buffer{Buf: exchangeConstructorFrame(t, mt.ResPQTypeID, proto.MessageFromServer)}); err != nil {
			t.Fatalf("send resPQ: %v", err)
		}
		conn.recv = append(conn.recv, malformed)
		if err := observed.Recv(context.Background(), &frame); err != nil {
			t.Fatalf("receive malformed req_DH_params: %v", err)
		}
		if diagnostics.stage != exchangeStageReadReqDH {
			t.Fatalf("malformed observation advanced stage to %q, want %q", diagnostics.stage, exchangeStageReadReqDH)
		}
	})

	t.Run("initial response preparation failure", func(t *testing.T) {
		const rawDetail = "RANDOM-SOURCE-SENTINEL"
		rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("generate RSA key: %v", err)
		}
		sink := &serverErrorLogSink{}
		server := New(exchange.PrivateKey{RSA: rsaKey}, 2, NewMemoryAuthKeyStore(), nil, slog.New(sink))
		server.cipher = tdcrypto.NewServerCipher(exchangeFailureReader{err: errors.New(rawDetail)})
		conn := &exchangeScriptConn{}
		err = server.runExchange(context.Background(), conn, &bin.Buffer{Buf: exchangeRequestFrame(t)}, netip.Addr{})
		if err == nil {
			t.Fatal("runExchange returned nil after random source failure")
		}
		server.logConnectionFailure(err)
		var record *serverErrorLogRecord
		for i := range sink.records {
			if sink.records[i].message == "server operation failed" {
				record = &sink.records[i]
			}
		}
		if record == nil {
			t.Fatalf("no exchange failure record in %+v", sink.records)
		}
		if record.attrs["stage"] != "read_req_pq" || record.attrs["cause"] != "other" {
			t.Fatalf("initial preparation diagnostics = %v, want read_req_pq/other", record.attrs)
		}
		if strings.Contains(record.message+fmt.Sprint(record.attrs), rawDetail) {
			t.Fatalf("initial preparation log exposed raw error: %+v", record)
		}
	})

	t.Run("cause classification and disconnect filtering", func(t *testing.T) {
		for _, test := range []struct {
			name      string
			err       error
			operation exchangeOperation
			want      exchangeFailureCause
		}{
			{name: "timeout wins over write", err: context.DeadlineExceeded, operation: exchangeOperationWrite, want: exchangeCauseTimeout},
			{name: "peer close wins over write", err: io.ErrClosedPipe, operation: exchangeOperationWrite, want: exchangeCausePeerClose},
			{name: "unexpected type", err: &bin.UnexpectedIDErr{ID: 123}, operation: exchangeOperationRead, want: exchangeCauseUnexpected},
			{name: "typed decode", err: &bin.InvalidLengthError{Length: -1, Where: "bytes"}, operation: exchangeOperationRead, want: exchangeCauseDecode},
			{name: "processing failure is not decode", err: errors.New("untyped processing failure"), operation: exchangeOperationRead, want: exchangeCauseOther},
			{name: "write context", err: errors.New("untyped send failure"), operation: exchangeOperationWrite, want: exchangeCauseWrite},
			{name: "unknown operation", err: errors.New("untyped failure"), want: exchangeCauseOther},
		} {
			t.Run(test.name, func(t *testing.T) {
				if got := classifyExchangeFailure(test.err, test.operation); got != test.want {
					t.Fatalf("cause = %q, want %q", got, test.want)
				}
			})
		}

		sink := &serverErrorLogSink{}
		server := New(exchange.PrivateKey{}, 2, NewMemoryAuthKeyStore(), nil, slog.New(sink))
		for _, err := range []error{
			io.EOF,
			net.ErrClosed,
			&net.OpError{Op: "read", Net: "tcp", Err: context.DeadlineExceeded},
			&net.OpError{Op: "write", Net: "tcp", Err: context.DeadlineExceeded},
		} {
			wrapped := &exchangeFailureError{
				err:   errors.Join(errAuthKeyExchangeFailure, err),
				stage: exchangeStageWriteResPQ,
				cause: classifyExchangeFailure(err, exchangeOperationWrite),
			}
			if !isDisconnect(wrapped) {
				t.Errorf("wrapped disconnect %T was not filtered", err)
				server.logConnectionFailure(wrapped)
			}
		}
		for _, record := range sink.records {
			if record.message == "server operation failed" {
				t.Errorf("disconnect produced an exchange WARN record: %+v", record)
			}
		}
	})
}

func exchangeRequestFrame(t *testing.T) []byte {
	t.Helper()
	var body bin.Buffer
	request := mt.ReqPqMultiRequest{Nonce: bin.Int128{1}}
	if err := request.Encode(&body); err != nil {
		t.Fatalf("encode req_pq_multi: %v", err)
	}
	return exchangeMessageFrame(t, body.Copy(), proto.MessageFromClient)
}

func exchangeConstructorFrame(t *testing.T, id uint32, direction proto.MessageType) []byte {
	t.Helper()
	var body bin.Buffer
	body.PutID(id)
	return exchangeMessageFrame(t, body.Copy(), direction)
}

func exchangeMessageFrame(t *testing.T, body []byte, direction proto.MessageType) []byte {
	t.Helper()
	message := proto.UnencryptedMessage{MessageID: int64(proto.NewMessageID(time.Now(), direction)), MessageData: body}
	var frame bin.Buffer
	if err := message.Encode(&frame); err != nil {
		t.Fatalf("encode unencrypted exchange message: %v", err)
	}
	return frame.Copy()
}

func malformedExchangeEnvelope(t *testing.T, id uint32) []byte {
	t.Helper()
	var frame bin.Buffer
	frame.PutLong(0)
	frame.PutLong(int64(proto.NewMessageID(time.Now(), proto.MessageFromClient)))
	frame.PutInt32(8) // Advertise bytes that are not present.
	frame.PutID(id)
	return frame.Copy()
}

type exchangeFailureReader struct{ err error }

func (r exchangeFailureReader) Read([]byte) (int, error) { return 0, r.err }

func TestConnectionFailureLogsUseSampledSafeCategories(t *testing.T) {
	sink := &serverErrorLogSink{}
	server := New(exchange.PrivateKey{}, 2, NewMemoryAuthKeyStore(), nil, slog.New(sink))
	const rawDetail = "UNTRUSTED-ERROR-DETAIL"
	tests := []struct {
		name     string
		err      error
		category string
	}{
		{name: "key persistence", err: errors.Join(errAuthKeyPersistenceFailure, errors.New(rawDetail)), category: "key_persistence"},
		{name: "exchange", err: errors.Join(errAuthKeyExchangeFailure, errors.New(rawDetail)), category: "exchange"},
		{name: "request handling", err: errors.Join(errRequestHandlingFailure, errors.New(rawDetail)), category: "request_handling"},
	}

	for _, test := range tests {
		server.logConnectionFailure(test.err)
	}
	server.logConnectionFailure(tests[0].err)
	if len(sink.records) != len(tests) {
		t.Fatalf("records after sampled repeats = %d, want %d", len(sink.records), len(tests))
	}

	for i, test := range tests {
		record := sink.records[i]
		if record.attrs["category"] != test.category {
			t.Errorf("%s category = %v, want %q", test.name, record.attrs["category"], test.category)
		}
		if record.attrs["suppressed"] != int64(0) {
			t.Errorf("first %s suppressed count = %v, want 0", test.name, record.attrs["suppressed"])
		}
		if test.category == string(serverFailureRequest) {
			for key, want := range map[string]any{"rpc_method": UnknownRPCMethod, "error": "internal"} {
				if got := record.attrs[key]; got != want {
					t.Errorf("request %s = %v, want %v", key, got, want)
				}
			}
			if _, ok := record.attrs["rpc_error_code"]; ok {
				t.Errorf("request log included an RPC code without an RPC error: %+v", record.attrs)
			}
		}
		if strings.Contains(record.message+fmt.Sprint(record.attrs), rawDetail) {
			t.Errorf("%s log exposed raw error details: %+v", test.name, record)
		}
	}

	server.keyPersistenceErrorLog.last.Store(time.Now().Add(-preAuthLogInterval).UnixNano())
	server.logConnectionFailure(tests[0].err)
	if len(sink.records) != len(tests)+1 {
		t.Fatalf("records after persistence sample window = %d, want %d", len(sink.records), len(tests)+1)
	}
	if got := sink.records[len(sink.records)-1].attrs["suppressed"]; got != int64(1) {
		t.Fatalf("key persistence suppressed count = %v, want 1", got)
	}
}

func TestTransportCloseLogReportsLastKeyExchangeStepWithoutSensitiveFields(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	sink := &serverErrorLogSink{}
	server := New(exchange.PrivateKey{RSA: rsaKey}, 2, NewMemoryAuthKeyStore(), nil, slog.New(sink))
	conn := &exchangeScriptConn{recv: [][]byte{exchangeRequestFrame(t)}}

	if err := server.serveConn(context.Background(), conn, netip.Addr{}, nil); err == nil {
		t.Fatal("serveConn returned nil after the client closed during key exchange")
	}

	var closeRecords []serverErrorLogRecord
	for _, record := range sink.records {
		if record.message == "transport connection closed" {
			closeRecords = append(closeRecords, record)
		}
	}
	if len(closeRecords) != 1 {
		t.Fatalf("transport close records = %d, want one; all records: %+v", len(closeRecords), sink.records)
	}
	record := closeRecords[0]
	if record.attrs["transport"] != "TCP" || record.attrs["step"] != "resPQ sent" || record.attrs["reason"] != "client close" {
		t.Fatalf("transport close record = %+v, want TCP/resPQ sent/client close", record)
	}
	if got := record.attrs["suppressed"]; got != int64(0) {
		t.Errorf("suppressed close records = %v, want 0", got)
	}
	for key := range record.attrs {
		if key != "transport" && key != "step" && key != "reason" && key != "suppressed" {
			t.Errorf("transport close record contains unexpected attribute %q: %+v", key, record.attrs)
		}
	}
}

type transportCloseDrainTestConn struct {
	frame      []byte
	reads      int
	secondRead chan struct{}
	closed     chan struct{}
	closeOnce  sync.Once
}

func (c *transportCloseDrainTestConn) Recv(ctx context.Context, b *bin.Buffer) error {
	c.reads++
	if c.reads == 1 {
		b.ResetTo(c.frame)
		return nil
	}
	if c.reads == 2 {
		close(c.secondRead)
	}
	select {
	case <-c.closed:
		return net.ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (*transportCloseDrainTestConn) Send(context.Context, *bin.Buffer) error { return nil }

func (c *transportCloseDrainTestConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

func TestServeConnDrainLogsAuthorizedServerCloseAtDebug(t *testing.T) {
	sink := &serverErrorLogSink{}
	key := shutdownTestAuthKey()
	keys := &shutdownLookupAuthKeyStore{
		key:     key,
		blocked: make(chan struct{}),
		release: make(chan struct{}),
	}
	server := New(exchange.PrivateKey{}, 2, keys, nil, slog.New(sink))
	server.shutdown.drainTimeout = 2 * time.Second
	server.shutdown.retirementWindow = 0
	server.shutdown.retirementSlots = 1
	server.shutdown.startServing()
	conn := &transportCloseDrainTestConn{
		frame:      shutdownClientFrame(t, key, 42, 1<<32, &mt.PingRequest{PingID: 1}),
		secondRead: make(chan struct{}),
		closed:     make(chan struct{}),
	}
	done := make(chan error, 1)
	go func() {
		done <- server.serveConnMode(
			server.shutdown.requestCtx,
			server.shutdown.requestCtx,
			conn,
			netip.Addr{},
			nil,
			nil,
			false,
			transportTCP,
			server.handler,
		)
	}()
	finished := false
	t.Cleanup(func() {
		server.shutdown.beginDrain()
		if !finished {
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("serve connection during cleanup: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Error("serve connection did not stop during cleanup")
			}
		}
		stopShutdownTimers(server.shutdown)
		server.shutdown.finishServing()
	})
	select {
	case <-conn.secondRead:
	case <-time.After(3 * time.Second):
		t.Fatal("authorized connection did not start its idle read")
	}
	server.shutdown.beginDrain()
	select {
	case err := <-done:
		finished = true
		if err != nil {
			t.Fatalf("serve connection during drain: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("serve connection did not finish draining")
	}

	var closeRecords []serverErrorLogRecord
	for _, record := range sink.records {
		if record.message == "transport connection closed" {
			closeRecords = append(closeRecords, record)
		}
	}
	if len(closeRecords) != 1 {
		t.Fatalf("transport close records = %d, want one; all records: %+v", len(closeRecords), sink.records)
	}
	if got := closeRecords[0].attrs["reason"]; got != "server close" {
		t.Errorf("drain close reason = %v, want server close", got)
	}
	if got := closeRecords[0].attrs["step"]; got != "authorized" {
		t.Errorf("drain close step = %v, want authorized", got)
	}
	if got := closeRecords[0].level; got != slog.LevelDebug {
		t.Errorf("drain close level = %v, want DEBUG", got)
	}
}

func TestServeConnThrottlesAuthorizedDecryptFailures(t *testing.T) {
	sink := &serverErrorLogSink{}
	key := shutdownTestAuthKey()
	keys := &authorizationRebindKeyStore{key: key, users: []int64{7}, owner: 7}
	server := New(exchange.PrivateKey{}, 2, keys, nil, slog.New(sink))
	const connections = 8

	closeWithUndecryptableFrame := func(msgID int64) error {
		validFrame := shutdownClientFrame(t, key, 42, msgID, &mt.PingRequest{PingID: msgID})
		invalidFrame := shutdownClientFrame(t, key, 42, msgID+1, &mt.PingRequest{PingID: msgID + 1})
		invalidFrame[8] ^= 0xff
		return server.serveConn(context.Background(), &exchangeScriptConn{recv: [][]byte{validFrame, invalidFrame}}, netip.Addr{}, nil)
	}

	for i := range connections {
		msgID := int64(2*i+1) << 32
		err := closeWithUndecryptableFrame(msgID)
		if err == nil || !strings.Contains(err.Error(), "decrypt message") {
			t.Fatalf("authorized connection error = %v, want an undecryptable-frame error", err)
		}
	}

	var closeRecords []serverErrorLogRecord
	for _, record := range sink.records {
		if record.message == "transport connection closed" {
			closeRecords = append(closeRecords, record)
		}
	}
	if len(closeRecords) != 1 {
		t.Fatalf("transport close records = %d, want one after %d authorized decrypt failures: %+v", len(closeRecords), connections, sink.records)
	}
	if got := closeRecords[0].level; got != slog.LevelInfo {
		t.Errorf("authorized error close level = %v, want INFO", got)
	}
	if got := closeRecords[0].attrs["step"]; got != "authorized" {
		t.Errorf("authorized error close step = %v, want authorized", got)
	}
	if got := closeRecords[0].attrs["reason"]; got != "server error" {
		t.Errorf("authorized error close reason = %v, want server error", got)
	}
	if got := closeRecords[0].attrs["suppressed"]; got != int64(0) {
		t.Errorf("first suppressed close count = %v, want 0", got)
	}

	server.authorizedCloseLog.last.Store(time.Now().Add(-preAuthLogInterval).UnixNano())
	if err := closeWithUndecryptableFrame(int64(2*connections+1) << 32); err == nil {
		t.Fatal("authorized connection returned nil after an undecryptable frame")
	}
	closeRecords = closeRecords[:0]
	for _, record := range sink.records {
		if record.message == "transport connection closed" {
			closeRecords = append(closeRecords, record)
		}
	}
	if len(closeRecords) != 2 {
		t.Fatalf("transport close records after sample interval = %d, want two: %+v", len(closeRecords), sink.records)
	}
	if got := closeRecords[1].attrs["suppressed"]; got != int64(connections-1) {
		t.Errorf("authorized close suppressed count = %v, want %d", got, connections-1)
	}
}

func TestTransportCloseReasonClassifiesClientCloseTimeoutAndServerError(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want transportCloseReason
	}{
		{name: "peer EOF", err: io.EOF, want: transportCloseReasonClientClose},
		{name: "truncated peer frame", err: io.ErrUnexpectedEOF, want: transportCloseReasonClientClose},
		{name: "server-initiated close", err: nil, want: transportCloseReasonServerClose},
		{name: "timeout", err: context.DeadlineExceeded, want: transportCloseReasonTimeout},
		{name: "server failure", err: errors.New("write failed"), want: transportCloseReasonServerError},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyTransportCloseReason(test.err); got != test.want {
				t.Fatalf("close reason = %q, want %q", got, test.want)
			}
		})
	}
}

func TestTransportCloseDiagnosticsTracksLatestHandshakeProgress(t *testing.T) {
	diagnostics := newTransportCloseDiagnostics("/apiws")
	diagnostics.observeRequest(mt.ReqPqMultiRequestTypeID)
	diagnostics.observeResponse(mt.ResPQTypeID)
	diagnostics.observeRequest(mt.ReqDHParamsRequestTypeID)
	diagnostics.markDHComplete()
	diagnostics.markAuthorized()
	diagnostics.observeRequest(mt.ReqPqMultiRequestTypeID)

	if diagnostics.transport != "/apiws" || diagnostics.step != transportCloseStepReqPQReceived {
		t.Fatalf("diagnostics after a new exchange = %+v, want /apiws/req_pq received", diagnostics)
	}
}

func TestConnectionFailureLogsAuthKeyLookupWithSampledCategory(t *testing.T) {
	sink := &serverErrorLogSink{}
	server := New(exchange.PrivateKey{}, 2, NewMemoryAuthKeyStore(), nil, slog.New(sink))
	const rawDetail = "UNTRUSTED-LOOKUP-ERROR"
	err := errors.Join(errAuthKeyExchangeFailure, errAuthKeyLookupFailure, errors.New(rawDetail))

	server.logConnectionFailure(err)
	server.logConnectionFailure(err)
	if len(sink.records) != 1 {
		t.Fatalf("records after sampled lookup repeats = %d, want 1", len(sink.records))
	}
	if record := sink.records[0]; record.message != "auth key lookup failed" || record.attrs["category"] != "auth_key_lookup" {
		t.Fatalf("lookup failure record = %+v, want fixed auth_key_lookup category", record)
	}
	if strings.Contains(sink.records[0].message+fmt.Sprint(sink.records[0].attrs), rawDetail) {
		t.Fatalf("lookup log exposed raw error details: %+v", sink.records[0])
	}

	server.authKeyLookupErrorLog.last.Store(time.Now().Add(-preAuthLogInterval).UnixNano())
	server.logConnectionFailure(err)
	if len(sink.records) != 2 {
		t.Fatalf("records after lookup sample window = %d, want 2", len(sink.records))
	}
	if got := sink.records[1].attrs["suppressed"]; got != int64(1) {
		t.Fatalf("lookup suppressed count = %v, want 1", got)
	}
}

func TestRequestHandlingFailureLogsSafeRPCContext(t *testing.T) {
	const (
		requestID        = int64(0x123456789)
		privateUserID    = int64(991337)
		privateSessionID = int64(0x112233445566)
		phoneNumber      = "+15551239999"
		payload          = "PRIVATE-SIGN-IN-PAYLOAD +15551239999"
		cause            = "PRIVATE-DATABASE-DETAIL"
	)
	authKeyID := [8]byte{0xde, 0xad, 0xbe, 0xef, 0xfe, 0xed, 0xfa, 0xce}
	sink := &serverErrorLogSink{}
	handler := NewDispatcher().HandleFuncNamed(tg.AuthSignInRequestTypeID, "auth.signIn", func(*Conn, *Request) error {
		return errors.New(cause)
	})
	server := New(exchange.PrivateKey{}, 2, NewMemoryAuthKeyStore(), handler, slog.New(sink))
	var body bin.Buffer
	body.PutID(tg.AuthSignInRequestTypeID)
	body.PutString(payload)
	req := &Request{
		AuthKeyID: authKeyID,
		UserID:    privateUserID,
		SessionID: privateSessionID,
		MsgID:     requestID,
		Buf:       &body,
		Ctx:       context.Background(),
	}

	err := server.handle(nil, req)
	if err == nil {
		t.Fatal("handle succeeded after auth.signIn failure")
	}
	server.logConnectionFailure(errors.Join(errRequestHandlingFailure, err))

	if len(sink.records) != 1 {
		t.Fatalf("failure records = %d, want 1", len(sink.records))
	}
	record := sink.records[0]
	if record.message != "server operation failed" || record.level != slog.LevelWarn {
		t.Fatalf("failure record = %+v, want WARN server operation failed", record)
	}
	for key, want := range map[string]any{
		"category":       "request_handling",
		"rpc_method":     "auth.signIn",
		"request_id":     requestID,
		"error_category": string(RPCResultInternal),
		"error":          "internal",
	} {
		if got := record.attrs[key]; got != want {
			t.Errorf("%s = %v, want %v", key, got, want)
		}
	}
	logged := record.message + fmt.Sprint(record.attrs)
	for _, forbidden := range []string{
		payload,
		phoneNumber,
		cause,
		strconv.FormatInt(privateUserID, 10),
		strconv.FormatInt(privateSessionID, 10),
		hex.EncodeToString(authKeyID[:]),
		fmt.Sprint(authKeyID),
	} {
		if strings.Contains(logged, forbidden) {
			t.Errorf("failure record exposed private request data %q: %s", forbidden, logged)
		}
	}
}

func TestUnknownMethodDecodeFailureLogsDecodeCause(t *testing.T) {
	sink := &serverErrorLogSink{}
	server := New(exchange.PrivateKey{}, 2, NewMemoryAuthKeyStore(), nil, slog.New(sink))
	err := server.handle(nil, &Request{
		Buf: &bin.Buffer{Buf: []byte{1}},
		Ctx: context.Background(),
	})
	if err == nil {
		t.Fatal("handle succeeded with a truncated request constructor")
	}
	server.logConnectionFailure(errors.Join(errRequestHandlingFailure, err))

	if len(sink.records) != 1 {
		t.Fatalf("failure records = %d, want 1", len(sink.records))
	}
	for key, want := range map[string]any{
		"rpc_method": UnknownRPCMethod,
		"cause":      "request_decode",
	} {
		if got := sink.records[0].attrs[key]; got != want {
			t.Errorf("%s = %v, want %v", key, got, want)
		}
	}
}

func TestUnregisteredMethodFailureLogsUnknownMethodCause(t *testing.T) {
	sink := &serverErrorLogSink{}
	server := New(exchange.PrivateKey{}, 2, NewMemoryAuthKeyStore(), NewDispatcher(), slog.New(sink))
	var body bin.Buffer
	body.PutID(0xffffffff)
	err := server.handle(nil, &Request{Buf: &body, Ctx: context.Background()})
	if err == nil {
		t.Fatal("handle succeeded with an unregistered request constructor")
	}
	server.logConnectionFailure(errors.Join(errRequestHandlingFailure, err))

	if len(sink.records) != 1 {
		t.Fatalf("failure records = %d, want 1", len(sink.records))
	}
	for key, want := range map[string]any{
		"rpc_method": UnknownRPCMethod,
		"cause":      "unknown_method",
	} {
		if got := sink.records[0].attrs[key]; got != want {
			t.Errorf("%s = %v, want %v", key, got, want)
		}
	}
}

func TestPlainHandlerFailureLogNamesRPCAndCause(t *testing.T) {
	const cause = "PRIVATE-HANDLER-DETAIL"
	sink := &serverErrorLogSink{}
	handler := NewDispatcher().HandleFuncNamed(tg.HelpGetAppConfigRequestTypeID, "help.getAppConfig", func(*Conn, *Request) error {
		return errors.New(cause)
	})
	server := New(exchange.PrivateKey{}, 2, NewMemoryAuthKeyStore(), handler, slog.New(sink))
	var body bin.Buffer
	body.PutID(tg.HelpGetAppConfigRequestTypeID)
	err := server.handle(nil, &Request{MsgID: 17, Buf: &body, Ctx: context.Background()})
	if err == nil {
		t.Fatal("handle succeeded after help.getAppConfig returned a plain error")
	}
	server.logConnectionFailure(errors.Join(errRequestHandlingFailure, err))

	if len(sink.records) != 1 {
		t.Fatalf("failure records = %d, want 1", len(sink.records))
	}
	record := sink.records[0]
	for key, want := range map[string]any{
		"rpc_method": "help.getAppConfig",
		"cause":      "handler_error",
	} {
		if got := record.attrs[key]; got != want {
			t.Errorf("%s = %v, want %v", key, got, want)
		}
	}
	if logged := record.message + fmt.Sprint(record.attrs); strings.Contains(logged, cause) {
		t.Fatalf("failure record exposed raw handler error: %s", logged)
	}
}

func TestResponseEncodingFailureLogNamesCause(t *testing.T) {
	const cause = "PRIVATE-ENCODING-DETAIL"
	clientTransport, serverTransport := transport.Intermediate.Pipe()
	t.Cleanup(func() {
		if err := clientTransport.Close(); err != nil {
			t.Errorf("close client transport: %v", err)
		}
		if err := serverTransport.Close(); err != nil {
			t.Errorf("close server transport: %v", err)
		}
	})
	conn := newConn(serverTransport, tdcrypto.NewServerCipher(tdcrypto.DefaultRand()), proto.NewMessageIDGen(time.Now), clock.System, time.Second, nil)
	sink := &serverErrorLogSink{}
	handler := NewDispatcher().HandleFuncNamed(tg.HelpGetAppConfigRequestTypeID, "help.getAppConfig", func(c *Conn, req *Request) error {
		return c.SendResult(req, serverErrorLogEncoder{err: errors.New(cause)})
	})
	server := New(exchange.PrivateKey{}, 2, NewMemoryAuthKeyStore(), handler, slog.New(sink))
	server.checkMsgIDTime = false
	var body bin.Buffer
	body.PutID(tg.HelpGetAppConfigRequestTypeID)
	err := server.handle(conn, &Request{MsgID: 17, Buf: &body, Ctx: context.Background()})
	if err == nil {
		t.Fatal("handle succeeded after response encoding failed")
	}
	server.logConnectionFailure(errors.Join(errRequestHandlingFailure, err))

	if len(sink.records) != 1 {
		t.Fatalf("failure records = %d, want 1", len(sink.records))
	}
	if got := sink.records[0].attrs["cause"]; got != "response_encoding" {
		t.Fatalf("cause = %v, want response_encoding", got)
	}
	if logged := sink.records[0].message + fmt.Sprint(sink.records[0].attrs); strings.Contains(logged, cause) {
		t.Fatalf("failure record exposed raw encoder error: %s", logged)
	}
}

func TestRequestFailureLogIncludesSafeRPCErrorCode(t *testing.T) {
	const rawError = "INTERNAL_PRIVATE_DETAIL"
	sink := &serverErrorLogSink{}
	server := New(exchange.PrivateKey{}, 2, NewMemoryAuthKeyStore(), nil, slog.New(sink))
	req := &Request{rpcMethod: "messages.getDialogs"}
	err := errors.Join(errRequestHandlingFailure, requestFailureContext(req, tgerr.New(500, rawError)))

	server.logConnectionFailure(err)
	if len(sink.records) != 1 {
		t.Fatalf("failure records = %d, want 1", len(sink.records))
	}
	record := sink.records[0]
	for key, want := range map[string]any{
		"rpc_method":     "messages.getDialogs",
		"error":          "rpc",
		"rpc_error_code": int64(500),
	} {
		if got := record.attrs[key]; got != want {
			t.Errorf("%s = %v, want %v", key, got, want)
		}
	}
	if logged := record.message + fmt.Sprint(record.attrs); strings.Contains(logged, rawError) {
		t.Fatalf("failure record exposed raw RPC error: %s", logged)
	}
}

func TestRequestFailureLogWithoutContextIncludesSafeRPCCode(t *testing.T) {
	const rawError = "INTERNAL_PRIVATE_DETAIL"
	sink := &serverErrorLogSink{}
	server := New(exchange.PrivateKey{}, 2, NewMemoryAuthKeyStore(), nil, slog.New(sink))
	err := errors.Join(errRequestHandlingFailure, tgerr.New(500, rawError))

	server.logConnectionFailure(err)
	if len(sink.records) != 1 {
		t.Fatalf("failure records = %d, want 1", len(sink.records))
	}
	record := sink.records[0]
	for key, want := range map[string]any{
		"rpc_method":     UnknownRPCMethod,
		"error":          "rpc",
		"rpc_error_code": int64(500),
	} {
		if got := record.attrs[key]; got != want {
			t.Errorf("%s = %v, want %v", key, got, want)
		}
	}
	if logged := record.message + fmt.Sprint(record.attrs); strings.Contains(logged, rawError) {
		t.Fatalf("failure record exposed raw RPC error: %s", logged)
	}
}
