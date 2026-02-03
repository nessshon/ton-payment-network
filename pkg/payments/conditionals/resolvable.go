package conditionals

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/pkg/payments/actions"
	"github.com/xssnick/ton-payment-network/pkg/payments/vm"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"math/big"
	"time"
)

func init() {
	payments.ConditionalTypes[string(resolvableStaticCode.Hash())] = func() payments.Conditional {
		return &ConditionalResolvable{}
	}
}

type ConditionalResolvableDetails struct {
	AssetID    uint32        `tlb:"## 32"`
	IsLong     bool          `tlb:"bool"`
	Leverage   uint16        `tlb:"## 16"`
	EntryPrice actions.Coins `tlb:"."`
}

type ConditionalResolvable struct {
	Key          ed25519.PublicKey
	Amount       *big.Int
	ResolverAddr *address.Address
	Details      ConditionalResolvableDetails

	PriceResolver PriceResolver
	Action        payments.Action
}

type PriceResolver interface {
	GetPriceAt(ctx context.Context, at int64) (*big.Int, error)
}

type ConditionalResolvableInstructionDetails struct {
	ResolverContractCodeHash []byte     `tlb:"bits 256"`
	ResolverContractData     *cell.Cell `tlb:"^"`
}

type ResolvableState struct {
	Key    []byte   `tlb:"bits 256"`
	Amount *big.Int `tlb:"## 128"`
	At     int64    `tlb:"## 64"`
}

func (c *ConditionalResolvable) Serialize() *cell.Cell {
	det, err := tlb.ToCell(c.Details)
	if err != nil {
		panic(err)
	}

	return cell.BeginCell().
		MustStoreBuilder(vm.PushIntOP(new(big.Int).SetBytes(c.Action.Serialize().Hash()))).
		MustStoreBuilder(vm.PushIntOP(new(big.Int).SetBytes(c.Key))).
		MustStoreBuilder(vm.PushIntOP(c.Amount)).
		MustStoreBuilder(vm.PushSliceRef(cell.BeginCell().MustStoreAddr(c.ResolverAddr).ToSlice())).
		MustStoreBuilder(vm.PushRef(det)).
		// we pack immutable part of code to ref for better BoC compression and cheaper transactions
		MustStoreRef(resolvableStaticCode). // implicit jump
		EndCell()
}

func (c *ConditionalResolvable) Parse(ctx context.Context, s *cell.Slice, actions payments.ActionResolver) error {
	actHashInt, err := vm.ReadIntOP(s)
	if err != nil {
		return fmt.Errorf("failed to parse fee: %w", err)
	}

	keyInt, err := vm.ReadIntOP(s)
	if err != nil {
		return fmt.Errorf("failed to parse key: %w", err)
	}

	key := keyInt.Bytes()
	if len(key) > 32 {
		return fmt.Errorf("too big key size")
	}

	amount, err := vm.ReadIntOP(s)
	if err != nil {
		return fmt.Errorf("failed to parse amount: %w", err)
	}
	if amount.BitLen() > 127 {
		return fmt.Errorf("failed to parse amount: incorrect bits len")
	}
	if amount.Sign() < 0 {
		return fmt.Errorf("failed to parse amount: cannot be negative")
	}

	slc, err := vm.ReadSliceOP(s)
	if err != nil {
		return fmt.Errorf("failed to parse addr slice: %w", err)
	}
	resolverAddr, err := slc.LoadAddr()
	if err != nil {
		return fmt.Errorf("failed to parse addr: %w", err)
	}

	dc, err := vm.ReadCellOP(s)
	if err != nil {
		return fmt.Errorf("failed to read details ref: %w", err)
	}
	var details ConditionalResolvableDetails
	if err = payments.LoadState(&details, dc); err != nil {
		return fmt.Errorf("failed to load details: %w", err)
	}

	if len(key) < 32 {
		// prepend it with zeroes
		key = append(make([]byte, 32-len(key)), key...)
	}

	actHash := actHashInt.Bytes()
	if len(actHash) > 32 {
		return fmt.Errorf("too big act hash size")
	}

	if len(actHash) < 32 {
		// prepend it with zeroes
		actHash = append(make([]byte, 32-len(actHash)), actHash...)
	}

	code, err := s.LoadRefCell()
	if err != nil {
		return fmt.Errorf("failed to parse code: %w", err)
	}

	if !bytes.Equal(code.Hash(), resolvableStaticCode.Hash()) {
		return fmt.Errorf("incorrect code")
	}

	if s.BitsLeft() != 0 || s.RefsNum() != 0 {
		return fmt.Errorf("unexpected data in condition")
	}

	a, err := actions.ResolveAction(ctx, actHash)
	if err != nil {
		return fmt.Errorf("failed to resolve action: %w", err)
	}

	if len(a.GetAffectedCoins()) != 1 {
		return fmt.Errorf("unexpected number of affected coins")
	}

	c.Action = a
	c.Key = key
	c.Amount = amount
	c.ResolverAddr = resolverAddr
	c.Details = details

	return nil
}

func (c *ConditionalResolvable) GetAction() payments.Action {
	return c.Action
}

func (c *ConditionalResolvable) GetKey() []byte {
	return c.Key
}

func (c *ConditionalResolvable) GetDeadline() time.Time {
	// no deadline
	return time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC)
}

func (c *ConditionalResolvable) GetLogInfo() map[string]any {
	ccs := c.Action.GetAffectedCoins()

	return map[string]any{
		"cond_type":   "resolvable",
		"key":         base64.StdEncoding.EncodeToString(c.Key),
		"amount":      ccs[0].MustAmount(c.Amount).String(),
		"contract":    c.ResolverAddr.String(),
		"asset_id":    c.Details.AssetID,
		"is_long":     c.Details.IsLong,
		"leverage":    c.Details.Leverage,
		"entry_price": c.Details.EntryPrice.Nano().String(),
	}
}

func (c *ConditionalResolvable) ValidateOnAdd() error {
	if c.Amount.Sign() < 0 {
		return fmt.Errorf("invalid amount")
	}

	// TODO: check resolver addr match
	return nil
}

func (c *ConditionalResolvable) ValidateState(ctx context.Context, oldState, newState *cell.Cell) error {
	if oldState != nil {
		return fmt.Errorf("state already accepted")
	}

	var state ResolvableState
	if err := payments.LoadState(&state, newState); err != nil {
		return err
	}

	if !bytes.Equal(state.Key, c.Key) {
		return fmt.Errorf("incorrect key")
	}

	price, err := c.PriceResolver.GetPriceAt(ctx, state.At)
	if err != nil {
		return fmt.Errorf("failed to get price: %w", err)
	}

	entryPrice := c.Details.EntryPrice.Nano()
	if entryPrice == nil || entryPrice.Sign() == 0 {
		return fmt.Errorf("invalid entry price")
	}

	// Calculate current ROI amount: amount * leverage * max(0, signed_delta) / entryPrice
	// signed_delta = (price - entryPrice) for long, (entryPrice - price) for short
	var delta *big.Int
	if c.Details.IsLong {
		delta = new(big.Int).Sub(price, entryPrice)
	} else {
		delta = new(big.Int).Sub(entryPrice, price)
	}

	// roi = (delta * Amount * Leverage) / entryPrice
	num := new(big.Int).Mul(delta, c.Amount)
	num.Mul(num, big.NewInt(int64(c.Details.Leverage)))
	roi := new(big.Int).Div(num, entryPrice)

	if state.Amount.Cmp(roi) > 0 {
		return fmt.Errorf("incorrect amount")
	}

	return nil
}

func (c *ConditionalResolvable) EmulateBalance(balances map[string]*payments.BalanceInfo, fromUs bool) error {
	// Resolve the single affected balance for this action
	b, err := payments.ResolveActionBalance(balances, c.Action)
	if err != nil {
		return fmt.Errorf("failed to resolve balance: %w", err)
	}

	price, err := c.PriceResolver.GetPriceAt(context.Background(), time.Now().Unix())
	if err != nil {
		return fmt.Errorf("failed to get price: %w", err)
	}

	entryPrice := c.Details.EntryPrice.Nano()
	if entryPrice == nil || entryPrice.Sign() == 0 {
		return fmt.Errorf("invalid entry price")
	}

	// signed delta depending on long/short
	var delta *big.Int
	if c.Details.IsLong {
		delta = new(big.Int).Sub(price, entryPrice)
	} else {
		delta = new(big.Int).Sub(entryPrice, price)
	}

	// roi = (delta * Amount * Leverage) / entryPrice
	num := new(big.Int).Mul(delta, c.Amount)
	num.Mul(num, big.NewInt(int64(c.Details.Leverage)))
	roi := new(big.Int).Div(num, entryPrice)

	if fromUs {
		locked := new(big.Int).Set(c.Amount)
		if roi.Sign() < 0 && roi.CmpAbs(c.Amount) > 0 {
			locked.Abs(roi)
		}
		b.ConditionalLocked.Add(b.ConditionalLocked, locked)
	} else {
		if roi.Sign() > 0 {
			b.ConditionalPending.Add(b.ConditionalPending, roi)
		}
	}

	return nil
}

func (c *ConditionalResolvable) Commit(updated payments.Conditional, actState *cell.Cell) (*cell.Cell, error) {
	return nil, fmt.Errorf("derivative conditionals cannot be committed")
}

func (c *ConditionalResolvable) PrepareCommit(condState *cell.Cell) (payments.Conditional, error) {
	return nil, fmt.Errorf("derivative conditionals cannot be committed")
}

func (c *ConditionalResolvable) CheckInstruction(detailsCell *cell.Cell, isFinalDest bool, balances map[string]*payments.BalanceInfo, finalState *cell.Cell) error {
	if detailsCell == nil {
		return fmt.Errorf("missing details cell")
	}

	if finalState != nil {
		return fmt.Errorf("final state is not supported for this conditional type")
	}

	if !isFinalDest {
		return fmt.Errorf("tunneling is not supported for this conditional type")
	}

	var details ConditionalResolvableInstructionDetails
	if err := payments.LoadState(&details, detailsCell); err != nil {
		return err
	}

	// TODO: check resolver contract

	return nil
}

func (c *ConditionalResolvable) PrepareNext(instructionDetailsCell *cell.Cell, nextAction payments.Action, nextDeadline time.Time) (payments.Conditional, error) {
	return nil, fmt.Errorf("derivative conditionals cannot be tunneled")
}

func (c *ConditionalResolvable) ScoreTunnelTarget(instructionDetailsCell *cell.Cell, targetBalances map[string]*payments.BalanceInfo) (*big.Int, error) {
	return nil, fmt.Errorf("derivative conditionals cannot be tunneled")
}

func (c *ConditionalResolvable) Execute(actionState, latestCondState *cell.Cell, locked map[string]*payments.LockedDepositInfo) (*cell.Cell, error) {
	var condState ResolvableState
	if err := payments.LoadState(&condState, latestCondState); err != nil {
		return nil, err
	}

	// basic validation
	if !bytes.Equal(condState.Key, c.Key) {
		return nil, fmt.Errorf("incorrect key")
	}

	// cap by configured amount if provided (non-zero)
	toAdd := new(big.Int).Set(condState.Amount)
	if c.Amount != nil && c.Amount.Sign() > 0 && toAdd.Cmp(c.Amount) > 0 {
		toAdd.Set(c.Amount)
	}

	// add resolved amount to the linked action's state
	if sendAct, ok := c.Action.(payments.ActionSend); ok {
		return sendAct.AddCoins(actionState, toAdd, locked)
	}

	return nil, fmt.Errorf("action does not support adding coins")
}

var resolvableStaticCode = func() *cell.Cell {
	// compiled using code:
	/*
		fun conditional_derivative(targetActionsInput: dict, condInput: slice, sender: address, actionHash: int, id: int256, amount: coins, expectedSender: address): dict {
			if (sender != expectedSender) {
				return targetActionsInput;
			}

			var input = condInput.loadAny<DerivativeResolve>();
			if (input.id != id) {
				return targetActionsInput;
			}

			if (amount != 0 && input.amount > amount) {
				// hard cap of liquidation
				input.amount = amount;
			}

			var (actInput, _) = targetActionsInput.uDictGet(256, actionHash);
			if (actInput == null) {
				// we must always have action to execute condition
				return targetActionsInput;
			}

			var v = actInput.loadAny<FeeActionInput>();
			v.amount += input.amount;

			targetActionsInput.uDictSet(256, actionHash, v.toCell().beginParse());

			return targetActionsInput;
		}
	*/

	data, err := hex.DecodeString("b5ee9c724101010100550000a63014c70593f2c0c9e103d3fffa003004bd93f2c0cae021c300955321bcc3009170e2926c129131e253028307f40e6fa130206e93f2c0cbe0fa00fa00d70b3f5024a0c801fa0201fa0212cb3fc9d0028307f4168f27983f")
	if err != nil {
		panic(err.Error())
	}

	code, err := cell.FromBOC(data)
	if err != nil {
		panic(err.Error())
	}
	return code
}()
