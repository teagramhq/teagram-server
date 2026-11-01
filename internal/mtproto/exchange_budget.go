package mtproto

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"time"

	"github.com/teagramhq/teagram-server/internal/store"
)

const (
	defaultKeyExchangesPerConnection = 2
	defaultKeyExchangesPerNetwork    = 20
	defaultKeyExchangeNetworkWindow  = time.Hour
	defaultKeyExchangeRate           = 5
	defaultKeyExchangeBurst          = 30

	keyExchangeTailnetSurface = "key_exchange_tailnet"
	keyExchangePublicSurface  = "key_exchange_public"
)

var errKeyExchangeRateLimited = errors.New("key exchange rate limited")

// ExchangeLimits bounds anonymous server key-exchange work. The connection
// count is local to one connection, per-network budgets are shared by the rate
// limit store, and the ceiling is process local and divided across replicas.
type ExchangeLimits struct {
	PerConnection int
	// PerNetwork bounds exchanges from one client network on the public listener.
	PerNetwork store.RateLimitConfig
	// TailnetPerNetwork bounds exchanges on tailnet listeners.
	TailnetPerNetwork store.RateLimitConfig
	GlobalRate        int
	GlobalBurst       int
}

// DefaultExchangeLimits returns the production key-exchange budgets. The
// public listener starts with a per-network bound; tailnet starts without one
// and can be bounded through its explicit tailnet override.
func DefaultExchangeLimits() ExchangeLimits {
	return ExchangeLimits{
		PerConnection: defaultKeyExchangesPerConnection,
		PerNetwork: store.RateLimitConfig{
			Limit:  defaultKeyExchangesPerNetwork,
			Window: defaultKeyExchangeNetworkWindow,
		},
		TailnetPerNetwork: store.RateLimitConfig{
			Limit:  0,
			Window: defaultKeyExchangeNetworkWindow,
		},
		GlobalRate:  defaultKeyExchangeRate,
		GlobalBurst: defaultKeyExchangeBurst,
	}
}

func validateExchangeLimits(limits ExchangeLimits) error {
	switch {
	case limits.PerConnection < 0:
		return fmt.Errorf("key exchange per-connection limit is %d: must not be negative, and 0 disables the cap", limits.PerConnection)
	case limits.PerNetwork.Limit < 0:
		return fmt.Errorf("key exchange per-network limit is %d: must not be negative, and 0 disables the cap", limits.PerNetwork.Limit)
	case limits.PerNetwork.Window < 0:
		return fmt.Errorf("key exchange per-network window is %s: must not be negative, and 0 disables the cap", limits.PerNetwork.Window)
	case limits.TailnetPerNetwork.Limit < 0:
		return fmt.Errorf("key exchange tailnet per-network limit is %d: must not be negative, and 0 disables the cap", limits.TailnetPerNetwork.Limit)
	case limits.TailnetPerNetwork.Window < 0:
		return fmt.Errorf("key exchange tailnet per-network window is %s: must not be negative, and 0 disables the cap", limits.TailnetPerNetwork.Window)
	case limits.GlobalRate < 0:
		return fmt.Errorf("key exchange global rate is %d: must not be negative, and 0 disables the ceiling", limits.GlobalRate)
	case limits.GlobalBurst < 0:
		return fmt.Errorf("key exchange global burst is %d: must not be negative, and 0 disables the ceiling", limits.GlobalBurst)
	}
	return nil
}

func divideExchangeLimits(limits ExchangeLimits, replicas int) (ExchangeLimits, error) {
	if err := validateExchangeLimits(limits); err != nil {
		return ExchangeLimits{}, err
	}
	if limits.GlobalRate == 0 || limits.GlobalBurst == 0 {
		limits.GlobalRate = 0
		limits.GlobalBurst = 0
		return limits, nil
	}
	rate, err := divideLocalLimit(limits.GlobalRate, replicas, "key exchange global rate")
	if err != nil {
		return ExchangeLimits{}, err
	}
	burst, err := divideLocalLimit(limits.GlobalBurst, replicas, "key exchange global burst")
	if err != nil {
		return ExchangeLimits{}, err
	}
	limits.GlobalRate = rate
	limits.GlobalBurst = burst
	return limits, nil
}

type keyExchangeRateLimitStore interface {
	CheckIPRateLimit(context.Context, netip.Addr, string, store.RateLimitConfig) (*store.RateLimitResult, error)
}

type keyExchangeBudget struct {
	limits  ExchangeLimits
	ceiling *exchangeTokenBucket
}

func newKeyExchangeBudget(limits ExchangeLimits) *keyExchangeBudget {
	var ceiling *exchangeTokenBucket
	if limits.GlobalRate > 0 && limits.GlobalBurst > 0 {
		ceiling = newExchangeTokenBucket(limits.GlobalRate, limits.GlobalBurst)
	}
	return &keyExchangeBudget{limits: limits, ceiling: ceiling}
}

// charge applies each layer in order. The per-connection counter advances
// before any shared work; the ceiling is untouched unless the network layer
// admits the exchange.
func (b *keyExchangeBudget) charge(
	ctx context.Context,
	keys AuthKeyStore,
	attempts *int,
	addr netip.Addr,
	listener store.RateLimitListener,
	now time.Time,
) (string, error) {
	if b.limits.PerConnection > 0 {
		if attempts == nil {
			return "connection", errors.New("missing key exchange connection counter")
		}
		(*attempts)++
		if *attempts > b.limits.PerConnection {
			return "connection", nil
		}
	}

	networkLimit := b.limits.PerNetwork
	if listener == store.RateLimitListenerTailnet {
		networkLimit = b.limits.TailnetPerNetwork
	}
	if networkLimit.Enabled() {
		if _, keyable := store.IPBucketKey(addr); keyable {
			surface, ok := keyExchangeSurface(listener)
			if !ok {
				return "network_store", fmt.Errorf("unknown key exchange listener %d", listener)
			}
			limiter, ok := keys.(keyExchangeRateLimitStore)
			if !ok {
				return "network_store", errors.New("key exchange rate-limit store is unavailable")
			}
			result, err := limiter.CheckIPRateLimit(ctx, addr, surface, networkLimit)
			if err != nil {
				return "network_store", err
			}
			if result != nil {
				return "network", nil
			}
		}
	}

	if b.ceiling != nil && !b.ceiling.take(now) {
		return "global", nil
	}
	return "", nil
}

func keyExchangeSurface(listener store.RateLimitListener) (string, bool) {
	switch listener {
	case store.RateLimitListenerTailnet:
		return keyExchangeTailnetSurface, true
	case store.RateLimitListenerPublic:
		return keyExchangePublicSurface, true
	default:
		return "", false
	}
}

type exchangeTokenBucket struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens float64
	last   time.Time
}

func newExchangeTokenBucket(rate, burst int) *exchangeTokenBucket {
	now := time.Now()
	return &exchangeTokenBucket{
		rate:   float64(rate),
		burst:  float64(burst),
		tokens: float64(burst),
		last:   now,
	}
}

func (b *exchangeTokenBucket) take(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if now.After(b.last) {
		b.tokens = min(b.burst, b.tokens+now.Sub(b.last).Seconds()*b.rate)
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

type memoryExchangeLimitKey struct {
	network netip.Prefix
	surface string
}

type memoryExchangeLimitCounter struct {
	started time.Time
	count   int
}

// CheckIPRateLimit gives the in-memory auth-key store a process-local network
// budget for tests and single-process embedders. Production uses pgAuthKeyStore,
// whose implementation shares these counters through Postgres.
func (s *memoryAuthKeyStore) CheckIPRateLimit(
	ctx context.Context,
	addr netip.Addr,
	surface string,
	cfg store.RateLimitConfig,
) (*store.RateLimitResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("check in-memory key exchange budget: %w", err)
	}
	if !cfg.Enabled() {
		return nil, nil //nolint:nilnil // disabled config admits every request
	}
	network, ok := store.IPBucketKey(addr)
	if !ok {
		return nil, store.ErrNoClientAddr
	}
	now := time.Now()
	key := memoryExchangeLimitKey{network: network, surface: surface}
	s.limitMu.Lock()
	defer s.limitMu.Unlock()
	if s.exchangeLimits == nil {
		s.exchangeLimits = make(map[memoryExchangeLimitKey]memoryExchangeLimitCounter)
	}
	counter := s.exchangeLimits[key]
	if counter.started.IsZero() || !now.Before(counter.started.Add(cfg.Window)) {
		counter = memoryExchangeLimitCounter{started: now}
	}
	if counter.count >= cfg.Limit {
		remaining := counter.started.Add(cfg.Window).Sub(now)
		if remaining < time.Second {
			remaining = time.Second
		} else {
			remaining = time.Duration((int64(remaining) + int64(time.Second) - 1) / int64(time.Second) * int64(time.Second))
		}
		s.exchangeLimits[key] = counter
		return &store.RateLimitResult{Wait: remaining}, nil
	}
	counter.count++
	s.exchangeLimits[key] = counter
	return nil, nil //nolint:nilnil // allowed request has consumed its token
}
