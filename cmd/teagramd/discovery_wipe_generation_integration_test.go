package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/exchange"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/discovery"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestDemoWipeGenerationSurvivesRestartAndRotatesAfterFreshMigration(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	databaseName := "t_demo_wipe_" + pgtest.RandomHex()
	dsn := createDemoWipeTestDatabase(t, ctx, databaseName)
	applyDemoWipeTestMigrations(t, ctx, dsn)

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA identity: %v", err)
	}

	var currentStore *store.Store
	t.Cleanup(func() {
		if currentStore != nil {
			if err := currentStore.Close(); err != nil {
				t.Errorf("close store: %v", err)
			}
		}
	})
	openStore := func() *store.Store {
		t.Helper()
		st, err := store.Open(ctx, dsn, pgtest.EncKey(),
			store.WithLogger(slog.New(slog.DiscardHandler)),
			store.WithoutBlobStore(),
		)
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
		currentStore = st
		return st
	}

	st := openStore()
	before := fetchDemoWipeDiscovery(t, ctx, st, key)
	firstGeneration := before.Demo.WipeGeneration

	if err := st.Close(); err != nil {
		t.Fatalf("close store before restart: %v", err)
	}
	currentStore = nil
	st = openStore()
	afterRestart := fetchDemoWipeDiscovery(t, ctx, st, key)
	if got := afterRestart.Demo.WipeGeneration; got != firstGeneration {
		t.Fatalf("restart changed wipe generation from %q to %q", firstGeneration, got)
	}

	if err := st.Close(); err != nil {
		t.Fatalf("close store before database wipe: %v", err)
	}
	currentStore = nil
	dropAndRecreateDemoWipeTestDatabase(t, ctx, databaseName)
	applyDemoWipeTestMigrations(t, ctx, dsn)

	st = openStore()
	afterMigrationGeneration, err := st.DemoWipeGeneration(ctx)
	if err != nil {
		t.Fatalf("read generation after fresh migration: %v", err)
	}
	if afterMigrationGeneration == firstGeneration {
		t.Fatalf("fresh migration reused wipe generation %q", firstGeneration)
	}
	if _, err := st.CreateUser(ctx, "+15551234567"); err != nil {
		t.Fatalf("seed account after database wipe: %v", err)
	}
	afterSeedGeneration, err := st.DemoWipeGeneration(ctx)
	if err != nil {
		t.Fatalf("read generation after reseeding: %v", err)
	}
	if afterSeedGeneration != afterMigrationGeneration {
		t.Fatalf("reseeding changed wipe generation from %q to %q", afterMigrationGeneration, afterSeedGeneration)
	}
	afterWipe := fetchDemoWipeDiscovery(t, ctx, st, key)
	if got := afterWipe.Demo.WipeGeneration; got != afterMigrationGeneration {
		t.Fatalf("discovery after wipe = %q, want fresh migration generation %q", got, afterMigrationGeneration)
	}
}

func createDemoWipeTestDatabase(t *testing.T, ctx context.Context, name string) string {
	t.Helper()

	conn, err := pgx.Connect(ctx, pgtest.AdminDSN())
	if err != nil {
		t.Fatalf("connect to Postgres admin database: %v", err)
	}
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		closeDemoWipeTestConn(t, ctx, conn)
		t.Fatalf("create test database: %v", err)
	}
	if err := conn.Close(ctx); err != nil {
		t.Fatalf("close Postgres admin connection: %v", err)
	}

	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		admin, err := pgx.Connect(cleanupCtx, pgtest.AdminDSN())
		if err != nil {
			t.Errorf("connect to Postgres for test database cleanup: %v", err)
			return
		}
		if _, err := admin.Exec(cleanupCtx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Errorf("drop test database: %v", err)
		}
		if err := admin.Close(cleanupCtx); err != nil {
			t.Errorf("close Postgres cleanup connection: %v", err)
		}
	})
	return pgtest.DSNFrom(name)
}

func dropAndRecreateDemoWipeTestDatabase(t *testing.T, ctx context.Context, name string) {
	t.Helper()

	conn, err := pgx.Connect(ctx, pgtest.AdminDSN())
	if err != nil {
		t.Fatalf("connect to Postgres admin database for wipe: %v", err)
	}
	identifier := pgx.Identifier{name}.Sanitize()
	if _, err := conn.Exec(ctx, "DROP DATABASE "+identifier+" WITH (FORCE)"); err != nil {
		closeDemoWipeTestConn(t, ctx, conn)
		t.Fatalf("drop test database: %v", err)
	}
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+identifier); err != nil {
		closeDemoWipeTestConn(t, ctx, conn)
		t.Fatalf("recreate test database: %v", err)
	}
	if err := conn.Close(ctx); err != nil {
		t.Fatalf("close Postgres admin connection after wipe: %v", err)
	}
}

func applyDemoWipeTestMigrations(t *testing.T, ctx context.Context, dsn string) {
	t.Helper()

	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve integration test source path")
	}
	migrationsDir := filepath.Join(filepath.Dir(sourceFile), "..", "..", "migrations")
	entries, err := os.ReadDir(migrationsDir)
	if err != nil {
		t.Fatalf("read migrations directory: %v", err)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to test database for migrations: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".sql" {
			continue
		}
		migrationPath := filepath.Join(migrationsDir, entry.Name())
		migration, err := os.ReadFile(migrationPath)
		if err != nil {
			closeDemoWipeTestConn(t, ctx, conn)
			t.Fatalf("read migration %s: %v", entry.Name(), err)
		}
		if _, err := conn.Exec(ctx, string(migration)); err != nil {
			closeDemoWipeTestConn(t, ctx, conn)
			t.Fatalf("apply migration %s: %v", entry.Name(), err)
		}
	}
	if err := conn.Close(ctx); err != nil {
		t.Fatalf("close test database migration connection: %v", err)
	}
}

func closeDemoWipeTestConn(t *testing.T, ctx context.Context, conn *pgx.Conn) {
	t.Helper()
	if err := conn.Close(ctx); err != nil {
		t.Errorf("close test database connection: %v", err)
	}
}

func fetchDemoWipeDiscovery(t *testing.T, ctx context.Context, st *store.Store, key *rsa.PrivateKey) discovery.Document {
	t.Helper()

	generation, err := st.DemoWipeGeneration(ctx)
	if err != nil {
		t.Fatalf("read stored wipe generation: %v", err)
	}
	cfg := config.Config{
		AdvertiseHost:             "demo.example.test",
		AdvertisePort:             443,
		DCID:                      2,
		AdvertiseDemoGuestSignUp:  true,
		PublicWebSocketListenAddr: "127.0.0.1:0",
	}
	_, publicDocument, err := listenerDiscoveryDocuments(cfg, &key.PublicKey, generation)
	if err != nil {
		t.Fatalf("build public discovery document: %v", err)
	}
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for discovery GET: %v", err)
	}
	server := mtproto.New(
		exchange.PrivateKey{RSA: key},
		cfg.DCID,
		nil,
		mtproto.HandlerFunc(func(*mtproto.Conn, *mtproto.Request) error { return nil }),
		slog.New(slog.DiscardHandler),
	)
	serveCtx, cancel := context.WithCancel(ctx)
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- server.ServeWebSocketWithHandlerAndDiscovery(
			serveCtx,
			listener,
			mtproto.HandlerFunc(func(*mtproto.Conn, *mtproto.Request) error { return nil }),
			nil,
			publicDocument,
		)
	}()
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			cancel()
			select {
			case err := <-serveErr:
				if err != nil {
					t.Errorf("stop discovery listener: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Error("discovery listener did not stop")
			}
		})
	}
	defer stop()

	requestCtx, requestCancel := context.WithTimeout(ctx, 5*time.Second)
	defer requestCancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet,
		"http://"+listener.Addr().String()+"/.well-known/teagramd/client", nil)
	if err != nil {
		t.Fatalf("build discovery GET: %v", err)
	}
	client := &http.Client{
		Transport: &http.Transport{Proxy: nil},
		Timeout:   5 * time.Second,
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("GET public discovery: %v", err)
	}
	body, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	client.CloseIdleConnections()
	if readErr != nil {
		t.Fatalf("read public discovery response: %v", readErr)
	}
	if closeErr != nil {
		t.Fatalf("close public discovery response: %v", closeErr)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("public discovery status = %d, want %d: %s", response.StatusCode, http.StatusOK, body)
	}
	var document discovery.Document
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatalf("decode public discovery response: %v", err)
	}
	if document.Demo == nil || document.Demo.WipeGeneration != generation {
		t.Fatalf("public discovery demo = %+v, want wipe generation %q", document.Demo, generation)
	}
	return document
}
