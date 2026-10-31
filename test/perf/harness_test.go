//go:build performance

package perf_test

import (
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/metrics"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/rsakey"
	"github.com/teagramhq/teagram-server/test/perf/perfreport"
)

// seqScanRows is the table size above which a sequential scan fails the run.
const seqScanRows = 10_000

// env is what a scenario addresses: the fixture's ids and two signed-in
// clients, the owner and one channel subscriber.
type env struct {
	f          fixture
	srv        *server
	owner      *client
	subscriber *client
	// logSQL, set by TG_PERF_SQL, prints the statements of each scenario's
	// counted sample: the first thing to read when a query count fails.
	logSQL func(scenario string, captured []capturedQuery)
	// elapsed, when a call sets it, replaces the call's wall time as its
	// latency.
	elapsed time.Duration
}

// scenario is one client-facing RPC as a client sends it. prepare runs before
// every sample, outside the measured window and before the server goes quiet;
// cleanup runs after it, likewise unmeasured.
type scenario struct {
	name    string
	kind    perfreport.Kind
	fullSet bool
	samples int
	note    string
	setup   func(ctx context.Context, e *env) (call func(ctx context.Context) error, note string, err error)
	prepare func(ctx context.Context, e *env) error
	cleanup func(ctx context.Context, e *env) error
}

// warmup samples fill the plan cache and the buffer pool before measuring.
const warmup = 2

// TestRPCPerformance measures every scenario on both fixtures against one
// in-process server per fixture and writes the report to TG_PERF_REPORT.
// perfreport judges it; this test fails only when it could not measure.
func TestRPCPerformance(t *testing.T) {
	dir := perfDir(t)
	out := os.Getenv("TG_PERF_REPORT")
	if out == "" {
		t.Skip("TG_PERF_REPORT is not set")
	}
	key, err := rsakey.Bootstrap(filepath.Join(t.TempDir(), "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	only := os.Getenv("TG_PERF_ONLY")
	rep := perfreport.Report{
		Version: perfreport.Version, FixtureVersion: os.Getenv("TG_PERF_FIXTURE_VERSION"), Filter: only,
	}
	for _, sc := range scenarios() {
		if only == "" || strings.Contains(sc.name, only) {
			rep.Scenarios = append(rep.Scenarios, sc.name)
		}
	}
	for _, s := range fixtures {
		dsn, ok := fixtureDSN(s)
		if !ok {
			t.Fatalf("TG_PERF_DSN_%s is not set", envName(s))
		}
		f, err := loadFixture(fixturePath(dir, s))
		if err != nil {
			t.Fatalf("fixture %s: %v (run TestSeedFixtures first)", s.Name, err)
		}
		rep.Results = append(rep.Results, measureFixture(t, t.Context(), dsn, blobDir(dir, s), key, f, only)...)
	}
	raw, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, raw, 0o600); err != nil { // #nosec G703 -- the path is the operator's TG_PERF_REPORT
		t.Fatal(err)
	}
}

func measureFixture(t *testing.T, ctx context.Context, dsn, blobs string, key *rsa.PrivateKey, f fixture, only string) []perfreport.Result {
	t.Helper()
	srv := bootServer(t, ctx, dsn, blobs, key)
	defer srv.stop()
	e := &env{f: f, srv: srv}
	if os.Getenv("TG_PERF_SQL") != "" {
		e.logSQL = func(scenario string, captured []capturedQuery) {
			counts := map[string]int{}
			var order []string
			for _, q := range captured {
				line := firstLine(statementBody(q.sql) + q.sql)
				if counts[line] == 0 {
					order = append(order, line)
				}
				counts[line]++
			}
			for _, line := range order {
				t.Logf("%s %s: %4d x %s", f.Spec.Name, scenario, counts[line], line)
			}
		}
	}
	e.owner = srv.login(t, ctx, f.Owner)
	defer e.owner.close()
	e.subscriber = srv.login(t, ctx, f.Subscriber)
	defer e.subscriber.close()
	explain, err := newExplainer(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := explain.close(ctx); err != nil {
			t.Errorf("close explain connection: %v", err)
		}
	}()

	var results []perfreport.Result
	for _, sc := range scenarios() {
		if only != "" && !strings.Contains(sc.name, only) {
			continue
		}
		r := measure(t, ctx, e, explain, sc)
		r.Fixture = f.Spec.Name
		t.Logf("%s %s: p50 %.2f ms, p95 %.2f ms, %d queries, %d rows read, %d B allocated %s",
			f.Spec.Name, sc.name, r.P50Ms, r.P95Ms, r.Queries, r.RowsRead, r.AllocBytes, r.Error)
		results = append(results, r)
	}
	return results
}

func measure(t *testing.T, ctx context.Context, e *env, explain *explainer, sc scenario) perfreport.Result {
	t.Helper()
	r := perfreport.Result{Name: sc.name, Kind: sc.kind, FullSet: sc.fullSet, Note: sc.note}
	fail := func(err error) perfreport.Result {
		r.Error = err.Error()
		return r
	}
	call, note, err := sc.setup(ctx, e)
	if err != nil {
		return fail(fmt.Errorf("setup: %w", err))
	}
	if note != "" {
		r.Note = strings.TrimSpace(r.Note + " " + note)
	}
	tracer := e.srv.tracer
	var latencies []float64
	var sqlTimes []float64
	var nonSQLTimes []float64
	statementTimes := map[string][]float64{}
	var queries, allocs []int64
	for i := range warmup + sc.samples {
		if sc.prepare != nil {
			if err := sc.prepare(ctx, e); err != nil {
				return fail(fmt.Errorf("prepare: %w", err))
			}
		}
		if err := tracer.awaitQuiet(ctx, 10*time.Second); err != nil {
			return fail(err)
		}
		measured := i >= warmup
		tracer.reset(i == warmup, sc.name == "contacts.getContacts")
		allocBefore := heapAllocs()
		e.elapsed = 0
		start := time.Now()
		if err := call(ctx); err != nil {
			return fail(err)
		}
		elapsed := time.Since(start)
		if e.elapsed > 0 {
			elapsed = e.elapsed
		}
		if err := tracer.awaitQuiet(ctx, 10*time.Second); err != nil {
			return fail(err)
		}
		allocated := heapAllocs() - allocBefore
		n, captured := tracer.window()
		statementDurations := make(map[string]time.Duration)
		for _, timing := range tracer.timingWindow() {
			statementDurations[timing.name] += timing.elapsed
		}
		if i == warmup {
			rows, scans, unexplained := explain.statements(ctx, captured)
			r.RowsRead, r.SeqScans, r.Unexplained = rows, scans, unexplained
			if e.logSQL != nil {
				e.logSQL(sc.name, captured)
			}
		}
		if sc.cleanup != nil {
			if err := sc.cleanup(ctx, e); err != nil {
				return fail(fmt.Errorf("cleanup: %w", err))
			}
		}
		if measured {
			totalMS := float64(elapsed.Microseconds()) / 1000
			var sqlElapsed time.Duration
			for name, duration := range statementDurations {
				sqlElapsed += duration
				statementTimes[name] = append(statementTimes[name], float64(duration.Microseconds())/1000)
			}
			latencies = append(latencies, totalMS)
			sqlTimes = append(sqlTimes, float64(sqlElapsed.Microseconds())/1000)
			nonSQLTimes = append(nonSQLTimes, totalMS-float64(sqlElapsed.Microseconds())/1000)
			queries = append(queries, int64(n))
			allocs = append(allocs, int64(allocated)) // #nosec G115 -- one RPC allocates far below MaxInt64
		}
	}
	slices.Sort(latencies)
	slices.Sort(queries)
	slices.Sort(allocs)
	r.Samples = len(latencies)
	r.P50Ms = percentile(latencies, 50)
	r.P95Ms = percentile(latencies, 95)
	r.Queries = int(queries[(len(queries)-1)/2])
	r.AllocBytes = allocs[(len(allocs)-1)/2]
	if sc.name == "contacts.getContacts" {
		slices.Sort(sqlTimes)
		slices.Sort(nonSQLTimes)
		t.Logf("%s %s server-time breakdown: total p50/p95 %.2f/%.2f ms; SQL p50/p95 %.2f/%.2f ms; non-SQL residual p50/p95 %.2f/%.2f ms",
			e.f.Spec.Name, sc.name, r.P50Ms, r.P95Ms,
			percentile(sqlTimes, 50), percentile(sqlTimes, 95),
			percentile(nonSQLTimes, 50), percentile(nonSQLTimes, 95))
		names := make([]string, 0, len(statementTimes))
		for name := range statementTimes {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			statementSamples := statementTimes[name]
			slices.Sort(statementSamples)
			t.Logf("%s %s SQL %s p50/p95: %.2f/%.2f ms", e.f.Spec.Name, sc.name, name,
				percentile(statementSamples, 50), percentile(statementSamples, 95))
		}
	}
	return r
}

// percentile is nearest-rank over sorted values.
func percentile(sorted []float64, p int) float64 {
	rank := (p*len(sorted) + 99) / 100
	return sorted[max(rank, 1)-1]
}

// heapAllocs is the process's cumulative heap allocation. The client shares
// the process, so a sample's delta includes decoding the reply; both fixtures
// and both branches carry the same client cost.
func heapAllocs() uint64 {
	s := []metrics.Sample{{Name: "/gc/heap/allocs:bytes"}}
	metrics.Read(s)
	return s[0].Value.Uint64()
}

// explainer plans each captured statement on its own connection, outside the
// server's pool and its tracer.
type explainer struct {
	conn  *pgx.Conn
	sizes map[string]float64
}

func newExplainer(ctx context.Context, dsn string) (*explainer, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, err
	}
	rows, err := conn.Query(ctx, `SELECT relname, reltuples FROM pg_class
		WHERE relkind IN ('r', 'p', 'm') AND relnamespace = 'public'::regnamespace`)
	if err != nil {
		return nil, err
	}
	sizes := map[string]float64{}
	for rows.Next() {
		var name string
		var tuples float32
		if err := rows.Scan(&name, &tuples); err != nil {
			return nil, err
		}
		sizes[name] = float64(tuples)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &explainer{conn: conn, sizes: sizes}, nil
}

func (x *explainer) close(ctx context.Context) error { return x.conn.Close(ctx) }

type planNode struct {
	Node     string     `json:"Node Type"`
	Relation string     `json:"Relation Name"`
	Rows     float64    `json:"Actual Rows"`
	Loops    float64    `json:"Actual Loops"`
	Removed  float64    `json:"Rows Removed by Filter"`
	Recheck  float64    `json:"Rows Removed by Index Recheck"`
	PlanRows float64    `json:"Plan Rows"`
	Plans    []planNode `json:"Plans"`
}

func (p planNode) walk(fn func(planNode)) {
	fn(p)
	for _, c := range p.Plans {
		c.walk(fn)
	}
}

// statements runs EXPLAIN ANALYZE on every captured statement inside a
// transaction it rolls back, so a write's re-execution leaves nothing behind.
// A statement that cannot run twice (a unique insert) is planned without
// ANALYZE instead, and its rows read are the planner's estimate.
func (x *explainer) statements(ctx context.Context, captured []capturedQuery) (rows int64, scans, unexplained []string) {
	for _, q := range captured {
		body := statementBody(q.sql)
		if body == "" {
			continue
		}
		plan, analyzed, err := x.plan(ctx, body, q.args)
		if err != nil {
			if line := firstLine(body); !slices.Contains(unexplained, line) {
				unexplained = append(unexplained, line)
			}
			continue
		}
		plan.walk(func(n planNode) {
			if n.Relation == "" {
				return
			}
			if analyzed {
				rows += int64((n.Rows + n.Removed + n.Recheck) * max(n.Loops, 1))
			} else {
				rows += int64(n.PlanRows)
			}
			if strings.Contains(n.Node, "Seq Scan") && x.sizes[n.Relation] > seqScanRows {
				scan := fmt.Sprintf("%s#%s (%.0f rows): %s", n.Relation, fingerprint(body), x.sizes[n.Relation], firstLine(body))
				if !slices.Contains(scans, scan) {
					scans = append(scans, scan)
				}
			}
		})
	}
	return rows, scans, unexplained
}

func (x *explainer) plan(ctx context.Context, sql string, args []any) (planNode, bool, error) {
	for _, analyze := range []bool{true, false} {
		option := "FORMAT JSON"
		if analyze {
			option = "ANALYZE, FORMAT JSON"
		}
		var raw []byte
		err := pgx.BeginFunc(ctx, x.conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = '10s'"); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, "EXPLAIN ("+option+") "+sql, args...).Scan(&raw); err != nil {
				return err
			}
			return errRollback
		})
		if !errors.Is(err, errRollback) {
			continue
		}
		var plans []struct {
			Plan planNode `json:"Plan"`
		}
		if err := json.Unmarshal(raw, &plans); err != nil || len(plans) == 0 {
			return planNode{}, false, fmt.Errorf("decode plan: %w", err)
		}
		return plans[0].Plan, analyze, nil
	}
	return planNode{}, false, fmt.Errorf("explain failed: %s", firstLine(sql))
}

var errRollback = errors.New("rollback")

// statementBody strips sqlc's leading name comment and returns the statement
// when EXPLAIN can plan it, or "" for transaction control, locks and NOTIFY.
func statementBody(sql string) string {
	lines := strings.Split(strings.TrimSpace(sql), "\n")
	for len(lines) > 0 && strings.HasPrefix(strings.TrimSpace(lines[0]), "--") {
		lines = lines[1:]
	}
	body := strings.TrimSpace(strings.Join(lines, "\n"))
	head := strings.ToUpper(strings.SplitN(body, " ", 2)[0])
	head = strings.TrimSpace(strings.SplitN(head, "\n", 2)[0])
	switch head {
	case "SELECT", "WITH", "INSERT", "UPDATE", "DELETE":
		if strings.Contains(strings.ToLower(body), "pg_advisory") || strings.Contains(strings.ToLower(body), "pg_notify") {
			return ""
		}
		return body
	}
	return ""
}

// fingerprint names a statement by its full text with whitespace collapsed, so
// a waiver can name one exact scan.
func fingerprint(sql string) string {
	sum := sha256.Sum256([]byte(strings.Join(strings.Fields(sql), " ")))
	return hex.EncodeToString(sum[:4])
}

func firstLine(sql string) string {
	line := strings.TrimSpace(strings.SplitN(strings.TrimSpace(sql), "\n", 2)[0])
	if len(line) > 120 {
		line = line[:120] + "…"
	}
	return line
}
