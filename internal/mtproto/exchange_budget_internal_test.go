package mtproto

import (
	"context"
	crand "crypto/rand"
	"crypto/rsa"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/crypto"
	"github.com/gotd/td/exchange"
	"github.com/gotd/td/mt"
	"github.com/gotd/td/proto"
	"github.com/gotd/td/transport"

	"github.com/teagramhq/teagram-server/internal/store"
)

type exchangeBudgetTestStore struct {
	mu           sync.Mutex
	keys         map[[8]byte]crypto.AuthKey
	rateResult   *store.RateLimitResult
	rateErr      error
	rateCalls    int
	rateAddrs    []netip.Addr
	rateSurfaces []string
	rateConfigs  []store.RateLimitConfig
	saves        int
}

func (s *exchangeBudgetTestStore) Save(_ context.Context, key crypto.AuthKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.keys == nil {
		s.keys = make(map[[8]byte]crypto.AuthKey)
	}
	s.keys[key.ID] = key
	s.saves++
	return nil
}

func (s *exchangeBudgetTestStore) Get(_ context.Context, id [8]byte, _ time.Duration) (crypto.AuthKey, int64, bool, PendingLogin, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key, ok := s.keys[id]
	return key, 0, false, PendingLogin{}, ok, nil
}

func (*exchangeBudgetTestStore) Touch(context.Context, [8]byte) error { return nil }

func (s *exchangeBudgetTestStore) CheckIPRateLimit(
	_ context.Context,
	addr netip.Addr,
	surface string,
	cfg store.RateLimitConfig,
) (*store.RateLimitResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rateCalls++
	s.rateAddrs = append(s.rateAddrs, addr)
	s.rateSurfaces = append(s.rateSurfaces, surface)
	s.rateConfigs = append(s.rateConfigs, cfg)
	return s.rateResult, s.rateErr
}

func TestExchangeBudgetRefusesBeforeExchange(t *testing.T) {
	keys := &exchangeBudgetTestStore{rateResult: &store.RateLimitResult{Wait: time.Hour}}
	rsaKey, err := rsa.GenerateKey(crand.Reader, crypto.RSAKeyBits)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	server := New(exchange.PrivateKey{RSA: rsaKey}, 2, keys, nil, nil)
	limits := DefaultExchangeLimits()
	limits.TailnetPerNetwork = limits.PerNetwork
	if err := server.SetExchangeLimits(limits); err != nil {
		t.Fatalf("set key exchange limits: %v", err)
	}
	conn := &exchangeScriptConn{}
	first := exchangeBudgetRequestFrame(t)

	if err := server.runExchange(context.Background(), conn, first, netip.MustParseAddr("192.0.2.8")); err == nil {
		t.Fatal("runExchange succeeded after the network budget refused it")
	}
	keys.mu.Lock()
	rateCalls, saves := keys.rateCalls, keys.saves
	keys.mu.Unlock()
	if rateCalls != 1 {
		t.Fatalf("network budget calls = %d, want one before exchange", rateCalls)
	}
	if saves != 0 {
		t.Fatalf("saved auth keys = %d, want zero after refusal", saves)
	}
	var want bin.Buffer
	want.PutInt32(-429)
	if len(conn.sent) != 1 || string(conn.sent[0]) != string(want.Buf) {
		t.Fatalf("refusal replies = %v, want only -429", conn.sent)
	}
}

func TestExchangeBudgetServerRefusesWhenRateLimitStoreFails(t *testing.T) {
	keys := &exchangeBudgetTestStore{rateErr: context.DeadlineExceeded}
	rsaKey, err := rsa.GenerateKey(crand.Reader, crypto.RSAKeyBits)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	server := New(exchange.PrivateKey{RSA: rsaKey}, 2, keys, nil, nil)
	limits := DefaultExchangeLimits()
	limits.TailnetPerNetwork = limits.PerNetwork
	if err := server.SetExchangeLimits(limits); err != nil {
		t.Fatalf("set key exchange limits: %v", err)
	}
	conn := &exchangeScriptConn{}
	first := exchangeBudgetRequestFrame(t)

	if err := server.runExchange(context.Background(), conn, first, netip.MustParseAddr("192.0.2.8")); err == nil {
		t.Fatal("runExchange succeeded after the network budget store failed")
	}
	keys.mu.Lock()
	rateCalls := keys.rateCalls
	keys.mu.Unlock()
	if rateCalls != 1 {
		t.Fatalf("network budget calls = %d, want one", rateCalls)
	}
	var want bin.Buffer
	want.PutInt32(-429)
	if len(conn.sent) != 1 || string(conn.sent[0]) != string(want.Buf) {
		t.Fatalf("store failure replies = %v, want only -429", conn.sent)
	}
}

func TestExchangeBudgetGlobalCeilingSends429BeforeExchange(t *testing.T) {
	keys := &exchangeBudgetTestStore{}
	rsaKey, err := rsa.GenerateKey(crand.Reader, crypto.RSAKeyBits)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	server := New(exchange.PrivateKey{RSA: rsaKey}, 2, keys, nil, nil)
	limits := ExchangeLimits{GlobalRate: 1, GlobalBurst: 1}
	if err := server.SetExchangeLimits(limits); err != nil {
		t.Fatalf("set key exchange limits: %v", err)
	}
	if !server.exchangeBudget.ceiling.take(time.Now()) {
		t.Fatal("could not exhaust global exchange ceiling")
	}
	conn := &exchangeScriptConn{}
	first := exchangeBudgetRequestFrame(t)

	if err := server.runExchange(context.Background(), conn, first, netip.MustParseAddr("192.0.2.9")); err == nil {
		t.Fatal("runExchange succeeded after the global ceiling was exhausted")
	}
	keys.mu.Lock()
	rateCalls, saves := keys.rateCalls, keys.saves
	keys.mu.Unlock()
	if rateCalls != 0 || saves != 0 {
		t.Fatalf("network checks=%d saved keys=%d, want both zero after global refusal", rateCalls, saves)
	}
	var want bin.Buffer
	want.PutInt32(-429)
	if len(conn.sent) != 1 || string(conn.sent[0]) != string(want.Buf) {
		t.Fatalf("global refusal replies = %v, want only -429", conn.sent)
	}
}

func TestExchangeBudgetChargesLayersInOrder(t *testing.T) {
	network := store.RateLimitConfig{Limit: 1, Window: time.Hour}
	limits := ExchangeLimits{
		PerConnection:     1,
		PerNetwork:        network,
		TailnetPerNetwork: network,
		GlobalRate:        1,
		GlobalBurst:       1,
	}
	budget := newKeyExchangeBudget(limits)
	keys := &exchangeBudgetTestStore{rateResult: &store.RateLimitResult{Wait: time.Hour}}
	var attempts int
	addr := netip.MustParseAddr("192.0.2.44")
	now := time.Now()

	layer, err := budget.charge(context.Background(), keys, &attempts, addr, store.RateLimitListenerTailnet, now)
	if layer != "network" || err != nil {
		t.Fatalf("first refusal = (%q, %v), want network budget", layer, err)
	}
	if budget.ceiling.tokens != 1 {
		t.Fatalf("global tokens after network refusal = %v, want 1 untouched token", budget.ceiling.tokens)
	}
	layer, err = budget.charge(context.Background(), keys, &attempts, addr, store.RateLimitListenerTailnet, now)
	if layer != "connection" || err != nil {
		t.Fatalf("second refusal = (%q, %v), want per-connection budget", layer, err)
	}
	keys.mu.Lock()
	rateCalls := keys.rateCalls
	keys.mu.Unlock()
	if rateCalls != 1 {
		t.Fatalf("network budget calls after connection refusal = %d, want one", rateCalls)
	}
	if budget.ceiling.tokens != 1 {
		t.Fatalf("global tokens after earlier-layer refusal = %v, want 1 untouched token", budget.ceiling.tokens)
	}

	globalOnlyAfterNetwork := newKeyExchangeBudget(ExchangeLimits{
		PerNetwork:        network,
		TailnetPerNetwork: network,
		GlobalRate:        1,
		GlobalBurst:       1,
	})
	keys = &exchangeBudgetTestStore{}
	if layer, err := globalOnlyAfterNetwork.charge(context.Background(), keys, nil, addr, store.RateLimitListenerTailnet, now); layer != "" || err != nil {
		t.Fatalf("first global attempt = (%q, %v), want admitted", layer, err)
	}
	if layer, err := globalOnlyAfterNetwork.charge(context.Background(), keys, nil, addr, store.RateLimitListenerTailnet, now); layer != "global" || err != nil {
		t.Fatalf("second global attempt = (%q, %v), want global ceiling refusal", layer, err)
	}
	keys.mu.Lock()
	rateCalls = keys.rateCalls
	keys.mu.Unlock()
	if rateCalls != 2 {
		t.Fatalf("network budget calls before global refusal = %d, want two", rateCalls)
	}
}

func TestExchangeBudgetWithoutKeyableAddressSkipsOnlyNetworkLayer(t *testing.T) {
	network := store.RateLimitConfig{Limit: 1, Window: time.Hour}
	budget := newKeyExchangeBudget(ExchangeLimits{
		PerConnection:     1,
		PerNetwork:        network,
		TailnetPerNetwork: network,
		GlobalRate:        1,
		GlobalBurst:       1,
	})
	keys := &exchangeBudgetTestStore{}
	var attempts int
	layer, err := budget.charge(context.Background(), keys, &attempts, netip.Addr{}, store.RateLimitListenerTailnet, time.Now())
	if layer != "" || err != nil {
		t.Fatalf("unkeyable-address attempt = (%q, %v), want admitted", layer, err)
	}
	keys.mu.Lock()
	rateCalls := keys.rateCalls
	keys.mu.Unlock()
	if rateCalls != 0 {
		t.Fatalf("network budget calls = %d, want skip only this layer", rateCalls)
	}
	if budget.ceiling.tokens != 0 {
		t.Fatalf("global tokens after unkeyable-address attempt = %v, want 0", budget.ceiling.tokens)
	}
}

func TestKeyExchangeListenerBudgetsUseSeparateSurfaces(t *testing.T) {
	tailnet, ok := keyExchangeSurface(store.RateLimitListenerTailnet)
	if !ok {
		t.Fatal("tailnet listener has no key exchange surface")
	}
	public, ok := keyExchangeSurface(store.RateLimitListenerPublic)
	if !ok {
		t.Fatal("public listener has no key exchange surface")
	}
	if tailnet == public {
		t.Fatalf("tailnet and public listener surfaces are both %q", tailnet)
	}
}

func TestExchangeBudgetUsesSeparateListenerNetworkCounters(t *testing.T) {
	perNetwork := store.RateLimitConfig{Limit: 20, Window: time.Hour}
	limits := ExchangeLimits{PerNetwork: perNetwork, TailnetPerNetwork: perNetwork}
	budget := newKeyExchangeBudget(limits)
	keys := &exchangeBudgetTestStore{}
	addr := netip.MustParseAddr("2001:db8:1::3")
	for _, listener := range []store.RateLimitListener{store.RateLimitListenerTailnet, store.RateLimitListenerPublic} {
		if layer, err := budget.charge(context.Background(), keys, nil, addr, listener, time.Now()); layer != "" || err != nil {
			t.Fatalf("listener %d budget = (%q, %v), want admitted", listener, layer, err)
		}
	}
	keys.mu.Lock()
	defer keys.mu.Unlock()
	if keys.rateCalls != 2 {
		t.Fatalf("network budget calls = %d, want one per listener", keys.rateCalls)
	}
	if keys.rateSurfaces[0] != keyExchangeTailnetSurface || keys.rateSurfaces[1] != keyExchangePublicSurface {
		t.Fatalf("listener surfaces = %v, want tailnet then public", keys.rateSurfaces)
	}
	if keys.rateAddrs[0] != addr || keys.rateAddrs[1] != addr {
		t.Fatalf("network addresses = %v, want %s for both listeners", keys.rateAddrs, addr)
	}
	if keys.rateConfigs[0] != limits.PerNetwork || keys.rateConfigs[1] != limits.PerNetwork {
		t.Fatalf("network configs = %v, want %v for both listeners", keys.rateConfigs, limits.PerNetwork)
	}
}

func TestDisabledTailnetNetworkBudgetAllowsOverTwentyWhilePublicStillRefuses(t *testing.T) {
	addr := netip.MustParseAddr("192.0.2.51")
	now := time.Now()
	limits := DefaultExchangeLimits()
	limits.TailnetPerNetwork.Limit = 0
	tailnetBudget := newKeyExchangeBudget(limits)
	tailnetKeys := &memoryAuthKeyStore{}
	for attempt := 1; attempt <= 21; attempt++ {
		var connectionAttempts int
		layer, err := tailnetBudget.charge(context.Background(), tailnetKeys, &connectionAttempts, addr, store.RateLimitListenerTailnet, now)
		if layer != "" || err != nil {
			t.Fatalf("tailnet attempt %d = (%q, %v), want admitted beyond the public default", attempt, layer, err)
		}
	}

	publicBudget := newKeyExchangeBudget(limits)
	publicKeys := &memoryAuthKeyStore{}
	for attempt := 1; attempt <= 21; attempt++ {
		var connectionAttempts int
		layer, err := publicBudget.charge(context.Background(), publicKeys, &connectionAttempts, addr, store.RateLimitListenerPublic, now)
		if attempt < 21 && (layer != "" || err != nil) {
			t.Fatalf("public attempt %d = (%q, %v), want admitted", attempt, layer, err)
		}
		if attempt == 21 && (layer != "network" || err != nil) {
			t.Fatalf("public attempt %d = (%q, %v), want network refusal", attempt, layer, err)
		}
	}
}

func TestDefaultExchangeBudgetDisablesTailnetNetworkLimitOnly(t *testing.T) {
	addr := netip.MustParseAddr("192.0.2.54")
	now := time.Now()
	for _, listener := range []store.RateLimitListener{store.RateLimitListenerTailnet, store.RateLimitListenerPublic} {
		budget := newKeyExchangeBudget(DefaultExchangeLimits())
		keys := &memoryAuthKeyStore{}
		for attempt := 1; attempt <= 21; attempt++ {
			var connectionAttempts int
			layer, err := budget.charge(context.Background(), keys, &connectionAttempts, addr, listener, now)
			if listener == store.RateLimitListenerTailnet && (layer != "" || err != nil) {
				t.Fatalf("tailnet attempt %d = (%q, %v), want admitted without a network limit", attempt, layer, err)
			}
			if listener == store.RateLimitListenerPublic && attempt < 21 && (layer != "" || err != nil) {
				t.Fatalf("listener %d attempt %d = (%q, %v), want admitted", listener, attempt, layer, err)
			}
			if listener == store.RateLimitListenerPublic && attempt == 21 && (layer != "network" || err != nil) {
				t.Fatalf("listener %d attempt %d = (%q, %v), want network refusal", listener, attempt, layer, err)
			}
		}
	}
}

func TestExplicitTailnetNetworkLimitAppliesOnlyToTailnetListener(t *testing.T) {
	limits := DefaultExchangeLimits()
	limits.PerConnection = 0
	limits.TailnetPerNetwork.Limit = 1
	addr := netip.MustParseAddr("192.0.2.52")
	now := time.Now()
	keys := &memoryAuthKeyStore{}
	budget := newKeyExchangeBudget(limits)

	if layer, err := budget.charge(context.Background(), keys, nil, addr, store.RateLimitListenerTailnet, now); layer != "" || err != nil {
		t.Fatalf("first tailnet attempt = (%q, %v), want admitted", layer, err)
	}
	if layer, err := budget.charge(context.Background(), keys, nil, addr, store.RateLimitListenerTailnet, now); layer != "network" || err != nil {
		t.Fatalf("second tailnet attempt = (%q, %v), want network refusal", layer, err)
	}
	if layer, err := budget.charge(context.Background(), keys, nil, addr, store.RateLimitListenerPublic, now); layer != "" || err != nil {
		t.Fatalf("first public attempt = (%q, %v), want its independent budget admitted", layer, err)
	}
}

func TestTailnetWithoutNetworkBudgetKeepsConnectionAndGlobalCaps(t *testing.T) {
	limits := DefaultExchangeLimits()
	addr := netip.MustParseAddr("192.0.2.53")
	now := time.Now().Add(time.Second)

	connectionBudget := newKeyExchangeBudget(limits)
	var attempts int
	for attempt := 1; attempt <= limits.PerConnection+1; attempt++ {
		layer, err := connectionBudget.charge(context.Background(), nil, &attempts, addr, store.RateLimitListenerTailnet, now)
		if attempt <= limits.PerConnection && (layer != "" || err != nil) {
			t.Fatalf("tailnet connection attempt %d = (%q, %v), want admitted", attempt, layer, err)
		}
		if attempt == limits.PerConnection+1 && (layer != "connection" || err != nil) {
			t.Fatalf("tailnet connection attempt %d = (%q, %v), want connection refusal", attempt, layer, err)
		}
	}

	globalBudget := newKeyExchangeBudget(limits)
	for attempt := 1; attempt <= limits.GlobalBurst+1; attempt++ {
		var connectionAttempts int
		layer, err := globalBudget.charge(context.Background(), nil, &connectionAttempts, addr, store.RateLimitListenerTailnet, now)
		if attempt <= limits.GlobalBurst && (layer != "" || err != nil) {
			t.Fatalf("tailnet global attempt %d = (%q, %v), want admitted", attempt, layer, err)
		}
		if attempt == limits.GlobalBurst+1 && (layer != "global" || err != nil) {
			t.Fatalf("tailnet global attempt %d = (%q, %v), want global refusal", attempt, layer, err)
		}
	}
}

func TestMemoryKeyExchangeNetworkBudgetUsesAddressBucketAndListener(t *testing.T) {
	keys := &memoryAuthKeyStore{}
	limits := store.RateLimitConfig{Limit: 1, Window: time.Hour}
	first := netip.MustParseAddr("2001:db8:1::1")
	sameNetwork := netip.MustParseAddr("2001:db8:1::abcd")
	otherNetwork := netip.MustParseAddr("2001:db8:2::1")

	if result, err := keys.CheckIPRateLimit(context.Background(), first, keyExchangeTailnetSurface, limits); err != nil || result != nil {
		t.Fatalf("first tailnet network attempt = (%v, %v), want admitted", result, err)
	}
	result, err := keys.CheckIPRateLimit(context.Background(), sameNetwork, keyExchangeTailnetSurface, limits)
	if err != nil || result == nil {
		t.Fatalf("same-network tailnet attempt = (%v, %v), want refused", result, err)
	}
	if result.Wait < time.Second || result.Wait > limits.Window {
		t.Fatalf("same-network wait = %s, want [1s, %s]", result.Wait, limits.Window)
	}
	if result, err := keys.CheckIPRateLimit(context.Background(), first, keyExchangePublicSurface, limits); err != nil || result != nil {
		t.Fatalf("same-network public attempt = (%v, %v), want independently admitted", result, err)
	}
	if result, err := keys.CheckIPRateLimit(context.Background(), otherNetwork, keyExchangeTailnetSurface, limits); err != nil || result != nil {
		t.Fatalf("other-network tailnet attempt = (%v, %v), want independently admitted", result, err)
	}
}

func TestExchangeLimitsRejectNegativeValuesAndDivideGlobalCeiling(t *testing.T) {
	limits := DefaultExchangeLimits()
	limits.PerConnection = -1
	if _, err := divideExchangeLimits(limits, 1); err == nil {
		t.Fatal("negative per-connection limit was accepted")
	}
	limits = DefaultExchangeLimits()
	limits.PerNetwork.Limit = -1
	if _, err := divideExchangeLimits(limits, 1); err == nil {
		t.Fatal("negative per-network limit was accepted")
	}
	limits = DefaultExchangeLimits()
	limits.PerNetwork.Window = -time.Second
	if _, err := divideExchangeLimits(limits, 1); err == nil {
		t.Fatal("negative per-network window was accepted")
	}
	limits = DefaultExchangeLimits()
	limits.TailnetPerNetwork.Limit = -1
	if _, err := divideExchangeLimits(limits, 1); err == nil {
		t.Fatal("negative tailnet per-network limit was accepted")
	}
	limits = DefaultExchangeLimits()
	limits.TailnetPerNetwork.Window = -time.Second
	if _, err := divideExchangeLimits(limits, 1); err == nil {
		t.Fatal("negative tailnet per-network window was accepted")
	}
	limits = DefaultExchangeLimits()
	limits.GlobalRate = -1
	if _, err := divideExchangeLimits(limits, 1); err == nil {
		t.Fatal("negative global rate was accepted")
	}
	limits = DefaultExchangeLimits()
	limits.GlobalBurst = -1
	if _, err := divideExchangeLimits(limits, 1); err == nil {
		t.Fatal("negative global burst was accepted")
	}
	limits = DefaultExchangeLimits()
	divided, err := divideExchangeLimits(limits, 2)
	if err != nil {
		t.Fatalf("divide exchange limits across two replicas: %v", err)
	}
	if divided.GlobalRate != 2 || divided.GlobalBurst != 15 {
		t.Fatalf("per-replica global ceiling = %d/s burst %d, want 2/s burst 15", divided.GlobalRate, divided.GlobalBurst)
	}
	limits = DefaultExchangeLimits()
	if _, err := divideExchangeLimits(limits, limits.GlobalRate+1); err == nil {
		t.Fatal("replica count above global rate was accepted")
	}
	limits.GlobalRate = 0
	divided, err = divideExchangeLimits(limits, 10)
	if err != nil {
		t.Fatalf("disabled global ceiling with more replicas: %v", err)
	}
	if divided.GlobalRate != 0 || divided.GlobalBurst != 0 {
		t.Fatalf("disabled global ceiling share = (%d/s, %d burst), want disabled", divided.GlobalRate, divided.GlobalBurst)
	}
}

func TestExchangeTokenBucketEnforcesRateAndBurst(t *testing.T) {
	bucket := newExchangeTokenBucket(2, 3)
	now := bucket.last
	for attempt := range 3 {
		if !bucket.take(now) {
			t.Fatalf("burst attempt %d was refused", attempt+1)
		}
	}
	if bucket.take(now) {
		t.Fatal("attempt past burst was admitted without refill")
	}
	if !bucket.take(now.Add(500 * time.Millisecond)) {
		t.Fatal("one token was not refilled after 500ms at 2/s")
	}
}

func exchangeBudgetRequestFrame(t *testing.T) *bin.Buffer {
	t.Helper()
	var requestBody bin.Buffer
	request := mt.ReqPqMultiRequest{Nonce: bin.Int128{1}}
	if err := request.Encode(&requestBody); err != nil {
		t.Fatalf("encode req_pq_multi: %v", err)
	}
	first := &bin.Buffer{}
	if err := (proto.UnencryptedMessage{
		MessageID:   int64(proto.NewMessageID(time.Now(), proto.MessageFromClient)),
		MessageData: requestBody.Copy(),
	}).Encode(first); err != nil {
		t.Fatalf("encode key exchange frame: %v", err)
	}
	return first
}

var _ AuthKeyStore = (*exchangeBudgetTestStore)(nil)
var _ interface {
	CheckIPRateLimit(context.Context, netip.Addr, string, store.RateLimitConfig) (*store.RateLimitResult, error)
} = (*exchangeBudgetTestStore)(nil)
var _ transport.Conn = (*exchangeScriptConn)(nil)
