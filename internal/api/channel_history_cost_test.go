package api_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/gotd/td/tg"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/store"
)

type historyQueryCounter struct {
	mu    sync.Mutex
	count int
}

func (c *historyQueryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.mu.Lock()
	c.count++
	c.mu.Unlock()
	return ctx
}

func (*historyQueryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (c *historyQueryCounter) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.count = 0
}

func (c *historyQueryCounter) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.count
}

func TestGroupHistoryStatementCountDoesNotDependOnAuthors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tracer := &historyQueryCounter{}
	s, _ := openStoreDSNWithOptions(t, store.WithQueryTracer(tracer))

	owner, err := s.CreateUser(ctx, "+15551270001")
	if err != nil {
		t.Fatal(err)
	}
	group, err := s.CreateChannel(ctx, owner.ID, "History cost", "", true)
	if err != nil {
		t.Fatal(err)
	}
	authors := make([]store.User, 100)
	authorIDs := make([]int64, len(authors))
	for i := range authors {
		authors[i], err = s.CreateUser(ctx, fmt.Sprintf("+15551271%04d", i))
		if err != nil {
			t.Fatalf("create group author %d: %v", i, err)
		}
		authorIDs[i] = authors[i].ID
	}
	if _, err := s.AddChannelMembers(ctx, group.ID, owner.ID, authorIDs); err != nil {
		t.Fatalf("add group authors: %v", err)
	}
	for i, author := range authors {
		if _, _, _, err := s.PostChannelMessageAs(ctx, group.ID, author.ID, "member post", int64(i+1), nil, 0); err != nil {
			t.Fatalf("post as group author %d: %v", i, err)
		}
	}

	request := &tg.MessagesGetHistoryRequest{Peer: channelPeer(owner.ID, group.ID), Limit: 100}
	tracer.reset()
	if _, err := api.GetHistoryForTest(s, owner.ID, request); err != nil {
		t.Fatalf("get history for many authors: %v", err)
	}
	manyAuthorStatements := tracer.total()

	for i := range 100 {
		if _, _, _, err := s.PostChannelMessageAs(ctx, group.ID, owner.ID, "owner post", int64(1001+i), nil, 0); err != nil {
			t.Fatalf("post as owner %d: %v", i, err)
		}
	}
	tracer.reset()
	if _, err := api.GetHistoryForTest(s, owner.ID, request); err != nil {
		t.Fatalf("get history for one author: %v", err)
	}
	oneAuthorStatements := tracer.total()

	if manyAuthorStatements != oneAuthorStatements {
		t.Fatalf("getHistory statements differ by page authors: many=%d one=%d", manyAuthorStatements, oneAuthorStatements)
	}
}

func TestGroupHistoryHidesFormerAuthorsWithoutAnotherLiveEdge(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	viewer, err := s.CreateUser(ctx, "+15551279991")
	if err != nil {
		t.Fatal(err)
	}
	former, err := s.CreateUser(ctx, "+15551279992")
	if err != nil {
		t.Fatal(err)
	}
	group, err := s.CreateChannel(ctx, viewer.ID, "Former author", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddChannelMembers(ctx, group.ID, viewer.ID, []int64{former.ID}); err != nil {
		t.Fatalf("add former author: %v", err)
	}
	if _, _, _, err := s.PostChannelMessageAs(ctx, group.ID, former.ID, "historical post", 1, nil, 0); err != nil {
		t.Fatalf("post as former author: %v", err)
	}
	if left, err := s.LeaveChannel(ctx, group.ID, former.ID); err != nil || !left {
		t.Fatalf("former author leave = %t, err %v", left, err)
	}

	result, err := api.GetHistoryForTest(s, viewer.ID, &tg.MessagesGetHistoryRequest{
		Peer:  channelPeer(viewer.ID, group.ID),
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("get group history: %v", err)
	}
	page := channelMessagesResult(t, result)
	for _, user := range page.Users {
		switch user := user.(type) {
		case *tg.User:
			if user.ID == former.ID {
				t.Fatal("former author profile remained visible without a live edge")
			}
		case *tg.UserEmpty:
			if user.ID == former.ID {
				return
			}
		}
	}
	t.Fatalf("history did not include UserEmpty for former author %d", former.ID)
}
