package tonpayments

import (
	"fmt"
	"math/big"

	"github.com/xssnick/ton-payment-network/pkg/payments/conditionals"
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
