package e2e_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/gotd/td/tg"
)

func TestMultiCodeSinkWaitForSuccessfulAckAcceptsWrappedRPC(t *testing.T) {
	sink := newMultiCodeSink()
	addSuccessfulAck := func(rpcTypeID uint64) {
		t.Helper()
		record := slog.NewRecord(time.Now(), slog.LevelDebug, "server operation succeeded", 0)
		record.AddAttrs(
			slog.String("operation", "msgs_ack"),
			slog.Uint64("rpc_type_id", rpcTypeID),
		)
		if err := sink.Handle(context.Background(), record); err != nil {
			t.Fatalf("record successful msgs_ack: %v", err)
		}
	}

	addSuccessfulAck(uint64(tg.InvokeWithReCaptchaRequestTypeID))
	sink.drainSuccessfulAckEvents()
	noAckCtx, cancelNoAck := context.WithTimeout(context.Background(), 10*time.Millisecond)
	if err := sink.waitForSuccessfulAck(noAckCtx); !errors.Is(err, context.DeadlineExceeded) {
		cancelNoAck()
		t.Fatalf("wait after draining stale ack = %v, want deadline exceeded", err)
	}
	cancelNoAck()

	addSuccessfulAck(uint64(tg.InvokeWithReCaptchaRequestTypeID))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := sink.waitForSuccessfulAck(ctx); err != nil {
		t.Fatalf("wait for successful ack of wrapped auth.sendCode: %v", err)
	}
}
