package mtproto

import (
	"context"
	"runtime"
	"testing"
	"time"
)

func TestRPCTraceRejectsRecordThatReachedAdmissionAfterClose(t *testing.T) {
	tracer := NewRPCTracer(RPCTracerConfig{Exporter: RPCSpanExporterFunc(func(context.Context, RPCSpan) error { return nil })})
	if !tracer.Enabled() {
		t.Fatal("new tracer is disabled")
	}
	if err := tracer.Close(); err != nil {
		t.Fatalf("close tracer: %v", err)
	}

	// Model Record being paused after its Enabled check and resumed after Close.
	tracer.recordEnabled(RPCSpan{Method: "messages.sendMessage", Result: RPCResultSuccess})
	snapshot := tracer.Snapshot()
	if !snapshot.ShutdownComplete || snapshot.Enqueued != 0 || snapshot.ExportOutstanding != 0 {
		t.Fatalf("late record changed completed shutdown snapshot: %+v", snapshot)
	}
}

func TestRPCTraceCloseDrainsRecordAdmittedBeforeShutdown(t *testing.T) {
	tracer := NewRPCTracer(RPCTracerConfig{
		Exporter:        RPCSpanExporterFunc(func(context.Context, RPCSpan) error { return nil }),
		ShutdownTimeout: 5 * time.Second,
	})
	if !tracer.beginRecord() {
		t.Fatal("new tracer rejected a record")
	}
	closed := make(chan error, 1)
	go func() { closed <- tracer.Close() }()

	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for tracer.recordGate.Load()&rpcTraceRecordGateClosedBit == 0 {
		select {
		case <-timer.C:
			tracer.endRecord()
			if err := <-closed; err != nil {
				t.Fatalf("close tracer after admission timeout: %v", err)
			}
			t.Fatal("shutdown did not close record admission")
		default:
			runtime.Gosched()
		}
	}
	select {
	case err := <-closed:
		t.Fatalf("shutdown completed before admitted record finished: %v", err)
	default:
	}

	tracer.enqueue(RPCSpan{Method: "messages.sendMessage", Result: RPCResultSuccess})
	tracer.endRecord()
	if err := <-closed; err != nil {
		t.Fatalf("close tracer: %v", err)
	}
	snapshot := tracer.Snapshot()
	if snapshot.Enqueued != 1 || snapshot.Exported != 1 || snapshot.ExportOutstanding != 0 || !snapshot.ShutdownComplete {
		t.Fatalf("admitted record shutdown snapshot = %+v, want one drained export", snapshot)
	}
}
