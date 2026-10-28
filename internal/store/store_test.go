package store_test

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

// testBlobs opens a blob store rooted in the test's own temporary directory.
func testBlobs(tb testing.TB) blob.Store {
	tb.Helper()
	b, err := blob.NewLocal(tb.TempDir())
	if err != nil {
		tb.Fatalf("blob store: %v", err)
	}
	return b
}

func open(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), pgtest.DSN(t), pgtest.EncKey(), store.WithBlobStore(testBlobs(t)))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return s
}

func openStore(tb testing.TB, dsn string) *store.Store {
	tb.Helper()
	s, err := store.Open(context.Background(), dsn, pgtest.EncKey(), store.WithBlobStore(testBlobs(tb)))
	if err != nil {
		tb.Fatalf("store.Open: %v", err)
	}
	tb.Cleanup(func() { _ = s.Close() }) //nolint:errcheck // best-effort close
	return s
}

// TestOpenRequiresBlobStore: the part bytes live in the blob backend, so a
// Store without one has no way to serve an upload. Left to default it would
// nil-panic on the first saveFilePart of a running server rather than refusing
// to start, which is the wrong end of the process to find out at.
func TestOpenRequiresBlobStore(t *testing.T) {
	t.Parallel()
	s, err := store.Open(context.Background(), pgtest.DSN(t), pgtest.EncKey())
	if err == nil {
		if cerr := s.Close(); cerr != nil {
			t.Errorf("close: %v", cerr)
		}
		t.Fatal("Open with no blob backend succeeded, want a startup failure")
	}
}

func TestOpenAllowsExplicitDatabaseOnlyStore(t *testing.T) {
	t.Parallel()
	s, err := store.Open(context.Background(), pgtest.DSN(t), pgtest.EncKey(), store.WithoutBlobStore())
	if err != nil {
		t.Fatalf("Open database-only store: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestDemoWipeGenerationMigrationSeedsOneOpaqueValue(t *testing.T) {
	t.Parallel()
	st := open(t)
	ctx := context.Background()

	var generation string
	if err := store.StorePool(st).QueryRow(ctx, `
		SELECT generation
		FROM demo_wipe_generation
		WHERE singleton = 1
	`).Scan(&generation); err != nil {
		t.Fatalf("read migrated demo wipe generation: %v", err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(generation) {
		t.Fatalf("demo wipe generation = %q, want 64 lowercase hexadecimal characters", generation)
	}

	var count int
	if err := store.StorePool(st).QueryRow(ctx, `SELECT count(*) FROM demo_wipe_generation`).Scan(&count); err != nil {
		t.Fatalf("count migrated demo wipe generations: %v", err)
	}
	if count != 1 {
		t.Fatalf("demo wipe generation rows = %d, want 1", count)
	}

	readGeneration, err := st.DemoWipeGeneration(ctx)
	if err != nil {
		t.Fatalf("read demo wipe generation through Store: %v", err)
	}
	if readGeneration != generation {
		t.Fatalf("Store generation = %q, want migrated value %q", readGeneration, generation)
	}
	commandGeneration, err := store.ReadDemoWipeGeneration(ctx, pgtest.DSN(t))
	if err != nil {
		t.Fatalf("read demo wipe generation for client-config: %v", err)
	}
	if commandGeneration != generation {
		t.Fatalf("client-config generation = %q, want migrated value %q", commandGeneration, generation)
	}
	if _, err := store.StorePool(st).Exec(ctx, `
		ALTER TABLE demo_wipe_generation
		DROP CONSTRAINT demo_wipe_generation_generation_check
	`); err != nil {
		t.Fatalf("remove generation constraint for invalid-value test: %v", err)
	}
	if _, err := store.StorePool(st).Exec(ctx, `
		UPDATE demo_wipe_generation
		SET generation = 'bad'
		WHERE singleton = 1
	`); err != nil {
		t.Fatalf("write invalid generation fixture: %v", err)
	}
	if _, err := st.DemoWipeGeneration(ctx); err == nil {
		t.Fatal("Store returned a malformed generation")
	}
	if _, err := store.StorePool(st).Exec(ctx, `DELETE FROM demo_wipe_generation WHERE singleton = 1`); err != nil {
		t.Fatalf("remove demo wipe generation fixture: %v", err)
	}
	if _, err := st.DemoWipeGeneration(ctx); err == nil {
		t.Fatal("Store returned a generation after its row was removed")
	}
}

func TestCreateUserAndLookup(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	const phone = "+15551230001"

	u, err := s.CreateUser(ctx, phone)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if u.ID == 0 {
		t.Fatal("user id is zero")
	}
	got, ok, err := s.UserByPhone(ctx, phone)
	if err != nil || !ok {
		t.Fatalf("lookup: ok=%v err=%v", ok, err)
	}
	if got.ID != u.ID {
		t.Errorf("id mismatch: %d vs %d", got.ID, u.ID)
	}
}

func TestCreateUserIsIdempotent(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	const phone = "+15551230002"

	a, err := s.CreateUser(ctx, phone)
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	b, err := s.CreateUser(ctx, phone)
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	if a.ID != b.ID {
		t.Errorf("duplicate phone made new id: %d != %d", a.ID, b.ID)
	}
}

func TestCodeLifecycle(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	const phone = "+15551230003"

	hash, code, err := s.IssueCode(ctx, phone)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if err := s.VerifyCode(ctx, phone, hash, code); err != nil {
		t.Fatalf("verify valid: %v", err)
	}
	wrong := "00000"
	if code == wrong {
		wrong = "11111"
	}
	if err := s.VerifyCode(ctx, phone, hash, wrong); !errors.Is(err, store.ErrCodeInvalid) {
		t.Errorf("want ErrCodeInvalid, got %v", err)
	}
	if err := s.VerifyCode(ctx, phone, "wronghash", code); !errors.Is(err, store.ErrCodeInvalid) {
		t.Errorf("wrong hash: want ErrCodeInvalid, got %v", err)
	}
}
