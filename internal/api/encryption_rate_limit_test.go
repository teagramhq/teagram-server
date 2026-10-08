package api_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/tg"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

// unbudgeted is the zero config: RateLimitConfig.Enabled is false for it, so a
// surface left at it enforces nothing. These tests use it to hold one budget open
// while they drive the other to exhaustion.
var unbudgeted = store.RateLimitConfig{}

// secretChatConn opens the raw connection these tests need to assert on what no
// store method reports: the limiter row, the rows created, and where the chat-id
// sequence stands.
func secretChatConn(t *testing.T, dsn string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return conn
}

// tokenCount reads one account's counter for a surface. No row reads as zero,
// which is what an account that has never been charged has.
func tokenCount(t *testing.T, conn *pgx.Conn, subjectID int64, surface string) int {
	t.Helper()
	var n int
	err := conn.QueryRow(context.Background(),
		`SELECT token_count FROM rate_limits WHERE subject_id = $1 AND surface = $2`,
		subjectID, surface).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0
	}
	if err != nil {
		t.Fatalf("read counter for %d on %s: %v", subjectID, surface, err)
	}
	return n
}

// secretChatRows counts the secret_chats rows one account has ever created. Rows
// are never deleted, so this is the durable cost the request budget bounds.
func secretChatRows(t *testing.T, conn *pgx.Conn, adminID int64) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(context.Background(),
		`SELECT count(*) FROM secret_chats WHERE admin_id = $1`, adminID).Scan(&n); err != nil {
		t.Fatalf("count secret chats for %d: %v", adminID, err)
	}
	return n
}

// sequenceLastValue reads the high-water mark of the chat-id sequence. Sequences
// do not roll back, so a denial that allocated an id before rolling back leaves
// this above the highest id any caller was ever handed.
func sequenceLastValue(t *testing.T, conn *pgx.Conn) int64 {
	t.Helper()
	var v int64
	if err := conn.QueryRow(context.Background(), `SELECT last_value FROM secret_chats_id_seq`).Scan(&v); err != nil {
		t.Fatalf("read chat id sequence: %v", err)
	}
	return v
}

// chatDate reads the stored date of one chat, the field a discard re-dates.
func chatDate(t *testing.T, conn *pgx.Conn, chatID int32) time.Time {
	t.Helper()
	var d time.Time
	if err := conn.QueryRow(context.Background(),
		`SELECT date FROM secret_chats WHERE id = $1`, chatID).Scan(&d); err != nil {
		t.Fatalf("read date of chat %d: %v", chatID, err)
	}
	return d
}

// pushLog counts the encryption notifications one account was sent, keyed by the
// chat each one names. Keying by chat is what separates a request's push from a
// discard's push of the same chat, and what makes "this refused call pushed
// nothing" an exact statement rather than a total.
type pushLog struct {
	mu     sync.Mutex
	target int64
	byChat map[int64]int
}

func (p *pushLog) add(userID, chatID int64) {
	if userID != p.target {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.byChat == nil {
		p.byChat = map[int64]int{}
	}
	p.byChat[chatID]++
}

func (p *pushLog) total() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, c := range p.byChat {
		n += c
	}
	return n
}

func (p *pushLog) forChat(chatID int64) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.byChat[chatID]
}

func (p *pushLog) chats() []int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	ids := make([]int64, 0, len(p.byChat))
	for id := range p.byChat {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// encryptionPushCounter starts a listener on dsn and records the encryption
// notifications addressed to target, which is what a push to that account starts
// with.
func encryptionPushCounter(t *testing.T, dsn string, target int64) *pushLog {
	t.Helper()
	log := &pushLog{target: target}
	_, stop, err := store.StartListener(context.Background(), dsn,
		func(context.Context, int64) {},
		func(context.Context, int64, int64) {},
		func(context.Context, int64, int64) {},
		func(context.Context, int64) {},
		func(_ context.Context, userID, chatID int64) { log.add(userID, chatID) },
		func(context.Context, int64, bool) {},
		func(context.Context, int64, int) {},
		func(context.Context, int64, int64, int64) {},
		func(context.Context, store.PeerType, int64, int32) {},
		nil,
	)
	if err != nil {
		t.Fatalf("start listener: %v", err)
	}
	t.Cleanup(func() { _ = stop() }) //nolint:errcheck // listener shutdown is test cleanup
	return log
}

// awaitPushes polls until want notifications have arrived. Notification dispatch
// is asynchronous, so a fixed sleep would race under load.
func awaitPushes(t *testing.T, log *pushLog, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for log.total() < want {
		if time.Now().After(deadline) {
			t.Fatalf("encryption notifications = %d, want %d (timed out)", log.total(), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// requestOK runs one requestEncryption that must succeed and returns the waiting
// chat it created.
func requestOK(t *testing.T, s *store.Store, admin, peer int64, budget store.RateLimitConfig, randomID int) *tg.EncryptedChatWaiting {
	t.Helper()
	enc, err := api.RequestEncryptionWithBudgetForTest(s, admin, budget, &tg.MessagesRequestEncryptionRequest{
		UserID:   api.InputUser(admin, peer),
		RandomID: randomID,
		GA:       validGA(),
	})
	if err != nil {
		t.Fatalf("requestEncryption random_id=%d: %v", randomID, err)
	}
	waiting, ok := enc.(*tg.EncryptedChatWaiting)
	if !ok {
		t.Fatalf("result type = %T, want *tg.EncryptedChatWaiting", enc)
	}
	return waiting
}

// discardOK runs one discardEncryption that must succeed.
func discardOK(t *testing.T, s *store.Store, caller int64, budget store.RateLimitConfig, chatID int) {
	t.Helper()
	if _, err := api.DiscardEncryptionWithBudgetForTest(s, caller, budget,
		&tg.MessagesDiscardEncryptionRequest{ChatID: chatID}); err != nil {
		t.Fatalf("discardEncryption chat=%d: %v", chatID, err)
	}
}

// acceptOK runs one acceptEncryption by the responder that must succeed.
func acceptOK(t *testing.T, s *store.Store, responder int64, chatID int32, fingerprint int64) {
	t.Helper()
	if _, err := api.AcceptEncryptionForTest(s, responder, &tg.MessagesAcceptEncryptionRequest{
		Peer: api.InputEncryptedChat(responder, chatID), GB: validGB(), KeyFingerprint: fingerprint,
	}); err != nil {
		t.Fatalf("acceptEncryption chat=%d: %v", chatID, err)
	}
}

// TestRequestEncryptionRateLimitBoundsTheRequestDiscardLoop is the loop the threat
// model names: one account requests and then discards, so the 10-row outstanding
// cap is freed on every iteration and refuses nothing. Before this budget that
// loop produced rows without limit.
//
// The bound is asserted four ways: rows created, chat ids allocated, the counter,
// and the notifications the responder was sent. Then the window rolls and the same
// account can request again.
func TestRequestEncryptionRateLimitBoundsTheRequestDiscardLoop(t *testing.T) {
	t.Parallel()
	s, dsn := openStoreDSN(t)
	x, y := twoUsersFor(t, s, "+15551390001", "+15551390002")

	// A long window on purpose: only the explicit ageing below can close it, so
	// the denials asserted here cannot race a real boundary under host load.
	budget := store.RateLimitConfig{Limit: 3, Window: time.Hour}
	const extra = 5

	pushes := encryptionPushCounter(t, dsn, y)
	conn := secretChatConn(t, dsn)

	var accepted []*tg.EncryptedChatWaiting
	var highest int32
	var denied int
	for i := range budget.Limit + extra {
		enc, err := api.RequestEncryptionWithBudgetForTest(s, x, budget, &tg.MessagesRequestEncryptionRequest{
			UserID:   api.InputUser(x, y),
			RandomID: 9000 + i,
			GA:       validGA(),
		})
		if err != nil {
			if !isFloodWait(err) {
				t.Fatalf("request %d: expected FLOOD_WAIT, got %v", i, err)
			}
			denied++
			continue
		}
		waiting, ok := enc.(*tg.EncryptedChatWaiting)
		if !ok {
			t.Fatalf("request %d: result type = %T, want *tg.EncryptedChatWaiting", i, enc)
		}
		accepted = append(accepted, waiting)
		if id := int32(waiting.ID); id > highest { //nolint:gosec // test id
			highest = id
		}
		// The discard is on its own budget and stays open here, so the loop keeps
		// freeing the outstanding cap exactly as an attacker's would.
		discardOK(t, s, x, unbudgeted, waiting.ID)
	}

	if len(accepted) != budget.Limit {
		t.Fatalf("accepted requests = %d, want %d", len(accepted), budget.Limit)
	}
	if denied != extra {
		t.Fatalf("denied requests = %d, want %d", denied, extra)
	}
	if got := secretChatRows(t, conn, x); got != budget.Limit {
		t.Errorf("secret_chats rows for %d = %d, want %d (a denied request wrote a row)", x, got, budget.Limit)
	}
	if got := tokenCount(t, conn, x, store.RequestEncryptionRateLimitSurface); got != budget.Limit {
		t.Errorf("request counter = %d, want %d", got, budget.Limit)
	}

	// The sequence's high-water mark is the highest id a caller was handed. A
	// denial that allocated an id before rolling back would leave it higher.
	if got := sequenceLastValue(t, conn); got != int64(highest) {
		t.Errorf("chat id sequence at %d, want %d (a denied request consumed an id)", got, highest)
	}

	// Roll the window and ask again: the bound is per window, not permanent.
	if err := api.AgeRateLimitWindowForTest(dsn, x, store.RequestEncryptionRateLimitSurface, budget.Window+time.Minute); err != nil {
		t.Fatalf("age window: %v", err)
	}
	after := requestOK(t, s, x, y, budget, 9999)
	if after.AdminID != x || after.ParticipantID != y {
		t.Fatalf("post-boundary chat parties = %d/%d, want %d/%d", after.AdminID, after.ParticipantID, x, y)
	}
	if got := tokenCount(t, conn, x, store.RequestEncryptionRateLimitSurface); got != 1 {
		t.Errorf("request counter after the window rolled = %d, want 1 (a fresh window)", got)
	}
	if got := secretChatRows(t, conn, x); got != budget.Limit+1 {
		t.Errorf("secret_chats rows after the window rolled = %d, want %d", got, budget.Limit+1)
	}

	// Push accounting. Each accepted chat was pushed to y twice: once for the
	// request, once for the discard. The post-boundary chat was pushed once. That last
	// notification is the barrier: any push a denied request had fired would have
	// arrived long before it, and it would show up as a third push on one of the
	// accepted chats or as a chat this test never created.
	awaitPushes(t, pushes, 2*budget.Limit+1)
	time.Sleep(100 * time.Millisecond)
	if got := pushes.total(); got != 2*budget.Limit+1 {
		t.Fatalf("encryption notifications to %d = %d, want %d (a denied request pushed)", y, got, 2*budget.Limit+1)
	}
	for _, w := range accepted {
		if got := pushes.forChat(int64(w.ID)); got != 2 {
			t.Errorf("chat %d was pushed %d times, want 2 (request and discard)", w.ID, got)
		}
	}
	if got := pushes.forChat(int64(after.ID)); got != 1 {
		t.Errorf("post-boundary chat %d was pushed %d times, want 1", after.ID, got)
	}
	if got := len(pushes.chats()); got != budget.Limit+1 {
		t.Errorf("distinct chats pushed = %d, want %d (%v)", got, budget.Limit+1, pushes.chats())
	}
}

// TestRequestEncryptionRateLimitLeavesOtherAccountsUnaffected proves the bound is
// per account and per surface. A spent request allowance must not stop another
// account from starting its own exchange, must not stop a responder from accepting
// or discarding, and must not stop the spender itself from cleaning up or from
// completing a key exchange it was offered.
func TestRequestEncryptionRateLimitLeavesOtherAccountsUnaffected(t *testing.T) {
	t.Parallel()
	s, dsn := openStoreDSN(t)
	x, y := twoUsersFor(t, s, "+15551390011", "+15551390012")
	z, w := twoUsersFor(t, s, "+15551390013", "+15551390014")

	budget := store.RateLimitConfig{Limit: 2, Window: time.Hour}
	discardBudget := store.RateLimitConfig{Limit: 30, Window: time.Hour}
	conn := secretChatConn(t, dsn)

	first := requestOK(t, s, x, y, budget, 1)
	second := requestOK(t, s, x, y, budget, 2)
	if _, err := api.RequestEncryptionWithBudgetForTest(s, x, budget, &tg.MessagesRequestEncryptionRequest{
		UserID: api.InputUser(x, y), RandomID: 3, GA: validGA(),
	}); !isFloodWait(err) {
		t.Fatalf("third request from x: expected FLOOD_WAIT, got %v", err)
	}
	if got := tokenCount(t, conn, x, store.RequestEncryptionRateLimitSurface); got != 2 {
		t.Fatalf("x request counter = %d, want 2", got)
	}

	// A different account starts its own exchange on a clean budget.
	yz := requestOK(t, s, y, z, budget, 10)
	// The responder accepts the chat it was offered; accept is not budgeted.
	acceptOK(t, s, y, int32(first.ID), 7) //nolint:gosec // test id
	// The responder cleans up on its own discard allowance.
	discardOK(t, s, y, discardBudget, first.ID)
	// x's spent request budget does not block x's own cleanup.
	discardOK(t, s, x, discardBudget, second.ID)
	// The exchange y started still completes, and so does one w starts from clean.
	acceptOK(t, s, z, int32(yz.ID), 8) //nolint:gosec // test id
	wz := requestOK(t, s, w, z, budget, 20)
	acceptOK(t, s, z, int32(wz.ID), 9) //nolint:gosec // test id

	if got := tokenCount(t, conn, x, store.DiscardEncryptionRateLimitSurface); got != 1 {
		t.Errorf("x discard counter = %d, want 1", got)
	}
	if got := tokenCount(t, conn, y, store.RequestEncryptionRateLimitSurface); got != 1 {
		t.Errorf("y request counter = %d, want 1 (x's budget leaked onto y)", got)
	}
	if got := tokenCount(t, conn, y, store.DiscardEncryptionRateLimitSurface); got != 1 {
		t.Errorf("y discard counter = %d, want 1", got)
	}
	if got := tokenCount(t, conn, z, store.RequestEncryptionRateLimitSurface); got != 0 {
		t.Errorf("z request counter = %d, want 0 (accepts are not budgeted)", got)
	}
	if got := tokenCount(t, conn, z, store.DiscardEncryptionRateLimitSurface); got != 0 {
		t.Errorf("z discard counter = %d, want 0", got)
	}
	if got := tokenCount(t, conn, w, store.RequestEncryptionRateLimitSurface); got != 1 {
		t.Errorf("w request counter = %d, want 1", got)
	}
}

// TestRequestEncryptionRateLimitConcurrentAcrossStores proves the bound holds
// across replicas: two independently opened stores, one account, k tokens left.
// Exactly k requests commit, the rest are refused, and the counter lands on the
// limit rather than above it.
func TestRequestEncryptionRateLimitConcurrentAcrossStores(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	x, y := twoUsersFor(t, s, "+15551390021", "+15551390022")

	// A second store on the same database stands in for a second replica: its own
	// pool and connections, the same Postgres clock and the same counters.
	replica, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithBlobStore(testBlobs(t)))
	if err != nil {
		t.Fatalf("open replica: %v", err)
	}
	t.Cleanup(func() {
		if err := replica.Close(); err != nil {
			t.Errorf("close replica: %v", err)
		}
	})

	budget := store.RateLimitConfig{Limit: 4, Window: time.Hour}
	conn := secretChatConn(t, dsn)

	// Spend two tokens first, so exactly two of the concurrent burst can win.
	requestOK(t, s, x, y, budget, 1)
	requestOK(t, s, x, y, budget, 2)

	const burst = 8
	stores := []*store.Store{s, replica}
	results := make([]error, burst)
	var wg sync.WaitGroup
	ready := make(chan struct{})
	wg.Add(burst)
	for i := range burst {
		go func(i int) {
			defer wg.Done()
			<-ready
			_, results[i] = api.RequestEncryptionWithBudgetForTest(stores[i%2], x, budget, &tg.MessagesRequestEncryptionRequest{
				UserID: api.InputUser(x, y), RandomID: 100 + i, GA: validGA(),
			})
		}(i)
	}
	close(ready)
	wg.Wait()

	var succeeded, flooded int
	for i, err := range results {
		switch {
		case err == nil:
			succeeded++
		case isFloodWait(err):
			flooded++
		default:
			t.Errorf("concurrent request %d: unexpected error %v", i, err)
		}
	}
	if succeeded != 2 {
		t.Errorf("concurrent successes = %d, want 2 (the tokens that were left)", succeeded)
	}
	if flooded != burst-2 {
		t.Errorf("concurrent FLOOD_WAITs = %d, want %d", flooded, burst-2)
	}
	if got := secretChatRows(t, conn, x); got != budget.Limit {
		t.Errorf("secret_chats rows for %d = %d, want %d (the burst overshot the budget)", x, got, budget.Limit)
	}
	if got := tokenCount(t, conn, x, store.RequestEncryptionRateLimitSurface); got != budget.Limit {
		t.Errorf("request counter = %d, want %d", got, budget.Limit)
	}
}

// TestRequestEncryptionRateLimitPreservesPrecedenceAtExhaustion pins the order.
// Everything that refused a request before this change still refuses it the same
// way, and still without spending budget: handler validation, the outstanding cap,
// and random_id dedup. The dedup case matters most: a client retrying a request
// it already made must succeed at an exhausted budget, because it creates nothing.
func TestRequestEncryptionRateLimitPreservesPrecedenceAtExhaustion(t *testing.T) {
	t.Parallel()
	s, dsn := openStoreDSN(t)
	x, y := twoUsersFor(t, s, "+15551390031", "+15551390032")

	// The budget equals the outstanding cap, so once it is filled the account is
	// simultaneously out of tokens and out of cap, and the two answers stay
	// distinguishable: PEER_FLOOD for the cap, FLOOD_WAIT for the budget.
	budget := store.RateLimitConfig{Limit: store.MaxOutstandingSecretChats, Window: time.Hour}
	conn := secretChatConn(t, dsn)

	var first *tg.EncryptedChatWaiting
	for i := range store.MaxOutstandingSecretChats {
		w := requestOK(t, s, x, y, budget, 500+i)
		if i == 0 {
			first = w
		}
	}
	spent := tokenCount(t, conn, x, store.RequestEncryptionRateLimitSurface)
	if spent != store.MaxOutstandingSecretChats {
		t.Fatalf("request counter = %d, want %d", spent, store.MaxOutstandingSecretChats)
	}

	// The cap is checked before the charge, so a full account still gets the error
	// it got before, and the counter does not move.
	_, err := api.RequestEncryptionWithBudgetForTest(s, x, budget, &tg.MessagesRequestEncryptionRequest{
		UserID: api.InputUser(x, y), RandomID: 600, GA: validGA(),
	})
	if got := rpcMessage(t, err); got != "PEER_FLOOD" {
		t.Errorf("request at the outstanding cap = %s, want PEER_FLOOD", got)
	}
	// Handler validation runs ahead of all of it: an unknown peer and a g_a
	// outside the group are refused before the budget is consulted.
	_, err = api.RequestEncryptionWithBudgetForTest(s, x, budget, &tg.MessagesRequestEncryptionRequest{
		UserID: api.InputUser(x, 987654321), RandomID: 601, GA: validGA(),
	})
	if got := rpcMessage(t, err); got != "USER_ID_INVALID" {
		t.Errorf("request to an unknown user = %s, want USER_ID_INVALID", got)
	}
	_, err = api.RequestEncryptionWithBudgetForTest(s, x, budget, &tg.MessagesRequestEncryptionRequest{
		UserID: api.InputUser(x, y), RandomID: 602, GA: []byte("not a group element"),
	})
	if got := rpcMessage(t, err); got != "DH_G_A_INVALID" {
		t.Errorf("request with an invalid g_a = %s, want DH_G_A_INVALID", got)
	}
	// The retry of an accepted request succeeds at an exhausted budget and returns
	// the original chat.
	enc, err := api.RequestEncryptionWithBudgetForTest(s, x, budget, &tg.MessagesRequestEncryptionRequest{
		UserID: api.InputUser(x, y), RandomID: 500, GA: validGA(),
	})
	if err != nil {
		t.Fatalf("dedup retry at an exhausted budget: %v", err)
	}
	replay, ok := enc.(*tg.EncryptedChatWaiting)
	if !ok {
		t.Fatalf("dedup retry type = %T, want *tg.EncryptedChatWaiting", enc)
	}
	if replay.ID != first.ID {
		t.Errorf("dedup retry returned chat %d, want the original %d", replay.ID, first.ID)
	}

	if got := tokenCount(t, conn, x, store.RequestEncryptionRateLimitSurface); got != spent {
		t.Errorf("request counter after the refusals and the retry = %d, want it unchanged at %d", got, spent)
	}
	if got := secretChatRows(t, conn, x); got != store.MaxOutstandingSecretChats {
		t.Errorf("secret_chats rows = %d, want %d", got, store.MaxOutstandingSecretChats)
	}
}

// TestRequestEncryptionRateLimitStorageFailureRollsBack shows the budget
// fails closed: when the limiter itself cannot be consulted the request is refused
// outright rather than running unbudgeted, and it leaves no row and no id behind.
func TestRequestEncryptionRateLimitStorageFailureRollsBack(t *testing.T) {
	t.Parallel()
	s, dsn := openStoreDSN(t)
	x, y := twoUsersFor(t, s, "+15551390041", "+15551390042")

	budget := store.RateLimitConfig{Limit: 5, Window: time.Hour}
	conn := secretChatConn(t, dsn)
	requestOK(t, s, x, y, budget, 1)
	rowsBefore := secretChatRows(t, conn, x)
	seqBefore := sequenceLastValue(t, conn)

	// Take the limiter's table away. This is a real Postgres failure on the charge,
	// not an injected one: the statement fails when the server plans it.
	if _, err := conn.Exec(context.Background(), `ALTER TABLE rate_limits RENAME TO rate_limits_removed`); err != nil {
		t.Fatalf("break limiter: %v", err)
	}

	_, err := api.RequestEncryptionWithBudgetForTest(s, x, budget, &tg.MessagesRequestEncryptionRequest{
		UserID: api.InputUser(x, y), RandomID: 2, GA: validGA(),
	})
	if got := rpcMessage(t, err); got != "INTERNAL" {
		t.Fatalf("request with the limiter unavailable = %s, want INTERNAL", got)
	}
	if got := secretChatRows(t, conn, x); got != rowsBefore {
		t.Errorf("secret_chats rows = %d, want %d (a failed charge wrote a row)", got, rowsBefore)
	}
	if got := sequenceLastValue(t, conn); got != seqBefore {
		t.Errorf("chat id sequence = %d, want %d (a failed charge consumed an id)", got, seqBefore)
	}
}

// TestDiscardEncryptionRateLimitBoundsDiscards drives the discard budget on its
// own and pins what a refusal must not do: no state change, no re-dating, no push,
// no counter movement. It also pins the two calls that must stay free: a
// non-party's attempt, refused by membership before any budget is consulted, and a
// discard of a chat that is already discarded, which is an idempotent success.
func TestDiscardEncryptionRateLimitBoundsDiscards(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	a, b := twoUsersFor(t, s, "+15551390051", "+15551390052")
	outsider := mustCreateUser(t, s, "+15551390053")

	budget := store.RateLimitConfig{Limit: 2, Window: time.Hour}
	pushes := encryptionPushCounter(t, dsn, b)
	conn := secretChatConn(t, dsn)

	chats := []*tg.EncryptedChatWaiting{
		requestOK(t, s, a, b, unbudgeted, 1),
		requestOK(t, s, a, b, unbudgeted, 2),
		requestOK(t, s, a, b, unbudgeted, 3),
	}

	discardOK(t, s, a, budget, chats[0].ID)
	discardOK(t, s, a, budget, chats[1].ID)
	if got := tokenCount(t, conn, a, store.DiscardEncryptionRateLimitSurface); got != 2 {
		t.Fatalf("discard counter = %d, want 2", got)
	}

	// The third discard is refused, and its chat must be exactly as it was.
	thirdID := int32(chats[2].ID) //nolint:gosec // test id
	dateBefore := chatDate(t, conn, thirdID)
	if _, err := api.DiscardEncryptionWithBudgetForTest(s, a, budget,
		&tg.MessagesDiscardEncryptionRequest{ChatID: chats[2].ID}); !isFloodWait(err) {
		t.Fatalf("third discard: expected FLOOD_WAIT, got %v", err)
	}
	chat, err := s.SecretChatByID(ctx, thirdID)
	if err != nil {
		t.Fatalf("load refused chat: %v", err)
	}
	if chat.State != store.SecretChatRequested {
		t.Errorf("refused chat state = %q, want %q (a refused discard changed state)", chat.State, store.SecretChatRequested)
	}
	if got := chatDate(t, conn, thirdID); !got.Equal(dateBefore) {
		t.Errorf("refused chat date = %v, want it unchanged at %v", got, dateBefore)
	}

	// A non-party is refused by membership, not by the budget, and pays nothing.
	_, err = api.DiscardEncryptionWithBudgetForTest(s, outsider, budget,
		&tg.MessagesDiscardEncryptionRequest{ChatID: chats[2].ID})
	if got := rpcMessage(t, err); got != "ENCRYPTION_ID_INVALID" {
		t.Errorf("non-party discard = %s, want ENCRYPTION_ID_INVALID", got)
	}
	// Discarding an already-discarded chat is an idempotent success at an
	// exhausted budget, and it is not charged.
	if _, err := api.DiscardEncryptionWithBudgetForTest(s, a, budget,
		&tg.MessagesDiscardEncryptionRequest{ChatID: chats[0].ID}); err != nil {
		t.Errorf("replay of an accepted discard at an exhausted budget: %v", err)
	}
	if got := tokenCount(t, conn, a, store.DiscardEncryptionRateLimitSurface); got != 2 {
		t.Errorf("discard counter after the refusals and the replay = %d, want it unchanged at 2", got)
	}
	if got := tokenCount(t, conn, outsider, store.DiscardEncryptionRateLimitSurface); got != 0 {
		t.Errorf("outsider discard counter = %d, want 0", got)
	}

	// Every accepted request pushed once to b, and each of the two accepted
	// discards pushed once more. The refused third discard is the point: its chat
	// was pushed for the request and for nothing after it. The replay of
	// an already-discarded chat added nothing either, and the non-party attempt
	// added nothing.
	awaitPushes(t, pushes, 5)
	time.Sleep(100 * time.Millisecond)
	if got := pushes.total(); got != 5 {
		t.Fatalf("encryption notifications to %d = %d, want 5 (a refused or replayed discard pushed)", b, got)
	}
	for _, chat := range chats[:2] {
		if got := pushes.forChat(int64(chat.ID)); got != 2 {
			t.Errorf("discarded chat %d was pushed %d times, want 2 (request and discard)", chat.ID, got)
		}
	}
	if got := pushes.forChat(int64(chats[2].ID)); got != 1 {
		t.Errorf("refused chat %d was pushed %d times, want 1 (only its request)", chats[2].ID, got)
	}
}

// TestDiscardEncryptionRateLimitConcurrentPartiesChargeOnce covers the case the
// guarded transition and the in-transaction charge have to agree on: both parties
// discard the same chat at the same time, through two replicas. Both calls are
// answered successfully: one wins the transition, the other gets the idempotent
// replay. Exactly one of them is charged.
func TestDiscardEncryptionRateLimitConcurrentPartiesChargeOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	a, b := twoUsersFor(t, s, "+15551390061", "+15551390062")

	replica, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithBlobStore(testBlobs(t)))
	if err != nil {
		t.Fatalf("open replica: %v", err)
	}
	t.Cleanup(func() {
		if err := replica.Close(); err != nil {
			t.Errorf("close replica: %v", err)
		}
	})

	waiting := requestOK(t, s, a, b, unbudgeted, 1)
	id := int32(waiting.ID) //nolint:gosec // test id
	acceptOK(t, s, b, id, 11)

	budget := store.RateLimitConfig{Limit: 1, Window: time.Hour}
	callers := []int64{a, b}
	stores := []*store.Store{s, replica}
	results := make([]error, 2)
	var wg sync.WaitGroup
	ready := make(chan struct{})
	wg.Add(2)
	for i := range 2 {
		go func(i int) {
			defer wg.Done()
			<-ready
			_, results[i] = api.DiscardEncryptionWithBudgetForTest(stores[i], callers[i], budget,
				&tg.MessagesDiscardEncryptionRequest{ChatID: int(id)})
		}(i)
	}
	close(ready)
	wg.Wait()

	for i, err := range results {
		if err != nil {
			t.Errorf("concurrent discard by %d: %v (both parties must be answered successfully)", callers[i], err)
		}
	}
	conn := secretChatConn(t, dsn)
	chargedA := tokenCount(t, conn, a, store.DiscardEncryptionRateLimitSurface)
	chargedB := tokenCount(t, conn, b, store.DiscardEncryptionRateLimitSurface)
	if chargedA+chargedB != 1 {
		t.Fatalf("discard charges: a=%d b=%d, want exactly one charged", chargedA, chargedB)
	}
	chat, err := s.SecretChatByID(ctx, id)
	if err != nil {
		t.Fatalf("load chat: %v", err)
	}
	if chat.State != store.SecretChatDiscarded {
		t.Errorf("chat state = %q, want %q", chat.State, store.SecretChatDiscarded)
	}
}

// TestDiscardEncryptionRateLimitStorageFailureRollsBack is the discard half of
// fail-closed: with the limiter unavailable the transition is refused, so state
// and date stay exactly as they were.
func TestDiscardEncryptionRateLimitStorageFailureRollsBack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	a, b := twoUsersFor(t, s, "+15551390071", "+15551390072")

	budget := store.RateLimitConfig{Limit: 5, Window: time.Hour}
	waiting := requestOK(t, s, a, b, unbudgeted, 1)
	id := int32(waiting.ID) //nolint:gosec // test id
	acceptOK(t, s, b, id, 12)

	conn := secretChatConn(t, dsn)
	dateBefore := chatDate(t, conn, id)
	if _, err := conn.Exec(context.Background(), `ALTER TABLE rate_limits RENAME TO rate_limits_removed`); err != nil {
		t.Fatalf("break limiter: %v", err)
	}

	_, err := api.DiscardEncryptionWithBudgetForTest(s, a, budget, &tg.MessagesDiscardEncryptionRequest{ChatID: int(id)})
	if got := rpcMessage(t, err); got != "INTERNAL" {
		t.Fatalf("discard with the limiter unavailable = %s, want INTERNAL", got)
	}
	chat, err := s.SecretChatByID(ctx, id)
	if err != nil {
		t.Fatalf("load chat: %v", err)
	}
	if chat.State != store.SecretChatActive {
		t.Errorf("chat state = %q, want %q (a refused discard changed state)", chat.State, store.SecretChatActive)
	}
	if got := chatDate(t, conn, id); !got.Equal(dateBefore) {
		t.Errorf("chat date = %v, want it unchanged at %v", got, dateBefore)
	}
}

// TestRequestEncryptionRateLimitDenialRecordsMetric and
// TestDiscardEncryptionRateLimitDenialRecordsMetric pin that each new surface is
// attributed to itself in the denial telemetry rather than landing in the
// unattributed bucket.
func TestRequestEncryptionRateLimitDenialRecordsMetric(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	a, b := twoUsersFor(t, s, "+15551390081", "+15551390082")

	metrics := store.NewNotificationMetrics()
	budget := store.RateLimitConfig{Limit: 1, Window: time.Hour}

	requestOK(t, s, a, b, budget, 1)
	if _, err := api.RequestEncryptionWithBudgetAndMetricsForTest(s, metrics, a, budget, &tg.MessagesRequestEncryptionRequest{
		UserID: api.InputUser(a, b), RandomID: 2, GA: validGA(),
	}); !isFloodWait(err) {
		t.Fatalf("second request by a: expected FLOOD_WAIT, got %v", err)
	}

	got := metrics.Snapshot().RateLimitDenials
	want := store.RateLimitDenialSurfaceCounts{SecretChatRequest: 1}
	if got.Count != 1 || got.BySurface != want || got.Dropped != 0 {
		t.Fatalf("denial snapshot = %+v, want count 1, surfaces %+v, dropped 0", got, want)
	}
}

func TestDiscardEncryptionRateLimitDenialRecordsMetric(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	a, b := twoUsersFor(t, s, "+15551390082", "+15551390083")

	metrics := store.NewNotificationMetrics()
	budget := store.RateLimitConfig{Limit: 1, Window: time.Hour}

	own := requestOK(t, s, a, b, unbudgeted, 1)
	discardOK(t, s, a, budget, own.ID)
	// A chat b initiated, which a is entitled to discard but can no longer afford.
	inbound := requestOK(t, s, b, a, unbudgeted, 2)
	if _, err := api.DiscardEncryptionWithBudgetAndMetricsForTest(s, metrics, a, budget,
		&tg.MessagesDiscardEncryptionRequest{ChatID: inbound.ID}); !isFloodWait(err) {
		t.Fatalf("second discard by a: expected FLOOD_WAIT, got %v", err)
	}

	got := metrics.Snapshot().RateLimitDenials
	want := store.RateLimitDenialSurfaceCounts{SecretChatDiscard: 1}
	if got.Count != 1 || got.BySurface != want || got.Dropped != 0 {
		t.Fatalf("denial snapshot = %+v, want count 1, surfaces %+v, dropped 0", got, want)
	}
}

// assertChargeFailureLogged requires exactly one error record naming the account,
// the surface and the underlying database failure. The client only ever sees
// INTERNAL, so the server log is the only place a fail-closed outage is
// diagnosable.
func assertChargeFailureLogged(t *testing.T, h *captureHandler, account int64, surface, wantErr string) {
	t.Helper()
	var records []slog.Record
	for _, r := range h.records {
		if r.Level == slog.LevelError {
			records = append(records, r)
		}
	}
	if len(records) != 1 {
		t.Fatalf("captured %d error records, want exactly one", len(records))
	}
	attrs := map[string]any{}
	records[0].Attrs(func(a slog.Attr) bool { attrs[a.Key] = a.Value.Any(); return true })
	if got := attrs["user_id"]; got != account {
		t.Errorf("log user_id = %v, want %d", got, account)
	}
	if got := attrs["surface"]; got != surface {
		t.Errorf("log surface = %v, want %q", got, surface)
	}
	if errText := fmt.Sprint(attrs["err"]); !strings.Contains(errText, wantErr) {
		t.Errorf("log err = %q, want it to carry the database failure %q", errText, wantErr)
	}
}

// TestRequestEncryptionRateLimitStorageFailureIsLogged and
// TestDiscardEncryptionRateLimitStorageFailureIsLogged pin the other half of
// fail-closed: the refusal is silent to the client, so the server has to
// record which account, which surface and which database failure caused it. A
// named check constraint makes the underlying error identifiable in the record.
func TestRequestEncryptionRateLimitStorageFailureIsLogged(t *testing.T) {
	t.Parallel()
	s, dsn := openStoreDSN(t)
	x, y := twoUsersFor(t, s, "+15551390091", "+15551390092")

	budget := store.RateLimitConfig{Limit: 5, Window: time.Hour}
	conn := secretChatConn(t, dsn)
	if _, err := conn.Exec(context.Background(),
		`ALTER TABLE rate_limits ADD CONSTRAINT request_charge_failure CHECK (token_count < 0)`); err != nil {
		t.Fatalf("break limiter: %v", err)
	}

	logs := &captureHandler{}
	_, err := api.RequestEncryptionWithBudgetAndLoggerForTest(s, slog.New(logs), x, budget,
		&tg.MessagesRequestEncryptionRequest{UserID: api.InputUser(x, y), RandomID: 1, GA: validGA()})
	if got := rpcMessage(t, err); got != "INTERNAL" {
		t.Fatalf("request with the limiter unavailable = %s, want INTERNAL", got)
	}
	assertChargeFailureLogged(t, logs, x, store.RequestEncryptionRateLimitSurface, "request_charge_failure")
}

func TestDiscardEncryptionRateLimitStorageFailureIsLogged(t *testing.T) {
	t.Parallel()
	s, dsn := openStoreDSN(t)
	a, b := twoUsersFor(t, s, "+15551390101", "+15551390102")

	waiting := requestOK(t, s, a, b, unbudgeted, 1)
	budget := store.RateLimitConfig{Limit: 5, Window: time.Hour}
	conn := secretChatConn(t, dsn)
	if _, err := conn.Exec(context.Background(),
		`ALTER TABLE rate_limits ADD CONSTRAINT discard_charge_failure CHECK (token_count < 0)`); err != nil {
		t.Fatalf("break limiter: %v", err)
	}

	logs := &captureHandler{}
	_, err := api.DiscardEncryptionWithBudgetAndLoggerForTest(s, slog.New(logs), a, budget,
		&tg.MessagesDiscardEncryptionRequest{ChatID: waiting.ID})
	if got := rpcMessage(t, err); got != "INTERNAL" {
		t.Fatalf("discard with the limiter unavailable = %s, want INTERNAL", got)
	}
	assertChargeFailureLogged(t, logs, a, store.DiscardEncryptionRateLimitSurface, "discard_charge_failure")
}

// mustCreateUser is the single-account form of twoUsersFor.
func mustCreateUser(t *testing.T, s *store.Store, phone string) int64 {
	t.Helper()
	u, err := s.CreateUser(context.Background(), phone)
	if err != nil {
		t.Fatalf("create user %s: %v", phone, err)
	}
	return u.ID
}
