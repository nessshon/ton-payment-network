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
	payments.ActionTypes[string(actionSendECVaultStaticCode.Hash())] = func() payments.Action {
		return &ActionSendECVault{}
	}
}

type ActionSendECVault struct {
	Coin *payments.CoinConfig

	VaultA *payments.VaultData
	VaultB *payments.VaultData

	EC uint32
}

func (a *ActionSendECVault) Serialize() *cell.Cell {
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

	return c.MustStoreBuilder(vm.PushIntOP(new(big.Int).SetUint64(uint64(a.EC)))).MustStoreRef(actionSendTonStaticCode).EndCell()
}

func (a *ActionSendECVault) Parse(ctx context.Context, balanceTypes payments.BalanceTypeResolver, s *cell.Slice) error {
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
	a.VaultA = vaultA
	a.VaultB = vaultB
	a.EC = uint32(ec.Uint64())
	return nil
}

func (a *ActionSendECVault) GetAffectedCoins() []*payments.CoinConfig {
	return []*payments.CoinConfig{a.Coin}
}

func (a *ActionSendECVault) PrepareNext(ctx context.Context, addrA, addrB *address.Address) (payments.Action, error) {
	if a.Coin.VaultResolver == nil {
		return nil, fmt.Errorf("no vault resolver set")
	}

	av, bv, err := a.Coin.VaultResolver.ResolveVaults(ctx, addrA, addrB)
	if err != nil {
		return nil, err
	}

	return &ActionSendECVault{
		Coin:   a.Coin,
		VaultA: av,
		VaultB: bv,
		EC:     a.EC,
	}, nil
}

func (a *ActionSendECVault) PrepareExecuteState(state *cell.Cell, party *address.Address, seqno uint64, withFee bool, finalBalances map[string]*payments.BalanceInfo) (*cell.Cell, *payments.PendingMessageInfo, error) {
	return prepareExecuteSendState(state, seqno, a.Coin, withFee, finalBalances, party, make([]byte, 4))
}

func (a *ActionSendECVault) StatesDiff(before, after *cell.Cell) (map[string]*big.Int, error) {
	return sendStatesDiff(before, after, a.Coin.BalanceID)
}

func (a *ActionSendECVault) GetFeesPerCommitPropose() (map[string]*big.Int, error) {
	return map[string]*big.Int{a.Coin.BalanceID: a.Coin.FeePerWithdrawPropose.Nano()}, nil
}

func (a *ActionSendECVault) IDCell() *cell.Cell {
	// TODO: cache maybe
	return cell.BeginCell().MustStoreSlice(a.Serialize().Hash(), 256).EndCell()
}

func (a *ActionSendECVault) EmulateBalance(state *cell.Cell, balances map[string]*payments.BalanceInfo, fromUs bool) error {
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

func (a *ActionSendECVault) AddCoins(actionState *cell.Cell, amount *big.Int, locked map[string]*payments.LockedDepositInfo) (*cell.Cell, error) {
	return sendAddCoins(a.Coin, actionState, amount, locked)
}

func (a *ActionSendECVault) GetEmptyState() *cell.Cell {
	return emptySendState()
}

func (a *ActionSendECVault) CheckCanRemove(commitedSeqno uint64, state *cell.Cell) (bool, error) {
	return checkCanRemove(commitedSeqno, state)
}

var actionSendECVaultStaticCode = func() *cell.Cell {
	// compiled using code:
	/*
		struct FeeActionInput {
			amount: coins
			commited: coins
			commitSeqno: uint64
		}

		@method_id(51)
		fun action_ec_vault(commitSeqno: int, actOur: slice, actTheir: slice?, isExecutedAtA: bool, vaultA: cell?, vaultB: cell?, ecId: int): void {
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

			var currenciesToSend: dict = createEmptyDict();
			currenciesToSend.uDictSetBuilder(32, ecId, beginCell().storeVarUInt32(amt));

			createMessage({
				bounce: false,
				dest: vault.address,
				value: FEE_EC_PAYOUT,
				body: InternalSignedSenderRequest{
					signature: vault.signature.load(),
					message: createMessage({
						bounce: false,
						dest: vault.target,
						value: (FEE_EC_PAYOUT, currenciesToSend),
						body: beginCell().storeUint(0,32),
					}).toCell(),
				},
			}).send(SEND_MODE_REGULAR | SEND_MODE_IGNORE_ERRORS);
		}
	*/

	data, err := hex.DecodeString("b5ee9c724101030100ca0002f605fa00fa00d70b3f7053076e9138995b06fa00fa00305077e209bb95a15064a105923034e25035a120c101925f05e0226eb39321c3009170e2993133d0fa40fa40d4d18e1932236eb393b3c300923070e293f2c12ce102d0fa40fa40d4d1e26d8020c85007fa06451306f4438209c9c38003d08308d718d123c88901020008000000000086cf16c8cf13c9c8cf850815ce01fa0212f40071cf0b6912ccc9c8ccc9c8cf9146905a3222d74bf2498308baf28912ceccc9c8cf850813ce01fa0271cf0b6accc972fb003653829e")
	if err != nil {
		panic(err.Error())
	}

	code, err := cell.FromBOC(data)
	if err != nil {
		panic(err.Error())
	}
	return code
}()
