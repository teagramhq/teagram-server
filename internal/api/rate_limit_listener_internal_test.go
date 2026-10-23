package api

import (
	"net/netip"
	"testing"

	"github.com/teagramhq/teagram-server/internal/store"
)

func TestRateLimitIPSubjectIDUsesAcceptedListenerScope(t *testing.T) {
	t.Parallel()

	key := netip.MustParsePrefix("192.0.2.24/32")
	tailnetID, err := keyToSubjectIDForListener(key, store.RateLimitListenerTailnet)
	if err != nil {
		t.Fatalf("tailnet subject ID: %v", err)
	}
	legacyID, err := keyToSubjectID(key)
	if err != nil {
		t.Fatalf("legacy subject ID: %v", err)
	}
	if tailnetID != legacyID {
		t.Fatalf("tailnet subject ID = %d, want existing value %d", tailnetID, legacyID)
	}

	publicID, err := keyToSubjectIDForListener(key, store.RateLimitListenerPublic)
	if err != nil {
		t.Fatalf("public subject ID: %v", err)
	}
	if publicID == tailnetID {
		t.Fatalf("public and tailnet listeners share subject ID %d", publicID)
	}

	if _, err := keyToSubjectIDForListener(key, store.RateLimitListener(255)); err == nil {
		t.Fatal("unknown listener scope was accepted")
	}
}
