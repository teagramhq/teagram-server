package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWithRPCDeadlineBoundsCall(t *testing.T) {
	const timeout = 25 * time.Millisecond
	err := withRPCDeadline(context.Background(), timeout, func(ctx context.Context) error {
		deadline, ok := ctx.Deadline()
		if !ok {
			return errors.New("RPC context has no deadline")
		}
		if remaining := time.Until(deadline); remaining <= 0 || remaining > timeout {
			return errors.New("RPC deadline is outside the requested timeout")
		}
		<-ctx.Done()
		return ctx.Err()
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("withRPCDeadline() error = %v, want context deadline exceeded", err)
	}
}
