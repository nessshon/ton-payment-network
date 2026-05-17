package derivatives

import (
	"context"
	"fmt"
	"time"

	"github.com/xssnick/ton-payment-network/tonpayments/db"
)

// CandleProvider provides 1-minute candles for symbols.
type CandleProvider interface {
	Fetch1m(ctx context.Context, symbol string, start, end time.Time, limit int) ([]db.Candle1m, error)
	FetchOrderBookAndVolume(ctx context.Context, symbol string, depthLimit int, volumeLimit int) (*OrderBookVolumeView, error)
}

// Manager coordinates fetching/storing 1m candles and provides helpers for APIs.
type Manager struct {
	store    Store
	provider CandleProvider
}

type Store interface {
	PutCandles1m(ctx context.Context, symbol string, candles []db.Candle1m) error
	GetCandles1m(ctx context.Context, symbol string, from, to int64, limit int) ([]db.Candle1m, error)
	GetLastCandleMinute(ctx context.Context, symbol string) (int64, error)
	SetLastCandleMinute(ctx context.Context, symbol string, minute int64) error
}

func NewManager(store Store, provider CandleProvider) *Manager {
	return &Manager{store: store, provider: provider}
}

// EnsureRange fetches and stores candles so that [from,to] minute range (seconds, minute-aligned)
// is fully covered without gaps. If to==0, fills up to now.
func (m *Manager) EnsureRange(ctx context.Context, symbol string, from, to int64) error {
	if to == 0 {
		to = time.Now().UTC().Unix()
	}
	if from == 0 {
		// resume from last+60 or backfill last 1000 minutes by default
		last, err := m.store.GetLastCandleMinute(ctx, symbol)
		if err != nil {
			return err
		}
		if last == 0 {
			from = alignMinute(to - 1000*60) // ~ 16h back
		} else {
			from = last + 60
		}
	}
	from = alignMinute(from)
	to = alignMinute(to)
	if from > to {
		return nil
	}

	// paginate in chunks
	cur := from
	for cur <= to {
		end := cur + 1500*60 - 60 // max 1500 candles per request
		if end > to {
			end = to
		}
		candles, err := m.provider.Fetch1m(ctx, symbol, time.Unix(cur, 0).UTC(), time.Unix(end, 0).UTC(), 1500)
		if err != nil {
			return fmt.Errorf("fetch candles: %w", err)
		}
		if len(candles) == 0 {
			// nothing returned; advance by window to avoid infinite loop
			cur = end + 60
			continue
		}
		if err := m.store.PutCandles1m(ctx, symbol, candles); err != nil {
			return err
		}
		last := candles[len(candles)-1].T
		if err := m.store.SetLastCandleMinute(ctx, symbol, last); err != nil {
			return err
		}
		cur = last + 60
	}
	return nil
}

func (m *Manager) GetCandles(ctx context.Context, symbol string, from, to int64, limit int) ([]db.Candle1m, error) {
	return m.store.GetCandles1m(ctx, symbol, from, to, limit)
}

func alignMinute(ts int64) int64 {
	return ts - (ts % 60)
}
