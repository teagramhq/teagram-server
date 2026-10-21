package store

import (
	"context"
	"testing"
)

func TestAlbumDedupRowsRechecksExpiryAfterFilteredLookup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	expiredChecks := 0
	var callOrder []string
	lookupExpired := func(context.Context) ([]int64, error) {
		expiredChecks++
		callOrder = append(callOrder, "expired")
		if expiredChecks == 1 {
			return nil, nil
		}
		return []int64{101}, nil
	}
	lookupLive := func(context.Context) ([]string, error) {
		callOrder = append(callOrder, "live")
		return nil, nil
	}

	rows, expired, err := albumDedupRows(ctx, 1, lookupExpired, lookupLive)
	if err != nil {
		t.Fatalf("lookup dedup rows: %v", err)
	}
	if !expired || rows != nil {
		t.Fatalf("lookup = (rows %v, expired %v), want no rows and expired=true", rows, expired)
	}
	if expiredChecks != 2 {
		t.Fatalf("expired token checks = %d, want initial check and post-lookup recheck", expiredChecks)
	}
	if len(callOrder) != 3 || callOrder[0] != "expired" || callOrder[1] != "live" || callOrder[2] != "expired" {
		t.Fatalf("lookup order = %v, want expired, live, expired", callOrder)
	}
}

func TestAlbumDedupRowsSkipsExpiryRecheckWhenAllRowsAreLive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	expiredChecks := 0
	lookupExpired := func(context.Context) ([]int64, error) {
		expiredChecks++
		return nil, nil
	}
	lookupLive := func(context.Context) ([]string, error) {
		return []string{"live"}, nil
	}

	rows, expired, err := albumDedupRows(ctx, 1, lookupExpired, lookupLive)
	if err != nil {
		t.Fatalf("lookup dedup rows: %v", err)
	}
	if expired || len(rows) != 1 || rows[0] != "live" {
		t.Fatalf("lookup = (rows %v, expired %v), want one live row", rows, expired)
	}
	if expiredChecks != 1 {
		t.Fatalf("expired token checks = %d, want only initial check", expiredChecks)
	}
}
