package store_test

import (
	"context"
	"testing"
)

func TestDeletedChannelPostSweepResumesAcrossCalls(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	creator := mustUser(t, s, "+15551499601")
	channel, err := s.CreateChannel(ctx, creator.ID, "Sweep deleted posts", "", false)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	first, _, _, err := s.PostChannelMessage(ctx, channel.ID, creator.ID, "first post", 1049601, nil, 0)
	if err != nil {
		t.Fatalf("post first message: %v", err)
	}
	second, _, _, err := s.PostChannelMessage(ctx, channel.ID, creator.ID, "second post", 1049602, nil, 0)
	if err != nil {
		t.Fatalf("post second message: %v", err)
	}
	if _, err := s.DeleteChannel(ctx, channel.ID, creator.ID); err != nil {
		t.Fatalf("delete channel: %v", err)
	}

	count, worked, err := s.SweepDeletedChannelPosts(ctx)
	if err != nil || count < 2 || !worked {
		t.Fatalf("first sweep = count %d worked %t err %v, want tombstoned posts", count, worked, err)
	}
	posts, err := s.ChannelMessages(ctx, channel.ID, []int64{first.LocalID, second.LocalID})
	if err != nil {
		t.Fatalf("read tombstoned posts: %v", err)
	}
	for _, localID := range []int64{first.LocalID, second.LocalID} {
		if post, ok := posts[localID]; !ok || !post.Deleted || post.Message != "" {
			t.Errorf("post %d after sweep = %+v, present=%t, want payload-free tombstone", localID, post, ok)
		}
	}

	count, worked, err = s.SweepDeletedChannelPosts(ctx)
	if err != nil || count != 0 || !worked {
		t.Fatalf("completion sweep = count %d worked %t err %v, want completed job", count, worked, err)
	}
	count, worked, err = s.SweepDeletedChannelPosts(ctx)
	if err != nil || count != 0 || worked {
		t.Fatalf("empty sweep = count %d worked %t err %v, want no remaining jobs", count, worked, err)
	}
}
