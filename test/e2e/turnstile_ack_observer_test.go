package e2e_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/gotd/td/tg"
)

func recordSuccessfulAck(t *testing.T, sink *multiCodeSink, rpcTypeIDs ...uint32) {
	t.Helper()
	attrs := []slog.Attr{slog.String("operation", "msgs_ack")}
	if len(rpcTypeIDs) == 1 {
		attrs = append(attrs, slog.Any("rpc_type_id", rpcTypeIDs[0]))
	} else {
		attrs = append(attrs, slog.Any("rpc_type_ids", rpcTypeIDs))
	}
	record := slog.NewRecord(time.Now(), slog.LevelDebug, "server operation succeeded", 0)
	record.AddAttrs(attrs...)
	if err := sink.Handle(context.Background(), record); err != nil {
		t.Fatalf("record successful msgs_ack: %v", err)
	}
}

func TestMultiCodeSinkWaitForSuccessfulAckMatchesWrappedOrLeafRPC(t *testing.T) {
	const unrelatedRPCTypeID = uint32(tg.HelpGetAppConfigRequestTypeID)
	targetRPCTypeIDs := []uint32{
		uint32(tg.InvokeWithReCaptchaRequestTypeID),
		uint32(tg.AuthSendCodeRequestTypeID),
	}
	tests := []struct {
		name      string
		loggedIDs []uint32
	}{
		{name: "wrapper constructor", loggedIDs: []uint32{targetRPCTypeIDs[0]}},
		{name: "leaf constructor", loggedIDs: []uint32{targetRPCTypeIDs[1]}},
		{name: "batched constructors", loggedIDs: []uint32{unrelatedRPCTypeID, targetRPCTypeIDs[1]}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sink := newMultiCodeSink()
			recordSuccessfulAck(t, sink, tt.loggedIDs...)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := sink.waitForSuccessfulAck(ctx, targetRPCTypeIDs...); err != nil {
				t.Fatalf("wait for successful ack with target constructor: %v", err)
			}
		})
	}
}

func TestMultiCodeSinkWaitForSuccessfulAckRejectsUnrelatedLateAck(t *testing.T) {
	sink := newMultiCodeSink()
	targetRPCTypeIDs := []uint32{
		uint32(tg.InvokeWithReCaptchaRequestTypeID),
		uint32(tg.AuthSendCodeRequestTypeID),
	}
	recordSuccessfulAck(t, sink, uint32(tg.HelpGetAppConfigRequestTypeID))
	sink.drainSuccessfulAckEvents()
	// flushAcks logs after releasing writeMu, so an earlier request's ACK can
	// arrive after the drain and must not release the sendCode wait.
	recordSuccessfulAck(t, sink, uint32(tg.HelpGetAppConfigRequestTypeID))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := sink.waitForSuccessfulAck(ctx, targetRPCTypeIDs...); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait after unrelated late ack = %v, want deadline exceeded", err)
	}
}
