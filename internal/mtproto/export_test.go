package mtproto

import (
	"os"
	"testing"
)

// LockSessionRegistryForTest holds the registry lock until the returned
// function is called.
func LockSessionRegistryForTest(registry *SessionRegistry) func() {
	registry.mu.Lock()
	return registry.mu.Unlock
}

// TestMain turns the msg_id time window off for servers built by this
// package's tests: their scripted frames use fixed msg_ids such as 1<<32.
// Tests of the window turn it back on with EnableMsgIDTimeCheck.
func TestMain(m *testing.M) {
	checkMsgIDTimeByDefault = false
	os.Exit(m.Run())
}

// EnableMsgIDTimeCheck turns the msg_id time window on for s, as in production.
func EnableMsgIDTimeCheck(s *Server) {
	s.checkMsgIDTime = true
}

// Bounds on the per-connection memory of answered requests.
const (
	RPCReplayMaxEntries    = rpcReplayMaxEntries
	RPCReplayMaxBytes      = rpcReplayMaxBytes
	RPCReplayMaxEntryBytes = rpcReplayMaxEntryBytes
)
