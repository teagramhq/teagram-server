package mtproto

// LockSessionRegistryForTest holds the registry lock until the returned
// function is called.
func LockSessionRegistryForTest(registry *SessionRegistry) func() {
	registry.mu.Lock()
	return registry.mu.Unlock
}
