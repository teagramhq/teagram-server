package mtproto

import (
	"testing"
	"time"
)

func TestServerShutdownHardCutoff(t *testing.T) {
	if defaultDrainTimeout != 90*time.Second {
		t.Fatalf("default drain timeout = %s, want 90s", defaultDrainTimeout)
	}

	shutdown := newServerShutdown()
	shutdown.drainTimeout = 25 * time.Millisecond
	shutdown.startServing()
	releaseRPC, admitted := shutdown.admitRPC()
	if !admitted {
		t.Fatal("RPC was not admitted before drain")
	}

	shutdown.beginDrain()
	if err := shutdown.requestCtx.Err(); err != nil {
		t.Fatalf("request context canceled at drain start: %v", err)
	}
	select {
	case <-shutdown.rpcDrained:
		t.Fatal("active RPC was removed from drain accounting before release")
	default:
	}

	select {
	case <-shutdown.requestCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("hard cutoff did not cancel the request context")
	}
	select {
	case <-shutdown.rpcDrained:
		t.Fatal("hard cutoff erased the active RPC from drain accounting")
	default:
	}

	releaseRPC()
	select {
	case <-shutdown.rpcDrained:
	case <-time.After(time.Second):
		t.Fatal("released RPC did not finish drain accounting")
	}
	shutdown.finishServing()
}

func TestServerShutdownCapsLateRetirementForCleanup(t *testing.T) {
	shutdown := newServerShutdown()
	shutdown.drainTimeout = 6 * time.Second
	shutdown.retirementWindow = time.Second
	shutdown.retirementSlots = 16
	shutdown.beginDrain()
	defer func() {
		if timer := shutdown.drainTimer.Load(); timer != nil {
			timer.Stop()
		}
		if timer := shutdown.outputTimer.Load(); timer != nil {
			timer.Stop()
		}
		shutdown.cancelReq()
		shutdown.cancelOutput()
	}()

	// Leave only 100ms before the five-second cleanup reserve expires, then
	// select the last slot whose normal retirement delay is one second.
	shutdown.drainStarted.Store(time.Now().Add(-900 * time.Millisecond).UnixNano())
	shutdown.retirementSeq.Store(shutdown.retirementSlots - 1)
	started := time.Now()
	shutdown.waitForRetirement(false)
	if elapsed := time.Since(started); elapsed > 300*time.Millisecond {
		t.Fatalf("late retirement took %s, want it capped to remaining drain slack", elapsed)
	}
}

func TestServerShutdownSkipsRetirementForClosedConnection(t *testing.T) {
	shutdown := newServerShutdown()
	shutdown.drainTimeout = 6 * time.Second
	shutdown.retirementWindow = time.Second
	shutdown.retirementSlots = 16
	shutdown.beginDrain()
	defer stopShutdownTimers(shutdown)
	shutdown.retirementSeq.Store(shutdown.retirementSlots - 1)

	started := time.Now()
	shutdown.waitForRetirement(true)
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("already closed connection waited %s for retirement", elapsed)
	}
}
