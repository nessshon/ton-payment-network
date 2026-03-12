package conditionals

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/pkg/payments/actions"
	condcontracts "github.com/xssnick/ton-payment-network/pkg/payments/conditionals/contracts"
	"github.com/xssnick/ton-payment-network/pkg/payments/conditionals/oracle"
	"github.com/xssnick/ton-payment-network/pkg/payments/vm"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

func init() {
	payments.ConditionalTypes[string(resolvableStaticCode.Hash())] = func() payments.Conditional {
		return &ConditionalResolvable{}
	}
}

const maxSupportedLeverage = uint16(20)

type ConditionalResolvableDetails struct {
	AssetID    uint32        `tlb:"## 32"`
	IsLong     bool          `tlb:"bool"`
	Leverage   uint16        `tlb:"## 16"`
	EntryPrice actions.Coins `tlb:"."`
}

type ConditionalResolvable struct {
	Key          ed25519.PublicKey
	Amount       *big.Int
	Fee          *big.Int
	IsInitiator  bool
	ResolverAddr *address.Address
	Details      ConditionalResolvableDetails

	PriceResolver PriceResolver
	Action        payments.Action
}

const derivativeResolveAcceptanceWindowSec int64 = 10

type PriceResolver interface {
	GetPriceAt(ctx context.Context, at int64) (*big.Int, error)
	GetLastPrice() (int64, *big.Int, error)
	GetPricesSince(from int64) []oracle.RangePrice
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

	isInitiator := int64(0)
	if c.IsInitiator {
		isInitiator = 1
	}

	return cell.BeginCell().
		MustStoreBuilder(vm.PushIntOP(new(big.Int).SetBytes(c.Action.Serialize().Hash()))).
		MustStoreBuilder(vm.PushIntOP(new(big.Int).SetBytes(c.Key))).
		MustStoreBuilder(vm.PushIntOP(c.Amount)).
		MustStoreBuilder(vm.PushIntOP(c.Fee)).
		MustStoreBuilder(vm.PushIntOP(big.NewInt(isInitiator))).
		MustStoreBuilder(vm.PushRef(det)).
		MustStoreRef(cell.BeginCell().
			MustStoreBuilder(vm.PushSliceRef(cell.BeginCell().MustStoreAddr(c.ResolverAddr).ToSlice())).
			// we pack immutable part of code to ref for better BoC compression and cheaper transactions
			MustStoreRef(resolvableStaticCode). // implicit jump
			EndCell()).
		EndCell()
}

func (c *ConditionalResolvable) Parse(ctx context.Context, s *cell.Slice, actions payments.ActionResolver) error {
	actHashInt, err := vm.ReadIntOP(s)
	if err != nil {
		return fmt.Errorf("failed to parse hash: %w", err)
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
	if amount.Sign() <= 0 {
		return fmt.Errorf("failed to parse amount: cannot be negative or zero")
	}

	fee, err := vm.ReadIntOP(s)
	if err != nil {
		return fmt.Errorf("failed to parse fee: %w", err)
	}
	if fee.BitLen() > 127 {
		return fmt.Errorf("failed to parse fee: incorrect bits len")
	}
	if fee.Sign() < 0 {
		return fmt.Errorf("failed to parse fee: cannot be negative")
	}

	isInitiator, err := vm.ReadIntOP(s)
	if err != nil {
		return fmt.Errorf("failed to parse is initiator: %w", err)
	}
	if isInitiator.Sign() != 0 && isInitiator.Cmp(big.NewInt(1)) != 0 {
		return fmt.Errorf("failed to parse is initiator: incorrect value")
	}

	dc, err := vm.ReadCellOP(s)
	if err != nil {
		return fmt.Errorf("failed to read details ref: %w", err)
	}
	var details ConditionalResolvableDetails
	if err = payments.LoadState(&details, dc); err != nil {
		return fmt.Errorf("failed to load details: %w", err)
	}

	s, err = s.LoadRef()
	if err != nil {
		return fmt.Errorf("failed to parse resolver ref: %w", err)
	}

	slc, err := vm.ReadSliceOP(s)
	if err != nil {
		return fmt.Errorf("failed to parse addr slice: %w", err)
	}
	resolverAddr, err := slc.LoadAddr()
	if err != nil {
		return fmt.Errorf("failed to parse addr: %w", err)
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
	c.Fee = fee
	c.IsInitiator = isInitiator.Sign() != 0
	c.ResolverAddr = resolverAddr
	c.Details = details
	c.PriceResolver = oracle.PriceResolvers[details.AssetID]

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
		"cond_type":    "resolvable",
		"key":          base64.StdEncoding.EncodeToString(c.Key),
		"amount":       ccs[0].MustAmount(c.Amount).String(),
		"fee":          ccs[0].MustAmount(c.Fee).String(),
		"is_initiator": c.IsInitiator,
		"contract":     c.ResolverAddr.String(),
		"asset_id":     c.Details.AssetID,
		"is_long":      c.Details.IsLong,
		"leverage":     c.Details.Leverage,
		"entry_price":  c.Details.EntryPrice.Nano().String(),
	}
}

func (c *ConditionalResolvable) ValidateOnAdd() error {
	if c.Amount == nil {
		return fmt.Errorf("invalid amount")
	}
	if c.Fee == nil {
		return fmt.Errorf("invalid fee")
	}
	if c.Amount.Sign() < 0 {
		return fmt.Errorf("invalid amount")
	}
	if c.Fee.Sign() < 0 {
		return fmt.Errorf("invalid fee")
	}

	if c.Details.Leverage == 0 || c.Details.Leverage > maxSupportedLeverage {
		return fmt.Errorf("unsupported leverage: %d (max %d)", c.Details.Leverage, maxSupportedLeverage)
	}

	if c.Action == nil {
		return fmt.Errorf("action is required")
	}
	if c.ResolverAddr == nil {
		return fmt.Errorf("resolver address is required")
	}
	ccs := c.Action.GetAffectedCoins()
	if len(ccs) != 1 || ccs[0] == nil {
		return fmt.Errorf("unexpected number of affected coins")
	}

	minFee := payments.CalcPercentFeeCeil(c.Amount, ccs[0].VirtualTunnelConfig.DerivativeFeePercent)
	minFee.Mul(minFee, big.NewInt(int64(c.Details.Leverage)))
	if c.Fee.Cmp(minFee) < 0 {
		return fmt.Errorf("invalid fee: expected at least %s, got %s", minFee.String(), c.Fee.String())
	}

	return nil
}

func (c *ConditionalResolvable) ValidateState(ctx context.Context, oldState, newState *cell.Cell) error {
	if oldState != nil {
		return fmt.Errorf("state already accepted")
	}

	if c.PriceResolver == nil {
		return fmt.Errorf("price resolver for asset %d is not configured", c.Details.AssetID)
	}

	var state ResolvableState
	if err := payments.LoadState(&state, newState); err != nil {
		return err
	}

	if !bytes.Equal(state.Key, c.Key) {
		return fmt.Errorf("incorrect key")
	}

	latestAt, _, err := c.PriceResolver.GetLastPrice()
	if err != nil || latestAt <= 0 {
		return fmt.Errorf("price resolver is not ready, cannot validate state")
	}
	if state.At > latestAt+2 {
		return fmt.Errorf("state timestamp is too far in the future")
	}
	if latestAt-state.At > derivativeResolveAcceptanceWindowSec {
		return fmt.Errorf("state timestamp is too old")
	}

	if state.Amount.Sign() < 0 {
		return fmt.Errorf("amount cannot be negative")
	}

	price, err := c.PriceResolver.GetPriceAt(ctx, state.At)
	if err != nil {
		return fmt.Errorf("failed to get price: %w", err)
	}

	entryPrice := c.Details.EntryPrice.Nano()
	if entryPrice == nil || entryPrice.Sign() == 0 {
		return fmt.Errorf("invalid entry price")
	}

	// Calculate current ROI amount: amount * leverage * signed_delta / entryPrice
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

	// In the zero-sum derivative model:
	// - When roi > 0 (this conditional profits): Amount must be 0,
	//   payment is received via the linked conditional.
	// - When roi <= 0 (this conditional loses): Amount must equal |roi|
	//   (capped at collateral), representing the loss payment.
	expectedAmount := new(big.Int)
	if roi.Sign() <= 0 {
		expectedAmount.Abs(roi)
		if c.Amount != nil && c.Amount.Sign() > 0 && expectedAmount.Cmp(c.Amount) > 0 {
			expectedAmount.Set(c.Amount)
		}
	}

	// Allow ±1 tolerance for integer division rounding.
	diff := new(big.Int).Sub(state.Amount, expectedAmount)
	diff.Abs(diff)
	if diff.Cmp(big.NewInt(1)) > 0 {
		return fmt.Errorf("incorrect amount: expected %s, got %s", expectedAmount, state.Amount)
	}

	return nil
}

func (c *ConditionalResolvable) EmulateBalance(balances map[string]*payments.BalanceInfo, fromUs bool) error {
	// Resolve the single affected balance for this action
	b, err := payments.ResolveActionBalance(balances, c.Action)
	if err != nil {
		return fmt.Errorf("failed to resolve balance: %w", err)
	}

	if c.PriceResolver == nil {
		return fmt.Errorf("price resolver for asset %d is not configured", c.Details.AssetID)
	}

	price, err := c.getBestEffortCurrentPrice()
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
		if c.IsInitiator {
			locked.Add(locked, c.Fee)
		} else {
			locked.Sub(locked, c.Fee)
			if locked.Sign() < 0 {
				locked.SetInt64(0)
			}
		}
		b.ConditionalLocked.Add(b.ConditionalLocked, locked)
	} else {
		if c.IsInitiator {
			roi.Add(roi, c.Fee)
		} else {
			roi.Sub(roi, c.Fee)
			if roi.Sign() < 0 {
				roi.SetInt64(0)
			}
		}
		if roi.Sign() > 0 {
			b.ConditionalPending.Add(b.ConditionalPending, roi)
		}
	}

	return nil
}

func (c *ConditionalResolvable) getBestEffortCurrentPrice() (*big.Int, error) {
	now := time.Now().Unix()
	var lastErr error

	// Try current and a few previous seconds to avoid transient `ErrTooNew`
	// during boundary races around resolver updates.
	for back := int64(0); back <= 3; back++ {
		price, err := c.PriceResolver.GetPriceAt(context.Background(), now-back)
		if err == nil && price != nil {
			return price, nil
		}
		if err != nil {
			lastErr = err
		}
		if err != nil && !errors.Is(err, oracle.ErrTooNew) && !errors.Is(err, oracle.ErrUnavailable) && !errors.Is(err, oracle.ErrNoData) {
			break
		}
	}

	// Fallback to the latest known sample.
	_, price, err := c.PriceResolver.GetLastPrice()
	if err == nil && price != nil {
		return price, nil
	}
	if err != nil {
		lastErr = err
	}

	if lastErr == nil {
		lastErr = oracle.ErrNoData
	}
	return nil, lastErr
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

	ccs := c.Action.GetAffectedCoins()
	if len(ccs) == 0 {
		return fmt.Errorf("no affected coins for derivative action")
	}

	balance := balances[ccs[0].BalanceID]
	if balance == nil || balance.Available().Cmp(c.Amount) < 0 {
		return fmt.Errorf("not enough balance to cover derivative collateral")
	}

	var details ConditionalResolvableInstructionDetails
	if err := payments.LoadState(&details, detailsCell); err != nil {
		return err
	}

	if len(details.ResolverContractCodeHash) != 32 {
		return fmt.Errorf("incorrect resolver contract code hash length")
	}
	if details.ResolverContractData == nil {
		return fmt.Errorf("missing resolver contract data")
	}

	stateInit, err := condcontracts.BuildDerivativeStateInit(details.ResolverContractData)
	if err != nil {
		return fmt.Errorf("failed to build resolver state init: %w", err)
	}

	if !bytes.Equal(stateInit.Code.Hash(), details.ResolverContractCodeHash) {
		return fmt.Errorf("incorrect resolver contract code hash")
	}

	resolverAddr, err := condcontracts.CalcDerivativeAddress(stateInit)
	if err != nil {
		return fmt.Errorf("failed to calc resolver address: %w", err)
	}
	if c.ResolverAddr == nil || !resolverAddr.Equals(c.ResolverAddr) {
		return fmt.Errorf("resolver address mismatch")
	}

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

	if condState.Amount.Sign() < 0 {
		return nil, fmt.Errorf("amount cannot be negative")
	}

	// cap by configured amount if provided (non-zero)
	toAdd := new(big.Int).Set(condState.Amount)
	if c.Amount != nil && c.Amount.Sign() > 0 && toAdd.Cmp(c.Amount) > 0 {
		toAdd.Set(c.Amount)
	}

	// add resolved amount to the linked action's state
	if sendAct, ok := c.Action.(payments.ActionSend); ok {
		if c.IsInitiator {
			toAdd.Add(toAdd, c.Fee)
		} else {
			toAdd.Sub(toAdd, c.Fee)
			if toAdd.Sign() < 0 {
				toAdd.SetInt64(0)
			}
		}
		return sendAct.AddCoins(actionState, toAdd, locked)
	}

	return nil, fmt.Errorf("action does not support adding coins")
}

var resolvableStaticCode = func() *cell.Cell {
	// compiled using code:
	/*
		@method_id(46)
		fun conditional_derivative(targetActionsInput: dict, condInput: slice, sender: address, actionHash: int, id: uint256, maxAmount: coins, fee: coins, isInitiator: bool, details: cell, expectedSender: address): dict {
			if (sender != expectedSender) {
				throw 201;
			}

			var input = condInput.loadAny<DerivativeResolve>();
			if (input.id != id) {
				throw 202;
			}

			if (maxAmount != 0 && input.amount > maxAmount) {
				// hard cap of liquidation
				input.amount = maxAmount;
			}

			var (actInput, _) = targetActionsInput.uDictGet(256, actionHash);
			if (actInput == null) {
				// we must always have action to execute condition
				throw 203;
			}

			var v = actInput.loadAny<FeeActionInput>();

			if (isInitiator) {
				v.amount += input.amount + fee;
			} else if (fee < input.amount) {
				v.amount += input.amount - fee;
			}

			targetActionsInput.uDictSet(256, actionHash, v.toCell().beginParse());

			return targetActionsInput;
		}
	*/

	data, err := hex.DecodeString("b5ee9c724101010100660000c83116c70593f2c0c9e105d3fffa003003bd93f2c0cae020c300945cbcc3009170e291319130e253148307f40e6fa130206e93f2c0cbe0fa00fa00d70b3f05945025a0a09e5352b9955025a1a003923234e203e2c801fa025003fa02cb3fc9d0028307f416927501c7")
	if err != nil {
		panic(err.Error())
	}

	code, err := cell.FromBOC(data)
	if err != nil {
		panic(err.Error())
	}
	return code
}()
