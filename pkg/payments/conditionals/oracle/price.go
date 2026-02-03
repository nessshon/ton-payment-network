package oracle

import (
	"context"
	"errors"
	"hash/crc32"
	"math/big"
	"strings"
	"sync"
	"time"
)

func GetResolverID(src, symbol string) uint32 {
	return crc32.ChecksumIEEE([]byte(strings.ToLower(src) + "_" + strings.ToUpper(symbol)))
}

var PriceResolvers = map[uint32]*Resolver{
	// Should be added from main
	// GetResolverID("binance", "BTCUSDT"): NewResolver(NewBinanceProvider("BTCUSDT", 6)),
}

var (
	// ErrTooNew is returned when the requested timestamp is in the future
	// relative to the latest known price sample.
	ErrTooNew = errors.New("requested price is too new and not yet available")
	// ErrTooOld is returned when the requested timestamp is older than our
	// retention window and therefore evicted from cache.
	ErrTooOld = errors.New("requested price is too old and not available")
	// ErrNoData is returned when resolver has not yet received any price.
	ErrNoData = errors.New("no price data available yet")
	// ErrUnavailable is returned when the slot for the requested timestamp
	// does not contain a sample (e.g., provider missed that second).
	ErrUnavailable = errors.New("price sample for requested second is unavailable")
)

// PriceProvider defines a generic source of price data.
// Implementations must return a price sample for the requested UNIX second `at`.
// The price must be returned as an integer scaled by PriceScale (e.g., 1e9).
// For example, 43210.123456789 USD -> 43210123456789.
//
// Implementations should be fast and honor the provided context for timeouts.
// The resolver will call Fetch(ctx, at) with a specific second.
type PriceProvider interface {
	Fetch(ctx context.Context, at int64) (ts int64, price *big.Int, err error)
}

// Resolver caches per-second prices for a sliding 120 second window and
// implements GetPriceAt() lookups from the cache.
// It runs a background worker that polls the provider every second.
//
// Resolver is safe for concurrent use.
type Resolver struct {
	provider   PriceProvider
	mu         sync.RWMutex
	prices     [120]*big.Int
	timestamps [120]int64
	latest     int64

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewResolver creates a new Resolver for the given provider, preloads a
// range of past prices, and starts a background worker that polls the provider
// once per second for the current second.
func NewResolver(provider PriceProvider) *Resolver {
	ctx, cancel := context.WithCancel(context.Background())
	r := &Resolver{
		provider: provider,
		ctx:      ctx,
		cancel:   cancel,
	}
	// Preload last 120 seconds (resolver window)
	r.preloadLastSeconds(120)
	// Start worker
	r.wg.Add(1)
	go r.worker()
	return r
}

// preloadLastSeconds loads up to `n` last seconds of prices from the provider into the cache.
// It is best-effort: missing seconds are skipped. The latest successfully fetched
// timestamp becomes r.latest.
func (r *Resolver) preloadLastSeconds(n int) {
	if n <= 0 {
		return
	}
	now := time.Now().Unix()
	start := now - int64(n) + 1
	if start < 0 {
		start = 0
	}
	for ts := start; ts <= now; ts++ {
		ctx, cancel := context.WithTimeout(r.ctx, 2*time.Second)
		_, price, err := r.provider.Fetch(ctx, ts)
		cancel()
		if err != nil || price == nil {
			continue
		}
		idx := int(ts % 120)
		r.mu.Lock()
		r.prices[idx] = new(big.Int).Set(price)
		r.timestamps[idx] = ts
		if ts > r.latest {
			r.latest = ts
		}
		r.mu.Unlock()
	}
}

// Close stops the background worker and waits for it to finish.
func (r *Resolver) Close() {
	if r == nil {
		return
	}
	r.cancel()
	r.wg.Wait()
}

// GetPriceAt returns the price cached for the given UNIX second timestamp.
// It is O(1) and lock-optimized for fast reads.
//
// Behavior:
//   - If the requested timestamp is newer than the latest known sample: ErrTooNew
//   - If older than retention window (120s back from latest): ErrTooOld
//   - If exactly within the window but the sample is missing: ErrUnavailable
func (r *Resolver) GetPriceAt(_ context.Context, at int64) (*big.Int, error) { // satisfies conditionals.PriceResolver
	r.mu.RLock()
	latest := r.latest
	if latest == 0 {
		r.mu.RUnlock()
		return nil, ErrNoData
	}
	if at > latest {
		r.mu.RUnlock()
		return nil, ErrTooNew
	}
	if latest-at >= 120 { // keep last 120 seconds inclusive
		r.mu.RUnlock()
		return nil, ErrTooOld
	}
	idx := int(at % 120)
	ts := r.timestamps[idx]
	price := r.prices[idx]
	r.mu.RUnlock()

	if ts != at || price == nil {
		return nil, ErrUnavailable
	}
	return new(big.Int).Set(price), nil
}

func (r *Resolver) worker() {
	defer r.wg.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	// Immediate first fetch without waiting a full second
	r.fetchOnce()

	for {
		select {
		case <-r.ctx.Done():
			return
		case <-ticker.C:
			r.fetchOnce()
		}
	}
}

func (r *Resolver) fetchOnce() {
	ctx, cancel := context.WithTimeout(r.ctx, 2*time.Second)
	defer cancel()

	at := time.Now().Unix()
	ts, price, err := r.provider.Fetch(ctx, at)
	if err != nil || price == nil {
		return
	}
	// ts should equal at; normalize to second precision just in case
	ts = ts - (ts % 1)
	idx := int(ts % 120)

	r.mu.Lock()
	r.prices[idx] = new(big.Int).Set(price)
	r.timestamps[idx] = ts
	if ts > r.latest {
		r.latest = ts
	}
	r.mu.Unlock()
}


// GetLastPrice returns the most recent available cached price sample.
// If there is no data yet, it returns ErrNoData. If the latest slot is empty,
// it scans backwards within the retention window (120s) and returns the
// freshest available sample; if none found, returns ErrUnavailable.
func (r *Resolver) GetLastPrice() (int64, *big.Int, error) {
	if r == nil {
		return 0, nil, ErrNoData
	}
	// Fast path: try the exact latest second
	r.mu.RLock()
	latest := r.latest
	if latest == 0 {
		r.mu.RUnlock()
		return 0, nil, ErrNoData
	}
	idx := int(latest % 120)
	ts := r.timestamps[idx]
	price := r.prices[idx]
	if ts == latest && price != nil {
		val := new(big.Int).Set(price)
		r.mu.RUnlock()
		return latest, val, nil
	}
	r.mu.RUnlock()
	// Fallback: walk backwards up to the retention window
	for i := 1; i < 120; i++ {
		candidate := latest - int64(i)
		r.mu.RLock()
		cidx := int(candidate % 120)
		if r.timestamps[cidx] == candidate && r.prices[cidx] != nil {
			val := new(big.Int).Set(r.prices[cidx])
			r.mu.RUnlock()
			return candidate, val, nil
		}
		r.mu.RUnlock()
	}
	return 0, nil, ErrUnavailable
}
