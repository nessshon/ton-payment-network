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
	payments.ActionTypes[string(actionSendJettonStaticCode.Hash())] = func() payments.Action {
		return &ActionSendJetton{}
	}
}

type ActionSendJetton struct {
	Coin *payments.CoinConfig

	AddressA *address.Address
	AddressB *address.Address

	RootAddr *address.Address
	WalletA  *address.Address
	WalletB  *address.Address
}

func (a *ActionSendJetton) Serialize() *cell.Cell {
	return cell.BeginCell().
		MustStoreBuilder(vm.PushSliceRef(cell.BeginCell().MustStoreAddr(a.AddressA).ToSlice())).
		MustStoreBuilder(vm.PushSliceRef(cell.BeginCell().MustStoreAddr(a.AddressB).ToSlice())).
		MustStoreBuilder(vm.PushSliceRef(cell.BeginCell().MustStoreAddr(a.RootAddr).ToSlice())).
		MustStoreRef(cell.BeginCell().
			MustStoreBuilder(vm.PushSliceRef(cell.BeginCell().MustStoreAddr(a.WalletA).ToSlice())).
			MustStoreBuilder(vm.PushSliceRef(cell.BeginCell().MustStoreAddr(a.WalletB).ToSlice())).
			// we pack immutable part of code to ref for better BoC compression and cheaper transactions
			MustStoreRef(actionSendJettonStaticCode). // implicit jump
			EndCell()).                               // implicit jump
		EndCell()
}

func (a *ActionSendJetton) Parse(ctx context.Context, balanceTypes payments.BalanceTypeResolver, s *cell.Slice) error {
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
	slc, err = vm.ReadSliceOP(s)
	if err != nil {
		return fmt.Errorf("failed to parse addr slice: %w", err)
	}
	root, err := slc.LoadAddr()
	if err != nil {
		return fmt.Errorf("failed to parse root addr: %w", err)
	}

	s2, err := s.LoadRef()
	if err != nil {
		return fmt.Errorf("failed to parse ref: %w", err)
	}
	if s.BitsLeft() != 0 || s.RefsNum() != 0 {
		return fmt.Errorf("unexpected data")
	}
	s = s2

	slc, err = vm.ReadSliceOP(s)
	if err != nil {
		return fmt.Errorf("failed to parse wallet A addr slice: %w", err)
	}
	walletA, err := slc.LoadAddr()
	if err != nil {
		return fmt.Errorf("failed to parse wallet A addr: %w", err)
	}

	slc, err = vm.ReadSliceOP(s)
	if err != nil {
		return fmt.Errorf("failed to parse wallet B addr slice: %w", err)
	}
	walletB, err := slc.LoadAddr()
	if err != nil {
		return fmt.Errorf("failed to parse wallet B addr: %w", err)
	}

	code, err := s.LoadRefCell()
	if err != nil {
		return fmt.Errorf("failed to parse code: %w", err)
	}

	if !bytes.Equal(code.Hash(), actionSendJettonStaticCode.Hash()) {
		return fmt.Errorf("incorrect code")
	}

	if s.BitsLeft() != 0 || s.RefsNum() != 0 {
		return fmt.Errorf("unexpected data")
	}

	bType := payments.GetJettonBalanceID(root)
	cc, err := balanceTypes.ResolveBalanceType(bType)
	if err != nil {
		return fmt.Errorf("failed to resolve balance type %s: %w", bType, err)
	}

	if cc.JettonClient == nil {
		return fmt.Errorf("jetton client is not set")
	}

	wA, err := cc.JettonClient.GetWalletAddress(ctx, addrA)
	if err != nil {
		return fmt.Errorf("failed to get wallet address A for %s: %w", addrA, err)
	}

	wB, err := cc.JettonClient.GetWalletAddress(ctx, addrB)
	if err != nil {
		return fmt.Errorf("failed to get wallet address B for %s: %w", addrB, err)
	}

	if !walletA.Equals(wA) || !walletB.Equals(wB) {
		return fmt.Errorf("incorrect jetton wallets")
	}

	a.Coin = cc
	a.AddressA = addrA
	a.AddressB = addrB
	a.RootAddr = root
	a.WalletA = walletA
	a.WalletB = walletB
	return nil
}

func (a *ActionSendJetton) GetAffectedCoins() []*payments.CoinConfig {
	return []*payments.CoinConfig{a.Coin}
}

func (a *ActionSendJetton) PrepareNext(ctx context.Context, addrA, addrB *address.Address) (payments.Action, error) {
	wA, err := a.Coin.JettonClient.GetWalletAddress(ctx, addrA)
	if err != nil {
		return nil, fmt.Errorf("failed to get wallet address A for %s: %w", addrA, err)
	}

	wB, err := a.Coin.JettonClient.GetWalletAddress(ctx, addrB)
	if err != nil {
		return nil, fmt.Errorf("failed to get wallet address B for %s: %w", addrB, err)
	}

	return &ActionSendJetton{
		Coin:     a.Coin,
		AddressA: addrA,
		AddressB: addrB,
		RootAddr: a.RootAddr,
		WalletA:  wA,
		WalletB:  wB,
	}, nil
}

func (a *ActionSendJetton) PrepareExecuteState(state *cell.Cell, party *address.Address, seqno uint64, withFee bool, finalBalances map[string]*payments.BalanceInfo) (*cell.Cell, *payments.PendingMessageInfo, error) {
	transferNotificationOp := []byte{0x9c, 0xd0, 0x62, 0x73}
	return prepareExecuteSendState(state, seqno, a.Coin, withFee, finalBalances, party, transferNotificationOp)
}

func (a *ActionSendJetton) StatesDiff(before, after *cell.Cell) (map[string]*big.Int, error) {
	return sendStatesDiff(before, after, a.Coin.BalanceID)
}

func (a *ActionSendJetton) GetFeesPerCommitPropose() (map[string]*big.Int, error) {
	return map[string]*big.Int{a.Coin.BalanceID: a.Coin.FeePerWithdrawPropose.Nano()}, nil
}

func (a *ActionSendJetton) IDCell() *cell.Cell {
	// TODO: cache maybe
	return cell.BeginCell().MustStoreSlice(a.Serialize().Hash(), 256).EndCell()
}

func (a *ActionSendJetton) EmulateBalance(state *cell.Cell, balances map[string]*payments.BalanceInfo, fromUs bool) error {
	var curState StateActionSend
	if err := payments.LoadState(&curState, state); err != nil {
		return err
	}

	id := payments.GetJettonBalanceID(a.RootAddr)

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

func (a *ActionSendJetton) AddCoins(actionState *cell.Cell, amount *big.Int, locked map[string]*payments.LockedDepositInfo) (*cell.Cell, error) {
	return sendAddCoins(a.Coin, actionState, amount, locked)
}

func (a *ActionSendJetton) GetEmptyState() *cell.Cell {
	return emptySendState()
}

func (a *ActionSendJetton) CheckCanRemove(commitedSeqno uint64, state *cell.Cell) (bool, error) {
	return checkCanRemove(commitedSeqno, state)
}

var actionSendJettonStaticCode = func() *cell.Cell {
	// compiled using code:
	/*
		struct FeeActionInput {
			amount: coins
			commited: coins
			commitSeqno: uint64
		}

		fun action_jettons(commitSeqno: int, actOur: slice, actTheir: slice?, isExecutedAtA: bool, addressA: address, addressB: address, jettonRoot: address, jettonWalletA: address, jettonWalletB: address): void {
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
				dest: isExecutedAtA ? jettonWalletA : jettonWalletB,
				value: ton("0.05"),
				body: AskToTransfer {
					queryId: 0,
					jettonAmount: amt,
					customPayload: null,
					transferRecipient: isExecutedAtA ? addressB : addressA,
					sendExcessesTo: contract.getAddress(),
					forwardTonAmount: 1,
					forwardPayload: beginCell().storeUint(0,32)
				}
			}).send(SEND_MODE_REGULAR | SEND_MODE_IGNORE_ERRORS);
		}
	*/

	data, err := hex.DecodeString("b5ee9c724101020100860001f23206fa00fa00d70b3f7053086e9139995b07fa00fa00305088e20abb95a15075a106923035e25046a120c101925f06e0544256e304820afaf08050236d06e304f82871c8cf9000000002c88bc0f8a7ea500000000000000008cf165007fa0213cece15f4005004fa02cf8112cf13c9c8cf850813ce01fa0271010010cf0b6accc972fb00a2ba014c")
	if err != nil {
		panic(err.Error())
	}

	code, err := cell.FromBOC(data)
	if err != nil {
		panic(err.Error())
	}
	return code
}()
