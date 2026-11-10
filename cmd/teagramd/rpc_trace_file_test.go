//go:build !fixtureauthprogress

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/exchange"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/mtproto"
)

func TestRPCTraceFileOutputDisabledWithoutPath(t *testing.T) {
	t.Setenv(rpcTraceFileEnv, "")
	tracer, exporter, err := newRPCTraceFileTracerFromEnv()
	if err != nil {
		t.Fatalf("configure disabled trace: %v", err)
	}
	if tracer != nil || exporter != nil {
		t.Fatalf("tracer/exporter = %v/%v, want disabled", tracer, exporter)
	}
}

func TestRPCTraceFileOutputRejectsParentTraversal(t *testing.T) {
	t.Setenv(rpcTraceFileEnv, "../rpc-trace.jsonl")
	if err := validateServeEnvironment(); err == nil {
		t.Fatal("serve accepted RPC trace output path with parent traversal")
	}
}

func TestRPCTraceFileOptInCreatesPrivateOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rpc-trace.jsonl")
	t.Setenv("TG_RPC_TRACE_FILE", path)

	listener, err := new(net.ListenConfig).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() {
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("close listener: %v", err)
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	server := mtproto.New(exchange.PrivateKey{}, 2, nil, mtproto.NewDispatcher(), nil)
	if err := serveMTProto(ctx, server, listener, nil, nil); err != nil {
		t.Fatalf("serve: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat trace file: %v", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("trace file mode = %s, want a private regular file", info.Mode())
	}
}

func TestRPCTraceFileExporterWritesHandledRPCImmediately(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rpc-trace.jsonl")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open trace file: %v", err)
	}
	exporter := &rpcTraceFileExporter{file: file}
	start := time.UnixMilli(1_762_800_000_000).UTC()
	now := start
	var spans []mtproto.RPCSpan
	spanExporter := mtproto.RPCSpanExporterFunc(func(ctx context.Context, span mtproto.RPCSpan) error {
		spans = append(spans, span)
		return exporter.Export(ctx, span)
	})
	tracer := mtproto.NewRPCTracer(mtproto.RPCTracerConfig{
		Exporter:            spanExporter,
		SynchronousExporter: true,
		Now:                 func() time.Time { return now },
	})
	t.Cleanup(func() {
		if err := tracer.Close(); err != nil {
			t.Errorf("close tracer: %v", err)
		}
		if err := exporter.Close(); err != nil {
			t.Errorf("close exporter: %v", err)
		}
	})

	dispatcher := mtproto.NewDispatcher().HandleFunc(tg.MessagesGetHistoryRequestTypeID, func(_ *mtproto.Conn, _ *mtproto.Request) error {
		now = now.Add(40_123 * time.Microsecond)
		return nil
	})
	handler := tracer.Wrap(dispatcher)
	for range 2 {
		var requestBuffer bin.Buffer
		requestBuffer.PutID(tg.MessagesGetHistoryRequestTypeID)
		request := &mtproto.Request{Buf: &requestBuffer, Ctx: context.Background()}
		if err := handler.OnMessage(nil, request); err != nil {
			t.Fatalf("handle messages.getHistory: %v", err)
		}
	}

	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read trace file while tracer is active: %v", err)
	}
	lines := bytes.Split(bytes.TrimSpace(contents), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("trace lines = %d, want 2: %s", len(lines), contents)
	}
	if len(spans) != len(lines) {
		t.Fatalf("exported spans = %d, want %d", len(spans), len(lines))
	}
	for i, line := range lines {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(line, &fields); err != nil {
			t.Fatalf("decode trace line %d: %v", i, err)
		}
		if len(fields) != 4 {
			t.Fatalf("trace line %d has fields %v, want only method, duration, result, and time", i, fields)
		}
		var record struct {
			Method     string `json:"method"`
			DurationUS int64  `json:"duration_us"`
			Result     string `json:"result"`
			EndUnixMS  int64  `json:"end_unix_ms"`
		}
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("decode trace record %d: %v", i, err)
		}
		if record.Method != "messages.getHistory" || record.DurationUS != spans[i].Duration.Microseconds() || record.Result != "success" {
			t.Fatalf("trace record %d = %+v, want messages.getHistory/40123/success", i, record)
		}
		wantEnd := spans[i].EndTime.UnixMilli()
		if record.EndUnixMS != wantEnd {
			t.Errorf("trace record %d end time = %d, want %d", i, record.EndUnixMS, wantEnd)
		}
		if record.DurationUS != 40_123 {
			t.Errorf("trace record %d duration = %dµs, want 40123µs", i, record.DurationUS)
		}
		if strings.Contains(string(line), "user_id") || strings.Contains(string(line), "payload") {
			t.Errorf("trace record %d contains a non-redacted field: %s", i, line)
		}
	}
}
