package store_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

type recordingTracer struct {
	mu   sync.Mutex
	sqls []string
}

func (r *recordingTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sqls = append(r.sqls, data.SQL)
	return ctx
}

func (*recordingTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestQueryTracerSeesStoreQueries(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tracer := &recordingTracer{}
	s, err := store.Open(ctx, pgtest.DSN(t), pgtest.EncKey(), store.WithoutBlobStore(), store.WithQueryTracer(tracer))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, _, err := s.UserByID(ctx, 1); err != nil {
		t.Fatal(err)
	}
	tracer.mu.Lock()
	defer tracer.mu.Unlock()
	for _, sql := range tracer.sqls {
		if strings.Contains(sql, "FROM users") {
			return
		}
	}
	t.Fatalf("traced queries = %q, want the user lookup", tracer.sqls)
}

func TestMaxChannelParticipantsOptionBoundsAdmission(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, err := store.Open(ctx, pgtest.DSN(t), pgtest.EncKey(), store.WithoutBlobStore(), store.WithMaxChannelParticipants(2))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	owner, err := s.CreateUser(ctx, "+15553330001")
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateUser(ctx, "+15553330002")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateUser(ctx, "+15553330003")
	if err != nil {
		t.Fatal(err)
	}
	ch, err := s.CreateChannel(ctx, owner.ID, "capped", "", false)
	if err != nil {
		t.Fatal(err)
	}
	added, err := s.AddChannelMembers(ctx, ch.ID, owner.ID, []int64{a.ID, b.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 1 || added[0] != a.ID {
		t.Fatalf("added = %v, want only %d under a two-seat bound", added, a.ID)
	}
}

func TestMaxContactsPerUserOptionBoundsAdmission(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, err := store.Open(ctx, pgtest.DSN(t), pgtest.EncKey(), store.WithoutBlobStore(), store.WithMaxContactsPerUser(2))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	owner, err := s.CreateUser(ctx, "+15553330101")
	if err != nil {
		t.Fatal(err)
	}
	contacts := make([]int64, 3)
	for i, phone := range []string{"+15553330102", "+15553330103", "+15553330104"} {
		user, err := s.CreateUser(ctx, phone)
		if err != nil {
			t.Fatal(err)
		}
		contacts[i] = user.ID
	}
	for _, contactID := range contacts[:2] {
		if changed, err := s.AddContact(ctx, owner.ID, contactID); err != nil || !changed {
			t.Fatalf("add contact %d: changed=%v err=%v", contactID, changed, err)
		}
	}
	if changed, err := s.AddContact(ctx, owner.ID, contacts[2]); changed || !errors.Is(err, store.ErrContactLimit) {
		t.Fatalf("third contact: changed=%v err=%v, want ErrContactLimit", changed, err)
	}
}

func TestMaxContactsPerUserOptionBoundsImport(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, err := store.Open(ctx, pgtest.DSN(t), pgtest.EncKey(), store.WithoutBlobStore(), store.WithMaxContactsPerUser(2))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	owner, err := s.CreateUser(ctx, "+15553330201")
	if err != nil {
		t.Fatal(err)
	}
	users := make([]store.User, 3)
	for i, phone := range []string{"+15553330202", "+15553330203", "+15553330204"} {
		if users[i], err = s.CreateUser(ctx, phone); err != nil {
			t.Fatal(err)
		}
	}
	if changed, err := s.AddContact(ctx, owner.ID, users[0].ID); err != nil || !changed {
		t.Fatalf("seed contact: changed=%v err=%v", changed, err)
	}
	book := []store.ContactImportInput{
		{ClientID: 1, Phone: users[1].Phone},
		{ClientID: 2, Phone: users[2].Phone},
	}
	if _, err := s.ImportContacts(ctx, owner.ID, book); !errors.Is(err, store.ErrContactLimit) {
		t.Fatalf("import past a two-seat bound error = %v, want ErrContactLimit", err)
	}
	if matches, err := s.ImportContacts(ctx, owner.ID, book[:1]); err != nil || len(matches.Matches) != 1 {
		t.Fatalf("import up to the bound = %+v err=%v, want one match", matches, err)
	}
}
