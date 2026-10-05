package mtproto

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/teagramhq/teagram-server/internal/srp"
	"github.com/teagramhq/teagram-server/internal/store"
)

const (
	// DefaultMaxPendingLoginConns caps deployment-wide connections waiting for
	// the second factor of a login. Postgres leases enforce this across replicas;
	// each held connection keeps a goroutine and an auth-key hold for the full
	// lease.
	DefaultMaxPendingLoginConns = 1024
	// DefaultPendingLoginLifetime is the connection lease after a password is
	// requested. It is twice the SRP challenge TTL so a client has time to fetch
	// the challenge and submit its proof while the challenge itself still keeps
	// its independent five-minute validity window.
	DefaultPendingLoginLifetime = 2 * srp.DefaultTTL
)

// pendingLoginLimiter is the in-memory fallback for pending-login connections.
// Production Postgres-backed servers use shared leases instead.
type pendingLoginLimiter struct {
	max   int64
	count atomic.Int64
}

func newPendingLoginLimiter(maxConns int) *pendingLoginLimiter {
	return &pendingLoginLimiter{max: int64(maxConns)}
}

// acquire claims one pending-login slot. Zero disables the cap.
func (l *pendingLoginLimiter) acquire() bool {
	if l.max <= 0 {
		return true
	}
	for {
		count := l.count.Load()
		if count >= l.max {
			return false
		}
		if l.count.CompareAndSwap(count, count+1) {
			return true
		}
	}
}

// release returns one slot to the in-memory pending-login pool.
func (l *pendingLoginLimiter) release() {
	if l.max > 0 {
		l.count.Add(-1)
	}
}

// pendingLoginHold is one connection's place in the pending-login cap. It is
// owned by the serving goroutine, so only the limiter's count needs atomic
// protection. The hold remains charged until the connection exits, including
// after a successful password check.
type pendingLoginHold struct {
	lim     *pendingLoginLimiter
	leases  pendingLoginLeaseStore
	lease   *store.LimitLease
	ttl     time.Duration
	charged bool
}

func (s *Server) newPendingLoginHold() *pendingLoginHold {
	return &pendingLoginHold{
		lim:    s.pendingLogins,
		leases: s.pendingLoginLeases,
		ttl:    s.pendingLoginLifetime + time.Second,
	}
}

type pendingLoginLeaseStore interface {
	TryAcquireLimitLease(context.Context, int64, string, int, time.Duration) (*store.LimitLease, *store.RateLimitResult, error)
	RenewLimitLease(context.Context, *store.LimitLease, time.Duration) error
	ReleaseLimitLease(context.Context, *store.LimitLease) error
}

const pendingLoginLeaseSurface = "pending_login"

// acquire claims the hold once. A Postgres-backed server uses a shared lease;
// in-memory auth-key stores retain the process-local limiter for tests and
// embedders.
func (h *pendingLoginHold) acquire(ctx context.Context) (bool, error) {
	if h.charged {
		return true, nil
	}
	if h.leases != nil {
		lease, denied, err := h.leases.TryAcquireLimitLease(ctx, 0, pendingLoginLeaseSurface, int(h.lim.max), h.ttl)
		if err != nil {
			return false, err
		}
		if denied != nil {
			return false, nil
		}
		h.lease = lease
		h.charged = true
		return true, nil
	}
	if !h.lim.acquire() {
		return false, nil
	}
	h.charged = true
	return true, nil
}

// renew extends the shared slot when a connection starts a new pending login.
// The in-memory fallback has no expiring lease to update.
func (h *pendingLoginHold) renew(ctx context.Context) error {
	if !h.charged || h.leases == nil || h.lease == nil {
		return nil
	}
	return h.leases.RenewLimitLease(ctx, h.lease, h.ttl)
}

// release returns the hold's slot. It is safe to call more than once.
func (h *pendingLoginHold) release(ctx context.Context) error {
	if !h.charged {
		return nil
	}
	h.charged = false
	if h.leases != nil {
		return h.leases.ReleaseLimitLease(ctx, h.lease)
	}
	h.lim.release()
	return nil
}
