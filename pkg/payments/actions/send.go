package actions

import (
	"context"
	"fmt"
	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"math/big"
)

// Coins decimal-safe from missuse
type Coins struct {
	Val *big.Int
}

func (c *Coins) Nano() *big.Int {
	return new(big.Int).Set(c.Val)
}

func (g *Coins) LoadFromCell(loader *cell.Slice) error {
	coins, err := loader.LoadBigCoins()
	if err != nil {
		return err
	}
	g.Val = coins
	return nil
}

func (g Coins) ToCell() (*cell.Cell, error) {
	return cell.BeginCell().MustStoreBigCoins(g.Val).EndCell(), nil
}

type StateActionSend struct {
	Amount        Coins  `tlb:"."`
	Commited      Coins  `tlb:"."`
	CommitedSeqno uint64 `tlb:"## 64"`
}

func NewSendActionFromBalanceID(ctx context.Context, cc *payments.CoinConfig, a, b string) (payments.ActionSend, error) {
	aAddr, err := address.ParseAddr(a)
	if err != nil {
		return nil, err
	}
	bAddr, err := address.ParseAddr(b)
	if err != nil {
		return nil, err
	}

	if cc.VaultResolver != nil {
		action, err := newVaultActionFromBalanceID(ctx, cc, aAddr, bAddr)
		if err != nil {
			return nil, err
		}
		if action != nil {
			return action, nil
		}
	}

	switch {
	case cc.BalanceID == payments.GetTONBalanceID():
		return &ActionSendTonInsured{
			Coin:     cc,
			AddressA: aAddr,
			AddressB: bAddr,
		}, nil
	case cc.JettonClient != nil:
		wa, err := cc.JettonClient.GetWalletAddress(ctx, aAddr)
		if err != nil {
			return nil, err
		}

		wb, err := cc.JettonClient.GetWalletAddress(ctx, bAddr)
		if err != nil {
			return nil, err
		}

		return &ActionSendJettonInsured{
			Coin:     cc,
			AddressA: aAddr,
			AddressB: bAddr,
			RootAddr: cc.JettonClient.GetRootAddress(),
			WalletA:  wa,
			WalletB:  wb,
		}, nil
	default:
		return &ActionSendECInsured{
			Coin:     cc,
			AddressA: aAddr,
			AddressB: bAddr,
			EC:       payments.GetECFromBalanceID(cc.BalanceID),
		}, nil
	}
}

func newVaultActionFromBalanceID(ctx context.Context, cc *payments.CoinConfig, aAddr, bAddr *address.Address) (payments.ActionSend, error) {
	vaultA, vaultB, err := cc.VaultResolver.ResolveVaults(ctx, aAddr, bAddr)
	if err != nil {
		return nil, err
	}
	if vaultA == nil && vaultB == nil {
		return nil, nil
	}

	switch {
	case cc.BalanceID == payments.GetTONBalanceID():
		return &ActionSendTonVault{
			Coin:   cc,
			VaultA: vaultA,
			VaultB: vaultB,
		}, nil
	case cc.JettonClient != nil:
		var walletA, walletB *address.Address
		if vaultA != nil {
			walletA, err = cc.JettonClient.GetWalletAddress(ctx, vaultA.Address)
			if err != nil {
				return nil, fmt.Errorf("failed to resolve vault jetton wallet A: %w", err)
			}
		}
		if vaultB != nil {
			walletB, err = cc.JettonClient.GetWalletAddress(ctx, vaultB.Address)
			if err != nil {
				return nil, fmt.Errorf("failed to resolve vault jetton wallet B: %w", err)
			}
		}

		return &ActionSendJettonVault{
			Coin:     cc,
			VaultA:   vaultA,
			VaultB:   vaultB,
			RootAddr: cc.JettonClient.GetRootAddress(),
			WalletA:  walletA,
			WalletB:  walletB,
		}, nil
	default:
		return &ActionSendECVault{
			Coin:   cc,
			VaultA: vaultA,
			VaultB: vaultB,
			EC:     payments.GetECFromBalanceID(cc.BalanceID),
		}, nil
	}
}

func prepareExecuteSendState(state *cell.Cell, seqno uint64, coin *payments.CoinConfig, withFee bool,
	finalBalances map[string]*payments.BalanceInfo, party *address.Address, completionOp []byte) (*cell.Cell, *payments.PendingMessageInfo, error) {
	var curState StateActionSend
	if err := payments.LoadState(&curState, state); err != nil {
		return nil, nil, err
	}

	if curState.CommitedSeqno >= seqno {
		return nil, nil, payments.ErrAlreadyCommitted
	}

	if withFee {
		b := finalBalances[coin.BalanceID]
		if b == nil || b.Available().Cmp(coin.FeePerWithdrawPropose.Nano()) < 0 {
			return nil, nil, fmt.Errorf("not enough balance to pay fee")
		}

		curState.Amount.Val.Add(curState.Amount.Val, coin.FeePerWithdrawPropose.Nano())
	}

	// if it was committed before, we should decrease the amount to avoid conflicts with uncoop close
	if curState.Commited.Val.Sign() != 0 {
		curState.Amount.Val.Sub(curState.Amount.Val, curState.Commited.Nano())
	}

	curState.Commited = curState.Amount
	curState.CommitedSeqno = seqno

	msg := &payments.PendingMessageInfo{
		Amounts: map[string]*big.Int{
			coin.BalanceID: curState.Commited.Nano(),
		},
		CompletionBodyPrefix: completionOp,
		CompletionAddress:    party.String(),
	}

	newState, err := tlb.ToCell(curState)
	if err != nil {
		return nil, nil, err
	}

	return newState, msg, nil
}

func emptySendState() *cell.Cell {
	state, err := tlb.ToCell(StateActionSend{
		Amount:   Coins{Val: big.NewInt(0)},
		Commited: Coins{Val: big.NewInt(0)},
	})
	if err != nil {
		panic(err.Error())
	}
	return state
}

func checkCanRemove(commitedSeqno uint64, state *cell.Cell) (bool, error) {
	var curState StateActionSend
	if err := payments.LoadState(&curState, state); err != nil {
		return false, err
	}

	amt := curState.Amount.Nano()
	if commitedSeqno >= curState.CommitedSeqno {
		amt = amt.Sub(amt, curState.Commited.Nano())
	}

	return amt.Sign() <= 0, nil
}

func sendStatesDiff(before, after *cell.Cell, balanceId string) (map[string]*big.Int, error) {
	var beforeState, afterState StateActionSend
	if err := payments.LoadState(&beforeState, before); err != nil {
		return nil, err
	}

	if err := payments.LoadState(&afterState, after); err != nil {
		return nil, err
	}

	return map[string]*big.Int{
		balanceId: new(big.Int).Sub(afterState.Amount.Val, beforeState.Amount.Val),
	}, nil
}

func sendAddCoins(cc *payments.CoinConfig, actionState *cell.Cell, amount *big.Int, locked map[string]*payments.LockedDepositInfo) (*cell.Cell, error) {
	var actState StateActionSend
	if err := payments.LoadState(&actState, actionState); err != nil {
		return nil, fmt.Errorf("failed to load old state: %w", err)
	}

	actState.Amount.Val = new(big.Int).Add(actState.Amount.Nano(), amount)

	newActionState, err := tlb.ToCell(actState)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize new action state: %w", err)
	}

	if locked != nil {
		if lk := locked[cc.BalanceID]; lk != nil {
			// mark part of the rented deposit as used
			lk.Used.Add(lk.Used, amount)
			if lk.Amount.Cmp(lk.Used) < 0 {
				// cap it
				lk.Used.Set(lk.Amount)
			}
		}
	}

	return newActionState, nil
}
