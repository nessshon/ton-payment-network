//go:build js && wasm

package oracle

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"syscall/js"

	"github.com/xssnick/tonutils-go/tvm/cell"
)

// WebProvider is a generic web-compatible price provider for WASM builds.
// It knows nothing about Binance or any specific exchange — it simply
// fetches pre-signed PriceInner cells (BOC array) from the backend API
// using JS fetch. The backend is responsible for price sourcing and signing.
type WebProvider struct {
	symbol   string
	proofKey ed25519.PublicKey
}

// NewWebProvider creates a web provider for the given symbol.
// symbol is the trading pair (e.g., "BTCUSDT").
// proofKey is the public key used to verify proof cell signatures.
func NewWebProvider(symbol string, proofKey ed25519.PublicKey) *WebProvider {
	return &WebProvider{
		symbol:   symbol,
		proofKey: proofKey,
	}
}

// ProofPublicKey implements proofKeyProvider.
func (w *WebProvider) ProofPublicKey() ed25519.PublicKey {
	return append(ed25519.PublicKey(nil), w.proofKey...)
}

// Fetch implements PriceProvider — not actually used since WebProvider
// relies on FetchSince exclusively. Returns an error if called directly.
func (w *WebProvider) Fetch(ctx context.Context, at int64) (int64, *big.Int, error) {
	return 0, nil, fmt.Errorf("WebProvider.Fetch: use FetchSince instead")
}

// FetchSince implements SinceProvider by fetching proof cell BOCs from the server.
func (w *WebProvider) FetchSince(ctx context.Context, since int64) ([]RangePrice, error) {
	url := fmt.Sprintf("/web-channel/api/v1/derivatives/prices?symbol=%s&since=%d", w.symbol, since)
	data, err := webFetchJSON(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("web fetch prices since: %w", err)
	}

	// Server returns JSON array of base64-encoded BOC strings.
	var bocs []string
	if err := json.Unmarshal(data, &bocs); err != nil {
		return nil, fmt.Errorf("web parse prices: %w", err)
	}

	result := make([]RangePrice, 0, len(bocs))
	for _, boc := range bocs {
		raw, err := base64.StdEncoding.DecodeString(boc)
		if err != nil {
			continue
		}
		proofCell, err := cell.FromBOC(raw)
		if err != nil {
			continue
		}

		at, price, err := ParsePriceProof(proofCell)
		if err != nil || price == nil {
			continue
		}

		result = append(result, RangePrice{
			At:    at,
			Price: price,
			Proof: proofCell,
		})
	}
	return result, nil
}

// webFetchJSON performs a JS fetch call and returns the response body as JSON bytes.
func webFetchJSON(ctx context.Context, url string) ([]byte, error) {
	ch := make(chan struct {
		data []byte
		err  error
	}, 1)

	fetch := js.Global().Get("fetch")
	if fetch.IsUndefined() {
		return nil, fmt.Errorf("fetch not supported")
	}

	promise := fetch.Invoke(url)
	then := js.FuncOf(func(this js.Value, args []js.Value) any {
		resp := args[0]
		jsonPromise := resp.Call("json")
		thenJson := js.FuncOf(func(this js.Value, args []js.Value) any {
			jsObj := args[0]
			str := js.Global().Get("JSON").Call("stringify", jsObj).String()
			ch <- struct {
				data []byte
				err  error
			}{[]byte(str), nil}
			return nil
		})
		jsonPromise.Call("then", thenJson)
		return nil
	})

	catch := js.FuncOf(func(this js.Value, args []js.Value) any {
		errMsg := args[0].String()
		ch <- struct {
			data []byte
			err  error
		}{nil, fmt.Errorf(errMsg)}
		return nil
	})

	promise.Call("then", then).Call("catch", catch)

	select {
	case result := <-ch:
		return result.data, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
