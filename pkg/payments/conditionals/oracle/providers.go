//go:build !(js && wasm)

package oracle

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/xssnick/tonutils-go/tvm/cell"
)

// BinanceProvider implements PriceProvider using Binance public REST API.
// It queries aggregated trades on USD-M futures via /fapi/v1/aggTrades to derive
// the price for a specific second. The price used is the last trade within that
// second. If there were no trades during that second, the provider returns ErrUnavailable.
// It converts the decimal string price to an integer scaled by the configured scale (e.g., 1e9).
//
// Note: Binance does not provide official 1-second kline snapshots via REST; using
// aggTrades with startTime/endTime is the practical way to get per-second prices.
// Network errors or no-trade seconds are tolerated by the resolver (such seconds
// will simply be marked unavailable).
type BinanceProvider struct {
	client      *http.Client
	symbol      string
	scale       int64
	proofSigner ed25519.PrivateKey
}

// NewBinanceProvider creates a provider for the given symbol, e.g. "BTCUSDT".
func NewBinanceProvider(symbol string, scale int64) *BinanceProvider {
	return &BinanceProvider{
		client:      http.DefaultClient,
		symbol:      strings.ToUpper(symbol),
		scale:       scale,
		proofSigner: append(ed25519.PrivateKey(nil), binanceProofSignerKey...),
	}
}

func (b *BinanceProvider) ProofPublicKey() ed25519.PublicKey {
	if len(b.proofSigner) != ed25519.PrivateKeySize {
		return nil
	}
	return append(ed25519.PublicKey(nil), b.proofSigner.Public().(ed25519.PublicKey)...)
}

func (b *BinanceProvider) SignProofCell(proof *cell.Cell) ([]byte, error) {
	if proof == nil {
		return nil, errors.New("proof cell is nil")
	}
	if len(b.proofSigner) != ed25519.PrivateKeySize {
		return nil, errors.New("binance proof signer key is invalid")
	}
	return proof.Sign(b.proofSigner), nil
}

// BuildProofCell builds a signed PriceInner cell from raw price data.
func (b *BinanceProvider) BuildProofCell(at int64, price *big.Int) (*cell.Cell, error) {
	return BuildPriceInnerCell(at, price, b.proofSigner)
}

type binanceAggTrade struct {
	Price string `json:"p"`
	Time  int64  `json:"T"`
}

// Fetch returns the price at a specific second `at` by querying Binance aggTrades within that second.
// If there are no trades during that exact second, it returns ErrUnavailable.
func (b *BinanceProvider) Fetch(ctx context.Context, at int64) (int64, *big.Int, error) {
	if b == nil {
		return 0, nil, errors.New("binance provider is nil")
	}

	start := at * 1000
	end := start + 999

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.binance.com/api/v3/aggTrades", nil)
	if err != nil {
		return 0, nil, err
	}

	client := b.client
	if client == nil {
		client = http.DefaultClient
	}
	if client == nil {
		return 0, nil, errors.New("binance http client is unavailable")
	}

	q := req.URL.Query()
	q.Set("symbol", b.symbol)
	q.Set("startTime", strconv.FormatInt(start, 10))
	q.Set("endTime", strconv.FormatInt(end, 10))
	q.Set("limit", "1000")
	req.URL.RawQuery = q.Encode()

	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	if resp == nil || resp.Body == nil {
		return 0, nil, errors.New("binance: empty http response")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return 0, nil, errors.New("binance: non-200 status: " + strconv.Itoa(resp.StatusCode))
	}

	var trs []binanceAggTrade
	dec := json.NewDecoder(resp.Body)
	if err = dec.Decode(&trs); err != nil {
		return 0, nil, err
	}
	if len(trs) == 0 {
		return at, nil, ErrUnavailable
	}
	last := trs[len(trs)-1]
	pi, err := parsePriceToScaledInt(last.Price, b.scale)
	if err != nil {
		return 0, nil, err
	}
	return at, pi, nil
}

// MockProvider is an in-memory price provider useful for tests.
// It returns the currently configured price with the current UNIX second timestamp.
type MockProvider struct {
	mu    sync.RWMutex
	price *big.Int
}

// NewMockProvider creates a MockProvider with the initial price set.
// The price must be already scaled according to the resolver expectations
// (e.g., if PriceScale is 1e9, pass the value scaled by 1e9).
func NewMockProvider(initial *big.Int) *MockProvider {
	mp := &MockProvider{price: new(big.Int)}
	if initial != nil {
		mp.price.Set(initial)
	}
	return mp
}

// SetPrice updates the provider's current price (scaled value expected).
func (m *MockProvider) SetPrice(p *big.Int) {
	if p == nil {
		return
	}
	m.mu.Lock()
	m.price = new(big.Int).Set(p)
	m.mu.Unlock()
}

// Fetch implements PriceProvider by returning the configured price for the requested second.
func (m *MockProvider) Fetch(ctx context.Context, at int64) (int64, *big.Int, error) {
	_ = ctx // context is unused as this provider is in-memory and instant
	m.mu.RLock()
	price := new(big.Int).Set(m.price)
	m.mu.RUnlock()
	return at, price, nil
}
