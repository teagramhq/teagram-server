package mtproto

import (
	"sync"
	"sync/atomic"
	"time"
)

const (
	peerCancelUserWindow         = 10 * time.Second
	peerCancelGlobalRefillPeriod = 500 * time.Millisecond
	peerCancelGlobalBurst        = 2
	peerCancelMaxUserStates      = 65_536
)

type peerCancelDenial string

const (
	peerCancelAllowed           peerCancelDenial = "allowed"
	peerCancelDeniedUserWindow  peerCancelDenial = "user_window"
	peerCancelDeniedGlobal      peerCancelDenial = "global_limit"
	peerCancelDeniedCapacity    peerCancelDenial = "capacity"
	peerCancelDeniedReservation peerCancelDenial = "reservation"
)

type peerCancelUserState struct {
	lastApplied time.Time
	reservation uint64
}

// peerCancelBudget is replica-local. Its mutex protects only bounded accounting
// state and is released before a caller can cancel an RPC or close a transport.
type peerCancelBudget struct {
	mu sync.Mutex

	now          func() time.Time
	userWindow   time.Duration
	refillPeriod time.Duration
	burst        float64
	maxUsers     int

	users            map[int64]peerCancelUserState
	globalTokens     float64
	globalLastRefill time.Time
	lastUserPrune    time.Time
	nextReservation  uint64
}

type peerCancelReservation struct {
	budget *peerCancelBudget
	userID int64
	id     uint64
	spent  atomic.Bool
}

func newPeerCancelBudget(now func() time.Time, maxUsers int, userWindow, refillPeriod time.Duration, burst int) *peerCancelBudget {
	if now == nil {
		now = time.Now
	}
	if maxUsers < 1 {
		maxUsers = peerCancelMaxUserStates
	}
	if userWindow <= 0 {
		userWindow = peerCancelUserWindow
	}
	if refillPeriod <= 0 {
		refillPeriod = peerCancelGlobalRefillPeriod
	}
	if burst < 1 {
		burst = peerCancelGlobalBurst
	}
	nowAt := now()
	return &peerCancelBudget{
		now:              now,
		userWindow:       userWindow,
		refillPeriod:     refillPeriod,
		burst:            float64(burst),
		maxUsers:         maxUsers,
		users:            make(map[int64]peerCancelUserState),
		globalTokens:     float64(burst),
		globalLastRefill: nowAt,
	}
}

func (b *peerCancelBudget) reserve(userID int64) (*peerCancelReservation, peerCancelDenial) {
	now := b.now()
	b.mu.Lock()
	defer b.mu.Unlock()

	b.refillLocked(now)

	var userReserved bool
	var reservationID uint64
	if userID > 0 {
		state, ok := b.users[userID]
		if !ok && len(b.users) >= b.maxUsers && (b.lastUserPrune.IsZero() || !now.Before(b.lastUserPrune.Add(b.refillPeriod))) {
			b.pruneUsersLocked(now)
			b.lastUserPrune = now
			state, ok = b.users[userID]
		}
		if ok {
			if state.reservation != 0 {
				return nil, peerCancelDeniedReservation
			}
			if !state.lastApplied.IsZero() && now.Before(state.lastApplied.Add(b.userWindow)) {
				return nil, peerCancelDeniedUserWindow
			}
		} else if len(b.users) >= b.maxUsers {
			return nil, peerCancelDeniedCapacity
		}
		b.nextReservation++
		if b.nextReservation == 0 {
			b.nextReservation++
		}
		state.reservation = b.nextReservation
		reservationID = b.nextReservation
		b.users[userID] = state
		userReserved = true
	}

	if b.globalTokens < 1 {
		if userReserved {
			delete(b.users, userID)
		}
		return nil, peerCancelDeniedGlobal
	}
	b.globalTokens--
	return &peerCancelReservation{budget: b, userID: userID, id: reservationID}, peerCancelAllowed
}

func (b *peerCancelBudget) refillLocked(now time.Time) {
	if !now.After(b.globalLastRefill) {
		return
	}
	elapsed := now.Sub(b.globalLastRefill)
	b.globalTokens += float64(elapsed) / float64(b.refillPeriod)
	if b.globalTokens > b.burst {
		b.globalTokens = b.burst
	}
	b.globalLastRefill = now
}

func (b *peerCancelBudget) pruneUsersLocked(now time.Time) {
	for userID, state := range b.users {
		if state.reservation == 0 && !state.lastApplied.IsZero() && !now.Before(state.lastApplied.Add(b.userWindow)) {
			delete(b.users, userID)
		}
	}
}

func (r *peerCancelReservation) commit() {
	if r == nil || !r.spent.CompareAndSwap(false, true) {
		return
	}
	b := r.budget
	now := b.now()
	b.mu.Lock()
	if r.userID > 0 {
		state, ok := b.users[r.userID]
		if ok && state.reservation == r.id {
			state.reservation = 0
			state.lastApplied = now
			b.users[r.userID] = state
		}
	}
	b.mu.Unlock()
}

func (r *peerCancelReservation) release() {
	if r == nil || !r.spent.CompareAndSwap(false, true) {
		return
	}
	b := r.budget
	b.mu.Lock()
	if r.userID > 0 {
		state, ok := b.users[r.userID]
		if ok && state.reservation == r.id {
			if state.lastApplied.IsZero() {
				delete(b.users, r.userID)
			} else {
				state.reservation = 0
				b.users[r.userID] = state
			}
		}
	}
	b.globalTokens++
	if b.globalTokens > b.burst {
		b.globalTokens = b.burst
	}
	b.mu.Unlock()
}
