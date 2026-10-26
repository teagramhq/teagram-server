package store_test

import (
	"context"
	"testing"
)

func TestChannelDeliverySnapshotForUsersLimitsMembershipRows(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	creator := mustUser(t, s, "+15551297001")
	online := mustUser(t, s, "+15551297002")
	offline := mustUser(t, s, "+15551297003")
	outsider := mustUser(t, s, "+15551297004")
	channel := mustChannel(t, s, creator.ID, "delivery snapshot")
	if _, err := s.AddChannelMembers(ctx, channel.ID, creator.ID, []int64{online.ID, offline.ID}); err != nil {
		t.Fatalf("add members: %v", err)
	}

	members, pts, err := s.ChannelDeliverySnapshotForUsers(ctx, channel.ID, []int64{online.ID, outsider.ID, online.ID})
	if err != nil {
		t.Fatalf("snapshot online candidates: %v", err)
	}
	if len(members) != 1 || members[0].UserID != online.ID {
		t.Fatalf("snapshot members = %+v, want only online member %d", members, online.ID)
	}
	if pts <= 0 {
		t.Fatalf("snapshot pts = %d, want a positive channel event ceiling", pts)
	}

	empty, emptyPts, err := s.ChannelDeliverySnapshotForUsers(ctx, channel.ID, nil)
	if err != nil {
		t.Fatalf("snapshot no candidates: %v", err)
	}
	if len(empty) != 0 || emptyPts != 0 {
		t.Fatalf("empty snapshot = (%+v, %d), want no members and no event ceiling", empty, emptyPts)
	}
}
