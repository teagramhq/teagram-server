package e2e_test

import (
	"context"
	"errors"
	"sync"
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
		ctx, cancel := newDialogFilterObservedContext()
		t.Cleanup(cancel)
		invoked, result := startDialogFilterReadinessCall(readiness, ctx, mark, newTerminalResult())

		awaitDialogFilterReadinessWait(t, ctx, result)
		assertDialogFilterRPCNotIssued(t, invoked)
		readiness.observe(telegram.ConnectionStateDisconnected)
		readiness.observe(telegram.ConnectionStateReady)
		assertDialogFilterReadinessCall(t, invoked, result)
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
		ctx, cancel := newDialogFilterObservedContext()
		t.Cleanup(cancel)
		invoked, result := startDialogFilterReadinessCall(readiness, ctx, mark, newTerminalResult())

		awaitDialogFilterReadinessWait(t, ctx, result)
		assertDialogFilterRPCNotIssued(t, invoked)
		readiness.observe(telegram.ConnectionStateConnecting)
		readiness.observe(telegram.ConnectionStateReady)
		assertDialogFilterReadinessCall(t, invoked, result)
	})

	t.Run("A2 fresh ready does not release B1 pre-mark ready", func(t *testing.T) {
		a2, b1 := newDialogFilterReadiness(), newDialogFilterReadiness()
		b1.observe(telegram.ConnectionStateReady)
		markA2, markB1 := a2.mark(), b1.mark()
		a2.observe(telegram.ConnectionStateDisconnected)
		a2.observe(telegram.ConnectionStateReady)
		callsA2 := 0

		failure, rpcErr := a2.callAfterFreshReady(context.Background(), markA2, newTerminalResult(), func() error {
			callsA2++
			return nil
		})
		if rpcErr != nil || failure != nil {
			t.Fatalf("A2 call result = (%v, %#v), want success", rpcErr, failure)
		}

		ctx, cancel := newDialogFilterObservedContext()
		t.Cleanup(cancel)
		invokedB1, resultB1 := startDialogFilterReadinessCall(b1, ctx, markB1, newTerminalResult())
		awaitDialogFilterReadinessWait(t, ctx, resultB1)
		assertDialogFilterRPCNotIssued(t, invokedB1)
		b1.observe(telegram.ConnectionStateDisconnected)
		b1.observe(telegram.ConnectionStateReady)
		assertDialogFilterReadinessCall(t, invokedB1, resultB1)

		if callsA2 != 1 {
			t.Fatalf("A2 RPC calls = %d, want one", callsA2)
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

type dialogFilterObservedContext struct {
	context.Context

	waiting chan struct{}
	once    sync.Once
}

type dialogFilterReadinessCallResult struct {
	failure *dialogFilterReadinessFailure
	err     error
}

func newDialogFilterObservedContext() (*dialogFilterObservedContext, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	return &dialogFilterObservedContext{Context: ctx, waiting: make(chan struct{})}, cancel
}

func (c *dialogFilterObservedContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func startDialogFilterReadinessCall(
	readiness *dialogFilterReadiness,
	ctx context.Context,
	mark uint64,
	lifecycle *terminalResult,
) (<-chan struct{}, <-chan dialogFilterReadinessCallResult) {
	invoked := make(chan struct{}, 1)
	result := make(chan dialogFilterReadinessCallResult, 1)
	go func() {
		failure, err := readiness.callAfterFreshReady(ctx, mark, lifecycle, func() error {
			invoked <- struct{}{}
			return nil
		})
		result <- dialogFilterReadinessCallResult{failure: failure, err: err}
	}()
	return invoked, result
}

func awaitDialogFilterReadinessWait(
	t *testing.T,
	ctx *dialogFilterObservedContext,
	result <-chan dialogFilterReadinessCallResult,
) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.waiting:
	case outcome := <-result:
		t.Fatalf("readiness call returned before waiting: (%#v, %v)", outcome.failure, outcome.err)
	case <-timer.C:
		t.Fatal("readiness call did not enter its live wait")
	}
}

func assertDialogFilterRPCNotIssued(t *testing.T, invoked <-chan struct{}) {
	t.Helper()
	select {
	case <-invoked:
		t.Fatal("RPC was issued before the client's fresh Ready")
	default:
	}
}

func assertDialogFilterReadinessCall(
	t *testing.T,
	invoked <-chan struct{},
	result <-chan dialogFilterReadinessCallResult,
) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case outcome := <-result:
		if outcome.failure != nil || outcome.err != nil {
			t.Fatalf("readiness call result = (%#v, %v), want success", outcome.failure, outcome.err)
		}
	case <-timer.C:
		t.Fatal("readiness call did not finish after fresh Ready")
	}
	select {
	case <-invoked:
	default:
		t.Fatal("RPC was not issued after the client's fresh Ready")
	}
	select {
	case <-invoked:
		t.Fatal("RPC was issued more than once")
	default:
	}
}
