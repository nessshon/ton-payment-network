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
	payments.ActionTypes[string(actionSendJettonVaultStaticCode.Hash())] = func() payments.Action {
		return &ActionSendJettonVault{}
	}
}

type ActionSendJettonVault struct {
	Coin *payments.CoinConfig

	VaultA *payments.VaultData
	VaultB *payments.VaultData

	RootAddr *address.Address
	WalletA  *address.Address
	WalletB  *address.Address
}

func (a *ActionSendJettonVault) Serialize() *cell.Cell {
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

	return c.MustStoreBuilder(vm.PushSliceRef(cell.BeginCell().MustStoreAddr(a.RootAddr).ToSlice())).
		MustStoreRef(cell.BeginCell().
			MustStoreBuilder(vm.PushSliceRef(cell.BeginCell().MustStoreAddr(a.WalletA).ToSlice())).
			MustStoreBuilder(vm.PushSliceRef(cell.BeginCell().MustStoreAddr(a.WalletB).ToSlice())).
			// we pack immutable part of code to ref for better BoC compression and cheaper transactions
			MustStoreRef(actionSendJettonStaticCode). // implicit jump
			EndCell()).                               // implicit jump
		EndCell()
}

func (a *ActionSendJettonVault) Parse(ctx context.Context, balanceTypes payments.BalanceTypeResolver, s *cell.Slice) error {
	var vaultA, vaultB *payments.VaultData

	cll, err := vm.ReadCellOrNullOP(s)
	if err != nil {
		return fmt.Errorf("failed to parse cell op: %w", err)
	}
	if cll != nil {
		var v payments.VaultData
		if err = tlb.LoadFromCell(&v, cll.BeginParse()); err != nil {
			return fmt.Errorf("failed to parse addr: %w", err)
		}
		vaultA = &v
	}

	cll, err = vm.ReadCellOrNullOP(s)
	if err != nil {
		return fmt.Errorf("failed to parse cell op: %w", err)
	}
	if cll != nil {
		var v payments.VaultData
		if err = tlb.LoadFromCell(&v, cll.BeginParse()); err != nil {
			return fmt.Errorf("failed to parse addr: %w", err)
		}
		vaultB = &v
	}

	slc, err := vm.ReadSliceOP(s)
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

	if vaultA != nil {
		w, err := cc.JettonClient.GetWalletAddress(ctx, vaultA.Address)
		if err != nil {
			return fmt.Errorf("failed to get wallet address A for %s: %w", vaultA.Address, err)
		}

		if !walletA.Equals(w) {
			return fmt.Errorf("incorrect jetton wallet A")
		}
	} else if walletA != nil {
		return fmt.Errorf("wallet A must be nil")
	}

	if vaultB != nil {
		w, err := cc.JettonClient.GetWalletAddress(ctx, vaultB.Address)
		if err != nil {
			return fmt.Errorf("failed to get wallet address B for %s: %w", vaultB.Address, err)
		}

		if !walletB.Equals(w) {
			return fmt.Errorf("incorrect jetton wallet B")
		}
	} else if walletB != nil {
		return fmt.Errorf("wallet B must be nil")
	}

	a.Coin = cc
	a.VaultA = vaultA
	a.VaultB = vaultB
	a.RootAddr = root
	a.WalletA = walletA
	a.WalletB = walletB
	return nil
}

func (a *ActionSendJettonVault) GetAffectedCoins() []*payments.CoinConfig {
	return []*payments.CoinConfig{a.Coin}
}

func (a *ActionSendJettonVault) PrepareNext(ctx context.Context, addrA, addrB *address.Address) (payments.Action, error) {
	wA, err := a.Coin.JettonClient.GetWalletAddress(ctx, addrA)
	if err != nil {
		return nil, fmt.Errorf("failed to get wallet address A for %s: %w", addrA, err)
	}

	wB, err := a.Coin.JettonClient.GetWalletAddress(ctx, addrB)
	if err != nil {
		return nil, fmt.Errorf("failed to get wallet address B for %s: %w", addrB, err)
	}

	if a.Coin.VaultResolver == nil {
		return nil, fmt.Errorf("no vault resolver set")
	}

	av, bv, err := a.Coin.VaultResolver.ResolveVaults(ctx, addrA, addrB)
	if err != nil {
		return nil, err
	}

	return &ActionSendJettonVault{
		Coin:     a.Coin,
		VaultA:   av,
		VaultB:   bv,
		RootAddr: a.RootAddr,
		WalletA:  wA,
		WalletB:  wB,
	}, nil
}

func (a *ActionSendJettonVault) PrepareExecuteState(state *cell.Cell, party *address.Address, seqno uint64, withFee bool, finalBalances map[string]*payments.BalanceInfo) (*cell.Cell, *payments.PendingMessageInfo, error) {
	transferNotificationOp := []byte{0x9c, 0xd0, 0x62, 0x73}
	return prepareExecuteSendState(state, seqno, a.Coin, withFee, finalBalances, party, transferNotificationOp)
}

func (a *ActionSendJettonVault) StatesDiff(before, after *cell.Cell) (map[string]*big.Int, error) {
	return sendStatesDiff(before, after, a.Coin.BalanceID)
}

func (a *ActionSendJettonVault) GetFeesPerCommitPropose() (map[string]*big.Int, error) {
	return map[string]*big.Int{a.Coin.BalanceID: a.Coin.FeePerWithdrawPropose.Nano()}, nil
}

func (a *ActionSendJettonVault) IDCell() *cell.Cell {
	// TODO: cache maybe
	return cell.BeginCell().MustStoreSlice(a.Serialize().Hash(), 256).EndCell()
}

func (a *ActionSendJettonVault) EmulateBalance(state *cell.Cell, balances map[string]*payments.BalanceInfo, fromUs bool) error {
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

func (a *ActionSendJettonVault) AddCoins(actionState *cell.Cell, amount *big.Int, locked map[string]*payments.LockedDepositInfo) (*cell.Cell, error) {
	return sendAddCoins(a.Coin, actionState, amount, locked)
}

func (a *ActionSendJettonVault) GetEmptyState() *cell.Cell {
	return emptySendState()
}

func (a *ActionSendJettonVault) CheckCanRemove(commitedSeqno uint64, state *cell.Cell) (bool, error) {
	return checkCanRemove(commitedSeqno, state)
}

var actionSendJettonVaultStaticCode = func() *cell.Cell {
	// compiled using code:
	/*
		struct FeeActionInput {
			amount: coins
			commited: coins
			commitSeqno: uint64
		}

		@method_id(52)
		fun action_jetton_vault(commitSeqno: int, actOur: slice, actTheir: slice?, isExecutedAtA: bool, vaultA: cell?, vaultB: cell?, jettonRoot: address, jettonWalletA: address, jettonWalletB: address): void {
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
				value: ton("0.04"),
				body: InternalSignedSenderRequest{
					signature: vault.signature.load(),
					message: createMessage({
						bounce: false,
						dest: isExecutedAtA ? jettonWalletA : jettonWalletB,
						value: ton("0.05"),
						body: AskToTransfer {
							queryId: 0,
							jettonAmount: amt,
							customPayload: null,
							transferRecipient: vault.target,
							sendExcessesTo: vault.address,
							forwardTonAmount: 1,
							forwardPayload: beginCell().storeUint(0,32)
						},
					}).toCell(),
				},
			}).send(SEND_MODE_REGULAR | SEND_MODE_IGNORE_ERRORS);
		}
	*/

	data, err := hex.DecodeString("b5ee9c724101030100e80002f63206fa00fa00d70b3f7053086e9139995b07fa00fa00305088e20abb95a15075a106923035e25046a120c101925f06e0216eb39322c3009170e29833d0fa40fa40d4d18e1931226eb39421b3c3009170e293f2c12ce102d0fa40fa40d4d1e2820a625a0001d08308d718d14467e304820afaf0806d71c889cf1626010200080000000000c2c88bc0f8a7ea500000000000000008cf165009fa0216ce17cef4005005fa02cf8112cf13c9c8cf850812ce5003fa0271cf0b6a12ccc9c8ccc9c8cf9146905a3224d74bf2498308baf28914ce13ccc9c8cf850813ce01fa0271cf0b6accc972fb00aa37efa5")
	if err != nil {
		panic(err.Error())
	}

	code, err := cell.FromBOC(data)
	if err != nil {
		panic(err.Error())
	}
	return code
}()
