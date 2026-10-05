//go:build e2e_admin_server

// Command e2e_admin_server runs two admin HTTP instances against one fresh
// Postgres database for the Playwright E2E suite (test/e2e/admin.spec.ts).
//
// Usage: TG_ADMIN_E2E_TOKEN=... go run -tags e2e_admin_server ./test/e2e/
//
// The primary listens on 127.0.0.1:2444 and starts a second process on
// 127.0.0.1:2445. Each process has its own registry and fleet publisher.
// Both serve the admin router built by internal/admin.AdminRouter. The token hash is the SHA-256 of
// TG_ADMIN_E2E_TOKEN, matching what config.Load does with TG_ADMIN_TOKEN_HASH.
// A ready line "admin server ready" is printed to stdout when the listener is
// bound. The process exits on SIGINT/SIGTERM or when the DSN database is
// dropped out from under it.
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/teagramhq/teagram-server/internal/admin"
	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/fleet"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

const adminListenAddr = "127.0.0.1:2444"
const peerListenAddr = "127.0.0.1:2445"

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	token := os.Getenv("TG_ADMIN_E2E_TOKEN")
	if token == "" {
		return os.ErrInvalid
	}
	sum := sha256.Sum256([]byte(token))
	tokenHash := hex.EncodeToString(sum[:])

	dsn := os.Getenv("TG_ADMIN_E2E_SHARED_DSN")
	dropDB := func() {}
	if dsn == "" {
		var err error
		dsn, dropDB, err = pgtest.DSNFor(os.Stderr)
		if err != nil {
			return err
		}
	}
	defer dropDB()
	listenAddr := os.Getenv("TG_ADMIN_E2E_LISTEN_ADDR")
	if listenAddr == "" {
		listenAddr = adminListenAddr
	}
	secondary := os.Getenv("TG_ADMIN_E2E_SECONDARY") == "1"

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	blobs, err := blob.NewLocal(os.TempDir() + "/tg-admin-e2e-blobs")
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, dsn, make([]byte, 32), store.WithBlobStore(blobs))
	if err != nil {
		return err
	}
	defer func() {
		if cerr := st.Close(); cerr != nil {
			log.Error("store close", "err", cerr)
		}
	}()

	var peer *peerProcess
	if !secondary {
		peer, err = startPeerProcess(ctx, dsn, token)
		if err != nil {
			return fmt.Errorf("start second admin replica: %w", err)
		}
		defer func() {
			if err := peer.stop(false); err != nil {
				log.Error("stop second admin replica", "err", err)
			}
		}()
	}

	// Background sweep, same cadence as the real server: the admin_sessions
	// table is bounded by this sweep, and the login flow is unaffected by it.
	sweepCtx, cancelSweep := context.WithCancel(ctx)
	defer cancelSweep()
	var sweepWG sync.WaitGroup
	sweepWG.Go(func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-sweepCtx.Done():
				return
			case <-ticker.C:
				if _, err := st.SweepExpiredAdminSessions(sweepCtx); err != nil {
					log.Error("sweep admin sessions", "err", err)
				}
			}
		}
	})
	defer sweepWG.Wait()

	registry := mtproto.NewSessionRegistry()
	deliveryLag := admin.NewDeliveryLagSampler()
	notifyMetrics := store.NewNotificationMetrics()
	processIdentity, err := admin.NewProcessIdentity(os.Getenv("TG_REPLICA_ID"))
	if err != nil {
		return err
	}
	metricsCache := admin.NewMetricsSnapshotCache(registry, st, processIdentity, deliveryLag, notifyMetrics)
	stopFleetPublisher := fleet.NewPublisher(
		st, registry, processIdentity.Generation, processIdentity.ReplicaID, fleet.BuildVersion(),
	).Start(ctx)
	defer func() {
		if err := stopFleetPublisher(); err != nil {
			log.Error("fleet telemetry shutdown", "err", err)
		}
	}()

	events := admin.NewBroadcaster(admin.BroadcasterConfig{
		Sample: metricsCache.Snapshot,
		Logger: log,
		Render: admin.DashboardFragmentRenderer,
	})
	eventsCtx, stopEvents := context.WithCancel(ctx)
	var eventsWG sync.WaitGroup
	eventsWG.Go(func() { events.Run(eventsCtx) })
	defer func() {
		stopEvents()
		eventsWG.Wait()
	}()

	router := admin.AdminRouter(admin.LoginHandlerConfig{
		Store:         st,
		TokenHash:     tokenHash,
		Logger:        log,
		AdminOrigin:   "http://" + listenAddr,
		Events:        events,
		DeliveryLag:   deliveryLag,
		Metrics:       metricsCache,
		NotifyMetrics: notifyMetrics,
	}, registry)

	// The browser suite uses these authenticated, server-rendered snapshots to
	// exercise dashboard states that the empty e2e database cannot produce on demand. This route exists only in the
	// e2e_admin_server binary; it is never registered by the production router.
	fixtureMux := http.NewServeMux()
	fixtureMux.Handle("/admin/e2e/dashboard-fixture", admin.RequireAdmin(admin.AdminMiddlewareConfig{
		Store:     st,
		TokenHash: tokenHash,
	})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m, ok := dashboardFixture(r.URL.Query().Get("kind"), processIdentity)
		if !ok {
			http.Error(w, "unknown dashboard fixture", http.StatusNotFound)
			return
		}
		fragments, err := admin.DashboardFragmentRenderer(m)
		if err != nil || len(fragments) != 1 {
			http.Error(w, "dashboard fixture unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(fragments[0].HTML))
	})))
	connections := newFleetConnectionControl(registry)
	fixtureMux.Handle("/admin/e2e/fleet/connections", admin.RequireAdmin(admin.AdminMiddlewareConfig{
		Store:     st,
		TokenHash: tokenHash,
	})(http.HandlerFunc(connections.handle)))
	if peer != nil {
		fixtureMux.Handle("/admin/e2e/fleet/stop-peer", admin.RequireAdmin(admin.AdminMiddlewareConfig{
			Store:     st,
			TokenHash: tokenHash,
		})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			if err := peer.stop(true); err != nil {
				http.Error(w, "could not stop second replica", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		})))
	}
	fixtureMux.Handle("/", router)

	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           admin.SecurityHeaders(fixtureMux),
		ReadHeaderTimeout: 5 * time.Second,
		MaxHeaderBytes:    8192,
	}
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return err
	}
	log.Info("admin server listening", "addr", listenAddr)
	// The Playwright fixture waits for this exact line on stdout.
	if _, err := os.Stdout.WriteString("admin server ready\n"); err != nil {
		return err
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve(ln)
	}()
	select {
	case <-ctx.Done():
		// Close the SSE streams before Shutdown, which otherwise waits on them
		// as in-flight requests.
		stopEvents()
		eventsWG.Wait()

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

type fleetConnection struct {
	accountID int64
	conn      *mtproto.Conn
}

type fleetConnectionControl struct {
	mu       sync.Mutex
	registry *mtproto.SessionRegistry
	current  []fleetConnection
}

func newFleetConnectionControl(registry *mtproto.SessionRegistry) *fleetConnectionControl {
	return &fleetConnectionControl{registry: registry}
}

func (c *fleetConnectionControl) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var request struct {
		AccountIDs []int64 `json:"account_ids"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	if err := decoder.Decode(&request); err != nil || len(request.AccountIDs) > 256 {
		http.Error(w, "invalid connection sample", http.StatusBadRequest)
		return
	}
	perAccount := make(map[int64]int, len(request.AccountIDs))
	for _, accountID := range request.AccountIDs {
		perAccount[accountID]++
		if accountID <= 0 || perAccount[accountID] > mtproto.MaxUserConns {
			http.Error(w, "invalid connection sample", http.StatusBadRequest)
			return
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	for _, connection := range c.current {
		c.registry.Remove(connection.accountID, connection.conn)
	}
	c.current = make([]fleetConnection, 0, len(request.AccountIDs))
	for _, accountID := range request.AccountIDs {
		connection := fleetConnection{accountID: accountID, conn: &mtproto.Conn{}}
		if !c.registry.Add(accountID, connection.conn) {
			http.Error(w, "connection sample exceeds registry capacity", http.StatusBadRequest)
			return
		}
		c.current = append(c.current, connection)
	}
	w.WriteHeader(http.StatusNoContent)
}

type peerProcess struct {
	command      *exec.Cmd
	done         chan struct{}
	mu           sync.Mutex
	waitErr      error
	forceStopped bool
}

func startPeerProcess(ctx context.Context, dsn, token string) (*peerProcess, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("find e2e server executable: %w", err)
	}
	command := exec.Command(executable)
	command.Env = environmentWith(map[string]string{
		"TG_ADMIN_E2E_TOKEN":       token,
		"TG_ADMIN_E2E_SHARED_DSN":  dsn,
		"TG_ADMIN_E2E_LISTEN_ADDR": peerListenAddr,
		"TG_ADMIN_E2E_SECONDARY":   "1",
		"TG_REPLICA_ID":            "edge-2",
	})
	command.Stderr = os.Stderr
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("capture second replica readiness: %w", err)
	}
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("launch second replica: %w", err)
	}
	peer := &peerProcess{command: command, done: make(chan struct{})}
	go func() {
		err := command.Wait()
		peer.mu.Lock()
		peer.waitErr = err
		close(peer.done)
		peer.mu.Unlock()
	}()
	ready := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(stdout).ReadString('\n')
		if err == nil && line != "admin server ready\n" {
			err = fmt.Errorf("unexpected readiness line %q", line)
		}
		ready <- err
	}()
	startupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	select {
	case err := <-ready:
		if err != nil {
			_ = peer.stop(true)
			return nil, fmt.Errorf("wait for second replica: %w", err)
		}
	case <-peer.done:
		return nil, fmt.Errorf("second replica exited before readiness: %w", peer.waitErr)
	case <-startupCtx.Done():
		_ = peer.stop(true)
		return nil, fmt.Errorf("second replica did not become ready: %w", startupCtx.Err())
	}
	return peer, nil
}

func (p *peerProcess) stop(force bool) error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	select {
	case <-p.done:
		err := p.waitErr
		forced := p.forceStopped
		p.mu.Unlock()
		if force || forced {
			return nil
		}
		return err
	default:
	}
	var signalErr error
	if force {
		signalErr = p.command.Process.Kill()
		p.forceStopped = true
	} else {
		signalErr = p.command.Process.Signal(syscall.SIGTERM)
	}
	p.mu.Unlock()
	if signalErr != nil && !errors.Is(signalErr, os.ErrProcessDone) {
		return signalErr
	}
	select {
	case <-p.done:
		p.mu.Lock()
		err := p.waitErr
		forced := p.forceStopped
		p.mu.Unlock()
		if force || forced {
			return nil
		}
		return err
	case <-time.After(5 * time.Second):
		if err := p.command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return err
		}
		<-p.done
		if force {
			return nil
		}
		return errors.New("second replica did not stop after SIGTERM")
	}
}

func environmentWith(overrides map[string]string) []string {
	env := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if _, replaced := overrides[key]; !replaced {
			env = append(env, entry)
		}
	}
	for key, value := range overrides {
		env = append(env, key+"="+value)
	}
	return env
}

func dashboardFixture(kind string, processIdentity admin.ProcessIdentity) (admin.MetricsResponse, bool) {
	sampledAt := time.Date(2026, 9, 15, 2, 0, 0, 0, time.UTC)
	m := admin.MetricsResponse{
		Timestamp:         sampledAt,
		SampleState:       admin.SampleStateAvailable,
		SampleAgeSeconds:  2,
		ProcessStartedAt:  processIdentity.StartedAt,
		ProcessGeneration: processIdentity.Generation,
		ReplicaID:         processIdentity.ReplicaID,
	}
	firstID, secondID := "edge-1", "edge-2"
	firstReplicaID := &firstID
	if processIdentity.ReplicaID != nil && *processIdentity.ReplicaID != "" {
		firstID = *processIdentity.ReplicaID
	} else {
		firstReplicaID = nil
	}
	secondGeneration := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	firstVersion, secondVersion := "v1.2.3", "v1.2.4"
	firstAccounts, secondAccounts, fleetAccounts := int64(2), int64(2), int64(3)
	firstReplica := admin.FleetReplica{
		ProcessGeneration: processIdentity.Generation,
		ReplicaID:         firstReplicaID,
		Version:           &firstVersion,
		ProcessStartedAt:  sampledAt.Add(-time.Hour),
		HeartbeatAt:       sampledAt.Add(-9 * time.Second),
		Connections:       2,
		Sessions:          2,
		DistinctAccounts:  &firstAccounts,
	}
	secondReplica := admin.FleetReplica{
		ProcessGeneration: secondGeneration,
		ReplicaID:         &secondID,
		Version:           &secondVersion,
		ProcessStartedAt:  sampledAt.Add(-30 * time.Minute),
		HeartbeatAt:       sampledAt.Add(-time.Second),
		Connections:       3,
		Sessions:          2,
		DistinctAccounts:  &secondAccounts,
	}

	switch kind {
	case "fleet-heartbeat-missing":
		// Model a fresh local metrics sample after this process's fleet heartbeat
		// expired or failed, leaving the serving generation out of the fleet.
	case "partial-delivery":
		m.DeliveryLag = admin.DeliveryLag{
			State:               admin.DeliveryLagAvailable,
			Coverage:            admin.DeliveryLagCoveragePartial,
			EligibleConnections: 2,
			SampledConnections:  1,
			SampledAt:           &sampledAt,
		}
	case "stale-delivery":
		worstPts := int64(4)
		m.SampleState = admin.SampleStateStale
		m.SampleAgeSeconds = 45
		m.DeliveryLag = admin.DeliveryLag{
			WorstPts:            &worstPts,
			State:               admin.DeliveryLagStale,
			Coverage:            admin.DeliveryLagCoveragePartial,
			EligibleConnections: 2,
			SampledConnections:  1,
			SampledAt:           &sampledAt,
		}
	case "no-connections":
		worstPts := int64(0)
		m.DeliveryLag = admin.DeliveryLag{
			WorstPts:            &worstPts,
			State:               admin.DeliveryLagAvailable,
			Coverage:            admin.DeliveryLagCoverageFull,
			EligibleConnections: 0,
			SampledConnections:  0,
			SampledAt:           &sampledAt,
		}
	case "zero-window":
		m.PushLatencyP50 = 50
		m.PushLatencyP95 = 95
		m.PushLatencySampleCount = 2
		m.PushWindowSeconds = 0
		m.PushOutcomes = admin.PushOutcomes{
			OwnerMismatch: 2,
			EncodeFailure: 1,
			WriteFailure:  3,
		}
		m.NotifyWindowSeconds = 0
		m.RateLimitDenialsWindowSeconds = 0
	case "missing-field":
		// Leave DeliveryLag at its zero value to model a response where the
		// optional delivery object is absent. The renderer must retain its
		// fixed unavailable copy and discard unknown capability names.
		m.Uninstrumented = []string{"unknown_metric"}
	case "absent-capability":
		m.Uninstrumented = []string{"push_latency_p50_ms", "unknown_metric"}
	case "fleet-acceptance":
		m.FleetSampledAt = sampledAt
		m.FleetConnections = 5
		m.FleetDistinctAccounts = &fleetAccounts
		m.FleetReplicas = []admin.FleetReplica{firstReplica, secondReplica}
	case "fleet-after-expiry":
		m.FleetSampledAt = sampledAt
		m.FleetConnections = 2
		m.FleetDistinctAccounts = &firstAccounts
		m.FleetReplicas = []admin.FleetReplica{firstReplica}
	case "fleet-failures":
		collisionID := "edge-1"
		firstReplica.DuplicateReplicaID = true
		firstReplica.ReplicaID = &collisionID
		secondReplica.ReplicaID = &collisionID
		secondReplica.DuplicateReplicaID = true
		secondReplica.Sessions = 100_001
		secondReplica.DistinctAccounts = nil
		m.ReplicaID = &collisionID
		m.SampleState = admin.SampleStateStale
		m.SampleAgeSeconds = 45
		m.FleetSampledAt = sampledAt
		m.FleetConnections = 5
		m.FleetReplicas = []admin.FleetReplica{firstReplica, secondReplica}
	case "fleet-restarted":
		m.ProcessGeneration = secondGeneration
		m.ProcessStartedAt = sampledAt.Add(-10 * time.Second)
		m.FleetSampledAt = sampledAt
		m.FleetConnections = 2
		m.FleetDistinctAccounts = &firstAccounts
		firstReplica.ProcessGeneration = secondGeneration
		firstReplica.ProcessStartedAt = sampledAt.Add(-10 * time.Second)
		firstReplica.HeartbeatAt = sampledAt
		m.FleetReplicas = []admin.FleetReplica{firstReplica}
	default:
		return admin.MetricsResponse{}, false
	}
	return m, true
}
