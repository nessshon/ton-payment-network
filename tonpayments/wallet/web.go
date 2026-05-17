//go:build js && wasm

package wallet

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/xssnick/ton-payment-network/tonpayments"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"sync"
	"syscall/js"
	"time"
)

type Wallet struct {
}

var walletTxMu sync.Mutex

func InitWallet() (*Wallet, error) {
	return &Wallet{}, nil
}

func (w *Wallet) WalletAddress() *address.Address {
	walletAddress := js.Global().Get("walletAddress")
	if walletAddress.Type() != js.TypeFunction {
		panic("walletAddress func is not registered globally")
	}
	res := walletAddress.Invoke().String()
	return address.MustParseAddr(res).Testnet(false).Bounce(false)
}

func (w *Wallet) DoTransaction(ctx context.Context, reason string, to *address.Address, amt tlb.Coins, body *cell.Cell) ([]byte, error) {
	return w.DoTransactionMany(ctx, reason, []tonpayments.WalletMessage{
		{
			To:     to,
			Amount: amt,
			Body:   body,
		},
	})
}

func (w *Wallet) DoTransactionMany(ctx context.Context, reason string, messages []tonpayments.WalletMessage) ([]byte, error) {
	if len(messages) > 4 {
		return nil, fmt.Errorf("attempt to execute more than 4 messages in web")
	}

	doTransaction := js.Global().Get("doTransaction")
	if doTransaction.Type() != js.TypeFunction {
		return nil, errors.New("doTransaction func is not registered globally")
	}

	notifyWalletRequestStatus("queued", reason, len(messages), "")
	walletTxMu.Lock()
	defer walletTxMu.Unlock()
	notifyWalletRequestStatus("requested", reason, len(messages), "")

	txMessages := make([]interface{}, len(messages))
	for i, msg := range messages {
		if msg.EC != nil {
			err := errors.New("ec is not supported on web")
			notifyWalletRequestStatus("failed", reason, len(messages), err.Error())
			return nil, err
		}

		var siBoc string
		if msg.StateInit != nil {
			stateCell, err := tlb.ToCell(msg.StateInit)
			if err != nil {
				return nil, err
			}
			msg.To = address.NewAddress(0, 0, stateCell.Hash()).Bounce(false)
			siBoc = base64.StdEncoding.EncodeToString(stateCell.ToBOC())
		}
		if msg.To == nil {
			err := errors.New("wallet message target is not specified")
			notifyWalletRequestStatus("failed", reason, len(messages), err.Error())
			return nil, err
		}

		txMsg := map[string]any{
			"to":      msg.To.String(),
			"amtNano": msg.Amount.Nano().String(),
		}

		if msg.Body != nil {
			txMsg["body"] = base64.StdEncoding.EncodeToString(msg.Body.ToBOC())
		}

		if siBoc != "" {
			txMsg["stateInit"] = siBoc
		}

		txMessages[i] = txMsg
	}

	promise := doTransaction.Invoke(
		js.ValueOf(reason),
		js.ValueOf(txMessages),
	)

	val, err := awaitPromise(promise)
	if err != nil {
		notifyWalletRequestStatus("failed", reason, len(messages), err.Error())
		return nil, err
	}

	hash, err := parseResultMsg(val.String())
	if err != nil {
		notifyWalletRequestStatus("failed", reason, len(messages), err.Error())
		return nil, err
	}

	notifyWalletRequestStatus("submitted", reason, len(messages), base64.StdEncoding.EncodeToString(hash))
	return hash, nil
}

func notifyWalletRequestStatus(phase, reason string, messages int, details string) {
	handler := js.Global().Get("onPaymentWalletRequestUpdated")
	if handler.Type() != js.TypeFunction {
		return
	}

	ev := map[string]any{
		"phase":    phase,
		"reason":   reason,
		"messages": messages,
		"at":       time.Now().UnixMilli(),
	}
	if details != "" {
		ev["details"] = details
	}

	handler.Invoke(js.ValueOf(ev))
}

func awaitPromise(p js.Value) (js.Value, error) {
	ch := make(chan struct{})
	var res js.Value
	var err error

	var ok, fail js.Func

	ok = js.FuncOf(func(this js.Value, args []js.Value) any {
		if len(args) > 0 {
			res = args[0]
		}
		ok.Release()
		fail.Release()
		close(ch)
		return nil
	})

	fail = js.FuncOf(func(this js.Value, args []js.Value) any {
		var msg string
		if len(args) == 0 || args[0].IsUndefined() || args[0].IsNull() {
			msg = "promise rejected"
		} else if args[0].Type() == js.TypeString {
			msg = args[0].String()
		} else {
			msg = js.Global().Get("String").Invoke(args[0]).String()
		}
		err = errors.New(msg)
		ok.Release()
		fail.Release()
		close(ch)
		return nil
	})

	p.Call("then", ok, fail)
	<-ch
	return res, err
}

func parseResultMsg(val string) ([]byte, error) {
	boc, err := base64.StdEncoding.DecodeString(val)
	if err != nil {
		return nil, fmt.Errorf("failed to decode res boc: %w", err)
	}

	msgCell, err := cell.FromBOC(boc)
	if err != nil {
		return nil, fmt.Errorf("failed to parse res cell: %w", err)
	}

	var msg tlb.ExternalMessageIn
	if err = tlb.Parse(&msg, msgCell); err != nil {
		return nil, fmt.Errorf("failed to parse res msg: %w", err)
	}

	return msg.NormalizedHash(), nil
}
