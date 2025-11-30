//go:build !(js && wasm)

package tonpayments

import (
	"context"
	"encoding/base64"
	"fmt"
	"github.com/xssnick/ton-payment-network/pkg/log"
	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/tonpayments/db"
	"github.com/xssnick/ton-payment-network/tonpayments/metrics"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"math/big"
	"strconv"
	"time"
)

func (s *Service) walletMonitor() {
	for {
		select {
		case <-s.globalCtx.Done():
			return
		case <-time.After(5 * time.Second):
		}

		err := func() error {
			ctx, cancel := context.WithTimeout(s.globalCtx, 10*time.Second)
			defer cancel()

			acc, err := s.ton.GetAccount(ctx, s.wallet.WalletAddress(), time.Time{})
			if err != nil {
				return fmt.Errorf("failed to get ton balance: %w", err)
			}

			if !acc.HasState {
				return fmt.Errorf("account is not yet initialized")
			}

			for ec, config := range s.cfg.SupportedCoins.ExtraCurrencies {
				var balance float64
				if !acc.ExtraCurrencies.IsEmpty() {
					val, err := acc.ExtraCurrencies.LoadValueByIntKey(big.NewInt(int64(ec)))
					if err != nil {
						log.Trace().Err(err).Msg("failed to get ec key")
						continue
					}

					x, err := val.LoadVarUInt(32)
					if err != nil {
						log.Trace().Err(err).Msg("failed to get ec value")
						continue
					}

					balance, _ = strconv.ParseFloat(tlb.MustFromNano(x, int(config.Decimals)).String(), 64)
				}

				metrics.WalletBalance.WithLabelValues(config.Symbol).Set(balance)
			}

			for jettonAddr, config := range s.cfg.SupportedCoins.Jettons {
				var balance float64
				jb, err := s.ton.GetJettonBalance(ctx, address.MustParseAddr(jettonAddr), s.wallet.WalletAddress(), time.Time{})
				if err != nil {
					log.Trace().Err(err).Msg("failed to get jetton balance")
					continue
				}

				balance, _ = strconv.ParseFloat(tlb.MustFromNano(jb, int(config.Decimals)).String(), 64)

				metrics.WalletBalance.WithLabelValues(config.Symbol).Set(balance)
			}

			balance, _ := strconv.ParseFloat(acc.Balance.String(), 64)
			metrics.WalletBalance.WithLabelValues("TON").Set(balance)

			return nil
		}()
		if err != nil {
			log.Trace().Err(err).Msg("failed to monitor wallet balance")
		}
	}
}

func (s *Service) channelsMonitor() {
	type split struct {
		balance map[string]float64
		condNum int
		actNum  int
	}
	stats := map[string]map[string]map[bool]*split{}

	for {
		select {
		case <-s.globalCtx.Done():
			return
		case <-time.After(5 * time.Second):
		}

		list, err := s.ListChannels(context.Background(), nil, db.ChannelStateActive)
		if err != nil {
			log.Error().Err(err).Msg("failed to list active channels")
			continue
		}

	next:
		for _, channel := range list {
			for _, isOurSide := range []bool{true, false} {
				balances, err := channel.CalcBalance(context.Background(), !isOurSide, s)
				if err != nil {
					log.Error().Err(err).Msg("failed to calc balance")
					continue next
				}

				sideCond, sideAct := channel.Our.Data.Conditionals, channel.Our.Data.ActionStates
				if !isOurSide {
					sideCond, sideAct = channel.Their.Data.Conditionals, channel.Their.Data.ActionStates
				}

				conds, _ := sideCond.LoadAll()
				acts, _ := sideAct.LoadAll()

				for _, coinConfig := range s.knownBalanceTypes {
					channelName := "other"
					s.urgentPeersMx.RLock()
					key := base64.StdEncoding.EncodeToString(channel.Their.OnchainInfo.Key)
					if s.urgentPeers[key] > 0 {
						channelName = key[:16]
					}
					s.urgentPeersMx.RUnlock()

					channelStats, exists := stats[channelName]
					if !exists {
						channelStats = map[string]map[bool]*split{}
						stats[channelName] = channelStats
					}

					coinStats, exists := channelStats[coinConfig.Symbol]
					if !exists {
						coinStats = map[bool]*split{}
						channelStats[coinConfig.Symbol] = coinStats
					}

					sideStats, exists := coinStats[isOurSide]
					if !exists {
						sideStats = &split{
							balance: map[string]float64{},
						}
						coinStats[isOurSide] = sideStats
					}
					sideStats.actNum = len(acts)
					sideStats.condNum = len(conds)

					b := balances[coinConfig.BalanceID]
					if b == nil {
						b = payments.NewBalanceInfo(coinConfig)
					}

					for _, category := range []string{"onchain", "action", "on_hold", "cond_locked", "cond_pending"} {
						var value *big.Int
						switch category {
						case "onchain":
							value = b.Onchain
						case "action":
							value = b.Action
						case "on_hold":
							value = b.OnHold
						case "cond_locked":
							value = b.ConditionalLocked
						case "cond_pending":
							value = b.ConditionalPending
						}

						parsedValue, _ := strconv.ParseFloat(tlb.MustFromNano(value, int(coinConfig.Decimals)).String(), 64)
						sideStats.balance[category] += parsedValue
					}
				}
			}
		}

		for channelName, channelStats := range stats {
			for coinSymbol, coinStats := range channelStats {
				for isOurSide, sideStats := range coinStats {
					metrics.ActiveConditionals.WithLabelValues(channelName, coinSymbol, strconv.FormatBool(isOurSide)).Set(float64(sideStats.condNum))
					metrics.ActiveActions.WithLabelValues(channelName, coinSymbol, strconv.FormatBool(isOurSide)).Set(float64(sideStats.actNum))

					sideStats.condNum = 0
					sideStats.actNum = 0

					for category, balance := range sideStats.balance {
						metrics.ChannelBalance.WithLabelValues(channelName, coinSymbol, strconv.FormatBool(isOurSide), category).Set(balance)

						sideStats.balance[category] = 0 // reset to calc in next iteration
					}
				}
			}
		}
	}
}

func (s *Service) taskMonitor() {
	taskStats := map[string]map[bool]map[bool]float64{}

	for {
		select {
		case <-s.globalCtx.Done():
			return
		case <-time.After(5 * time.Second):
		}

		list, err := s.db.ListActiveTasks(context.Background(), PaymentsTaskPool)
		if err != nil {
			log.Error().Err(err).Msg("failed to list active tasks")
			continue
		}

		for _, task := range list {
			hasError := task.LastError != ""
			executeLater := task.ExecuteAfter.After(time.Now())

			typeStats, exists := taskStats[task.Type]
			if !exists {
				typeStats = map[bool]map[bool]float64{}
				taskStats[task.Type] = typeStats
			}

			errorStats, exists := typeStats[hasError]
			if !exists {
				errorStats = map[bool]float64{}
				typeStats[hasError] = errorStats
			}

			errorStats[executeLater] += 1
		}

		for jobType, errorStats := range taskStats {
			for hasError, timingStats := range errorStats {
				for executeLater, taskCount := range timingStats {
					metrics.QueuedTasks.WithLabelValues(jobType, strconv.FormatBool(hasError), strconv.FormatBool(executeLater)).Set(taskCount)

					timingStats[executeLater] = 0 // reset to calc in next iteration
				}
			}
		}
	}
}
