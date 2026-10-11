package e2e_test

import (
	"context"
	"sync"

	"github.com/gotd/td/telegram"
)

type dialogFilterReadiness struct {
	mu              sync.Mutex
	generation      uint64
	readyGeneration uint64
	state           telegram.ConnectionState
	changed         chan struct{}
}

func newDialogFilterReadiness() *dialogFilterReadiness {
	return &dialogFilterReadiness{changed: make(chan struct{})}
}

func (r *dialogFilterReadiness) observe(state telegram.ConnectionState) {
	r.mu.Lock()
	defer r.mu.Unlock()

	switch state {
	case telegram.ConnectionStateDisconnected:
		r.generation++
		r.state = state
	case telegram.ConnectionStateConnecting:
		r.state = state
	case telegram.ConnectionStateReady:
		r.readyGeneration = r.generation
		r.state = state
	default:
		return
	}

	close(r.changed)
	r.changed = make(chan struct{})
}

func (r *dialogFilterReadiness) mark() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.generation
}

func (r *dialogFilterReadiness) callAfterFreshReady(
	ctx context.Context,
	mark uint64,
	lifecycle *terminalResult,
	call func() error,
) (*dialogFilterReadinessFailure, error) {
	for {
		if failure := r.stoppedFailure(ctx, mark, lifecycle); failure != nil {
			return failure, nil
		}

		ready, changed := r.readyAfter(mark)
		if ready {
			if failure := r.stoppedFailure(ctx, mark, lifecycle); failure != nil {
				return failure, nil
			}
			return nil, call()
		}

		select {
		case <-changed:
		case <-lifecycle.done:
		case <-ctx.Done():
		}
	}
}

func (r *dialogFilterReadiness) readyAfter(mark uint64) (bool, <-chan struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.generation > mark && r.readyGeneration == r.generation && r.state == telegram.ConnectionStateReady, r.changed
}

func (r *dialogFilterReadiness) stoppedFailure(ctx context.Context, mark uint64, lifecycle *terminalResult) *dialogFilterReadinessFailure {
	state := r.missingState(mark)
	if lifecycle.finished() {
		return &dialogFilterReadinessFailure{cause: "cause=" + safeErrorClass(lifecycle.error()), state: state}
	}
	if ctx.Err() != nil {
		return &dialogFilterReadinessFailure{cause: contextFailureDescription(ctx), state: state}
	}
	return nil
}

func (r *dialogFilterReadiness) missingState(mark uint64) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.generation <= mark {
		return "no-post-mark-disconnect"
	}
	return "no-fresh-ready"
}

type dialogFilterReadinessFailure struct {
	cause string
	state string
}

func (f *dialogFilterReadinessFailure) diagnostic() string {
	return f.cause + " state=" + f.state
}
