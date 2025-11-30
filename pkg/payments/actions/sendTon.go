package actions

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/pkg/payments/vm"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"math/big"
)

func init() {
	payments.ActionTypes[string(actionSendTonStaticCode.Hash())] = func() payments.Action {
		return &ActionSendTon{}
	}
}

type ActionSendTon struct {
	Coin *payments.CoinConfig

	AddressA *address.Address
	AddressB *address.Address
}

func (a *ActionSendTon) Serialize() *cell.Cell {
	return cell.BeginCell().
		MustStoreBuilder(vm.PushSliceRef(cell.BeginCell().MustStoreAddr(a.AddressA).ToSlice())).
		MustStoreBuilder(vm.PushSliceRef(cell.BeginCell().MustStoreAddr(a.AddressB).ToSlice())).
		// we pack immutable part of code to ref for better BoC compression and cheaper transactions
		MustStoreRef(actionSendTonStaticCode). // implicit jump
		EndCell()
}

func (a *ActionSendTon) Parse(ctx context.Context, balanceTypes payments.BalanceTypeResolver, s *cell.Slice) error {
	slc, err := vm.ReadSliceOP(s)
	if err != nil {
		return fmt.Errorf("failed to parse addr slice: %w", err)
	}
	addrA, err := slc.LoadAddr()
	if err != nil {
		return fmt.Errorf("failed to parse addr: %w", err)
	}
	slc, err = vm.ReadSliceOP(s)
	if err != nil {
		return fmt.Errorf("failed to parse addr slice: %w", err)
	}
	addrB, err := slc.LoadAddr()
	if err != nil {
		return fmt.Errorf("failed to parse addr: %w", err)
	}

	code, err := s.LoadRefCell()
	if err != nil {
		return fmt.Errorf("failed to parse code: %w", err)
	}

	if !bytes.Equal(code.Hash(), actionSendTonStaticCode.Hash()) {
		return fmt.Errorf("incorrect code")
	}

	if s.BitsLeft() != 0 || s.RefsNum() != 0 {
		return fmt.Errorf("unexpected data in condition")
	}

	bType := payments.GetTONBalanceID()
	cc, err := balanceTypes.ResolveBalanceType(bType)
	if err != nil {
		return fmt.Errorf("failed to resolve balance type %s: %w", bType, err)
	}

	a.Coin = cc
	a.AddressA = addrA
	a.AddressB = addrB
	return nil
}

func (a *ActionSendTon) GetAffectedCoins() []*payments.CoinConfig {
	return []*payments.CoinConfig{a.Coin}
}

func (a *ActionSendTon) PrepareNext(ctx context.Context, addrA, addrB *address.Address) (payments.Action, error) {
	return &ActionSendTon{
		Coin:     a.Coin,
		AddressA: addrA,
		AddressB: addrB,
	}, nil
}

func (a *ActionSendTon) PrepareExecuteState(state *cell.Cell, party *address.Address, seqno uint64, withFee bool, finalBalances map[string]*payments.BalanceInfo) (*cell.Cell, *payments.PendingMessageInfo, error) {
	return prepareExecuteSendState(state, seqno, a.Coin, withFee, finalBalances, party, make([]byte, 4))
}

func (a *ActionSendTon) StatesDiff(before, after *cell.Cell) (map[string]*big.Int, error) {
	return sendStatesDiff(before, after, a.Coin.BalanceID)
}

func (a *ActionSendTon) GetFeesPerCommitPropose() (map[string]*big.Int, error) {
	return map[string]*big.Int{a.Coin.BalanceID: a.Coin.FeePerWithdrawPropose.Nano()}, nil
}

func (a *ActionSendTon) IDCell() *cell.Cell {
	// TODO: cache maybe
	return cell.BeginCell().MustStoreSlice(a.Serialize().Hash(), 256).EndCell()
}

func (a *ActionSendTon) EmulateBalance(state *cell.Cell, balances map[string]*payments.BalanceInfo, fromUs bool) error {
	var curState StateActionSend
	if err := payments.LoadState(&curState, state); err != nil {
		return err
	}

	id := payments.GetTONBalanceID()

	b := balances[id]
	if b == nil {
		b = payments.NewBalanceInfo(a.Coin)
		balances[id] = b
	}

	amt := new(big.Int).Sub(curState.Amount.Nano(), curState.Commited.Nano())
	if fromUs {
		b.Action.Sub(b.Action, amt)
	} else {
		b.Action.Add(b.Action, amt)
	}

	return nil
}

func (a *ActionSendTon) AddCoins(actionState *cell.Cell, amount *big.Int, locked map[string]*payments.LockedDepositInfo) (*cell.Cell, error) {
	return sendAddCoins(a.Coin, actionState, amount, locked)
}

func (a *ActionSendTon) GetEmptyState() *cell.Cell {
	return emptySendState()
}

func (a *ActionSendTon) CheckCanRemove(commitedSeqno uint64, state *cell.Cell) (bool, error) {
	return checkCanRemove(commitedSeqno, state)
}

var actionSendTonStaticCode = func() *cell.Cell {
	// compiled using code:
	/*
		struct FeeActionInput {
			amount: coins
			commited: coins
			commitSeqno: uint64
		}

		fun action_ton(commitSeqno: int, actOur: slice, actTheir: slice?, isExecutedAtA: bool, addressA: address, addressB: address): void {
			var our = actOur.loadAny<FeeActionInput>();
			var their = FeeActionInput{amount: 0, commited: 0, commitSeqno: 0};
			if (actTheir != null) {
				their = actTheir.loadAny<FeeActionInput>();
			}

			// commit seqno for both states must be in sync, so we can check only one
			if (commitSeqno >= our.commitSeqno) {
				our.amount -= our.commited;
				their.amount -= their.commited;
			}

			var amt = our.amount - their.amount;
			if (amt <= 0) {
				return;
			}

			createMessage({
				bounce: false,
				dest: isExecutedAtA ? addressB : addressA,
				value: amt,
				body: beginCell().storeUint(0,32),
			}).send(SEND_MODE_REGULAR | SEND_MODE_IGNORE_ERRORS);
		}
	*/

	data, err := hex.DecodeString("b5ee9c724101010100520000a004fa00fa00d70b3f7053066e9137995b05fa00fa00305066e208bb95a15053a104923033e25024a120c101925f04e05023e304c8cf9000000002c8cf13c9c8cf850812ce58fa0271cf0b6accc972fb002816d1fa")
	if err != nil {
		panic(err.Error())
	}

	code, err := cell.FromBOC(data)
	if err != nil {
		panic(err.Error())
	}
	return code
}()
