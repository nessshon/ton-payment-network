package tonpayments

import (
	"fmt"
	"math/big"

	"github.com/xssnick/ton-payment-network/pkg/payments/conditionals"
	condcontracts "github.com/xssnick/ton-payment-network/pkg/payments/conditionals/contracts"
	"github.com/xssnick/ton-payment-network/pkg/payments/conditionals/oracle"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

// buildIncomingDerivativeResolve computes the resolve state for the incoming
// (Their-side) derivative conditional, given the outgoing (Our-side) one.
// The incoming conditional has the opposite IsLong, so the settle amounts
// are symmetric: when we lose on outgoing, we profit on incoming and vice-versa.
func (s *Service) buildIncomingDerivativeResolve(outCond *conditionals.ConditionalResolvable, incomingKey []byte) (*cell.Cell, error) {
	resolver := oracle.PriceResolvers[outCond.Details.AssetID]
	if resolver == nil {
		return nil, fmt.Errorf("no price resolver for asset %d", outCond.Details.AssetID)
	}

	at, lastPrice, err := resolver.GetLastPrice()
	if err != nil || lastPrice == nil || lastPrice.Sign() <= 0 {
		return nil, fmt.Errorf("failed to get current price: %w", err)
	}

	entryPrice := outCond.Details.EntryPrice.Nano()
	if entryPrice == nil || entryPrice.Sign() <= 0 {
		return nil, fmt.Errorf("invalid entry price")
	}

	isOurLong := outCond.Details.IsLong

	var delta *big.Int
	if isOurLong {
		delta = new(big.Int).Sub(lastPrice, entryPrice)
	} else {
		delta = new(big.Int).Sub(entryPrice, lastPrice)
	}

	positionSize := new(big.Int).Mul(outCond.Amount, big.NewInt(int64(outCond.Details.Leverage)))
	pnl := new(big.Int).Mul(positionSize, delta)
	pnl.Div(pnl, entryPrice)

	// Incoming settle: when we profit (pnl > 0), the incoming side pays us.
	settleAmount := big.NewInt(0)
	if pnl.Sign() > 0 {
		profit := new(big.Int).Set(pnl)
		if outCond.Amount.Sign() > 0 && profit.Cmp(outCond.Amount) > 0 {
			profit.Set(outCond.Amount)
		}
		settleAmount = profit
	}

	resState := conditionals.ResolvableState{
		Key:    incomingKey,
		Amount: settleAmount,
		At:     at,
	}
	return tlb.ToCell(resState)
}

func buildDerivativeResolveForPrice(cond *conditionals.ConditionalResolvable, at int64, lastPrice *big.Int) (*cell.Cell, error) {
	if cond == nil {
		return nil, fmt.Errorf("conditional is nil")
	}
	if lastPrice == nil || lastPrice.Sign() <= 0 {
		return nil, fmt.Errorf("invalid derivative exit price")
	}

	entryPrice := cond.Details.EntryPrice.Nano()
	if entryPrice == nil || entryPrice.Sign() <= 0 {
		return nil, fmt.Errorf("invalid entry price")
	}
	if cond.Amount == nil || cond.Amount.Sign() <= 0 {
		return nil, fmt.Errorf("invalid derivative amount")
	}
	if cond.Details.Leverage == 0 {
		return nil, fmt.Errorf("invalid derivative leverage")
	}

	var delta *big.Int
	if cond.Details.IsLong {
		delta = new(big.Int).Sub(lastPrice, entryPrice)
	} else {
		delta = new(big.Int).Sub(entryPrice, lastPrice)
	}

	positionSize := new(big.Int).Mul(cond.Amount, big.NewInt(int64(cond.Details.Leverage)))
	pnl := new(big.Int).Mul(positionSize, delta)
	pnl.Div(pnl, entryPrice)

	settleAmount := big.NewInt(0)
	if pnl.Sign() < 0 {
		loss := new(big.Int).Abs(pnl)
		if loss.Cmp(cond.Amount) > 0 {
			loss.Set(cond.Amount)
		}
		settleAmount = loss
	}

	resState := conditionals.ResolvableState{
		Key:    cond.GetKey(),
		Amount: settleAmount,
		At:     at,
	}
	return tlb.ToCell(resState)
}

func buildDerivativePriceInner(resolver *oracle.Resolver, at int64, price *big.Int) (condcontracts.PriceInner, error) {
	if resolver == nil {
		return condcontracts.PriceInner{}, fmt.Errorf("resolver is nil")
	}
	if at <= 0 || at > int64(^uint32(0)) {
		return condcontracts.PriceInner{}, fmt.Errorf("invalid price timestamp")
	}
	if price == nil || price.Sign() <= 0 {
		return condcontracts.PriceInner{}, fmt.Errorf("invalid price")
	}

	priceCoins, err := tlb.FromNano(price, 9)
	if err != nil {
		return condcontracts.PriceInner{}, fmt.Errorf("failed to convert price: %w", err)
	}

	proof := condcontracts.PriceProof{
		At:    uint32(at),
		Price: priceCoins,
	}
	proofCell, err := tlb.ToCell(proof)
	if err != nil {
		return condcontracts.PriceInner{}, fmt.Errorf("failed to serialize price proof: %w", err)
	}

	signature, err := resolver.SignProofCell(proofCell)
	if err != nil {
		return condcontracts.PriceInner{}, fmt.Errorf("failed to sign price proof: %w", err)
	}

	return condcontracts.PriceInner{
		Signature: struct {
			V []byte `tlb:"bits 512"`
		}{
			V: append([]byte(nil), signature...),
		},
		SignedBody: proofCell,
	}, nil
}

func buildDerivativeResolverCommitMessage(resolver *oracle.Resolver, entryPrice *big.Int, entryAt, exitAt int64, exitPrice *big.Int) (*cell.Cell, error) {
	entry, err := buildDerivativePriceInner(resolver, entryAt, entryPrice)
	if err != nil {
		return nil, fmt.Errorf("failed to build derivative entry proof: %w", err)
	}

	exit, err := buildDerivativePriceInner(resolver, exitAt, exitPrice)
	if err != nil {
		return nil, fmt.Errorf("failed to build derivative exit proof: %w", err)
	}

	return tlb.ToCell(condcontracts.Commit{
		Entry: entry,
		Exit:  exit,
	})
}
