package e2e_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gotd/td/telegram"
)

type dialogFilterRPCError struct{}

func (*dialogFilterRPCError) Error() string { return "RPC failure" }

func TestDialogFilterFreshConnectionReadiness(t *testing.T) {
	t.Run("pre-mark ready does not release", func(t *testing.T) {
		readiness := newDialogFilterReadiness()
		readiness.observe(telegram.ConnectionStateReady)
		mark := readiness.mark()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		calls := 0

		failure, rpcErr := readiness.callAfterFreshReady(ctx, mark, newTerminalResult(), func() error {
			calls++
			return nil
		})

		if rpcErr != nil {
			t.Fatalf("RPC error = %v, want nil", rpcErr)
		}
		if failure == nil || failure.state != "no-post-mark-disconnect" {
			t.Fatalf("readiness failure = %#v, want no-post-mark-disconnect", failure)
		}
		if calls != 0 {
			t.Fatalf("RPC calls = %d, want none", calls)
		}
	})

	t.Run("disconnect then ready releases one RPC", func(t *testing.T) {
		readiness := newDialogFilterReadiness()
		mark := readiness.mark()
		readiness.observe(telegram.ConnectionStateDisconnected)
		readiness.observe(telegram.ConnectionStateReady)
		calls := 0

		failure, rpcErr := readiness.callAfterFreshReady(context.Background(), mark, newTerminalResult(), func() error {
			calls++
			return nil
		})

		if rpcErr != nil || failure != nil {
			t.Fatalf("call result = (%v, %#v), want success", rpcErr, failure)
		}
		if calls != 1 {
			t.Fatalf("RPC calls = %d, want one", calls)
		}
	})

	t.Run("newer disconnect invalidates ready", func(t *testing.T) {
		readiness := newDialogFilterReadiness()
		mark := readiness.mark()
		readiness.observe(telegram.ConnectionStateDisconnected)
		readiness.observe(telegram.ConnectionStateReady)
		readiness.observe(telegram.ConnectionStateDisconnected)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		calls := 0

		failure, rpcErr := readiness.callAfterFreshReady(ctx, mark, newTerminalResult(), func() error {
			calls++
			return nil
		})

		if rpcErr != nil {
			t.Fatalf("RPC error = %v, want nil", rpcErr)
		}
		if failure == nil || failure.state != "no-fresh-ready" {
			t.Fatalf("readiness failure = %#v, want no-fresh-ready", failure)
		}
		if calls != 0 {
			t.Fatalf("RPC calls = %d, want none", calls)
		}
	})

	t.Run("A2 fresh ready does not release B1 pre-mark ready", func(t *testing.T) {
		a2, b1 := newDialogFilterReadiness(), newDialogFilterReadiness()
		b1.observe(telegram.ConnectionStateReady)
		markA2, markB1 := a2.mark(), b1.mark()
		a2.observe(telegram.ConnectionStateDisconnected)
		a2.observe(telegram.ConnectionStateReady)
		callsA2, callsB1 := 0, 0

		failure, rpcErr := a2.callAfterFreshReady(context.Background(), markA2, newTerminalResult(), func() error {
			callsA2++
			return nil
		})
		if rpcErr != nil || failure != nil {
			t.Fatalf("A2 call result = (%v, %#v), want success", rpcErr, failure)
		}

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		failure, rpcErr = b1.callAfterFreshReady(ctx, markB1, newTerminalResult(), func() error {
			callsB1++
			return nil
		})
		if rpcErr != nil {
			t.Fatalf("B1 RPC error = %v, want nil", rpcErr)
		}
		if failure == nil || failure.state != "no-post-mark-disconnect" {
			t.Fatalf("B1 readiness failure = %#v, want no-post-mark-disconnect", failure)
		}
		if callsA2 != 1 || callsB1 != 0 {
			t.Fatalf("RPC calls A2/B1 = %d/%d, want 1/0", callsA2, callsB1)
		}
	})

	t.Run("terminal client exit prevents invocation", func(t *testing.T) {
		readiness := newDialogFilterReadiness()
		mark := readiness.mark()
		lifecycle := newTerminalResult()
		lifecycle.complete(errProbeClientExit)
		calls := 0

		failure, rpcErr := readiness.callAfterFreshReady(context.Background(), mark, lifecycle, func() error {
			calls++
			return nil
		})

		if rpcErr != nil {
			t.Fatalf("RPC error = %v, want nil", rpcErr)
		}
		if failure == nil || failure.state != "no-post-mark-disconnect" {
			t.Fatalf("readiness failure = %#v, want no-post-mark-disconnect", failure)
		}
		if calls != 0 {
			t.Fatalf("RPC calls = %d, want none", calls)
		}
	})

	for _, tc := range []struct {
		name string
		ctx  func() (context.Context, context.CancelFunc)
	}{
		{
			name: "context cancellation prevents invocation",
			ctx: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, func() {}
			},
		},
		{
			name: "context deadline prevents invocation",
			ctx: func() (context.Context, context.CancelFunc) {
				return context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			readiness := newDialogFilterReadiness()
			mark := readiness.mark()
			ctx, cancel := tc.ctx()
			defer cancel()
			calls := 0

			failure, rpcErr := readiness.callAfterFreshReady(ctx, mark, newTerminalResult(), func() error {
				calls++
				return nil
			})

			if rpcErr != nil {
				t.Fatalf("RPC error = %v, want nil", rpcErr)
			}
			if failure == nil || failure.state != "no-post-mark-disconnect" {
				t.Fatalf("readiness failure = %#v, want no-post-mark-disconnect", failure)
			}
			if calls != 0 {
				t.Fatalf("RPC calls = %d, want none", calls)
			}
		})
	}

	t.Run("RPC error after release is unchanged", func(t *testing.T) {
		readiness := newDialogFilterReadiness()
		mark := readiness.mark()
		readiness.observe(telegram.ConnectionStateDisconnected)
		readiness.observe(telegram.ConnectionStateReady)
		wantErr := &dialogFilterRPCError{}

		failure, gotErr := readiness.callAfterFreshReady(context.Background(), mark, newTerminalResult(), func() error {
			return wantErr
		})

		var unchangedErr *dialogFilterRPCError
		if !errors.As(gotErr, &unchangedErr) || unchangedErr != wantErr || errors.Unwrap(gotErr) != nil {
			t.Fatalf("RPC error = %T, want unchanged *dialogFilterRPCError", gotErr)
		}
		if failure != nil {
			t.Fatalf("readiness failure = %#v, want nil", failure)
		}
	})
}
