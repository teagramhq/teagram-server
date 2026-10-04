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
