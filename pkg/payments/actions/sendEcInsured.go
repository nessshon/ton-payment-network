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
	payments.ActionTypes[string(actionSendECStaticCode.Hash())] = func() payments.Action {
		return &ActionSendECInsured{}
	}
}

type ActionSendECInsured struct {
	Coin *payments.CoinConfig

	AddressA *address.Address
	AddressB *address.Address

	EC uint32
}

func (a *ActionSendECInsured) Serialize() *cell.Cell {
	return cell.BeginCell().
		MustStoreBuilder(vm.PushSliceRef(cell.BeginCell().MustStoreAddr(a.AddressA).ToSlice())).
		MustStoreBuilder(vm.PushSliceRef(cell.BeginCell().MustStoreAddr(a.AddressB).ToSlice())).
		MustStoreBuilder(vm.PushIntOP(new(big.Int).SetUint64(uint64(a.EC)))).
		// we pack immutable part of code to ref for better BoC compression and cheaper transactions
		MustStoreRef(actionSendECStaticCode). // implicit jump
		EndCell()
}

func (a *ActionSendECInsured) Parse(ctx context.Context, balanceTypes payments.BalanceTypeResolver, s *cell.Slice) error {
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

	ec, err := vm.ReadIntOP(s)
	if err != nil {
		return fmt.Errorf("failed to parse ec int: %w", err)
	}

	if ec.BitLen() > 32 || ec.Sign() <= 0 {
		return fmt.Errorf("incorrect ec")
	}

	code, err := s.LoadRefCell()
	if err != nil {
		return fmt.Errorf("failed to parse code: %w", err)
	}

	if !bytes.Equal(code.Hash(), actionSendECStaticCode.Hash()) {
		return fmt.Errorf("incorrect code")
	}

	if s.BitsLeft() != 0 || s.RefsNum() != 0 {
		return fmt.Errorf("unexpected data in condition")
	}

	bType := payments.GetECBalanceID(uint32(ec.Uint64()))
	cc, err := balanceTypes.ResolveBalanceType(bType)
	if err != nil {
		return fmt.Errorf("failed to resolve balance type %s: %w", bType, err)
	}

	a.Coin = cc
	a.AddressA = addrA
	a.AddressB = addrB
	a.EC = uint32(ec.Uint64())
	return nil
}

func (a *ActionSendECInsured) GetAffectedCoins() []*payments.CoinConfig {
	return []*payments.CoinConfig{a.Coin}
}

func (a *ActionSendECInsured) PrepareNext(ctx context.Context, addrA, addrB *address.Address) (payments.Action, error) {
	return &ActionSendECInsured{
		Coin:     a.Coin,
		AddressA: addrA,
		AddressB: addrB,
		EC:       a.EC,
	}, nil
}

func (a *ActionSendECInsured) PrepareExecuteState(state *cell.Cell, party *address.Address, seqno uint64, withFee bool, finalBalances map[string]*payments.BalanceInfo) (*cell.Cell, *payments.PendingMessageInfo, error) {
	return prepareExecuteSendState(state, seqno, a.Coin, withFee, finalBalances, party, make([]byte, 4))
}

func (a *ActionSendECInsured) StatesDiff(before, after *cell.Cell) (map[string]*big.Int, error) {
	return sendStatesDiff(before, after, a.Coin.BalanceID)
}

func (a *ActionSendECInsured) GetFeesPerCommitPropose() (map[string]*big.Int, error) {
	return map[string]*big.Int{a.Coin.BalanceID: a.Coin.FeePerWithdrawPropose.Nano()}, nil
}

func (a *ActionSendECInsured) IDCell() *cell.Cell {
	// TODO: cache maybe
	return cell.BeginCell().MustStoreSlice(a.Serialize().Hash(), 256).EndCell()
}

func (a *ActionSendECInsured) EmulateBalance(state *cell.Cell, balances map[string]*payments.BalanceInfo, fromUs bool) error {
	var curState StateActionSend
	if err := payments.LoadState(&curState, state); err != nil {
		return err
	}

	id := payments.GetECBalanceID(a.EC)

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

func (a *ActionSendECInsured) AddCoins(actionState *cell.Cell, amount *big.Int, locked map[string]*payments.LockedDepositInfo) (*cell.Cell, error) {
	return sendAddCoins(a.Coin, actionState, amount, locked)
}

func (a *ActionSendECInsured) GetEmptyState() *cell.Cell {
	return emptySendState()
}

func (a *ActionSendECInsured) CheckCanRemove(commitedSeqno uint64, state *cell.Cell) (bool, error) {
	return checkCanRemove(commitedSeqno, state)
}

var actionSendECStaticCode = func() *cell.Cell {
	// compiled using code:
	/*
		struct FeeActionInput {
			amount: coins
			commited: coins
			commitSeqno: uint64
		}

		fun action_ec(commitSeqno: int, actOur: slice, actTheir: slice?, isExecutedAtA: bool, addressA: address, addressB: address, ecId: int): void {
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

			var currenciesToSend: dict = createEmptyDict();
			currenciesToSend.uDictSetBuilder(32, ecId, beginCell().storeVarUInt32(amt));

			return createMessage({
				bounce: false,
				dest: isExecutedAtA ? addressB : addressA,
				value: (FEE_EC_PAYOUT, currenciesToSend),
				body: beginCell().storeUint(0,32),
			}).send(SEND_MODE_REGULAR | SEND_MODE_IGNORE_ERRORS);
		}
	*/

	data, err := hex.DecodeString("b5ee9c724101010100670000ca05fa00fa00d70b3f7053076e9138995b06fa00fa00305077e209bb95a15064a105923034e25035a120c101925f05e06d8020c85003fa06034515f4435033e3048209c9c380c8cf9000000002c8cf13c9c8cf850813ce01fa0212f40071cf0b69ccc972fb00617c7465")
	if err != nil {
		panic(err.Error())
	}

	code, err := cell.FromBOC(data)
	if err != nil {
		panic(err.Error())
	}
	return code
}()
