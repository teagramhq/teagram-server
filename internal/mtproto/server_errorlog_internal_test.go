package mtproto

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	tdcrypto "github.com/gotd/td/crypto"
	"github.com/gotd/td/exchange"
	"github.com/gotd/td/mt"
	"github.com/gotd/td/proto"
)

type serverErrorLogRecord struct {
	message string
	level   slog.Level
	attrs   map[string]any
}

type serverErrorLogSink struct {
	records []serverErrorLogRecord
}

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
