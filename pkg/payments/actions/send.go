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

type StateActionSend struct {
	Amount        tlb.Coins `tlb:"."`
	Commited      tlb.Coins `tlb:"."`
	CommitedSeqno uint64    `tlb:"## 64"`
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

	switch {
	case cc.BalanceID == payments.GetTONBalanceID():
		return &ActionSendTon{
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

		return &ActionSendJetton{
			Coin:     cc,
			AddressA: aAddr,
			AddressB: bAddr,
			RootAddr: cc.JettonClient.GetRootAddress(),
			WalletA:  wa,
			WalletB:  wb,
		}, nil
	default:
		return &ActionSendEC{
			Coin:     cc,
			AddressA: aAddr,
			AddressB: bAddr,
			EC:       payments.GetECFromBalanceID(cc.BalanceID),
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

		curState.Amount = curState.Amount.MustAdd(coin.FeePerWithdrawPropose)
	}

	// if it was committed before, we should decrease the amount to avoid conflicts with uncoop close
	if !curState.Commited.IsZero() {
		curState.Amount = curState.Amount.MustSub(curState.Commited)
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
		Amount:   tlb.ZeroCoins,
		Commited: tlb.ZeroCoins,
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
		balanceId: afterState.Amount.MustSub(beforeState.Amount).Nano(),
	}, nil
}

func sendAddCoins(cc *payments.CoinConfig, actionState *cell.Cell, amount *big.Int, locked map[string]*payments.LockedDepositInfo) (*cell.Cell, error) {
	var actState StateActionSend
	if err := payments.LoadState(&actState, actionState); err != nil {
		return nil, fmt.Errorf("failed to load old state: %w", err)
	}

	actState.Amount = cc.MustAmount(new(big.Int).Add(actState.Amount.Nano(), amount))

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
