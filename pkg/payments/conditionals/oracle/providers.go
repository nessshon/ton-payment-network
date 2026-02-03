package oracle

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
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
	client *http.Client
	symbol string
	scale  int64
}

// NewBinanceProvider creates a provider for the given symbol, e.g. "BTCUSDT".
func NewBinanceProvider(symbol string, scale int64) *BinanceProvider {
	return &BinanceProvider{
		client: &http.Client{Timeout: 3 * time.Second},
		symbol: strings.ToUpper(symbol),
		scale:  scale,
	}
}

type binanceAggTrade struct {
	Price string `json:"p"`
	Time  int64  `json:"T"`
}

// Fetch returns the price at a specific second `at` by querying Binance aggTrades within that second.
// If there are no trades during that exact second, it returns ErrUnavailable.
func (b *BinanceProvider) Fetch(ctx context.Context, at int64) (int64, *big.Int, error) {
	start := at * 1000
	end := start + 999

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://fapi.binance.com/fapi/v1/aggTrades", nil)
	if err != nil {
		return 0, nil, err
	}
	q := req.URL.Query()
	q.Set("symbol", b.symbol)
	q.Set("startTime", strconv.FormatInt(start, 10))
	q.Set("endTime", strconv.FormatInt(end, 10))
	q.Set("limit", "1000")
	req.URL.RawQuery = q.Encode()

	resp, err := b.client.Do(req)
	if err != nil {
		return 0, nil, err
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

// parsePriceToScaledInt converts a decimal price string (e.g., "43123.42") to an
// integer scaled by scale (e.g., 1e9). It truncates extra fractional digits without rounding.
func parsePriceToScaledInt(s string, scale int64) (*big.Int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("empty price")
	}
	neg := false
	if s[0] == '-' {
		neg = true
		s = s[1:]
	}
	parts := strings.SplitN(s, ".", 2)
	intPart := parts[0]
	fracPart := ""
	if len(parts) == 2 {
		fracPart = parts[1]
	}

	res := new(big.Int)
	if intPart != "" {
		ip, ok := new(big.Int).SetString(intPart, 10)
		if !ok {
			return nil, errors.New("invalid integer part")
		}
		res.Set(ip)
	}
	res.Mul(res, big.NewInt(scale))

	if fracPart != "" {
		// keep up to scale digits
		scaleDigits := numDigits(scale) - 1 // since scale is power of 10
		if len(fracPart) > scaleDigits {
			fracPart = fracPart[:scaleDigits]
		}
		fp := new(big.Int)
		if fracPart != "" {
			v, ok := new(big.Int).SetString(fracPart, 10)
			if !ok {
				return nil, errors.New("invalid fractional part")
			}
			// multiply by 10^(scaleDigits - len(fracPart))
			pow := pow10(scaleDigits - len(fracPart))
			v.Mul(v, pow)
			fp.Set(v)
		}
		res.Add(res, fp)
	}
	if neg {
		res.Neg(res)
	}
	return res, nil
}

func numDigits(scale int64) int {
	// scale is expected to be a power of 10
	if scale <= 0 {
		return 0
	}
	c := 0
	for scale > 0 {
		scale /= 10
		c++
	}
	return c
}

func pow10(n int) *big.Int {
	if n <= 0 {
		return big.NewInt(1)
	}
	res := big.NewInt(1)
	ten := big.NewInt(10)
	for i := 0; i < n; i++ {
		res.Mul(res, ten)
	}
	return res
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
