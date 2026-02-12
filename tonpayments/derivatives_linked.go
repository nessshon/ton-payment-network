package tonpayments

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/big"

	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/pkg/payments/actions"
	"github.com/xssnick/ton-payment-network/pkg/payments/conditionals"
	"github.com/xssnick/ton-payment-network/tonpayments/db"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

var derivativeLinkedKeySalt = []byte("derivative-linked-v1")

func derivativeLinkedKey(base []byte) ed25519.PublicKey {
	payload := make([]byte, 0, len(derivativeLinkedKeySalt)+len(base))
	payload = append(payload, derivativeLinkedKeySalt...)
	payload = append(payload, base...)

	hash := sha256.Sum256(payload)
	return ed25519.PublicKey(hash[:])
}

func reverseDerivativeAction(act payments.Action) (payments.Action, error) {
	switch a := act.(type) {
	case *actions.ActionSendTon:
		return &actions.ActionSendTon{
			Coin:     a.Coin,
			AddressA: a.AddressB,
			AddressB: a.AddressA,
		}, nil
	case *actions.ActionSendJetton:
		return &actions.ActionSendJetton{
			Coin:     a.Coin,
			AddressA: a.AddressB,
			AddressB: a.AddressA,
			RootAddr: a.RootAddr,
			WalletA:  a.WalletB,
			WalletB:  a.WalletA,
		}, nil
	case *actions.ActionSendEC:
		return &actions.ActionSendEC{
			Coin:     a.Coin,
			AddressA: a.AddressB,
			AddressB: a.AddressA,
			EC:       a.EC,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported derivative action type: %T", act)
	}
}

func (s *Service) buildLinkedDerivativeConditional(base *conditionals.ConditionalResolvable) (*conditionals.ConditionalResolvable, error) {
	if base == nil {
		return nil, fmt.Errorf("base derivative conditional is nil")
	}

	if len(base.GetKey()) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("invalid derivative key size: %d", len(base.GetKey()))
	}

	reversedAction, err := reverseDerivativeAction(base.GetAction())
	if err != nil {
		return nil, err
	}

	details := base.Details
	details.IsLong = !details.IsLong

	linkedAmount := big.NewInt(0)
	if base.Amount != nil {
		linkedAmount = new(big.Int).Set(base.Amount)
	}

	return &conditionals.ConditionalResolvable{
		Key:           derivativeLinkedKey(base.GetKey()),
		Amount:        linkedAmount,
		ResolverAddr:  base.ResolverAddr,
		Details:       details,
		PriceResolver: base.PriceResolver,
		Action:        reversedAction,
	}, nil
}

func ensureConditionalOnSide(side *db.Side, cond payments.Conditional) (bool, error) {
	serialized := cond.Serialize()
	key := cell.BeginCell().MustStoreSlice(serialized.Hash(), 256).EndCell()

	_, err := side.Data.Conditionals.LoadValue(key)
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, cell.ErrNoSuchKeyInDict) {
		return false, fmt.Errorf("failed to check conditional existence: %w", err)
	}

	if err = side.Data.Conditionals.Set(key, serialized); err != nil {
		return false, fmt.Errorf("failed to add conditional: %w", err)
	}
	return true, nil
}

func ensureActionStateOnSide(side *db.Side, act payments.Action) (bool, error) {
	actID := act.IDCell()

	_, err := side.Data.ActionStates.LoadValue(actID)
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, cell.ErrNoSuchKeyInDict) {
		return false, fmt.Errorf("failed to check action state existence: %w", err)
	}

	if err = side.Data.ActionStates.Set(actID, act.GetEmptyState()); err != nil {
		return false, fmt.Errorf("failed to init action state: %w", err)
	}
	return true, nil
}

// computeLinkedDerivativeSettle computes the resolve state for the linked
// (inverse) derivative conditional. Because derivative PnL is zero-sum,
// exactly one side has a positive settle and the other is 0.
// Returns nil when the linked settle is 0 (the linked conditional should
// just be deleted without execution).
func computeLinkedDerivativeSettle(ctx context.Context, primaryResolve *cell.Cell, linked *conditionals.ConditionalResolvable) (*cell.Cell, error) {
	var primaryState conditionals.ResolvableState
	if err := payments.LoadState(&primaryState, primaryResolve); err != nil {
		return nil, fmt.Errorf("failed to parse primary resolve state: %w", err)
	}

	// Zero-sum: if primary settle > 0, linked settle is always 0.
	if primaryState.Amount.Sign() > 0 {
		return nil, nil
	}

	// Primary settle == 0 -> linked side may need to pay the loss.
	if linked.PriceResolver == nil {
		return nil, fmt.Errorf("no price resolver for linked asset %d", linked.Details.AssetID)
	}

	price, err := linked.PriceResolver.GetPriceAt(ctx, primaryState.At)
	if err != nil {
		return nil, fmt.Errorf("failed to get price at %d: %w", primaryState.At, err)
	}

	entryPrice := linked.Details.EntryPrice.Nano()
	if entryPrice == nil || entryPrice.Sign() <= 0 {
		return nil, fmt.Errorf("invalid linked entry price")
	}

	var delta *big.Int
	if linked.Details.IsLong {
		delta = new(big.Int).Sub(price, entryPrice)
	} else {
		delta = new(big.Int).Sub(entryPrice, price)
	}

	positionSize := new(big.Int).Mul(linked.Amount, big.NewInt(int64(linked.Details.Leverage)))
	pnl := new(big.Int).Mul(positionSize, delta)
	pnl.Div(pnl, entryPrice)

	if pnl.Sign() >= 0 {
		// Linked side has no loss to pay.
		return nil, nil
	}

	settle := new(big.Int).Abs(pnl)
	if linked.Amount.Sign() > 0 && settle.Cmp(linked.Amount) > 0 {
		settle.Set(linked.Amount) // cap at collateral
	}

	st := conditionals.ResolvableState{
		Key:    linked.Key,
		Amount: settle,
		At:     primaryState.At,
	}
	return tlb.ToCell(st)
}
