package mtproto

import (
	"context"
	"testing"
	"time"

	"github.com/gotd/td/clock"
)

func TestTailnetUnboundConnectionsDoNotSpendCallerCap(t *testing.T) {
	lim := newUnboundKeyLimiter(1)
	key := [8]byte{1}
	public := &unboundKeyHold{lim: lim}
	if !public.charge(key, 0) {
		t.Fatal("first limited-path connection was refused")
	}
	if (&unboundKeyHold{lim: lim}).charge(key, 0) {
		t.Fatal("second limited-path connection passed the per-key cap")
	}

	tailnet := &unboundKeyHold{lim: lim, path: ListenerPathTailnet}
	for range 50 {
		if !tailnet.charge(key, 0) {
			t.Fatal("tailnet connection was refused by the per-key cap")
		}
	}
	if got := lim.conns[key]; got != 1 {
		t.Fatalf("limited-path holds after tailnet connections = %d, want 1", got)
	}
	tailnet.release()
	public.release()
	if (&unboundKeyHold{lim: lim}).charge(key, 0) == false {
		t.Fatal("limited-path slot was not released")
	}
}

func TestTailnetPendingLoginDoesNotClaimConnectionLease(t *testing.T) {
	lim := newPendingLoginLimiter(1)
	s := &Server{pendingLogins: lim}
	hold := s.newPendingLoginHold(ListenerPathTailnet)

	for range 50 {
		admitted, err := hold.acquire(context.Background())
		if err != nil || !admitted {
			t.Fatalf("tailnet pending login = (%v, %v), want admitted", admitted, err)
		}
	}
	if got := lim.count.Load(); got != 0 {
		t.Fatalf("pending-login slots held by tailnet = %d, want 0", got)
	}
	if hold.charged {
		t.Fatal("tailnet pending login recorded a charged slot")
	}
}

func TestTailnetConnectionBypassesPerConnectionFloodAndQueueLimits(t *testing.T) {
	conn := &Conn{listenerPath: ListenerPathTailnet, clock: clock.System}
	for range unimplementedCloseAt + 1 {
		if got := conn.ChargeUnimplemented(); got != UnimplementedAnswer {
			t.Fatalf("tailnet unimplemented verdict = %v, want answer", got)
		}
		if got := conn.ChargeLangpack(); got != UnimplementedAnswer {
			t.Fatalf("tailnet langpack verdict = %v, want answer", got)
		}
	}
	if conn.unimplemented.calls != 0 || conn.langpack.calls != 0 {
		t.Fatalf("tailnet connection retained flood counts: unimplemented=%d langpack=%d", conn.unimplemented.calls, conn.langpack.calls)
	}

	set := concurrentRPCSet{active: maxConcurrentRPCsPerConn, entries: make(map[int64]chan struct{})}
	for i := range maxConcurrentRPCsPerConn + maxQueuedConcurrentRPCsPerConn {
		set.entries[int64(i+1)] = make(chan struct{})
	}
	work := concurrentRPCWork{msgID: 100}
	if started, _ := set.start(100, work, false); started {
		t.Fatal("limited path exceeded its concurrent RPC queue cap")
	}
	if started, _ := set.start(101, work, true); !started {
		t.Fatal("tailnet path was refused by the concurrent RPC queue cap")
	}
}

func TestTailnetPeerDisconnectSkipsPerUserBudget(t *testing.T) {
	now := time.Unix(1_800_000_200, 0)
	budget := newPeerCancelBudget(func() time.Time { return now }, 8, peerCancelUserWindow, peerCancelGlobalRefillPeriod, 10)
	for range 3 {
		ctx, cancel := context.WithCancel(context.Background())
		conn := &Conn{listenerPath: ListenerPathTailnet, peerCancels: budget}
		active := &activePeerRPC{userID: 41, ctx: ctx, cancel: cancel}
		if !conn.beginActiveRPC(active) {
			t.Fatal("tailnet connection refused active RPC")
		}
		conn.observePeerDisconnect()
		conn.finishActiveRPC(active)
		cancel()
	}
	if len(budget.users) != 0 {
		t.Fatalf("tailnet peer cancellations created %d per-user budget rows, want none", len(budget.users))
	}
	if got := budget.globalTokens; got != 7 {
		t.Fatalf("tailnet cancellations used global safety tokens = %v, want 7", got)
	}
}
