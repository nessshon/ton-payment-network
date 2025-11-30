//go:build !(js && wasm)

package chain

import (
	"context"
	"errors"
	"github.com/xssnick/ton-payment-network/pkg/log"
	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/tonpayments"
	"github.com/xssnick/ton-payment-network/tonpayments/chain/client"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/ton"
	"time"
)

type accFetchTask struct {
	master   *ton.BlockIDExt
	tx       *tlb.Transaction
	addr     *address.Address
	callback func(ev *tonpayments.ChannelUpdatedEvent)
}

func (v *Scanner) accFetcherWorker(threads int) {
	for y := 0; y < threads; y++ {
		go func() {
			for {
				select {
				case <-v.globalCtx.Done():
					return
				case task := <-v.taskPool:
					v.accFetch(task)
				}
			}
		}()
	}
}

func (v *Scanner) accFetch(task accFetchTask) {
	var ev *tonpayments.ChannelUpdatedEvent
	defer func() {
		task.callback(ev)
	}()

	var acc *tlb.Account
	{
		ctx := v.globalCtx
		for i := 0; i < 20; i++ { // TODO: retry without loosing
			var err error
			ctx, err = v.api.Client().StickyContextNextNode(ctx)
			if err != nil {
				v.log.Debug().Err(err).Str("addr", task.addr.String()).Msg("failed to pick next node")
				break
			}

			qCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			acc, err = v.api.WaitForBlock(task.master.SeqNo).GetAccount(qCtx, task.master, task.addr)
			cancel()
			if err != nil {
				v.log.Debug().Err(err).Str("addr", task.addr.String()).Msg("failed to get account")
				time.Sleep(100 * time.Millisecond)
				continue
			}
			break
		}
	}

	if acc == nil || !acc.IsActive || acc.State.Status != tlb.AccountStatusActive || acc.Code == nil || acc.Data == nil {
		// not active or failed
		return
	}

	p, err := v.client.ParseChannel(task.addr, acc.Code, acc.Data, true)
	if err != nil {
		if !errors.Is(err, payments.ErrVerificationNotPassed) {
			v.log.Warn().Err(err).Str("addr", task.addr.String()).Msg("failed to parse payment channel")
		}
		return
	}

	log.Debug().Str("address", task.addr.String()).Msg("account fetched and parsed, reporting channel update event")

	res, err := client.ConvertTx(task.tx)
	if err != nil {
		v.log.Warn().Err(err).Str("addr", task.addr.String()).Msg("failed to convert transaction")
		return
	}

	ev = &tonpayments.ChannelUpdatedEvent{
		Transaction:   res,
		LatestChannel: p,
	}
}
