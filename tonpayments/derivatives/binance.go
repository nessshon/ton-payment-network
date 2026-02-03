package derivatives

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/xssnick/ton-payment-network/tonpayments/db"
)

// CandleProvider provides 1-minute candles for symbols.
type CandleProvider interface {
	Fetch1m(ctx context.Context, symbol string, start, end time.Time, limit int) ([]db.Candle1m, error)
}

// BinanceFuturesProvider implements CandleProvider using Binance USD-M futures REST API.
// Endpoint: GET /fapi/v1/klines with interval=1m
// Ref: https://binance-docs.github.io/apidocs/futures/en/#kline-candlestick-data
// Note: start and end are in milliseconds; limit max 1500.
type BinanceFuturesProvider struct {
	client *http.Client
}

func NewBinanceFuturesProvider() *BinanceFuturesProvider {
	return &BinanceFuturesProvider{client: &http.Client{Timeout: 6 * time.Second}}
}

type binanceKline = [12]any

func (b *BinanceFuturesProvider) Fetch1m(ctx context.Context, symbol string, start, end time.Time, limit int) ([]db.Candle1m, error) {
	if limit <= 0 || limit > 1500 {
		limit = 1500
	}
	u, _ := url.Parse("https://fapi.binance.com/fapi/v1/klines")
	q := u.Query()
	q.Set("symbol", strings.ToUpper(symbol))
	q.Set("interval", "1m")
	if !start.IsZero() {
		q.Set("startTime", strconv.FormatInt(start.UnixMilli(), 10))
	}
	if !end.IsZero() {
		q.Set("endTime", strconv.FormatInt(end.UnixMilli(), 10))
	}
	q.Set("limit", strconv.Itoa(limit))
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("binance: http %d", resp.StatusCode)
	}
	var raw [][]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
 out := make([]db.Candle1m, 0, len(raw))
	for _, v := range raw {
		if len(v) < 6 {
			return nil, errors.New("binance: malformed kline entry")
		}
		// fields: openTime, open, high, low, close, volume, closeTime, ...
		openTimeMs, _ := toInt64(v[0])
		open, _ := toString(v[1])
		high, _ := toString(v[2])
		low, _ := toString(v[3])
		close, _ := toString(v[4])
		vol, _ := toString(v[5])
  out = append(out, db.Candle1m{
			T: openTimeMs / 1000,
			O: open,
			H: high,
			L: low,
			C: close,
			V: vol,
		})
	}
	return out, nil
}

func toInt64(v any) (int64, error) {
	switch t := v.(type) {
	case float64:
		return int64(t), nil
	case string:
		return strconv.ParseInt(t, 10, 64)
	default:
		return 0, fmt.Errorf("unexpected type %T", v)
	}
}

func toString(v any) (string, error) {
	switch t := v.(type) {
	case string:
		return t, nil
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), nil
	default:
		return "", fmt.Errorf("unexpected type %T", v)
	}
}

// MockCandleProvider returns pre-seeded candles and can be used in unit tests.
type MockCandleProvider struct {
	Candles map[string][]db.Candle1m
}

func (m *MockCandleProvider) Fetch1m(ctx context.Context, symbol string, start, end time.Time, limit int) ([]db.Candle1m, error) {
	arr := m.Candles[strings.ToUpper(symbol)]
	if len(arr) == 0 {
		return nil, nil
	}
	// naive filter by time window
 res := make([]db.Candle1m, 0, len(arr))
	for _, c := range arr {
		if !start.IsZero() && c.T < start.Unix() {
			continue
		}
		if !end.IsZero() && c.T > end.Unix() {
			continue
		}
		res = append(res, c)
		if limit > 0 && len(res) >= limit {
			break
		}
	}
	return res, nil
}
