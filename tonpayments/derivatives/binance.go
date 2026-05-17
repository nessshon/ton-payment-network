//go:build !js || !wasm

package derivatives

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xssnick/ton-payment-network/tonpayments/db"
)

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
type binanceDepth struct {
	LastUpdateID    int64      `json:"lastUpdateId"`
	EventTime       int64      `json:"E"`
	TransactionTime int64      `json:"T"`
	Bids            [][]string `json:"bids"`
	Asks            [][]string `json:"asks"`
}

type binanceTrade struct {
	ID       int64  `json:"id"`
	Price    string `json:"price"`
	Qty      string `json:"qty"`
	QuoteQty string `json:"quoteQty"`
	Time     int64  `json:"time"`
}

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

func (b *BinanceFuturesProvider) FetchOrderBookAndVolume(ctx context.Context, symbol string, depthLimit int, volumeLimit int) (*OrderBookVolumeView, error) {
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	if symbol == "" {
		return nil, errors.New("binance: symbol is required")
	}

	if depthLimit <= 0 {
		depthLimit = 20
	}
	depthLimit = normalizeBinanceDepthLimit(depthLimit)

	if volumeLimit <= 0 || volumeLimit > 3600 {
		volumeLimit = 120
	}

	var (
		depth     *binanceDepth
		trades    []binanceTrade
		errDepth  error
		errTrades error
		wg        sync.WaitGroup
	)

	wg.Add(2)
	go func() {
		defer wg.Done()
		depth, errDepth = b.fetchDepth(ctx, symbol, depthLimit)
	}()
	go func() {
		defer wg.Done()
		// Recent trades API limit is capped at 1000.
		tradesLimit := volumeLimit * 24
		if tradesLimit < 200 {
			tradesLimit = 200
		}
		if tradesLimit > 1000 {
			tradesLimit = 1000
		}
		trades, errTrades = b.fetchRecentTrades(ctx, symbol, tradesLimit)
	}()
	wg.Wait()

	if errDepth != nil {
		return nil, errDepth
	}
	if errTrades != nil {
		return nil, errTrades
	}

	volumes := buildSecondVolumeSeries(trades, volumeLimit)
	lastVolume := "0"
	at := time.Now().UTC().Unix()
	if len(volumes) > 0 {
		last := volumes[len(volumes)-1]
		lastVolume = last.Volume
		at = last.At
	}
	if depth != nil {
		if depth.TransactionTime > 0 {
			at = depth.TransactionTime / 1000
		} else if depth.EventTime > 0 {
			at = depth.EventTime / 1000
		}
	}

	return &OrderBookVolumeView{
		Symbol:        symbol,
		At:            at,
		Volume:        lastVolume,
		VolumeHistory: volumes,
		Bids:          convertDepthLevels(depth.Bids),
		Asks:          convertDepthLevels(depth.Asks),
	}, nil
}

func (b *BinanceFuturesProvider) fetchRecentTrades(ctx context.Context, symbol string, limit int) ([]binanceTrade, error) {
	if limit <= 0 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}

	u, _ := url.Parse("https://fapi.binance.com/fapi/v1/trades")
	q := u.Query()
	q.Set("symbol", symbol)
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
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		msg := strings.TrimSpace(string(body))
		if msg == "" {
			return nil, fmt.Errorf("binance trades: http %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("binance trades: http %d: %s", resp.StatusCode, msg)
	}

	var out []binanceTrade
	if err = json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

func buildSecondVolumeSeries(trades []binanceTrade, points int) []VolumePoint {
	if points <= 0 {
		points = 120
	}
	now := time.Now().UTC().Unix()
	if len(trades) > 0 {
		last := trades[len(trades)-1].Time / 1000
		if last > now {
			now = last
		}
	}

	start := now - int64(points) + 1
	if start < 0 {
		start = 0
	}

	volumeBySecond := make(map[int64]float64, points)
	for _, tr := range trades {
		sec := tr.Time / 1000
		if sec < start || sec > now {
			continue
		}
		qty, err := strconv.ParseFloat(strings.TrimSpace(tr.Qty), 64)
		if err != nil || math.IsNaN(qty) || math.IsInf(qty, 0) {
			continue
		}
		volumeBySecond[sec] += qty
	}

	out := make([]VolumePoint, 0, points)
	for sec := start; sec <= now; sec++ {
		v := volumeBySecond[sec]
		out = append(out, VolumePoint{
			At:     sec,
			Volume: strconv.FormatFloat(v, 'f', -1, 64),
		})
	}
	return out
}

func (b *BinanceFuturesProvider) fetchDepth(ctx context.Context, symbol string, limit int) (*binanceDepth, error) {
	u, _ := url.Parse("https://fapi.binance.com/fapi/v1/depth")
	q := u.Query()
	q.Set("symbol", symbol)
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
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		msg := strings.TrimSpace(string(body))
		if msg == "" {
			return nil, fmt.Errorf("binance depth: http %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("binance depth: http %d: %s", resp.StatusCode, msg)
	}

	var out binanceDepth
	if err = json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}

	return &out, nil
}

func normalizeBinanceDepthLimit(limit int) int {
	allowed := []int{5, 10, 20, 50, 100, 500, 1000}
	if limit <= allowed[0] {
		return allowed[0]
	}
	for _, v := range allowed {
		if limit <= v {
			return v
		}
	}
	return allowed[len(allowed)-1]
}

func convertDepthLevels(raw [][]string) []OrderBookLevel {
	if len(raw) == 0 {
		return nil
	}

	out := make([]OrderBookLevel, 0, len(raw))
	for _, lvl := range raw {
		if len(lvl) < 2 {
			continue
		}
		out = append(out, OrderBookLevel{
			Price:    lvl[0],
			Quantity: lvl[1],
		})
	}
	return out
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
	Candles    map[string][]db.Candle1m
	BookVolume map[string]OrderBookVolumeView
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

func (m *MockCandleProvider) FetchOrderBookAndVolume(ctx context.Context, symbol string, depthLimit int, volumeLimit int) (*OrderBookVolumeView, error) {
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	if m.BookVolume != nil {
		if val, ok := m.BookVolume[symbol]; ok {
			copied := val
			copied.Bids = append([]OrderBookLevel(nil), val.Bids...)
			copied.Asks = append([]OrderBookLevel(nil), val.Asks...)
			copied.VolumeHistory = append([]VolumePoint(nil), val.VolumeHistory...)
			return &copied, nil
		}
	}

	candles, err := m.Fetch1m(ctx, symbol, time.Time{}, time.Time{}, volumeLimit)
	if err != nil {
		return nil, err
	}

	volumes := make([]VolumePoint, 0, len(candles))
	lastVolume := "0"
	at := time.Now().UTC().Unix()
	for _, c := range candles {
		volumes = append(volumes, VolumePoint{
			At:     c.T,
			Volume: c.V,
		})
	}
	if len(candles) > 0 {
		last := candles[len(candles)-1]
		lastVolume = last.V
		at = last.T
	}

	return &OrderBookVolumeView{
		Symbol:        symbol,
		At:            at,
		Volume:        lastVolume,
		VolumeHistory: volumes,
	}, nil
}
