package derivatives

import (
	"context"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/xssnick/ton-payment-network/tonpayments/db"
)

type memStore struct {
	candles map[string][]db.Candle1m
	last    map[string]int64
}

func (m *memStore) PutCandles1m(ctx context.Context, symbol string, candles []db.Candle1m) error {
	m.candles[symbol] = append(m.candles[symbol], candles...)
	return nil
}
func (m *memStore) GetCandles1m(ctx context.Context, symbol string, from, to int64, limit int) ([]db.Candle1m, error) {
	var out []db.Candle1m
	for _, c := range m.candles[symbol] {
		if (from == 0 || c.T >= from) && (to == 0 || c.T <= to) {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].T < out[j].T })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
func (m *memStore) GetLastCandleMinute(ctx context.Context, symbol string) (int64, error) {
	return m.last[symbol], nil
}
func (m *memStore) SetLastCandleMinute(ctx context.Context, symbol string, minute int64) error {
	m.last[symbol] = minute
	return nil
}

func TestManagerEnsureRange(t *testing.T) {
	store := &memStore{candles: map[string][]db.Candle1m{}, last: map[string]int64{}}
	prov := &MockCandleProvider{Candles: map[string][]db.Candle1m{}}

	start := time.Unix(1700000000, 0).UTC()
	mk := func(n int) []db.Candle1m {
		arr := make([]db.Candle1m, n)
		for i := 0; i < n; i++ {
			arr[i] = db.Candle1m{T: start.Add(time.Duration(i)*time.Minute).Unix(), O: "1", H: "1", L: "1", C: "1", V: "0"}
		}
		return arr
	}
	prov.Candles["BTCUSDT"] = mk(10)

	mgr := NewManager(store, prov)
	ctx := context.Background()
	from := start.Unix()
	to := start.Add(9 * time.Minute).Unix()
	if err := mgr.EnsureRange(ctx, "btcusdt", from, to); err != nil {
		t.Fatalf("ensure range failed: %v", err)
	}

	got, _ := store.GetCandles1m(ctx, "btcusdt", from, to, 100)
	want := mk(10)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected candles stored: %#v != %#v", got, want)
	}
	if last, _ := store.GetLastCandleMinute(ctx, "btcusdt"); last != want[len(want)-1].T {
		t.Fatalf("unexpected last minute: %d", last)
	}
}
