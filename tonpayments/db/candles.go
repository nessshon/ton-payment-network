package db

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"
)

// Candle1m is a 1-minute OHLCV candle. Prices and volume are strings to avoid
// precision loss (store scaled integers as decimal strings if needed).
type Candle1m struct {
	T int64  `json:"t"` // unix seconds, start of minute
	O string `json:"o"`
	H string `json:"h"`
	L string `json:"l"`
	C string `json:"c"`
	V string `json:"v"`
}

// PutCandles1m upserts a batch of candles for a given symbol.
func (d *DB) PutCandles1m(ctx context.Context, symbol string, candles []Candle1m) error {
	if symbol == "" {
		return fmt.Errorf("empty symbol")
	}
	prefix := "candle:" + symbol + ":"
	return d.Transaction(ctx, func(ctx context.Context) error {
		tx := d.storage.GetExecutor(ctx)
		for _, c := range candles {
			key := []byte(prefix + encodeInt64Key(c.T))
			val, err := json.Marshal(&c)
			if err != nil {
				return fmt.Errorf("marshal candle: %w", err)
			}
			if err = tx.Put(key, val); err != nil {
				return fmt.Errorf("put candle: %w", err)
			}
		}
		return nil
	})
}

// GetCandles1m returns candles in [from, to] inclusive by start time, ordered ascending.
func (d *DB) GetCandles1m(ctx context.Context, symbol string, from, to int64, limit int) ([]Candle1m, error) {
	if symbol == "" {
		return nil, fmt.Errorf("empty symbol")
	}
	if to != 0 && from > to {
		from, to = to, from
	}
	prefix := []byte("candle:" + symbol + ":")
	tx := d.storage.GetExecutor(ctx)
	it := tx.NewIterator(prefix, true)
	defer it.Release()

	startKey := []byte("candle:" + symbol + ":" + encodeInt64Key(from))
	// iterate forward; skip until >= startKey
	var res []Candle1m
	for it.Next() {
		k := it.Key()
		if string(k) < string(startKey) {
			continue
		}
		if to != 0 {
			// extract timestamp part
			if len(k) >= len(prefix)+12 { // base58? we used binary->base64 string
				// we encoded with base64; just decode by stripping prefix and base64 decode
			}
		}
		var c Candle1m
		if err := json.Unmarshal(it.Value(), &c); err != nil {
			return nil, fmt.Errorf("unmarshal candle: %w", err)
		}
		if to != 0 && c.T > to {
			break
		}
		res = append(res, c)
		if limit > 0 && len(res) >= limit {
			break
		}
	}
	if err := it.Error(); err != nil {
		return nil, fmt.Errorf("iterator error: %w", err)
	}
	return res, nil
}

// GetLastCandleMinute returns the last stored minute start for the symbol, or 0.
func (d *DB) GetLastCandleMinute(ctx context.Context, symbol string) (int64, error) {
	if symbol == "" {
		return 0, fmt.Errorf("empty symbol")
	}
	tx := d.storage.GetExecutor(ctx)
	key := []byte("candle_last:" + symbol)
	b, err := tx.Get(key)
	if err != nil {
		if err == ErrNotFound {
			return 0, nil
		}
		return 0, fmt.Errorf("get last candle: %w", err)
	}
	if len(b) != 8 {
		return 0, nil
	}
	return int64(binary.BigEndian.Uint64(b)), nil
}

// SetLastCandleMinute sets the last synced minute start for the symbol.
func (d *DB) SetLastCandleMinute(ctx context.Context, symbol string, minute int64) error {
	if symbol == "" {
		return fmt.Errorf("empty symbol")
	}
	return d.Transaction(ctx, func(ctx context.Context) error {
		tx := d.storage.GetExecutor(ctx)
		buf := make([]byte, 8)
		binary.BigEndian.PutUint64(buf, uint64(minute))
		if err := tx.Put([]byte("candle_last:"+symbol), buf); err != nil {
			return fmt.Errorf("set last candle: %w", err)
		}
		return nil
	})
}

func encodeInt64Key(v int64) string {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, uint64(v))
	return base64.StdEncoding.EncodeToString(buf)
}

// ListCandleSymbols returns list of symbols that have candle_last markers.
func (d *DB) ListCandleSymbols(ctx context.Context) ([]string, error) {
	tx := d.storage.GetExecutor(ctx)
	prefix := []byte("candle_last:")
	it := tx.NewIterator(prefix, true)
	defer it.Release()

	var out []string
	for it.Next() {
		k := string(it.Key())
		if !strings.HasPrefix(k, string(prefix)) {
			continue
		}
		sym := strings.TrimPrefix(k, string(prefix))
		if sym != "" {
			out = append(out, sym)
		}
	}
	if err := it.Error(); err != nil {
		return nil, fmt.Errorf("iterator error: %w", err)
	}
	return out, nil
}
