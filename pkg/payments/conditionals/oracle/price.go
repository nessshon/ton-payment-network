package oracle

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"hash/crc32"
	"math/big"
	"strings"
	"sync"
	"time"

	condcontracts "github.com/xssnick/ton-payment-network/pkg/payments/conditionals/contracts"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
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
	// ErrPriceUpdateUnsupported is returned when trying to override price on
	// non-mock providers.
	ErrPriceUpdateUnsupported = errors.New("price update is not supported by this resolver provider")
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

// RangePrice holds a single price sample returned by RangeProvider.
type RangePrice struct {
	At    int64
	Price *big.Int
	Proof *cell.Cell // PriceInner cell (signature + PriceProof body), optional
}

// RangeProvider is an optional interface that PriceProvider implementations
// can satisfy to support fetching a range of prices in a single request.
// This is used by the Resolver to efficiently preload the cache at startup.
type RangeProvider interface {
	FetchRange(ctx context.Context, from, to int64) ([]RangePrice, error)
}

// SinceProvider is an optional interface for providers that support
// fetching all prices since a given timestamp. The server returns
// prices from since+1 to its latest, clamped to the retention window.
type SinceProvider interface {
	FetchSince(ctx context.Context, since int64) ([]RangePrice, error)
}

type proofKeyProvider interface {
	ProofPublicKey() ed25519.PublicKey
}

type proofSignerProvider interface {
	SignProofCell(proof *cell.Cell) ([]byte, error)
}

// proofBuilder is an optional interface for providers that can
// build a signed PriceInner cell from raw price data.
type proofBuilder interface {
	BuildProofCell(at int64, price *big.Int) (*cell.Cell, error)
}

// Resolver caches per-second prices for a sliding 120 second window and
// implements GetPriceAt() lookups from the cache.
// It runs a background worker that polls the provider every second.
//
// Resolver is safe for concurrent use.
type Resolver struct {
	provider   PriceProvider
	proofKey   ed25519.PublicKey // cached proof public key from provider
	mu         sync.RWMutex
	prices     [120]*big.Int
	timestamps [120]int64
	latest     int64

	// Proof cell cache: each slot holds a PriceInner cell (signature + PriceProof).
	proofCells      [120]*cell.Cell
	proofTimestamps [120]int64

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
	// Cache proof public key from provider if available
	if kp, ok := provider.(proofKeyProvider); ok {
		r.proofKey = kp.ProofPublicKey()
	}
	// Preload last 120 seconds (resolver window)
	r.preloadLastSeconds(120)
	// Start worker
	r.wg.Add(1)
	go r.worker()
	return r
}

// preloadLastSeconds loads prices into the cache using a single batch request.
// It prefers SinceProvider (sends since=0 to get all available history),
// falls back to RangeProvider with explicit from/to.
func (r *Resolver) preloadLastSeconds(n int) {
	if n <= 0 {
		return
	}

	ctx, cancel := context.WithTimeout(r.ctx, 15*time.Second)
	defer cancel()

	var prices []RangePrice
	var err error

	if sp, ok := r.provider.(SinceProvider); ok {
		prices, err = sp.FetchSince(ctx, 0)
	} else if rp, ok := r.provider.(RangeProvider); ok {
		now := time.Now().Unix()
		start := now - int64(n) + 1
		if start < 0 {
			start = 0
		}
		prices, err = rp.FetchRange(ctx, start, now)
	} else {
		return
	}

	if err != nil || len(prices) == 0 {
		return
	}

	r.storePrices(prices)
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

	for {
		select {
		case <-r.ctx.Done():
			return
		case <-ticker.C:
			r.fetchOnce()
		}
	}
}

// SetPrice overrides the current price for mock-like providers and updates the
// latest cache slot immediately. It is useful in integration tests where we need
// deterministic price moves without waiting for external feeds.
func (r *Resolver) SetPrice(price *big.Int) error {
	if price == nil {
		return errors.New("price is nil")
	}

	setter, ok := r.provider.(interface{ SetPrice(*big.Int) })
	if !ok {
		return ErrPriceUpdateUnsupported
	}

	setter.SetPrice(price)

	ts := time.Now().Unix()
	idx := int(ts % 120)

	r.mu.Lock()
	r.prices[idx] = new(big.Int).Set(price)
	r.timestamps[idx] = ts
	if ts > r.latest {
		r.latest = ts
	}
	r.mu.Unlock()

	return nil
}

func (r *Resolver) fetchOnce() {
	ctx, cancel := context.WithTimeout(r.ctx, 5*time.Second)
	defer cancel()

	// Prefer SinceProvider: fetch all new prices since our latest
	if sp, ok := r.provider.(SinceProvider); ok {
		r.mu.RLock()
		since := r.latest
		r.mu.RUnlock()

		prices, err := sp.FetchSince(ctx, since)
		if err != nil || len(prices) == 0 {
			return
		}
		r.storePrices(prices)
		return
	}

	// Fallback: single fetch
	at := time.Now().Unix()
	ts, price, err := r.provider.Fetch(ctx, at)
	if err != nil || price == nil {
		return
	}
	ts = ts - (ts % 1)

	rp := RangePrice{At: ts, Price: price}

	// Build proof cell if provider supports it
	if pb, ok := r.provider.(proofBuilder); ok {
		proofCell, err := pb.BuildProofCell(ts, price)
		if err == nil {
			rp.Proof = proofCell
		}
	}

	r.storePrices([]RangePrice{rp})
}

// storePrices writes a batch of prices into the circular cache.
// If a price has a Proof cell and the resolver has a proof key,
// the proof signature is verified before caching.
func (r *Resolver) storePrices(prices []RangePrice) {
	for _, p := range prices {
		if p.Price == nil {
			continue
		}
		idx := int(p.At % 120)
		r.mu.Lock()
		r.prices[idx] = new(big.Int).Set(p.Price)
		r.timestamps[idx] = p.At
		if p.At > r.latest {
			r.latest = p.At
		}

		// Store proof cell if present and valid
		if p.Proof != nil {
			if len(r.proofKey) == 0 || VerifyProofCell(p.Proof, r.proofKey) {
				r.proofCells[idx] = p.Proof
				r.proofTimestamps[idx] = p.At
			}
		}
		r.mu.Unlock()
	}
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

// GetPricesSince returns all cached prices from since+1 to latest.
// If since is too old (more than 120s behind latest), it clamps to the
// available retention window.
func (r *Resolver) GetPricesSince(since int64) []RangePrice {
	if r == nil {
		return nil
	}

	r.mu.RLock()
	latest := r.latest
	r.mu.RUnlock()

	if latest == 0 {
		return nil
	}

	from := since + 1
	// Clamp to retention window
	if latest-from >= 120 {
		from = latest - 119
	}
	if from < 0 {
		from = 0
	}
	if from > latest {
		return nil
	}

	var result []RangePrice
	for at := from; at <= latest; at++ {
		idx := int(at % 120)
		r.mu.RLock()
		ts := r.timestamps[idx]
		p := r.prices[idx]
		r.mu.RUnlock()

		if ts == at && p != nil {
			result = append(result, RangePrice{
				At:    at,
				Price: new(big.Int).Set(p),
			})
		}
	}
	return result
}

// GetSignedPricesSince returns proof cells from since+1 to latest.
// Each returned cell is a PriceInner (signature + PriceProof body).
func (r *Resolver) GetSignedPricesSince(since int64) []*cell.Cell {
	if r == nil {
		return nil
	}

	r.mu.RLock()
	latest := r.latest
	r.mu.RUnlock()

	if latest == 0 {
		return nil
	}

	from := since + 1
	if latest-from >= 120 {
		from = latest - 119
	}
	if from < 0 {
		from = 0
	}
	if from > latest {
		return nil
	}

	var result []*cell.Cell
	for at := from; at <= latest; at++ {
		idx := int(at % 120)
		r.mu.RLock()
		ts := r.proofTimestamps[idx]
		pc := r.proofCells[idx]
		r.mu.RUnlock()

		if ts == at && pc != nil {
			result = append(result, pc)
		}
	}
	return result
}

// GetProofPublicKey returns the trusted proof signer key when provider supports
// signed-proof verification. For unsigned providers it returns nil.
func (r *Resolver) GetProofPublicKey() ed25519.PublicKey {
	if r == nil || len(r.proofKey) == 0 {
		return nil
	}
	return append(ed25519.PublicKey(nil), r.proofKey...)
}

func (r *Resolver) SignProofCell(proof *cell.Cell) ([]byte, error) {
	if r == nil {
		return nil, fmt.Errorf("resolver is nil")
	}
	if proof == nil {
		return nil, fmt.Errorf("proof cell is nil")
	}

	p, ok := r.provider.(proofSignerProvider)
	if !ok {
		return nil, fmt.Errorf("resolver provider does not support proof signing")
	}
	return p.SignProofCell(proof)
}

// ParsePriceProof extracts At and Price from a PriceInner cell.
func ParsePriceProof(proofCell *cell.Cell) (at int64, price *big.Int, err error) {
	if proofCell == nil {
		return 0, nil, errors.New("proof cell is nil")
	}

	var pi condcontracts.PriceInner
	if err := tlb.Parse(&pi, proofCell); err != nil {
		return 0, nil, fmt.Errorf("failed to parse PriceInner: %w", err)
	}
	if pi.SignedBody == nil {
		return 0, nil, errors.New("PriceInner has nil signed body")
	}

	var pp condcontracts.PriceProof
	if err := tlb.Parse(&pp, pi.SignedBody); err != nil {
		return 0, nil, fmt.Errorf("failed to parse PriceProof: %w", err)
	}

	return int64(pp.At), pp.Price.Nano(), nil
}

// VerifyProofCell verifies the signature on a PriceInner cell against a public key.
func VerifyProofCell(proofCell *cell.Cell, pubKey ed25519.PublicKey) bool {
	if proofCell == nil || len(pubKey) != ed25519.PublicKeySize {
		return false
	}

	var pi condcontracts.PriceInner
	if err := tlb.Parse(&pi, proofCell); err != nil {
		return false
	}
	if pi.SignedBody == nil || len(pi.Signature.V) != ed25519.SignatureSize {
		return false
	}

	return ed25519.Verify(pubKey, pi.SignedBody.Hash(), pi.Signature.V)
}

// BuildPriceInnerCell creates a signed PriceInner cell from raw price data.
func BuildPriceInnerCell(at int64, price *big.Int, signer ed25519.PrivateKey) (*cell.Cell, error) {
	if at <= 0 || at > int64(^uint32(0)) {
		return nil, errors.New("invalid timestamp")
	}

	priceCoins, err := tlb.FromNano(price, 9)
	if err != nil {
		return nil, fmt.Errorf("failed to convert price: %w", err)
	}

	proof := condcontracts.PriceProof{
		At:    uint32(at),
		Price: priceCoins,
	}
	proofCell, err := tlb.ToCell(proof)
	if err != nil {
		return nil, fmt.Errorf("failed to build PriceProof cell: %w", err)
	}

	signature := proofCell.Sign(signer)

	priceInner := condcontracts.PriceInner{
		Signature: struct {
			V []byte `tlb:"bits 512"`
		}{
			V: append([]byte(nil), signature...),
		},
		SignedBody: proofCell,
	}
	return tlb.ToCell(priceInner)
}
