package tonpayments

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/pkg/payments/actions"
	paymentvault "github.com/xssnick/ton-payment-network/pkg/payments/vault"
	"github.com/xssnick/ton-payment-network/tonpayments/db"
	"github.com/xssnick/tonutils-go/address"
)

type vaultWalletAdapter struct {
	inner Wallet
}

func (v vaultWalletAdapter) WalletAddress() *address.Address {
	return v.inner.WalletAddress()
}

func (v vaultWalletAdapter) DoTransactionMany(ctx context.Context, reason string, messages []paymentvault.WalletMessage) ([]byte, error) {
	converted := make([]WalletMessage, 0, len(messages))
	for _, message := range messages {
		converted = append(converted, WalletMessage{
			To:        message.To,
			Amount:    message.Amount,
			Body:      message.Body,
			StateInit: message.StateInit,
			EC:        message.EC,
		})
	}
	return v.inner.DoTransactionMany(ctx, reason, converted)
}

func (s *Service) initVaultManager() error {
	if !s.vaultCfg.UseOnOurSide && !s.vaultCfg.AllowOnTheirSide {
		return nil
	}

	if len(s.vaultCfg.PrivateKey) == 0 {
		return fmt.Errorf("vault private key is required when vault is enabled on our side")
	}

	privateKey, err := paymentvault.ParsePrivateKey(s.vaultCfg.PrivateKey)
	if err != nil {
		return fmt.Errorf("invalid vault private key: %w", err)
	}

	limits := map[string]paymentvault.CoinLimits{}
	for symbol, limit := range s.vaultCfg.Coins {
		coin := s.knownBalanceTypesSymbols[strings.ToUpper(strings.TrimSpace(symbol))]
		if coin == nil {
			return fmt.Errorf("unknown vault coin %s", symbol)
		}

		minBalance := coin.MustAmountDecimal(limit.MinBalance)
		maxBalance := coin.MustAmountDecimal(limit.MaxBalance)
		if maxBalance.Compare(minBalance) < 0 {
			return fmt.Errorf("vault max balance must be greater or equal to min balance for %s", coin.Symbol)
		}

		limits[coin.Symbol] = paymentvault.CoinLimits{
			MinBalance: minBalance,
			MaxBalance: maxBalance,
		}
	}

	manager, err := paymentvault.NewManager(s.ton, vaultWalletAdapter{inner: s.wallet}, privateKey, s.knownBalanceTypesSymbols, limits)
	if err != nil {
		return fmt.Errorf("initialize vault manager: %w", err)
	}
	s.vaultManager = manager
	return nil
}

func (s *Service) LocalVaultAddress() *address.Address {
	if s.vaultManager == nil {
		return nil
	}
	return s.vaultManager.Address()
}

func (s *Service) InspectVault(ctx context.Context, addr *address.Address, symbols []string) (*paymentvault.Snapshot, error) {
	if s.vaultManager == nil {
		return nil, fmt.Errorf("vault manager is not configured")
	}
	return s.vaultManager.Snapshot(ctx, addr, symbols)
}

func (s *Service) StartVaultMonitor(ctx context.Context, addr *address.Address, interval time.Duration, symbols []string, fn func(*paymentvault.Snapshot, error)) error {
	if s.vaultManager == nil {
		return fmt.Errorf("vault manager is not configured")
	}
	s.vaultManager.StartMonitor(ctx, addr, interval, symbols, fn)
	return nil
}

func (s *Service) ResolveVaults(ctx context.Context, addrA *address.Address, addrB *address.Address) (*payments.VaultData, *payments.VaultData, error) {
	if s.vaultManager == nil || !s.vaultCfg.UseOnOurSide {
		return nil, nil, nil
	}

	channel, ourIsA, err := s.resolveVaultChannel(ctx, addrA, addrB)
	if err != nil {
		return nil, nil, err
	}

	var (
		ourAddr   *address.Address
		theirAddr *address.Address
	)
	if ourIsA {
		ourAddr = addrA
		theirAddr = addrB
	} else {
		ourAddr = addrB
		theirAddr = addrA
	}

	if channel == nil {
		return nil, nil, nil
	}

	data, err := s.vaultManager.BuildVaultData(ourAddr, theirAddr)
	if err != nil {
		return nil, nil, fmt.Errorf("build vault data: %w", err)
	}

	if ourIsA {
		return data, nil, nil
	}
	return nil, data, nil
}

func (s *Service) resolveVaultChannel(ctx context.Context, addrA *address.Address, addrB *address.Address) (*db.Channel, bool, error) {
	if addrA != nil {
		channel, err := s.GetActiveChannel(ctx, addrA.String())
		if err == nil {
			return channel, true, nil
		}
		if err != nil && !errors.Is(err, db.ErrNotFound) && !errors.Is(err, ErrNotActive) {
			return nil, false, err
		}
	}

	if addrB != nil {
		channel, err := s.GetActiveChannel(ctx, addrB.String())
		if err == nil {
			return channel, false, nil
		}
		if err != nil && !errors.Is(err, db.ErrNotFound) && !errors.Is(err, ErrNotActive) {
			return nil, false, err
		}
	}

	return nil, false, nil
}

func (s *Service) validateIncomingAction(action payments.Action) error {
	if actions.IsVaultAction(action) && !s.vaultCfg.AllowOnTheirSide {
		return fmt.Errorf("vault actions are not accepted on this node")
	}
	return nil
}
