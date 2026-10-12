package mtproto

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// SessionRegistry tracks the live connections for each authenticated user so the
// update-delivery path can push server-initiated messages to a user's sockets. A
// user may have several connections (multiple devices/sessions) at once.
type SessionRegistry struct {
	mu                sync.Mutex
	m                 map[int64][]*Conn
	onStatusChange    func(context.Context, int64, bool)
	statusChangeTails map[int64]*StatusChange
	maxUserConns      int
	recoveryConns     []*Conn
	recoveryCursor    int
	totalConns        atomic.Int64
}

// StatusChange is an ordered connection-status notification for one user. Its
// fields are private so notifications can only be produced by SessionRegistry.
type StatusChange struct {
	userID  int64
	online  bool
	after   <-chan struct{}
	done    chan struct{}
	claimed atomic.Bool
}

// LiveAccountsSnapshot is a bounded copy of the authenticated connections in
// the registry. AccountIDs is populated only when Complete is true; an
// over-limit snapshot never exposes a truncated prefix as a complete set.
type LiveAccountsSnapshot struct {
	Connections int
	Sessions    int
	AccountIDs  []int64
	Complete    bool
}

// DeliveryLagSample is the aggregate result of sampling the live authenticated
// connections in one registry snapshot. It carries no account or connection
// identity; the registry keeps those details private to the sampling pass.
type DeliveryLagSample struct {
	EligibleConnections int
	SampledConnections  int
	WorstPts            int64
	// Complete is false when the registry snapshot or sampling pass stopped at
	// its context deadline. It is intentionally not part of the admin payload;
	// the sampler uses it to avoid treating an interrupted empty pass as full.
	Complete bool
}

type deliveryLagConnection struct {
	ownerID int64
	conn    *Conn
}

type deliveryLagHead struct {
	head int64
	ok   bool
}

// NewSessionRegistry creates an empty registry.
func NewSessionRegistry() *SessionRegistry {
	return &SessionRegistry{m: map[int64][]*Conn{}, maxUserConns: MaxUserConns}
}

// MaxUserConns is the deployment-wide cap on live connections one user may
// hold. Server divides it across replicas and each registry enforces its share.
// Delivery walks every one of them per notification, so an account holding
// sockets without bound multiplies the cost of each of its own updates. A real
// client holds one socket per session and a handful of sessions, so the cap is
// far above legitimate use and only bites a client opening sockets in a loop.
const MaxUserConns = 20

func (r *SessionRegistry) setMaxUserConns(limit int) error {
	if limit < 1 {
		return fmt.Errorf("per-user connection cap is %d: must be positive", limit)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for userID, conns := range r.m {
		if len(conns) > limit {
			return fmt.Errorf("per-user connection cap %d is below user %d's current count %d", limit, userID, len(conns))
		}
	}
	r.maxUserConns = limit
	return nil
}

const (
	deliveryLagSnapshotLimit = 1024
	deliveryLagLockRetry     = time.Millisecond
)

// Add registers c as a live connection for userID, reporting whether it fit
// under the per-user cap. At the cap the new connection is refused and the live
// ones are left alone: dropping the oldest would take a working session away
// from a user for opening one more, which is the worse failure of the two.
func (r *SessionRegistry) Add(userID int64, c *Conn) bool {
	added, _ := r.add(userID, c, false)
	return added
}

// AddWithStatus registers c and returns an online notification for userID. The
// returned notification must be passed to NotifyStatusChange after releasing
// application locks.
func (r *SessionRegistry) AddWithStatus(userID int64, c *Conn) (bool, *StatusChange) {
	return r.add(userID, c, true)
}

func (r *SessionRegistry) add(userID int64, c *Conn, trackStatus bool) (bool, *StatusChange) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.m[userID]) >= r.maxUserConns {
		return false, nil
	}
	r.m[userID] = append(r.m[userID], c)
	r.recoveryConns = append(r.recoveryConns, c)
	r.totalConns.Add(1)
	if trackStatus {
		return true, r.queueStatusChangeLocked(userID, true)
	}
	return true, nil
}

// Remove deregisters c from userID and reports whether it removed the last
// connection. The removal and last-connection decision are one registry
// operation, so concurrent removals cannot both report the same transition.
func (r *SessionRegistry) Remove(userID int64, c *Conn) bool {
	last, _ := r.remove(userID, c, false)
	return last
}

// RemoveWithStatus deregisters c and returns the offline notification when it
// removes the last live connection for userID. The returned transition must be
// passed to NotifyStatusChange after releasing application locks.
func (r *SessionRegistry) RemoveWithStatus(userID int64, c *Conn) (bool, *StatusChange) {
	return r.remove(userID, c, true)
}

func (r *SessionRegistry) remove(userID int64, c *Conn, trackStatus bool) (bool, *StatusChange) {
	r.mu.Lock()
	defer r.mu.Unlock()
	conns := r.m[userID]
	removed := false
	for i, x := range conns {
		if x == c {
			r.m[userID] = append(conns[:i], conns[i+1:]...)
			r.totalConns.Add(-1)
			removed = true
			break
		}
	}
	for i, x := range r.recoveryConns {
		if x == c {
			r.recoveryConns = append(r.recoveryConns[:i], r.recoveryConns[i+1:]...)
			if len(r.recoveryConns) == 0 {
				r.recoveryCursor = 0
			} else {
				if i < r.recoveryCursor {
					r.recoveryCursor--
				}
				r.recoveryCursor %= len(r.recoveryConns)
			}
			break
		}
	}
	last := removed && len(r.m[userID]) == 0
	if last {
		delete(r.m, userID)
		if trackStatus {
			return true, r.queueStatusChangeLocked(userID, false)
		}
	}
	return last, nil
}

// queueStatusChangeLocked adds a transition to the per-user callback order.
// The registry lock is held only while linking the transition, never while its
// callback runs.
func (r *SessionRegistry) queueStatusChangeLocked(userID int64, online bool) *StatusChange {
	change := &StatusChange{userID: userID, online: online, done: make(chan struct{})}
	if tail := r.statusChangeTails[userID]; tail != nil {
		change.after = tail.done
	}
	if r.statusChangeTails == nil {
		r.statusChangeTails = make(map[int64]*StatusChange)
	}
	r.statusChangeTails[userID] = change
	return change
}

// setStatusChangeHandler installs the connection-lifecycle status callback.
// The callback is copied under mu and always invoked after releasing it.
func (r *SessionRegistry) setStatusChangeHandler(fn func(context.Context, int64, bool)) {
	r.mu.Lock()
	r.onStatusChange = fn
	r.mu.Unlock()
}

// NotifyStatusChange invokes a transition's callback after all earlier
// transitions for that user have completed. Neither the registry lock nor a
// per-user lock is held while waiting or running callback I/O.
func (r *SessionRegistry) NotifyStatusChange(ctx context.Context, change *StatusChange) {
	r.notifyStatusChange(ctx, change, nil)
}

// NotifyStatusChangeWithContext creates the callback context after all earlier
// transitions for that user have completed. The factory and callback both run
// without the registry lock held.
func (r *SessionRegistry) NotifyStatusChangeWithContext(
	ctx context.Context,
	change *StatusChange,
	contextFactory func(context.Context) (context.Context, context.CancelFunc),
) {
	r.notifyStatusChange(ctx, change, contextFactory)
}

func (r *SessionRegistry) notifyStatusChange(
	ctx context.Context,
	change *StatusChange,
	contextFactory func(context.Context) (context.Context, context.CancelFunc),
) {
	if change == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if !change.claimed.CompareAndSwap(false, true) {
		<-change.done
		return
	}
	defer func() {
		close(change.done)
		r.mu.Lock()
		if r.statusChangeTails[change.userID] == change {
			delete(r.statusChangeTails, change.userID)
		}
		r.mu.Unlock()
	}()
	if change.after != nil {
		<-change.after
	}
	r.mu.Lock()
	fn := r.onStatusChange
	r.mu.Unlock()
	if fn != nil {
		callbackCtx := ctx
		if contextFactory != nil {
			var cancel context.CancelFunc
			callbackCtx, cancel = contextFactory(ctx)
			defer cancel()
		}
		fn(callbackCtx, change.userID, change.online)
	}
}

// DialogFilterRecoveryCandidates returns a bounded round-robin snapshot. The
// cursor advances across ticks so a large registry cannot starve later conns.
func (r *SessionRegistry) DialogFilterRecoveryCandidates(limit int) []*Conn {
	if limit <= 0 {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	count := min(limit, len(r.recoveryConns))
	if count == 0 {
		return nil
	}
	out := make([]*Conn, count)
	start := r.recoveryCursor % len(r.recoveryConns)
	for i := range count {
		out[i] = r.recoveryConns[(start+i)%len(r.recoveryConns)]
	}
	r.recoveryCursor = (start + count) % len(r.recoveryConns)
	return out
}

// Conns returns a snapshot copy of userID's live connections, so callers can
// iterate and write without holding the registry lock.
func (r *SessionRegistry) Conns(userID int64) []*Conn {
	r.mu.Lock()
	defer r.mu.Unlock()
	conns := r.m[userID]
	out := make([]*Conn, len(conns))
	copy(out, conns)
	return out
}

// TotalConns returns the total number of live connections across all users.
func (r *SessionRegistry) TotalConns() int {
	return int(r.totalConns.Load())
}

// TotalSessions returns the number of distinct users with at least one live
// connection — i.e. authenticated sessions currently attached.
func (r *SessionRegistry) TotalSessions() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.m)
}

// SnapshotLiveAccounts copies the distinct account IDs and connection count
// under the registry lock. It acquires that lock only for bounded snapshot
// work, and returns an unavailable set rather than copying a partial prefix
// when the account cap is exceeded.
func (r *SessionRegistry) SnapshotLiveAccounts(ctx context.Context, maxAccounts int) (LiveAccountsSnapshot, error) {
	if maxAccounts < 0 {
		return LiveAccountsSnapshot{}, errors.New("max accounts must not be negative")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if !r.tryLock(ctx) {
		return LiveAccountsSnapshot{}, fmt.Errorf("acquire registry snapshot lock: %w", ctx.Err())
	}
	defer r.mu.Unlock()

	snapshot := LiveAccountsSnapshot{
		Connections: int(r.totalConns.Load()),
		Sessions:    len(r.m),
	}
	if err := ctx.Err(); err != nil {
		return LiveAccountsSnapshot{}, fmt.Errorf("snapshot live accounts: %w", err)
	}
	if snapshot.Sessions > maxAccounts {
		return snapshot, nil
	}
	snapshot.AccountIDs = make([]int64, 0, snapshot.Sessions)
	for userID := range r.m {
		if err := ctx.Err(); err != nil {
			return LiveAccountsSnapshot{}, fmt.Errorf("snapshot live accounts: %w", err)
		}
		if userID <= 0 {
			return LiveAccountsSnapshot{}, errors.New("live account ID must be positive")
		}
		snapshot.AccountIDs = append(snapshot.AccountIDs, userID)
	}
	if err := ctx.Err(); err != nil {
		return LiveAccountsSnapshot{}, fmt.Errorf("snapshot live accounts: %w", err)
	}
	snapshot.Complete = true
	return snapshot, nil
}

// SampleDeliveryLag snapshots at most 1024 connections, then reads each
// distinct account head once and compares it with every selected connection.
// The exact eligible count is maintained separately, so a large registry can
// be reported as partial without allocating or inspecting its entire
// population. The accountHead callback runs after the registry lock is
// released, so database work never blocks connection registration, removal, or
// delivery.
//
// An account-head error leaves all of that account's connections unsampled.
// The caller can therefore distinguish a partial or empty sample from a
// complete zero without publishing a fabricated caught-up result.
func (r *SessionRegistry) SampleDeliveryLag(
	ctx context.Context,
	accountHead func(context.Context, int64) (int64, error),
) DeliveryLagSample {
	if ctx == nil {
		ctx = context.Background()
	}
	connections, eligible, complete := r.deliveryLagSnapshot(ctx)
	sample := DeliveryLagSample{EligibleConnections: eligible, Complete: complete}
	if len(connections) == 0 || accountHead == nil {
		return sample
	}

	heads := make(map[int64]deliveryLagHead, len(connections))
	for _, connection := range connections {
		if ctx.Err() != nil {
			sample.Complete = false
			break
		}
		result, ok := heads[connection.ownerID]
		if !ok {
			head, err := accountHead(ctx, connection.ownerID)
			if ctx.Err() != nil {
				sample.Complete = false
				break
			}
			result = deliveryLagHead{head: head, ok: err == nil}
			heads[connection.ownerID] = result
		}
		if !result.ok || connection.conn == nil {
			continue
		}

		if ctx.Err() != nil {
			sample.Complete = false
			break
		}
		sample.SampledConnections++
		watermark := int64(connection.conn.LastPushedPts())
		if result.head <= watermark {
			continue
		}
		lag := result.head - watermark
		if lag > sample.WorstPts {
			sample.WorstPts = lag
		}
	}
	if ctx.Err() != nil {
		sample.Complete = false
	}
	return sample
}

// deliveryLagSnapshot obtains the registry lock only while copying the
// bounded work set. TryLock plus the sampling context makes lock acquisition
// deadline-aware; callers still receive the exact O(1) count maintained by
// Add and Remove when contention prevents a snapshot.
func (r *SessionRegistry) deliveryLagSnapshot(ctx context.Context) ([]deliveryLagConnection, int, bool) {
	if !r.tryLock(ctx) {
		return nil, int(r.totalConns.Load()), false
	}
	defer r.mu.Unlock()

	eligible := int(r.totalConns.Load())
	if ctx.Err() != nil {
		return nil, eligible, false
	}
	limit := min(eligible, deliveryLagSnapshotLimit)
	connections := make([]deliveryLagConnection, 0, limit)
	for ownerID, conns := range r.m {
		for i := 0; i < len(conns) && len(connections) < limit; i++ {
			if ctx.Err() != nil {
				return connections, eligible, false
			}
			connections = append(connections, deliveryLagConnection{ownerID: ownerID, conn: conns[i]})
		}
		if len(connections) == limit {
			return connections, eligible, true
		}
	}
	return connections, eligible, true
}

func (r *SessionRegistry) tryLock(ctx context.Context) bool {
	for {
		if ctx.Err() != nil {
			return false
		}
		if r.mu.TryLock() {
			return true
		}

		timer := time.NewTimer(deliveryLagLockRetry)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return false
		case <-timer.C:
		}
	}
}
