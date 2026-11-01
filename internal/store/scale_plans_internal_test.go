package store

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store/db"
)

// Capture SQL from the real sqlc methods and FleetSnapshot, including their
// parameters. A query change therefore changes the plan checked by this test.
type scaleQueries struct {
	mu      sync.Mutex
	queries []pgx.TraceQueryStartData
}

func (s *scaleQueries) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.Contains(strings.ToUpper(data.SQL), "SELECT") {
		s.queries = append(s.queries, data)
	}
	return ctx
}

func (*scaleQueries) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

type scalePlan struct {
	Node                  string      `json:"Node Type"`
	Relation              string      `json:"Relation Name"`
	SortKey               []string    `json:"Sort Key"`
	Cost                  float64     `json:"Total Cost"`
	Rows                  float64     `json:"Actual Rows"`
	Loops                 float64     `json:"Actual Loops"`
	RemovedByFilter       float64     `json:"Rows Removed by Filter"`
	RemovedByIndexRecheck float64     `json:"Rows Removed by Index Recheck"`
	Hits                  int64       `json:"Shared Hit Blocks"`
	Reads                 int64       `json:"Shared Read Blocks"`
	Plans                 []scalePlan `json:"Plans"`
}

func (p scalePlan) sequentialTables() []string {
	var tables []string
	if strings.Contains(p.Node, "Seq Scan") {
		tables = append(tables, p.Relation)
	}
	for _, child := range p.Plans {
		tables = append(tables, child.sequentialTables()...)
	}
	return tables
}

func (p scalePlan) containsNode(node string) bool {
	if p.Node == node {
		return true
	}
	for _, child := range p.Plans {
		if child.containsNode(node) {
			return true
		}
	}
	return false
}

func (p scalePlan) sortsOn(key string) bool {
	if slices.Contains(p.SortKey, key) {
		return true
	}
	for _, child := range p.Plans {
		if child.sortsOn(key) {
			return true
		}
	}
	return false
}

func (p scalePlan) rowsReadFrom(table string) float64 {
	var rows float64
	if p.Relation == table {
		rows += (p.Rows + p.RemovedByFilter + p.RemovedByIndexRecheck) * p.Loops
	}
	for _, child := range p.Plans {
		rows += child.rowsReadFrom(table)
	}
	return rows
}

func TestScalePlanRowsReadIncludesRemovedRowsAndLoops(t *testing.T) {
	var plan scalePlan
	if err := json.Unmarshal([]byte(`{"Relation Name":"dialogs","Actual Rows":2,"Actual Loops":3,"Rows Removed by Filter":5,"Rows Removed by Index Recheck":7}`), &plan); err != nil {
		t.Fatal(err)
	}

	if got, want := plan.rowsReadFrom("dialogs"), float64(42); got != want {
		t.Fatalf("rows read = %.0f, want %.0f", got, want)
	}
}

func TestContactsWithUsersAvoidsWindowAggAndBoundsReverseEdgeReads(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	dsn := pgtest.DSN(t)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for _, sql := range []string{
		`UPDATE server_administration SET election_closed = true WHERE singleton_id = 1`,
		`INSERT INTO users (id, phone, first_name)
		 SELECT n, '+contact-mutual-scale' || n, 'Scale ' || n FROM generate_series(1, 50099) n`,
		`INSERT INTO user_contacts (owner_id, contact_id)
		 SELECT 1, n FROM generate_series(2, 10001) n`,
		`INSERT INTO user_contacts (owner_id, contact_id) VALUES (2, 1)`,
		`INSERT INTO user_contacts (owner_id, contact_id)
		 SELECT n, 1 FROM generate_series(10002, 50099) n`,
		`ANALYZE user_contacts`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("seed contacts mutual-edge plan: %v", err)
		}
	}

	recorded := &scaleQueries{}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Tracer = recorded
	traced, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer traced.Close()
	rows, err := db.New(traced).ContactsWithUsers(ctx, 1)
	if err != nil {
		t.Fatalf("list contacts with users: %v", err)
	}
	if len(rows) != 10000 {
		t.Fatalf("contacts with users = %d rows, want 10000", len(rows))
	}
	if rows[0].Total != 10000 {
		t.Fatalf("contacts with users total = %d, want 10000", rows[0].Total)
	}
	for _, row := range rows {
		wantMutual := row.ContactID == 2
		if row.Mutual != wantMutual {
			t.Errorf("contact %d mutual = %t, want %t", row.ContactID, row.Mutual, wantMutual)
		}
	}

	recorded.mu.Lock()
	queries := append([]pgx.TraceQueryStartData(nil), recorded.queries...)
	recorded.mu.Unlock()
	if len(queries) != 1 {
		t.Fatalf("contacts with users query count = %d, want 1", len(queries))
	}
	var raw []byte
	_, query, ok := strings.Cut(queries[0].SQL, "\n")
	if !ok {
		t.Fatalf("contacts query is missing its sqlc header: %s", queries[0].SQL)
	}
	if err := pool.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+query, queries[0].Args...).Scan(&raw); err != nil {
		t.Fatalf("explain contacts with users: %v", err)
	}
	var plans []struct {
		Plan scalePlan `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &plans); err != nil {
		t.Fatal(err)
	}
	if plans[0].Plan.containsNode("WindowAgg") {
		t.Fatal("contacts query uses a WindowAgg to attach the same total to every contact")
	}
	if plans[0].Plan.sortsOn("owned_contacts.contact_id") {
		t.Fatal("contacts query sorts the owned contact IDs before the mutual-edge index probe")
	}
	// The full list, count, reverse probes, and photo filter may each read a
	// contact row, but inbound fan-in must not add to that bound.
	if rowsRead := plans[0].Plan.rowsReadFrom("user_contacts"); rowsRead > 4*float64(len(rows)) {
		t.Fatalf("contacts query read %.0f user_contacts rows for %d owned contacts despite 40,098 inbound edges", rowsRead, len(rows))
	}
}

func (p scalePlan) recordRelations(seen map[string]bool) {
	if p.Relation != "" {
		seen[p.Relation] = true
	}
	for _, child := range p.Plans {
		child.recordRelations(seen)
	}
}

func TestHotQueryPlansAtScale(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	dsn := pgtest.DSN(t)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	// This fixture has no history TTL settings and checks read plans, so skip
	// row-wise TTL stamping while bulk-seeding message rows.
	// Different owners/channels interleave on disk, as independent traffic
	// does. Expired fleet generations dominate retained telemetry; four are live.
	for _, sql := range []string{
		`UPDATE server_administration SET election_closed = true WHERE singleton_id = 1`,
		`INSERT INTO users (id, phone, first_name)
		 SELECT n, '+scale' || n, 'Scale ' || n FROM generate_series(1, 10000) n`,
		`INSERT INTO usernames (handle, owner_type, owner_id)
		 SELECT 'scale' || n, 'user', n FROM generate_series(1, 10000) n`,
		`INSERT INTO auth_keys (id, user_id, key_value, ever_bound_at)
		 SELECT n, n, decode(repeat('ab', 32), 'hex'), now() FROM generate_series(1, 10000) n`,
		`INSERT INTO update_state (user_id, pts, next_local_id)
		 SELECT n, 100, 101 FROM generate_series(1, 10000) n`,
		`ALTER TABLE messages DISABLE TRIGGER messages_stamp_history_ttl`,
		`INSERT INTO messages (owner_id, local_id, peer_type, peer_id, from_id, message, out)
		 SELECT u, m, 1, (u + m % 10) % 10000 + 1, u, 'scale message', true
		 FROM generate_series(1, 100) m CROSS JOIN generate_series(1, 10000) u`,
		`ALTER TABLE messages ENABLE TRIGGER messages_stamp_history_ttl`,
		`INSERT INTO message_events (owner_id, pts, type, local_id)
		 SELECT u, m, 1, m FROM generate_series(1, 100) m CROSS JOIN generate_series(1, 10000) u`,
		`INSERT INTO dialogs (owner_id, peer_type, peer_id, top_message)
		 SELECT u, 1, (u + p) % 10000 + 1, 90 + p
		 FROM generate_series(0, 9) p CROSS JOIN generate_series(1, 10000) u`,
		`INSERT INTO channels (id, title, creator_id)
		 SELECT n, 'Scale channel', 1 FROM generate_series(1, 10000) n`,
		`INSERT INTO channel_state (channel_id, pts, next_local_id)
		 SELECT n, CASE WHEN n <= 100 THEN 100 ELSE 0 END,
		 CASE WHEN n <= 100 THEN 101 ELSE 1 END FROM generate_series(1, 10000) n`,
		`INSERT INTO channel_participants (channel_id, user_id)
		 SELECT (u + p) % 10000 + 1, u
		 FROM generate_series(0, 9) p CROSS JOIN generate_series(1, 10000) u`,
		`ALTER TABLE channel_messages DISABLE TRIGGER channel_messages_stamp_history_ttl`,
		`INSERT INTO channel_messages (channel_id, local_id, from_id, message)
		 SELECT c, m, 1, 'scale channel post'
		 FROM generate_series(1, 100) m CROSS JOIN generate_series(1, 100) c`,
		`ALTER TABLE channel_messages ENABLE TRIGGER channel_messages_stamp_history_ttl`,
		`INSERT INTO channel_events (channel_id, pts, type, local_id)
		 SELECT c, m, 1, m FROM generate_series(1, 100) m CROSS JOIN generate_series(1, 100) c`,
		`INSERT INTO fleet_process_snapshots
		 (generation, replica_id, process_started_at, heartbeat_at, expires_at, connections, sessions, accounts_complete)
		 SELECT lpad(to_hex(n), 32, '0'), 'replica-' || n, now() - interval '1 hour',
		 now() - interval '10 seconds',
		 CASE WHEN n <= 4 THEN now() + interval '1 hour' ELSE now() - interval '1 second' END,
		 10, 10, true FROM generate_series(1, 10000) n`,
		`INSERT INTO fleet_live_accounts (generation, user_id)
		 SELECT lpad(to_hex(n), 32, '0'), u
		 FROM generate_series(1, 10000) n CROSS JOIN generate_series(1, 10) u`,
		`ANALYZE`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("seed scale data: %v", err)
		}
	}
	for _, volume := range []struct {
		sql  string
		want int64
	}{
		{"SELECT count(*) FROM users", 10000},
		{"SELECT count(*) FROM messages", 1000000},
		{"SELECT count(*) FROM message_events", 1000000},
		{"SELECT count(*) FROM dialogs", 100000},
	} {
		var got int64
		if err := pool.QueryRow(ctx, volume.sql).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got < volume.want {
			t.Fatalf("%s = %d, want at least %d", volume.sql, got, volume.want)
		}
	}

	recorded := &scaleQueries{}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Tracer = recorded
	traced, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer traced.Close()
	q := db.New(traced)
	scaleUserIDs := make([]int64, 100)
	for i := range scaleUserIDs {
		scaleUserIDs[i] = int64(i + 1)
	}
	// Both initial and cursor pages, and both pinned-dialog modes, must keep
	// their indexed access. No enable_seqscan override masks planner decisions.
	for _, offset := range []int64{0, 95} {
		if _, err := q.DialogsForOwner(ctx, db.DialogsForOwnerParams{OwnerID: 42, OffsetID: offset, Lim: 5}); err != nil {
			t.Fatal(err)
		}
		for _, exclude := range []bool{false, true} {
			if offset == 0 {
				if _, err := q.DialogsFirstPageWithPins(ctx, db.DialogsFirstPageWithPinsParams{
					OwnerID: 42, ExcludePinned: exclude, Lim: 5,
				}); err != nil {
					t.Fatal(err)
				}
				continue
			}
			if _, err := q.DialogsCursorPageWithPins(ctx, db.DialogsCursorPageWithPinsParams{
				OwnerID: 42, OffsetID: offset, ExcludePinned: exclude, Lim: 5,
			}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := q.HistoryPage(ctx, db.HistoryPageParams{
			OwnerID: 42, PeerType: 1, PeerID: 43, OffsetID: offset, Lim: 5,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := q.ChannelHistoryPage(ctx, db.ChannelHistoryPageParams{
			ChannelID: 42, OffsetID: offset, Lim: 5,
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, err := range []error{
		scaleQueryError(q.AuthKeyByID(ctx, db.AuthKeyByIDParams{ID: 42, PendingLifetimeMicros: 60000000})),
		scaleQueryError(q.GetUserByUsernameWithLoginMode(ctx, "scale42")),
		scaleQueryError(q.GetUserLoginMode(ctx, 42)),
		scaleQueryError(q.UserByID(ctx, 42)),
		scaleQueryError(q.UserByPhone(ctx, new("+scale42"))),
		scaleQueryError(q.UsersByID(ctx, []int64{42, 43})),
		scaleQueryError(q.GetUsernameByHandle(ctx, "SCALE42")),
		scaleQueryError(q.GetUserByUsername(ctx, "SCALE42")),
		scaleQueryError(q.EntitledUserIDs(ctx, db.EntitledUserIDsParams{
			ViewerID: 42,
			Ids:      scaleUserIDs,
		})),
		scaleQueryError(q.GetState(ctx, 42)),
		scaleQueryError(q.EventsSince(ctx, db.EventsSinceParams{OwnerID: 42, Pts: 50})),
		scaleQueryError(q.EventsWindow(ctx, db.EventsWindowParams{OwnerID: 42, FromPts: 50, ToPts: 90, Lim: 20})),
		scaleQueryError(q.CountDialogsForOwner(ctx, 42)),
		scaleQueryError(q.CountDialogsExcludingPinned(ctx, 42)),
		scaleQueryError(q.HistoryPageAround(ctx, db.HistoryPageAroundParams{OwnerID: 42, PeerType: 1, PeerID: 43, OffsetID: 90, AddOffset: -2, Lim: 5})),
		scaleQueryError(q.GetChannelState(ctx, 42)),
		scaleQueryError(q.ChannelParticipantByUser(ctx, db.ChannelParticipantByUserParams{ChannelID: 43, UserID: 42})),
		scaleQueryError(q.ChannelEventsWindow(ctx, db.ChannelEventsWindowParams{ChannelID: 42, FromPts: 5, ToPts: 9, Lim: 20})),
		scaleQueryError(q.ChannelMessagesByLocalIDs(ctx, db.ChannelMessagesByLocalIDsParams{ChannelID: 42, LocalIds: []int64{6, 7}})),
		scaleQueryError(q.ChannelHistoryPageWithOffset(ctx, db.ChannelHistoryPageWithOffsetParams{ChannelID: 42, OffsetID: 9, AddOffset: 1, MaxID: 10, MinID: 1, Lim: 5})),
		scaleQueryError(q.ChannelHistoryPageAround(ctx, db.ChannelHistoryPageAroundParams{ChannelID: 42, OffsetID: 9, AddOffset: -2, MaxID: 10, MinID: 1, Lim: 5})),
		scaleQueryError(q.ChannelsForUser(ctx, 42)),
		scaleQueryError(q.ChannelDialogsForUser(ctx, db.ChannelDialogsForUserParams{UserID: 42})),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	st := &Store{pool: traced}
	fleet, err := st.FleetSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(fleet.Replicas) != 4 || fleet.DistinctAccounts == nil || *fleet.DistinctAccounts != 10 {
		t.Fatalf("fleet = %+v, want four live replicas and ten distinct accounts", fleet)
	}
	seeded := map[string]bool{
		"users": true, "usernames": true, "auth_keys": true, "update_state": true,
		"messages": true, "message_events": true, "dialogs": true,
		"channels": true, "channel_state": true, "channel_participants": true,
		"channel_messages": true, "channel_events": true,
		"fleet_process_snapshots": true, "fleet_live_accounts": true,
	}
	recorded.mu.Lock()
	queries := append([]pgx.TraceQueryStartData(nil), recorded.queries...)
	recorded.mu.Unlock()
	seen := make(map[string]bool)
	for _, query := range queries {
		var raw []byte
		if err := pool.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+query.SQL, query.Args...).Scan(&raw); err != nil {
			t.Fatalf("explain %s: %v", query.SQL, err)
		}
		var plans []struct {
			Plan          scalePlan `json:"Plan"`
			ExecutionTime float64   `json:"Execution Time"`
		}
		if err := json.Unmarshal(raw, &plans); err != nil {
			t.Fatal(err)
		}
		plans[0].Plan.recordRelations(seen)
		for _, table := range plans[0].Plan.sequentialTables() {
			if seeded[table] {
				t.Errorf("sequential scan over seeded %s: %s\n%s", table, query.SQL, raw)
			}
		}
		p := plans[0]
		t.Logf("%s: cost %.2f, %.3f ms, %.0f rows, %d buffer hits, %d reads",
			strings.SplitN(strings.TrimSpace(query.SQL), "\n", 2)[0],
			p.Plan.Cost, p.ExecutionTime, p.Plan.Rows, p.Plan.Hits, p.Plan.Reads)
	}
	for table := range seeded {
		if !seen[table] {
			t.Errorf("no hot query plan exercised seeded table %s", table)
		}
	}
}

func TestEntitledUserIDsUsesDialogIndexForLargeOwner(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	dsn := pgtest.DSN(t)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for _, sql := range []string{
		`UPDATE server_administration SET election_closed = true WHERE singleton_id = 1`,
		`INSERT INTO users (id, phone, first_name)
		 SELECT n, '+entitlement-scale' || n, 'Scale ' || n FROM generate_series(1, 10000) n`,
		`ALTER TABLE dialogs DISABLE TRIGGER dialogs_initialize_owner_count_before_write`,
		`ALTER TABLE dialogs DISABLE TRIGGER dialogs_track_owner_count`,
		`INSERT INTO dialogs (owner_id, peer_type, peer_id, top_message)
		 SELECT 1, 1, n, n FROM generate_series(5001, 10000) n
		 UNION ALL
		 SELECT n, 1, 1, n FROM generate_series(1001, 6000) n`,
		`ALTER TABLE dialogs ENABLE TRIGGER dialogs_track_owner_count`,
		`ALTER TABLE dialogs ENABLE TRIGGER dialogs_initialize_owner_count_before_write`,
		`ANALYZE dialogs`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("seed entitlement plan: %v", err)
		}
	}

	ids := []int64{1}
	for id := int64(8001); id <= 8100; id++ {
		ids = append(ids, id)
	}
	for id := int64(1001); id <= 1100; id++ {
		ids = append(ids, id)
	}
	for id := int64(10001); id <= 10100; id++ {
		ids = append(ids, id)
	}
	recorded := &scaleQueries{}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Tracer = recorded
	traced, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer traced.Close()
	q := db.New(traced)
	page, err := q.DialogsFirstPageWithPins(ctx, db.DialogsFirstPageWithPinsParams{
		OwnerID: 1, Lim: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 100 {
		t.Fatalf("dialog page has %d rows, want 100", len(page))
	}
	rows, err := q.EntitledUserIDs(ctx, db.EntitledUserIDsParams{ViewerID: 1, Ids: ids})
	if err != nil {
		t.Fatal(err)
	}
	want := map[int64]bool{1: true}
	for id := int64(8001); id <= 8100; id++ {
		want[id] = true
	}
	for id := int64(1001); id <= 1100; id++ {
		want[id] = true
	}
	got := make(map[int64]bool, len(rows))
	for _, row := range rows {
		id, ok := row.(int64)
		if !ok {
			t.Fatalf("entitled id has type %T, want int64", row)
		}
		got[id] = true
	}
	if len(got) != len(want) {
		t.Fatalf("entitled ids = %v, want %v", got, want)
	}
	for id := range want {
		if !got[id] {
			t.Errorf("entitled ids missing %d", id)
		}
	}

	recorded.mu.Lock()
	queries := append([]pgx.TraceQueryStartData(nil), recorded.queries...)
	recorded.mu.Unlock()
	if len(queries) != 2 {
		t.Fatalf("dialog and entitlement query count = %d, want 2", len(queries))
	}
	for _, query := range queries {
		var raw []byte
		if err := pool.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+query.SQL, query.Args...).Scan(&raw); err != nil {
			t.Fatalf("explain dialog query: %v", err)
		}
		var plans []struct {
			Plan scalePlan `json:"Plan"`
		}
		if err := json.Unmarshal(raw, &plans); err != nil {
			t.Fatal(err)
		}
		for _, table := range plans[0].Plan.sequentialTables() {
			if table == "dialogs" {
				t.Fatalf("dialog query sequentially scans dialogs: %s", raw)
			}
		}
		if strings.Contains(query.SQL, "DialogsFirstPageWithPins") {
			if rows := plans[0].Plan.rowsReadFrom("dialogs"); rows > 100 {
				t.Fatalf("dialog page reads %.0f rows for 100 returned dialogs", rows)
			}
			continue
		}
		if rows := plans[0].Plan.rowsReadFrom("dialogs"); rows > float64(2*len(ids)) {
			t.Fatalf("entitlement query reads %.0f dialog rows for %d candidate ids", rows, len(ids))
		}
	}
}

// A pooled connection switches a prepared statement to its generic plan after
// five executions, so the dialog page queries must stay bounded there too.
func TestDialogPagesWithPinsGenericPlansReadOnlyThePage(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, pgtest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for _, sql := range []string{
		`UPDATE server_administration SET election_closed = true WHERE singleton_id = 1`,
		`INSERT INTO users (id, phone, first_name)
		 SELECT n, '+generic-dialogs' || n, 'Generic ' || n FROM generate_series(1, 5000) n`,
		`INSERT INTO dialogs (owner_id, peer_type, peer_id, top_message)
		 SELECT 1, 1, n, n FROM generate_series(2, 5000) n`,
		`ANALYZE dialogs`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("seed generic dialog plan: %v", err)
		}
	}
	recorded := &scaleQueries{}
	cfg, err := pgxpool.ParseConfig(pgtest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Tracer = recorded
	traced, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer traced.Close()
	q := db.New(traced)
	if _, err := q.DialogsFirstPageWithPins(ctx, db.DialogsFirstPageWithPinsParams{OwnerID: 1, Lim: 20}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.DialogsCursorPageWithPins(ctx, db.DialogsCursorPageWithPinsParams{OwnerID: 1, OffsetID: 4000, Lim: 20}); err != nil {
		t.Fatal(err)
	}
	recorded.mu.Lock()
	queries := append([]pgx.TraceQueryStartData(nil), recorded.queries...)
	recorded.mu.Unlock()
	if len(queries) != 2 {
		t.Fatalf("dialog page query count = %d, want 2", len(queries))
	}
	firstSQL, cursorSQL := queries[0].SQL, queries[1].SQL

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SET plan_cache_mode = force_generic_plan"); err != nil {
		t.Fatal(err)
	}
	for _, page := range []struct {
		name, sql, args string
	}{
		{"first", firstSQL, "(1, false, 20)"},
		{"first excluding pinned", firstSQL, "(1, true, 20)"},
		{"cursor", cursorSQL, "(1, 4000, 0, 0, false, 20)"},
		{"cursor tie", cursorSQL, "(1, 4000, 1, 3999, true, 20)"},
	} {
		statement := "generic_dialogs_" + strings.ReplaceAll(page.name, " ", "_")
		if _, err := conn.Exec(ctx, "PREPARE "+statement+" AS "+page.sql); err != nil {
			t.Fatalf("prepare %s page: %v", page.name, err)
		}
		var raw []byte
		if err := conn.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) EXECUTE "+statement+page.args,
			pgx.QueryExecModeSimpleProtocol).Scan(&raw); err != nil {
			t.Fatalf("explain %s page: %v", page.name, err)
		}
		var plans []struct {
			Plan scalePlan `json:"Plan"`
		}
		if err := json.Unmarshal(raw, &plans); err != nil {
			t.Fatal(err)
		}
		if rows := plans[0].Plan.rowsReadFrom("dialogs"); rows > 40 {
			t.Fatalf("%s page generic plan reads %.0f dialog rows for 20 returned: %s", page.name, rows, raw)
		}
	}
}

func TestContactStatesBoundsReverseLookupForLargeOwner(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	dsn := pgtest.DSN(t)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for _, query := range []string{
		`UPDATE server_administration SET election_closed = true WHERE singleton_id = 1`,
		`INSERT INTO users (id, phone, first_name)
		 SELECT n, '+contact-scale' || n, 'Scale ' || n FROM generate_series(1, 5001) n`,
		`INSERT INTO user_contacts (owner_id, contact_id)
		 SELECT 1, n FROM generate_series(2, 5001) n`,
		`ANALYZE user_contacts`,
	} {
		if _, err := pool.Exec(ctx, query); err != nil {
			t.Fatalf("seed contact-state plan: %v", err)
		}
	}

	ids := make([]int64, 100)
	for i := range ids {
		ids[i] = int64(i + 2)
	}
	recorded := &scaleQueries{}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Tracer = recorded
	traced, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer traced.Close()
	contacts, err := db.New(traced).ContactStates(ctx, db.ContactStatesParams{OwnerID: 1, ContactIds: ids})
	if err != nil {
		t.Fatal(err)
	}
	if len(contacts) != len(ids) {
		t.Fatalf("contact states = %d, want %d", len(contacts), len(ids))
	}
	recorded.mu.Lock()
	queries := append([]pgx.TraceQueryStartData(nil), recorded.queries...)
	recorded.mu.Unlock()
	if len(queries) != 1 {
		t.Fatalf("contact-state query count = %d, want 1", len(queries))
	}
	var raw []byte
	_, query, ok := strings.Cut(queries[0].SQL, "\n")
	if !ok {
		t.Fatalf("contact-state query is missing its sqlc header: %s", queries[0].SQL)
	}
	if err := pool.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+query, queries[0].Args...).Scan(&raw); err != nil {
		t.Fatalf("explain contact-state query: %v", err)
	}
	var plans []struct {
		Plan scalePlan `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &plans); err != nil {
		t.Fatal(err)
	}
	for _, table := range plans[0].Plan.sequentialTables() {
		if table == "user_contacts" {
			t.Fatalf("contact-state query sequentially scans user_contacts: %s", raw)
		}
	}
	if rows := plans[0].Plan.rowsReadFrom("user_contacts"); rows > float64(2*len(ids)) {
		t.Fatalf("contact-state query reads %.0f contact rows for %d selected users", rows, len(ids))
	}
}

func TestLastSeenPrivacyForViewerUsesContactIDsAtLargeUserScale(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	dsn := pgtest.DSN(t)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for _, query := range []string{
		`UPDATE server_administration SET election_closed = true WHERE singleton_id = 1`,
		`INSERT INTO users (id, phone, first_name)
		 SELECT n, '+contact-user-scale' || n, 'Scale ' || n FROM generate_series(1, 115002) n`,
		`INSERT INTO user_contacts (owner_id, contact_id)
		 SELECT 1, n FROM generate_series(2, 5001) n`,
		`ANALYZE users`,
	} {
		if _, err := pool.Exec(ctx, query); err != nil {
			t.Fatalf("seed contact privacy plan: %v", err)
		}
	}

	contactRows, err := db.New(pool).ListContacts(ctx, 1)
	if err != nil {
		t.Fatalf("list contact edges: %v", err)
	}
	if len(contactRows) != 5000 {
		t.Fatalf("contact edges = %d, want 5000", len(contactRows))
	}
	ids := make([]int64, len(contactRows))
	for i, row := range contactRows {
		ids[i] = row.ContactID
	}

	recorded := &scaleQueries{}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Tracer = recorded
	traced, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer traced.Close()
	privacyRows, err := db.New(traced).EvaluateLastSeenPrivacyForViewer(ctx, db.EvaluateLastSeenPrivacyForViewerParams{
		ViewerID: 1,
		OwnerIds: ids,
	})
	if err != nil {
		t.Fatalf("evaluate contact privacy: %v", err)
	}
	if len(privacyRows) != 5000 {
		t.Fatalf("contact privacy rows = %d, want 5000", len(privacyRows))
	}
	recorded.mu.Lock()
	queries := append([]pgx.TraceQueryStartData(nil), recorded.queries...)
	recorded.mu.Unlock()
	if len(queries) != 1 {
		t.Fatalf("contact privacy query count = %d, want 1", len(queries))
	}
	var raw []byte
	_, query, ok := strings.Cut(queries[0].SQL, "\n")
	if !ok {
		t.Fatalf("contact privacy query is missing its sqlc header: %s", queries[0].SQL)
	}
	if err := pool.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+query, queries[0].Args...).Scan(&raw); err != nil {
		t.Fatalf("explain contact privacy query: %v", err)
	}
	var plans []struct {
		Plan scalePlan `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &plans); err != nil {
		t.Fatal(err)
	}
	for _, table := range plans[0].Plan.sequentialTables() {
		if table == "users" {
			t.Fatalf("contact privacy query sequentially scans users for caller contact ids: %s", raw)
		}
	}
	if rows := plans[0].Plan.rowsReadFrom("users"); rows > 5001 {
		t.Fatalf("contact privacy query reads %.0f users for 5000 contact ids", rows)
	}
}

func TestHistoryPageAroundReadsOnlyItsWindow(t *testing.T) {
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for _, sql := range []string{
		`UPDATE server_administration SET election_closed = true WHERE singleton_id = 1`,
		`INSERT INTO users (id, phone, first_name) VALUES (1, '+history-plan-owner', 'Owner'), (2, '+history-plan-peer', 'Peer')`,
		`INSERT INTO messages (owner_id, local_id, peer_type, peer_id, from_id, message, out)
			 SELECT 1, n, 1, 2, 1, 'history plan message', true FROM generate_series(1, 20000) n`,
		`ANALYZE messages`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("seed history plan data: %v", err)
		}
	}

	recorded := &scaleQueries{}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Tracer = recorded
	traced, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer traced.Close()
	const pageSize = 100
	rows, err := db.New(traced).HistoryPageAround(ctx, db.HistoryPageAroundParams{
		OwnerID: 1, PeerType: 1, PeerID: 2, OffsetID: 1, AddOffset: -pageSize, Lim: pageSize,
	})
	if err != nil {
		t.Fatalf("history page around: %v", err)
	}
	if len(rows) != pageSize || rows[0].LocalID != pageSize || rows[len(rows)-1].LocalID != 1 {
		t.Fatalf("oldest history page = %d rows from %d to %d, want %d rows from %d to 1", len(rows), rows[0].LocalID, rows[len(rows)-1].LocalID, pageSize, pageSize)
	}

	recorded.mu.Lock()
	queries := append([]pgx.TraceQueryStartData(nil), recorded.queries...)
	recorded.mu.Unlock()
	if len(queries) != 1 {
		t.Fatalf("history page issued %d statements, want 1", len(queries))
	}
	var raw []byte
	if err := pool.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+queries[0].SQL, queries[0].Args...).Scan(&raw); err != nil {
		t.Fatalf("explain history page: %v", err)
	}
	var plans []struct {
		Plan scalePlan `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &plans); err != nil {
		t.Fatalf("decode history page plan: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("history page explain returned %d plans, want 1", len(plans))
	}
	readRows := plans[0].Plan.rowsReadFrom("messages")
	if readRows > 2*pageSize+10 {
		t.Fatalf("history page read %.0f message rows for a %d-row page, want at most %d", readRows, pageSize, 2*pageSize+10)
	}
}

func TestHistoryPageByDateAroundReadsOnlyItsWindow(t *testing.T) {
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for _, sql := range []string{
		`UPDATE server_administration SET election_closed = true WHERE singleton_id = 1`,
		`INSERT INTO users (id, phone, first_name) VALUES (1, '+history-date-plan-owner', 'Owner'), (2, '+history-date-plan-peer', 'Peer')`,
		`INSERT INTO messages (owner_id, local_id, peer_type, peer_id, from_id, message, out, date)
			 SELECT 1, n, 1, 2, 1, 'history date plan message', true, to_timestamp(n::double precision)
			 FROM generate_series(1, 20000) n`,
		`ANALYZE messages`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("seed date history plan data: %v", err)
		}
	}

	recorded := &scaleQueries{}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Tracer = recorded
	traced, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer traced.Close()
	const pageSize = 100
	const shiftCount = pageSize / 2
	rows, err := db.New(traced).HistoryPageByDateAround(ctx, db.HistoryPageByDateAroundParams{
		OwnerID: 1, PeerType: 1, PeerID: 2, OffsetDate: 10001, AddOffset: -shiftCount, Lim: pageSize,
	})
	if err != nil {
		t.Fatalf("history page by date around: %v", err)
	}
	if len(rows) != pageSize {
		t.Fatalf("date history page has %d rows, want %d", len(rows), pageSize)
	}
	if rows[0].LocalID != 10000+shiftCount || rows[shiftCount-1].LocalID != 10001 ||
		rows[shiftCount].LocalID != 10000 || rows[len(rows)-1].LocalID != 10001-shiftCount {
		t.Fatalf("date history page boundaries = %d, %d, %d, %d, want %d, 10001, 10000, %d",
			rows[0].LocalID, rows[shiftCount-1].LocalID, rows[shiftCount].LocalID, rows[len(rows)-1].LocalID,
			10000+shiftCount, 10001-shiftCount)
	}

	recorded.mu.Lock()
	queries := append([]pgx.TraceQueryStartData(nil), recorded.queries...)
	recorded.mu.Unlock()
	if len(queries) != 1 {
		t.Fatalf("date history page issued %d statements, want 1", len(queries))
	}
	var raw []byte
	if err := pool.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+queries[0].SQL, queries[0].Args...).Scan(&raw); err != nil {
		t.Fatalf("explain date history page: %v", err)
	}
	var plans []struct {
		Plan scalePlan `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &plans); err != nil {
		t.Fatalf("decode date history page plan: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("date history page explain returned %d plans, want 1", len(plans))
	}
	readRows := plans[0].Plan.rowsReadFrom("messages")
	if readRows > pageSize+10 {
		t.Fatalf("date history page read %.0f message rows for a %d-row page, want at most %d", readRows, pageSize, pageSize+10)
	}
}

func TestHistoryPageByDateAroundReadsMiddleWindow(t *testing.T) {
	ctx := t.Context()
	dsn := pgtest.DSN(t)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for _, sql := range []string{
		`UPDATE server_administration SET election_closed = true WHERE singleton_id = 1`,
		`INSERT INTO users (id, phone, first_name) VALUES (1, '+history-date-middle-owner', 'Owner'), (2, '+history-date-middle-peer', 'Peer')`,
		`INSERT INTO messages (owner_id, local_id, peer_type, peer_id, from_id, date, message, out)
			 SELECT 1, n, 1, 2, 1, to_timestamp(n), 'history date message', true FROM generate_series(1, 20000) n`,
		`ANALYZE messages`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("seed middle date history plan data: %v", err)
		}
	}

	recorded := &scaleQueries{}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Tracer = recorded
	traced, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer traced.Close()
	const pageSize = 100
	rows, err := db.New(traced).HistoryPageByDateAround(ctx, db.HistoryPageByDateAroundParams{
		OwnerID: 1, PeerType: 1, PeerID: 2, OffsetDate: 10001, AddOffset: -pageSize / 2, Lim: pageSize,
	})
	if err != nil {
		t.Fatalf("history page by date around: %v", err)
	}
	if len(rows) != pageSize || rows[0].LocalID != 10050 || rows[len(rows)-1].LocalID != 9951 {
		t.Fatalf("middle date history page = %d rows from %d to %d, want %d rows from 10050 to 9951", len(rows), rows[0].LocalID, rows[len(rows)-1].LocalID, pageSize)
	}

	recorded.mu.Lock()
	queries := append([]pgx.TraceQueryStartData(nil), recorded.queries...)
	recorded.mu.Unlock()
	if len(queries) != 1 {
		t.Fatalf("date history page issued %d statements, want 1", len(queries))
	}
	var raw []byte
	if err := pool.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+queries[0].SQL, queries[0].Args...).Scan(&raw); err != nil {
		t.Fatalf("explain middle date history page: %v", err)
	}
	var plans []struct {
		Plan scalePlan `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &plans); err != nil {
		t.Fatalf("decode middle date history page plan: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("middle date history page explain returned %d plans, want 1", len(plans))
	}
	readRows := plans[0].Plan.rowsReadFrom("messages")
	if readRows > 2*pageSize+10 {
		t.Fatalf("middle date history page read %.0f message rows for a %d-row page, want at most %d", readRows, pageSize, 2*pageSize+10)
	}
}

func TestChannelHistoryPageByDateReadsOnlyItsWindow(t *testing.T) {
	ctx := t.Context()
	dsn := pgtest.DSN(t)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for _, sql := range []string{
		`UPDATE server_administration SET election_closed = true WHERE singleton_id = 1`,
		`INSERT INTO users (id, phone, first_name) VALUES (1, '+channel-history-date-plan-owner', 'Owner')`,
		`INSERT INTO channels (id, title, creator_id) VALUES (1, 'Channel date history plan', 1)`,
		`INSERT INTO channel_state (channel_id) VALUES (1)`,
		`INSERT INTO channel_participants (channel_id, user_id) VALUES (1, 1)`,
		`ALTER TABLE channel_messages DISABLE TRIGGER channel_messages_stamp_history_ttl`,
		`ALTER TABLE channel_messages DISABLE TRIGGER channel_messages_summary_maintain`,
		`INSERT INTO channel_messages (channel_id, local_id, from_id, date, message)
		 SELECT 1, n, 1, to_timestamp(n), 'channel history date plan message'
		 FROM generate_series(1, 20000) n`,
		`ALTER TABLE channel_messages ENABLE TRIGGER channel_messages_summary_maintain`,
		`ALTER TABLE channel_messages ENABLE TRIGGER channel_messages_stamp_history_ttl`,
		`ANALYZE channel_messages`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("seed channel date history plan data: %v", err)
		}
	}

	recorded := &scaleQueries{}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Tracer = recorded
	traced, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer traced.Close()
	const pageSize = 100
	assertReadBound := func(label string) {
		t.Helper()
		recorded.mu.Lock()
		queries := append([]pgx.TraceQueryStartData(nil), recorded.queries...)
		recorded.mu.Unlock()
		if len(queries) != 1 {
			t.Fatalf("%s issued %d statements, want 1", label, len(queries))
		}
		var raw []byte
		if err := pool.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+queries[0].SQL, queries[0].Args...).Scan(&raw); err != nil {
			t.Fatalf("explain %s: %v", label, err)
		}
		var plans []struct {
			Plan scalePlan `json:"Plan"`
		}
		if err := json.Unmarshal(raw, &plans); err != nil {
			t.Fatalf("decode %s plan: %v", label, err)
		}
		if len(plans) != 1 {
			t.Fatalf("%s explain returned %d plans, want 1", label, len(plans))
		}
		readRows := plans[0].Plan.rowsReadFrom("channel_messages")
		if readRows > 2*pageSize+10 {
			t.Fatalf("%s read %.0f channel rows for a %d-row page, want at most %d", label, readRows, pageSize, 2*pageSize+10)
		}
	}
	rows, err := db.New(traced).ChannelHistoryPageByDate(ctx, db.ChannelHistoryPageByDateParams{
		ChannelID: 1, OwnerID: 1, OffsetDate: 10000, Lim: pageSize,
	})
	if err != nil {
		t.Fatalf("channel history page by date: %v", err)
	}
	if len(rows) != pageSize {
		t.Fatalf("channel date history page returned %d rows, want %d", len(rows), pageSize)
	}
	if rows[0].LocalID != 9999 || rows[len(rows)-1].LocalID != 9900 {
		t.Fatalf("channel date history page starts at %d and ends at %d, want 9999 to 9900", rows[0].LocalID, rows[len(rows)-1].LocalID)
	}

	assertReadBound("channel date history page")

	if _, err := pool.Exec(ctx, `UPDATE channel_participants SET available_min_id = 19800 WHERE channel_id = 1 AND user_id = 1`); err != nil {
		t.Fatalf("set channel date history floor: %v", err)
	}
	recorded.mu.Lock()
	recorded.queries = nil
	recorded.mu.Unlock()
	rows, err = db.New(traced).ChannelHistoryPageByDate(ctx, db.ChannelHistoryPageByDateParams{
		ChannelID: 1, OwnerID: 1, OffsetDate: 10000, AddOffset: -pageSize, Lim: pageSize,
	})
	if err != nil {
		t.Fatalf("channel history page by date with hidden pre-history: %v", err)
	}
	if len(rows) != pageSize {
		t.Fatalf("channel date history page with hidden pre-history returned %d rows, want %d", len(rows), pageSize)
	}
	if rows[0].LocalID != 19900 || rows[len(rows)-1].LocalID != 19801 {
		t.Fatalf("channel date history page with hidden pre-history starts at %d and ends at %d, want 19900 to 19801", rows[0].LocalID, rows[len(rows)-1].LocalID)
	}
	assertReadBound("channel date history page with hidden pre-history")
}

func TestChannelHistoryPageByDateReadsOnlyItsWindowWithExpiredBacklog(t *testing.T) {
	ctx := t.Context()
	dsn := pgtest.DSN(t)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for _, sql := range []string{
		`UPDATE server_administration SET election_closed = true WHERE singleton_id = 1`,
		`INSERT INTO users (id, phone, first_name) VALUES (1, '+channel-history-date-expired-owner', 'Owner')`,
		`INSERT INTO channels (id, title, creator_id) VALUES (1, 'Channel date expired history plan', 1)`,
		`INSERT INTO channel_state (channel_id) VALUES (1)`,
		`INSERT INTO channel_participants (channel_id, user_id, available_min_id) VALUES (1, 1, 25000)`,
		`ALTER TABLE channel_messages DISABLE TRIGGER channel_messages_stamp_history_ttl`,
		`ALTER TABLE channel_messages DISABLE TRIGGER channel_messages_summary_maintain`,
		`INSERT INTO channel_messages (channel_id, local_id, from_id, date, message)
		 SELECT 1, n, 1, to_timestamp(n), 'channel history hidden post'
		 FROM generate_series(1, 25000) n`,
		`INSERT INTO channel_messages (channel_id, local_id, from_id, date, message, ttl_period, expires_at)
		 SELECT 1, n, 1, to_timestamp(n), 'expired channel history post', 86400,
		        to_timestamp(n) + make_interval(secs => 86400)
		 FROM generate_series(25001, 29000) n`,
		`INSERT INTO channel_messages (channel_id, local_id, from_id, date, message)
		 SELECT 1, n, 1, to_timestamp(n), 'live channel history post'
		 FROM generate_series(29001, 30000) n`,
		`ALTER TABLE channel_messages ENABLE TRIGGER channel_messages_summary_maintain`,
		`ALTER TABLE channel_messages ENABLE TRIGGER channel_messages_stamp_history_ttl`,
		`ANALYZE channel_messages`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("seed expired channel date history plan data: %v", err)
		}
	}

	recorded := &scaleQueries{}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Tracer = recorded
	traced, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer traced.Close()
	const pageSize = 100
	rows, err := db.New(traced).ChannelHistoryPageByDate(ctx, db.ChannelHistoryPageByDateParams{
		ChannelID: 1, OwnerID: 1, OffsetDate: 20000, AddOffset: -pageSize, Lim: pageSize,
	})
	if err != nil {
		t.Fatalf("channel history page by date with expired backlog: %v", err)
	}
	if len(rows) != pageSize {
		t.Fatalf("channel date history page with expired backlog returned %d rows, want %d", len(rows), pageSize)
	}
	if rows[0].LocalID != 29100 || rows[len(rows)-1].LocalID != 29001 {
		t.Fatalf("channel date history page with expired backlog starts at %d and ends at %d, want 29100 to 29001", rows[0].LocalID, rows[len(rows)-1].LocalID)
	}

	recorded.mu.Lock()
	queries := append([]pgx.TraceQueryStartData(nil), recorded.queries...)
	recorded.mu.Unlock()
	if len(queries) != 1 {
		t.Fatalf("expired backlog date history issued %d statements, want 1", len(queries))
	}
	var raw []byte
	if err := pool.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+queries[0].SQL, queries[0].Args...).Scan(&raw); err != nil {
		t.Fatalf("explain expired backlog date history: %v", err)
	}
	var plans []struct {
		Plan scalePlan `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &plans); err != nil {
		t.Fatalf("decode expired backlog date history plan: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("expired backlog date history explain returned %d plans, want 1", len(plans))
	}
	readRows := plans[0].Plan.rowsReadFrom("channel_messages")
	if readRows > 2*pageSize+10 {
		t.Fatalf("expired backlog date history read %.0f channel rows for a %d-row page, want at most %d", readRows, pageSize, 2*pageSize+10)
	}
}

func TestChannelHistoryPageByDateShiftSkipsExpiredRows(t *testing.T) {
	ctx := t.Context()
	dsn := pgtest.DSN(t)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for _, sql := range []string{
		`UPDATE server_administration SET election_closed = true WHERE singleton_id = 1`,
		`INSERT INTO users (id, phone, first_name) VALUES (1, '+channel-history-date-shift-owner', 'Owner')`,
		`INSERT INTO channels (id, title, creator_id) VALUES (1, 'Channel date shifted history plan', 1)`,
		`INSERT INTO channel_state (channel_id) VALUES (1)`,
		`INSERT INTO channel_participants (channel_id, user_id) VALUES (1, 1)`,
		`ALTER TABLE channel_messages DISABLE TRIGGER channel_messages_stamp_history_ttl`,
		`ALTER TABLE channel_messages DISABLE TRIGGER channel_messages_summary_maintain`,
		`INSERT INTO channel_messages (channel_id, local_id, from_id, date, message)
		 SELECT 1, n, 1, to_timestamp(n), 'live channel history post'
		 FROM generate_series(1, 1000) n`,
		`INSERT INTO channel_messages (channel_id, local_id, from_id, date, message, ttl_period, expires_at)
		 SELECT 1, n, 1, to_timestamp(n), 'expired channel history post', 86400,
		        to_timestamp(n) + make_interval(secs => 86400)
		 FROM generate_series(1001, 5000) n`,
		`INSERT INTO channel_messages (channel_id, local_id, from_id, date, message, ttl_period, expires_at)
		 SELECT 1, n, 1, statement_timestamp() - interval '1 day', 'live channel history post', 604800,
		        statement_timestamp() + interval '6 days'
		 FROM generate_series(5001, 7000) n`,
		`ALTER TABLE channel_messages ENABLE TRIGGER channel_messages_summary_maintain`,
		`ALTER TABLE channel_messages ENABLE TRIGGER channel_messages_stamp_history_ttl`,
		`ANALYZE channel_messages`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("seed shifted expired channel date history plan data: %v", err)
		}
	}

	recorded := &scaleQueries{}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Tracer = recorded
	traced, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer traced.Close()
	const pageSize = 100
	rows, err := db.New(traced).ChannelHistoryPageByDate(ctx, db.ChannelHistoryPageByDateParams{
		ChannelID: 1, OwnerID: 1, OffsetDate: 1000, AddOffset: -pageSize - 1, Lim: pageSize,
	})
	if err != nil {
		t.Fatalf("channel history page by date with shifted expired backlog: %v", err)
	}
	if len(rows) != pageSize || rows[0].LocalID != 5100 || rows[len(rows)-1].LocalID != 5001 {
		t.Fatalf("shifted channel date history page = %d rows from %d to %d, want 100 rows from 5100 to 5001", len(rows), rows[0].LocalID, rows[len(rows)-1].LocalID)
	}

	recorded.mu.Lock()
	queries := append([]pgx.TraceQueryStartData(nil), recorded.queries...)
	recorded.mu.Unlock()
	if len(queries) != 1 {
		t.Fatalf("shifted expired backlog date history issued %d statements, want 1", len(queries))
	}
	var raw []byte
	if err := pool.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+queries[0].SQL, queries[0].Args...).Scan(&raw); err != nil {
		t.Fatalf("explain shifted expired backlog date history: %v", err)
	}
	var plans []struct {
		Plan scalePlan `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &plans); err != nil {
		t.Fatalf("decode shifted expired backlog date history plan: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("shifted expired backlog date history explain returned %d plans, want 1", len(plans))
	}
	readRows := plans[0].Plan.rowsReadFrom("channel_messages")
	if readRows > 2*pageSize+10 {
		t.Fatalf("shifted expired backlog date history read %.0f channel rows for a %d-row page, want at most %d", readRows, pageSize, 2*pageSize+10)
	}
}

func TestChannelHistoryPageByDateBoundsOlderOffsetsAndExpiredTail(t *testing.T) {
	ctx := t.Context()
	dsn := pgtest.DSN(t)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for _, sql := range []string{
		`UPDATE server_administration SET election_closed = true WHERE singleton_id = 1`,
		`INSERT INTO users (id, phone, first_name) VALUES (1, '+channel-history-older-offset-owner', 'Owner')`,
		`INSERT INTO channels (id, title, creator_id) VALUES (1, 'Channel older offset history plan', 1)`,
		`INSERT INTO channel_state (channel_id) VALUES (1)`,
		`INSERT INTO channel_participants (channel_id, user_id) VALUES (1, 1)`,
		`ALTER TABLE channel_messages DISABLE TRIGGER channel_messages_stamp_history_ttl`,
		`ALTER TABLE channel_messages DISABLE TRIGGER channel_messages_summary_maintain`,
		`INSERT INTO channel_messages (channel_id, local_id, from_id, date, message)
		 SELECT 1, n, 1, to_timestamp(n), 'live older channel post'
		 FROM generate_series(1, 1000) n`,
		`INSERT INTO channel_messages (channel_id, local_id, from_id, date, message, ttl_period, expires_at)
		 SELECT 1, n, 1, to_timestamp(n), 'expired channel post before anchor', 86400,
		        to_timestamp(n) + make_interval(secs => 86400)
		 FROM generate_series(1001, 5000) n`,
		`INSERT INTO channel_messages (channel_id, local_id, from_id, date, message, ttl_period, expires_at)
		 SELECT 1, n, 1, statement_timestamp() - interval '1 day', 'live channel post before tail', 604800,
		        statement_timestamp() + interval '6 days'
		 FROM generate_series(5001, 6999) n`,
		`INSERT INTO channel_messages (channel_id, local_id, from_id, date, message, ttl_period, expires_at)
		 VALUES (1, 7000, 1, statement_timestamp(), 'latest live channel post', 604800,
		         statement_timestamp() + interval '7 days')`,
		`INSERT INTO channel_messages (channel_id, local_id, from_id, date, message, ttl_period, expires_at)
		 SELECT 1, n, 1, to_timestamp(n), 'expired channel tail post', 86400,
		        to_timestamp(n) + make_interval(secs => 86400)
		 FROM generate_series(7001, 11000) n`,
		`ALTER TABLE channel_messages ENABLE TRIGGER channel_messages_summary_maintain`,
		`ALTER TABLE channel_messages ENABLE TRIGGER channel_messages_stamp_history_ttl`,
		`ANALYZE channel_messages`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("seed older-offset expired channel date history: %v", err)
		}
	}

	recorded := &scaleQueries{}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Tracer = recorded
	traced, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer traced.Close()
	const pageSize = 100
	runPage := func(label string, offsetDate, addOffset int64, firstID, lastID int64) {
		t.Helper()
		recorded.mu.Lock()
		recorded.queries = nil
		recorded.mu.Unlock()
		rows, err := db.New(traced).ChannelHistoryPageByDate(ctx, db.ChannelHistoryPageByDateParams{
			ChannelID: 1, OwnerID: 1, OffsetDate: offsetDate, AddOffset: addOffset, Lim: pageSize,
		})
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if len(rows) != pageSize || rows[0].LocalID != firstID || rows[len(rows)-1].LocalID != lastID {
			t.Fatalf("%s returned %d rows from %d to %d, want %d rows from %d to %d", label, len(rows), rows[0].LocalID, rows[len(rows)-1].LocalID, pageSize, firstID, lastID)
		}

		recorded.mu.Lock()
		queries := append([]pgx.TraceQueryStartData(nil), recorded.queries...)
		recorded.mu.Unlock()
		if len(queries) != 1 {
			t.Fatalf("%s issued %d statements, want 1", label, len(queries))
		}
		var raw []byte
		if err := pool.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+queries[0].SQL, queries[0].Args...).Scan(&raw); err != nil {
			t.Fatalf("explain %s: %v", label, err)
		}
		var plans []struct {
			Plan scalePlan `json:"Plan"`
		}
		if err := json.Unmarshal(raw, &plans); err != nil {
			t.Fatalf("decode %s plan: %v", label, err)
		}
		if len(plans) != 1 {
			t.Fatalf("%s explain returned %d plans, want 1", label, len(plans))
		}
		readRows := plans[0].Plan.rowsReadFrom("channel_messages")
		if readRows > 2*pageSize+10 {
			t.Fatalf("%s read %.0f channel rows for a %d-row page, want at most %d", label, readRows, pageSize, 2*pageSize+10)
		}
	}

	now := time.Now()
	runPage("positive offset skips expired backlog", now.Add(-48*time.Hour).Unix(), pageSize, 900, 801)
	runPage("positive offset includes nearest older TTL post", now.Add(-12*time.Hour).Unix(), 1, 6998, 6899)
	runPage("negative shift beyond the date anchor uses visible tail", now.Add(-12*time.Hour).Unix(), -pageSize-1, 7000, 6901)
	runPage("date miss uses visible tail", now.Add(time.Hour).Unix(), -pageSize-1, 7000, 6901)
}

func scaleQueryError[T any](_ T, err error) error { return err }
