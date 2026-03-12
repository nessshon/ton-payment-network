package actions

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/pkg/payments/vm"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"math/big"
)

func init() {
	payments.ActionTypes[string(actionSendTonVaultStaticCode.Hash())] = func() payments.Action {
		return &ActionSendTonVault{}
	}
}

type ActionSendTonVault struct {
	Coin *payments.CoinConfig

	VaultA *payments.VaultData
	VaultB *payments.VaultData
}

func (a *ActionSendTonVault) Serialize() *cell.Cell {
	c := cell.BeginCell()
	if a.VaultA != nil {
		v, err := tlb.ToCell(a.VaultA)
		if err != nil {
			panic(err.Error())
		}
		c.MustStoreBuilder(vm.PushRef(v))
	} else {
		c.MustStoreBuilder(vm.PushNull())
	}

	if a.VaultB != nil {
		v, err := tlb.ToCell(a.VaultB)
		if err != nil {
			panic(err.Error())
		}
		c.MustStoreBuilder(vm.PushRef(v))
	} else {
		c.MustStoreBuilder(vm.PushNull())
	}

	return c.MustStoreRef(actionSendTonStaticCode).EndCell()
}

func (a *ActionSendTonVault) Parse(ctx context.Context, balanceTypes payments.BalanceTypeResolver, s *cell.Slice) error {
	var vaultA, vaultB *payments.VaultData

	slc, err := vm.ReadCellOrNullOP(s)
	if err != nil {
		return fmt.Errorf("failed to parse cell op: %w", err)
	}
	if slc != nil {
		var v payments.VaultData
		if err = tlb.LoadFromCell(&v, slc.BeginParse()); err != nil {
			return fmt.Errorf("failed to parse addr: %w", err)
		}
		vaultA = &v
	}

	slc, err = vm.ReadCellOrNullOP(s)
	if err != nil {
		return fmt.Errorf("failed to parse cell op: %w", err)
	}
	if slc != nil {
		var v payments.VaultData
		if err = tlb.LoadFromCell(&v, slc.BeginParse()); err != nil {
			return fmt.Errorf("failed to parse addr: %w", err)
		}
		vaultB = &v
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
	a.VaultA = vaultA
	a.VaultB = vaultB
	return nil
}

func (a *ActionSendTonVault) GetAffectedCoins() []*payments.CoinConfig {
	return []*payments.CoinConfig{a.Coin}
}

func (a *ActionSendTonVault) PrepareNext(ctx context.Context, addrA, addrB *address.Address) (payments.Action, error) {
	if a.Coin.VaultResolver == nil {
		return nil, fmt.Errorf("no vault resolver set")
	}

	av, bv, err := a.Coin.VaultResolver.ResolveVaults(ctx, addrA, addrB)
	if err != nil {
		return nil, err
	}

	return &ActionSendTonVault{
		Coin:   a.Coin,
		VaultA: av,
		VaultB: bv,
	}, nil
}

func (a *ActionSendTonVault) PrepareExecuteState(state *cell.Cell, party *address.Address, seqno uint64, withFee bool, finalBalances map[string]*payments.BalanceInfo) (*cell.Cell, *payments.PendingMessageInfo, error) {
	return prepareExecuteSendState(state, seqno, a.Coin, withFee, finalBalances, party, make([]byte, 4))
}

func (a *ActionSendTonVault) StatesDiff(before, after *cell.Cell) (map[string]*big.Int, error) {
	return sendStatesDiff(before, after, a.Coin.BalanceID)
}

func (a *ActionSendTonVault) GetFeesPerCommitPropose() (map[string]*big.Int, error) {
	return map[string]*big.Int{a.Coin.BalanceID: a.Coin.FeePerWithdrawPropose.Nano()}, nil
}

func (a *ActionSendTonVault) IDCell() *cell.Cell {
	// TODO: cache maybe
	return cell.BeginCell().MustStoreSlice(a.Serialize().Hash(), 256).EndCell()
}

func (a *ActionSendTonVault) EmulateBalance(state *cell.Cell, balances map[string]*payments.BalanceInfo, fromUs bool) error {
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
		// b.Action.Sub(b.Action, amt)
	} else {
		b.Action.Add(b.Action, amt)
	}

	return nil
}

func (a *ActionSendTonVault) AddCoins(actionState *cell.Cell, amount *big.Int, locked map[string]*payments.LockedDepositInfo) (*cell.Cell, error) {
	return sendAddCoins(a.Coin, actionState, amount, locked)
}

func (a *ActionSendTonVault) GetEmptyState() *cell.Cell {
	return emptySendState()
}

func (a *ActionSendTonVault) CheckCanRemove(commitedSeqno uint64, state *cell.Cell) (bool, error) {
	return checkCanRemove(commitedSeqno, state)
}

var actionSendTonVaultStaticCode = func() *cell.Cell {
	// compiled using code:
	/*
		struct FeeActionInput {
			amount: coins
			commited: coins
			commitSeqno: uint64
		}

		@method_id(50)
		fun action_ton_vault(commitSeqno: int, actOur: slice, actTheir: slice?, isExecutedAtA: bool, vaultA: cell?, vaultB: cell?): void {
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

			var vault: VaultData;
			if (vaultA != null && isExecutedAtA) {
				vault = VaultData.fromCell(vaultA);
			} else if (vaultB != null && !isExecutedAtA) {
				vault = VaultData.fromCell(vaultB);
			} else {
				throw 300;
			}

			createMessage({
				bounce: false,
				dest: vault.address,
				value: ton("0.015"),
				body: InternalSignedSenderRequest{
					signature: vault.signature.load(),
					message: createMessage({
						bounce: false,
						dest: vault.target,
						value: amt,
						body: beginCell().storeUint(0,32),
					}).toCell(),
				},
			}).send(SEND_MODE_REGULAR | SEND_MODE_IGNORE_ERRORS);
		}
	*/

	data, err := hex.DecodeString("b5ee9c724101020100b70001f604fa00fa00d70b3f7053066e9137995b05fa00fa00305066e208bb95a15053a104923033e25024a120c101925f04e0236eb39321c3009170e29a6c2101d0fa40fa40d4d18e1833216eb393b3c300923070e293f2c12ce1d0fa40fa40d4d1e28208e4e1c001d08308d718d1c8cf9000000002c8cf13c9c8cf85081501006ece5005fa0271cf0b6a13ccc9c8ccc9c8cf9146905a3224d74bf2498308baf28914ce13ccc9c8cf850813ce01fa0271cf0b6accc972fb0014ca88b2")
	if err != nil {
		panic(err.Error())
	}

	code, err := cell.FromBOC(data)
	if err != nil {
		panic(err.Error())
	}
	return code
}()
