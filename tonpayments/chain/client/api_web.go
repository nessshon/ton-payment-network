//go:build js && wasm

package client

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"math/big"
	"syscall/js"
	"time"
)

type TON struct {
}

func NewTON() *TON {
	return &TON{}
}

func (t *TON) GetAccount(ctx context.Context, addr *address.Address, after time.Time) (*Account, error) {
	url := fmt.Sprintf("/web-channel/api/v1/ton/account?address=%s&after=%d", addr.String(), after.Unix())
	resp, err := fetchJSON(ctx, url)
	if err != nil {
		return nil, err
	}

	var acc Account
	if err := json.Unmarshal(resp, &acc); err != nil {
		return nil, err
	}

	return &acc, nil
}

func (t *TON) GetJettonWalletAddress(ctx context.Context, root, addr *address.Address) (*address.Address, error) {
	url := fmt.Sprintf("/web-channel/api/v1/ton/jetton/wallet?jetton=%s&address=%s", root.String(), addr.String())
	resp, err := fetchJSON(ctx, url)
	if err != nil {
		return nil, err
	}

	var result struct{ Address string }
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, err
	}

	return address.ParseAddr(result.Address)
}

func (t *TON) GetJettonBalance(ctx context.Context, root, addr *address.Address, after time.Time) (*big.Int, error) {
	url := fmt.Sprintf("/web-channel/api/v1/ton/jetton/balance?jetton=%s&address=%s&after=%d", root.String(), addr.String(), after.Unix())
	resp, err := fetchJSON(ctx, url)
	if err != nil {
		return nil, err
	}

	var result struct{ Balance string }
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, err
	}

	bal := new(big.Int)
	bal.SetString(result.Balance, 10)
	return bal, nil
}

func (t *TON) GetLastTransaction(ctx context.Context, addr *address.Address, after time.Time) (*Transaction, *Account, error) {
	url := fmt.Sprintf("/web-channel/api/v1/ton/transaction/last?address=%s&after=%d", addr.String(), after.Unix())
	resp, err := fetchJSON(ctx, url)
	if err != nil {
		return nil, nil, err
	}

	var result struct {
		Account     *Account
		Transaction *Transaction
	}

	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, nil, err
	}

	return result.Transaction, result.Account, nil
}

func fetchJSON(ctx context.Context, url string) ([]byte, error) {
	ch := make(chan struct {
		data []byte
		err  error
	}, 1)

	// JS fetch
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

func (t *TON) GetTransactionsList(ctx context.Context, addr *address.Address, lt uint64, hash []byte) ([]Transaction, error) {
	url := fmt.Sprintf("/web-channel/api/v1/ton/transaction/list?address=%s&lt=%d&hash=%s", addr.String(), lt, base64.URLEncoding.EncodeToString(hash))
	resp, err := fetchJSON(ctx, url)
	if err != nil {
		return nil, err
	}

	var result struct {
		Transactions []Transaction
	}

	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, err
	}

	return result.Transactions, nil
}

func (t *TON) GetTransactionByInMsgHash(ctx context.Context, addr *address.Address, msgHash []byte, after time.Time) (*Transaction, error) {
	url := fmt.Sprintf("/web-channel/api/v1/ton/transaction/by_in_msg_hash?address=%s&hash=%s&after=%d",
		addr.String(), base64.URLEncoding.EncodeToString(msgHash), after.Unix())
	resp, err := fetchJSON(ctx, url)
	if err != nil {
		return nil, err
	}

	var result struct {
		Transaction Transaction
	}

	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, err
	}

	return &result.Transaction, nil
}

func (t *TON) SendWaitExternalMessage(ctx context.Context, to *address.Address, body *cell.Cell) ([]byte, error) {
	type request struct {
		Body *cell.Cell `json:"body"`
	}

	url := fmt.Sprintf("/web-channel/api/v1/ton/external?address=%s", to.String())
	resp, err := postJSON(ctx, url, request{body})
	if err != nil {
		return nil, err
	}

	var result struct {
		MsgHash []byte
	}

	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, err
	}

	return result.MsgHash, nil
}

func postJSON(ctx context.Context, url string, payload any) ([]byte, error) {
	ch := make(chan struct {
		data []byte
		err  error
	}, 1)

	fetch := js.Global().Get("fetch")
	if fetch.IsUndefined() {
		return nil, fmt.Errorf("fetch not supported")
	}

	b, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	opts := map[string]any{
		"method": "POST",
		"headers": map[string]any{
			"Content-Type": "application/json",
		},
		"body": string(b),
	}

	promise := fetch.Invoke(url, js.ValueOf(opts))

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
