//go:build !(js && wasm)

package chain

import (
	"context"
	"github.com/xssnick/ton-payment-network/pkg/log"
	"github.com/xssnick/ton-payment-network/tonpayments"
	"github.com/xssnick/ton-payment-network/tonpayments/db"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"time"
)

func (v *Scanner) OnChannelUpdate(_ context.Context, ch *db.Channel, statusChanged bool) {
	if !statusChanged {
		return
	}

	v.mx.Lock()
	defer v.mx.Unlock()

	if ch.Status == db.ChannelStateInactive {
		if c := v.activeChannels[ch.Our.Address]; c != nil {
			c() // stop listener
			delete(v.activeChannels, ch.Our.Address)
		}
		log.Info().Str("address", ch.Our.Address).Msg("stop listening for channel events")
		return
	}

	if v.activeChannels[ch.Our.Address] == nil {
		ctx, cancel := context.WithCancel(v.globalCtx)
		v.activeChannels[ch.Our.Address] = cancel

		ltOur, ltTheir := uint64(0), uint64(0)
		if ch.Our.LatestProcessedLT > 0 {
			// to report last tx
			ltOur = ch.Our.LatestProcessedLT - 1
		}
		if ch.Their.LatestProcessedLT > 0 {
			// to report last tx
			ltTheir = ch.Their.LatestProcessedLT - 1
		}

		log.Info().Str("address", ch.Our.Address).Msg("start listening for onchain channel events")
		go v.startForContract(ctx, ch.Our.Address, address.MustParseAddr(ch.Our.Address), ltOur)
		go v.startForContract(ctx, ch.Our.Address, address.MustParseAddr(ch.Their.Address), ltTheir)
	}
}

func (v *Scanner) startForContract(ctx context.Context, channel string, addr *address.Address, sinceLT uint64) {
	originalCtx := ctx
	for {
		ch := make(chan *tlb.Transaction, 1)
		go v.api.SubscribeOnTransactions(ctx, addr, sinceLT, ch)

		for transaction := range ch {
			for {
				m, err := v.api.GetMasterchainInfo(ctx)
				if err != nil {
					select {
					case <-ctx.Done():
						return
					default:
					}

					time.Sleep(1 * time.Second)
					log.Warn().Err(err).Str("channel", channel).Str("address", addr.String()).Msg("failed to fetch master block, will retry in 1s")
					continue
				}

				log.Debug().Str("channel", channel).Str("address", addr.String()).Msg("found new transaction, fetching account")
				v.accFetch(accFetchTask{
					master: m,
					tx:     transaction,
					addr:   addr,
					callback: func(ev *tonpayments.ChannelUpdatedEvent) {
						if ev == nil {
							log.Warn().Str("channel", channel).Str("address", addr.String()).Msg("transaction channel updated event is nil (skipped tx)")
							return
						}

						v.events <- ev
					},
				})
				break
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
			log.Warn().Str("channel", channel).Str("address", addr.String()).Msg("SubscribeOnTransactions stopped listening because of LS reported tx not in DB, will retry with another LS...")

			var err error
			ctx, err = v.api.Client().StickyContextNextNode(ctx)
			if err != nil {
				log.Error().Str("channel", channel).Err(err).Msg("all nodes failed, will retry all again :(")
				ctx = originalCtx
			}
		}
	}
}
