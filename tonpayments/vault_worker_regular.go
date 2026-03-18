//go:build !(js && wasm)

package tonpayments

import (
	"context"
	"time"

	"github.com/xssnick/ton-payment-network/pkg/log"
)

var VaultWorkerInterval = 30 * time.Second

func (s *Service) startVaultWorker() {
	if s.vaultManager == nil || !s.vaultCfg.UseOnOurSide {
		return
	}

	go func() {
		runOnce := func() {
			ctx, cancel := context.WithTimeout(s.globalCtx, VaultWorkerInterval)
			defer cancel()

			if err := s.vaultManager.Reconcile(ctx); err != nil {
				log.Error().Err(err).Msg("failed to reconcile vault")
			}
		}

		runOnce()

		ticker := time.NewTicker(VaultWorkerInterval)
		defer ticker.Stop()

		for {
			select {
			case <-s.globalCtx.Done():
				return
			case <-ticker.C:
				runOnce()
			}
		}
	}()
}
