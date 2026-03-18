package vault

import (
	"context"
	"crypto/ed25519"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/xssnick/ton-payment-network/pkg/payments"
	chainclient "github.com/xssnick/ton-payment-network/tonpayments/chain/client"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/liteclient"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/ton"
	tonwallet "github.com/xssnick/tonutils-go/ton/wallet"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

type testnetWalletAdapter struct {
	wallet *tonwallet.Wallet
}

func (t *testnetWalletAdapter) WalletAddress() *address.Address {
	return t.wallet.WalletAddress()
}

func (t *testnetWalletAdapter) DoTransactionMany(ctx context.Context, _ string, messages []WalletMessage) ([]byte, error) {
	list := make([]*tonwallet.Message, 0, len(messages))
	for _, message := range messages {
		target := message.To
		if message.StateInit != nil {
			stateCell, err := tlb.ToCell(*message.StateInit)
			if err != nil {
				return nil, err
			}
			target = address.NewAddress(0, 0, stateCell.Hash())
		}

		msg := tonwallet.SimpleMessage(target, message.Amount, message.Body)
		if message.EC != nil {
			msg.InternalMessage.ExtraCurrencies = cell.NewDict(32)
			for id, coins := range message.EC {
				if err := msg.InternalMessage.ExtraCurrencies.SetIntKey(big.NewInt(int64(id)), cell.BeginCell().MustStoreBigVarUInt(coins.Nano(), 32).EndCell()); err != nil {
					return nil, err
				}
			}
		}
		if message.StateInit != nil {
			msg.InternalMessage.Bounce = false
			msg.InternalMessage.StateInit = message.StateInit
		}
		list = append(list, msg)
	}

	hash, err := t.wallet.SendManyWaitTxHash(ctx, list)
	if err != nil {
		return nil, err
	}
	return hash, nil
}

func TestIntegration_VaultTON_Testnet(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}
	if os.Getenv("PAYMENTS_TESTNET_VAULT") == "" {
		t.Skip("set PAYMENTS_TESTNET_VAULT=1 to run real testnet vault flow")
	}

	seed := strings.Fields(strings.TrimSpace(os.Getenv("WALLET_SEED")))
	if len(seed) < 12 {
		t.Skip("WALLET_SEED is required for testnet vault integration flow")
	}

	client := liteclient.NewConnectionPool()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := client.AddConnection(ctx, "109.236.80.69:49913", "AxFZRHVD1qIO9Fyva52P4vC3tRvk8ac1KKOG0c6IVio="); err != nil {
		t.Fatalf("connect to testnet lite server failed: %v", err)
	}

	api := ton.NewAPIClient(client).WithRetry().WithTimeout(10 * time.Second)
	rawWallet, err := tonwallet.FromSeed(api, seed, tonwallet.HighloadV2R2)
	if err != nil {
		rawWallet, err = tonwallet.FromSeed(api, seed, tonwallet.HighloadV2R2, true)
		if err != nil {
			t.Fatalf("init testnet wallet failed: %v", err)
		}
	}

	chain := chainclient.NewTON(api)
	acc, err := chain.GetAccount(context.Background(), rawWallet.WalletAddress(), time.Time{})
	if err != nil {
		t.Fatalf("fetch wallet account failed: %v", err)
	}
	if acc == nil || !acc.HasState || acc.Balance.Nano().Cmp(tlb.MustFromTON("0.2").Nano()) < 0 {
		t.Fatalf("testnet wallet %s must be funded first", rawWallet.WalletAddress().String())
	}

	_, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate vault key failed: %v", err)
	}

	manager, err := NewManager(chain, &testnetWalletAdapter{wallet: rawWallet}, privateKey, map[string]*payments.CoinConfig{
		"TON": {
			Symbol:    "TON",
			Decimals:  9,
			BalanceID: payments.GetTONBalanceID(),
		},
	}, nil)
	if err != nil {
		t.Fatalf("new vault manager failed: %v", err)
	}

	deployCtx, deployCancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer deployCancel()
	if err = manager.EnsureDeployed(deployCtx); err != nil {
		t.Fatalf("deploy vault failed: %v", err)
	}

	waitForVaultBalance(t, chain, manager.Address(), 90*time.Second, func(balance *big.Int) bool {
		return balance.Sign() > 0
	}, "waiting vault deployment")

	topUpAmount := tlb.MustFromTON("0.03").Nano()
	tonCoin := &payments.CoinConfig{
		Symbol:    "TON",
		Decimals:  9,
		BalanceID: payments.GetTONBalanceID(),
	}
	beforeTopUpAcc, err := chain.GetAccount(context.Background(), manager.Address(), time.Time{})
	if err != nil {
		t.Fatalf("reload vault account before top up failed: %v", err)
	}
	beforeTopUp := new(big.Int).Set(beforeTopUpAcc.Balance.Nano())

	if _, err = manager.TopUp(context.Background(), tonCoin, topUpAmount); err != nil {
		t.Fatalf("vault top up failed: %v", err)
	}

	minIncrease := new(big.Int).Add(beforeTopUp, new(big.Int).Div(topUpAmount, big.NewInt(2)))
	waitForVaultBalance(t, chain, manager.Address(), 120*time.Second, func(balance *big.Int) bool {
		return balance.Cmp(minIncrease) >= 0
	}, "waiting vault top up")

	beforeAcc, err := chain.GetAccount(context.Background(), manager.Address(), time.Time{})
	if err != nil {
		t.Fatalf("reload vault account before transfer failed: %v", err)
	}
	before := new(big.Int).Set(beforeAcc.Balance.Nano())

	withdrawAmount := tlb.MustFromTON("0.02").Nano()
	if _, err = manager.Transfer(context.Background(), tonCoin, rawWallet.WalletAddress(), withdrawAmount); err != nil {
		t.Fatalf("vault transfer failed: %v", err)
	}

	waitForVaultBalance(t, chain, manager.Address(), 120*time.Second, func(balance *big.Int) bool {
		minDrop := new(big.Int).Sub(before, withdrawAmount)
		return balance.Cmp(minDrop) <= 0
	}, "waiting vault outgoing transfer")

	t.Logf("[vault-testnet] wallet=%s vault=%s", rawWallet.WalletAddress().String(), manager.Address().String())
}

func waitForVaultBalance(t *testing.T, chain *chainclient.TON, addr *address.Address, timeout time.Duration, ready func(*big.Int) bool, msg string) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		acc, err := chain.GetAccount(context.Background(), addr, time.Time{})
		if err == nil && acc != nil && acc.HasState {
			balance := acc.Balance.Nano()
			if ready(balance) {
				return
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("%s: timeout after %s", msg, timeout)
}
