//nolint:testpackage // The cancellation budget is package-private by design.
package mtproto

import (
	"testing"
	"time"
)

func TestPeerCancelBudgetAppliesRollingUserAndGlobalLimits(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	budget := newPeerCancelBudget(func() time.Time { return now }, 8, 10*time.Second, 500*time.Millisecond, 2)

	first, denial := budget.reserve(11)
	if denial != peerCancelAllowed {
		t.Fatalf("first reservation denial = %q, want allowed", denial)
	}
	first.commit()
	if _, denial := budget.reserve(11); denial != peerCancelDeniedUserWindow {
		t.Fatalf("same-user reservation denial = %q, want user window", denial)
	}

	second, denial := budget.reserve(12)
	if denial != peerCancelAllowed {
		t.Fatalf("second user reservation denial = %q, want allowed", denial)
	}
	second.commit()
	if _, denial := budget.reserve(0); denial != peerCancelDeniedGlobal {
		t.Fatalf("pre-login reservation denial = %q, want global limit", denial)
	}

	now = now.Add(500 * time.Millisecond)
	third, denial := budget.reserve(0)
	if denial != peerCancelAllowed {
		t.Fatalf("refilled pre-login reservation denial = %q, want allowed", denial)
	}
	third.commit()

	now = now.Add(9500 * time.Millisecond)
	fourth, denial := budget.reserve(11)
	if denial != peerCancelAllowed {
		t.Fatalf("reservation at rolling-window boundary = %q, want allowed", denial)
	}
	fourth.release()
}

func TestPeerCancelBudgetRefundsUnappliedAndBoundsUserState(t *testing.T) {
	now := time.Unix(1_800_000_100, 0)
	budget := newPeerCancelBudget(func() time.Time { return now }, 1, 10*time.Second, 500*time.Millisecond, 2)

	first, denial := budget.reserve(21)
	if denial != peerCancelAllowed {
		t.Fatalf("first reservation denial = %q, want allowed", denial)
	}
	first.release()

	first, denial = budget.reserve(21)
	if denial != peerCancelAllowed {
		t.Fatalf("reservation after refund = %q, want allowed", denial)
	}
	first.commit()
	if _, denial := budget.reserve(22); denial != peerCancelDeniedCapacity {
		t.Fatalf("over-capacity reservation denial = %q, want capacity", denial)
	}

	now = now.Add(10 * time.Second)
	second, denial := budget.reserve(22)
	if denial != peerCancelAllowed {
		t.Fatalf("reservation after expired state pruning = %q, want allowed", denial)
	}
	second.release()
}

func TestPeerCancelBudgetThrottlesCapacityPruning(t *testing.T) {
	now := time.Unix(1_800_000_300, 0)
	budget := newPeerCancelBudget(func() time.Time { return now }, 1, 10*time.Second, 500*time.Millisecond, 2)
	first, denial := budget.reserve(31)
	if denial != peerCancelAllowed {
		t.Fatalf("initial reservation denial = %q, want allowed", denial)
	}
	first.commit()

	now = now.Add(9*time.Second + 800*time.Millisecond)
	if _, denial := budget.reserve(32); denial != peerCancelDeniedCapacity {
		t.Fatalf("pre-expiry capacity denial = %q, want capacity", denial)
	}
	now = now.Add(200 * time.Millisecond)
	if _, denial := budget.reserve(32); denial != peerCancelDeniedCapacity {
		t.Fatalf("capacity denial immediately after expiry = %q, want throttled capacity", denial)
	}
	now = now.Add(300 * time.Millisecond)
	second, denial := budget.reserve(32)
	if denial != peerCancelAllowed {
		t.Fatalf("reservation after the bounded prune interval = %q, want allowed", denial)
	}
	second.release()
}
