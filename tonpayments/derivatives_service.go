package tonpayments

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"time"

	"crypto/ed25519"

	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/pkg/payments/actions"
	"github.com/xssnick/ton-payment-network/pkg/payments/conditionals"
	"github.com/xssnick/ton-payment-network/pkg/payments/conditionals/oracle"
	deriv "github.com/xssnick/ton-payment-network/tonpayments/derivatives"
	"github.com/xssnick/ton-payment-network/tonpayments/transport"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

// DerivativesService provides APIs for position state (stubbed open/close).
type DerivativesService struct {
	core *Service
}

func NewDerivativesService(core *Service) *DerivativesService {
	return &DerivativesService{core: core}
}

// GetDerivativesPosition scans Our-side conditionals and finds a ConditionalResolvable, returning a computed view.
func (s *DerivativesService) GetDerivativesPosition(ctx context.Context, channelAddr string, symbol string) (any, error) {
	ch, err := s.core.GetChannel(ctx, channelAddr)
	if err != nil {
		return nil, err
	}

	var condView *deriv.PositionView

	all, err := ch.Our.Data.Conditionals.LoadAll()
	if err != nil {
		return nil, err
	}

	// We can format as integers (scales cancel out in ratios) to keep things simple and precise.
	for _, kv := range all {
		code := kv.Value.MustToCell()
		cnd, err := payments.CodeToConditional(ctx, code, s.core)
		if err != nil {
			continue
		}
		res, ok := cnd.(*conditionals.ConditionalResolvable)
		if !ok {
			continue
		}
		// Found first matching position.
		entryStr := res.Details.EntryPrice.Val.String()

		// Get last price from resolver for this asset
		resolver := oracle.PriceResolvers[res.Details.AssetID]
		if resolver == nil {
			return nil, errors.New("no price resolver")
		}
		_, lastPrice, err := resolver.GetLastPrice()
		if err != nil || lastPrice == nil {
			return nil, errors.New("no price available")
		}
		lastClose := lastPrice.String()

		entryF, _ := strconv.ParseFloat(entryStr, 64)
		currF, _ := strconv.ParseFloat(lastClose, 64)
		pnl := deriv.ComputePnLPercent(entryF, currF, int(res.Details.Leverage), res.Details.IsLong)
		liq := deriv.ComputeLiquidationPrice(entryF, int(res.Details.Leverage), res.Details.IsLong)

		condView = &deriv.PositionView{
			ChannelAddress:   channelAddr,
			IsLong:           res.Details.IsLong,
			Leverage:         int(res.Details.Leverage),
			EntryPrice:       entryStr,
			CurrentPrice:     lastClose,
			PnLPercent:       pnl,
			LiquidationPrice: trimFloat(liq),
		}
		break
	}

	if condView == nil {
		return nil, errors.New("no active position found")
	}
	return condView, nil
}

func (s *DerivativesService) OpenPosition(ctx context.Context, channelAddr string, balanceId, symbolToOpen string, side string, leverage int, amount string, typ string, price tlb.Coins) (string, error) {
	ch, err := s.core.GetActiveChannel(ctx, channelAddr)
	if err != nil {
		return "", fmt.Errorf("failed to get channel: %w", err)
	}

	// Resolve symbol to ID and CoinConfig
	// For now assuming we use TON as collateral for everything
	cc, err := s.core.ResolveCoinConfig(balanceId)
	if err != nil {
		return "", fmt.Errorf("failed to resolve TON config: %w", err)
	}

	amt, err := tlb.FromDecimal(amount, int(cc.Decimals))
	if err != nil {
		return "", fmt.Errorf("failed to parse amount: %w", err)
	}

	var assetID uint32
	// TODO: real mapping
	switch symbolToOpen {
	case "TON":
		assetID = 1
	case "BTC":
		assetID = 2
	default:
		return "", fmt.Errorf("unknown symbol: %s", symbolToOpen)
	}

	// Determine entry price
	var entryPrice tlb.Coins
	if typ == "limit" {
		// parse price
		entryPrice = price
	} else {
		// market - get from oracle
		resolver := oracle.PriceResolvers[assetID]
		if resolver == nil {
			return "", fmt.Errorf("no price resolver for asset %d", assetID)
		}

		_, lastPrice, err := resolver.GetLastPrice()
		if err != nil {
			return "", fmt.Errorf("failed to get current price: %w", err)
		}

		// TODO: add slippage
		entryPrice = cc.MustAmount(lastPrice)
	}

	deadline := time.Now().Add(24 * time.Hour) // default deadline?

	// Ephemeral key for the condition
	_, vPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return "", fmt.Errorf("failed to generate key: %w", err)
	}
	instructionKey := vPriv.Public().(ed25519.PublicKey)

	firstPart := transport.TunnelChainPart{
		Target:   ch.Their.OnchainInfo.Key,
		Capacity: amt.Nano(),
		Fee:      big.NewInt(0), // TODO: calc fee
		Deadline: deadline,
	}

	resDetails := conditionals.ConditionalResolvableDetails{
		AssetID:    assetID,
		IsLong:     side == "long",
		Leverage:   uint16(leverage),
		EntryPrice: actions.Coins{Val: entryPrice.Nano()},
	}

	// Construct instruction details
	// We need resolver code hash and data.
	// For now we use hardcoded or from config if available.
	// Putting placeholders as I don't have the exact values here.
	// Assuming the node we send to knows the contracts.
	// Actually, `ProcessAction` extracts `ConditionalResolvableInstructionDetails`.
	// So we must provide it.

	// TODO: Populate these with real values from config
	resInstDetails := conditionals.ConditionalResolvableInstructionDetails{
		ResolverContractCodeHash: make([]byte, 32),
		ResolverContractData:     cell.BeginCell().EndCell(),
	}

	detailsCell, err := tlb.ToCell(resInstDetails)
	if err != nil {
		return "", fmt.Errorf("failed to serialize instruction details: %w", err)
	}

	instruction := transport.AddConditionalInstruction{
		NextInstructionKey: instructionKey, // not used for final dest but field exists
		NextTarget:         ch.Their.OnchainInfo.Key,
		NextDeadline:       deadline.Unix(),
		Details:            detailsCell,
	}

	// Assuming resolver address is known or derived
	resolverAddr := address.MustParseAddr(ch.Their.Address) // Just placeholder! use real one

	err = s.core.CreateDerivativeCond(ctx, instructionKey, vPriv, firstPart, instruction, cc, amt.Nano(), resDetails, resolverAddr)
	if err != nil {
		return "", fmt.Errorf("failed to create derivative condition: %w", err)
	}

	return base64.StdEncoding.EncodeToString(vPriv.Public().(ed25519.PublicKey)), nil
}

func (s *DerivativesService) ClosePosition(ctx context.Context, channelAddr string, symbol string, typ string) error {
	ch, err := s.core.GetActiveChannel(ctx, channelAddr)
	if err != nil {
		return fmt.Errorf("failed to get channel: %w", err)
	}

	// Resolve symbol to ID
	var assetID uint32
	switch symbol {
	case "TON":
		assetID = 1
	case "BTC":
		assetID = 2
	default:
		return fmt.Errorf("unknown symbol: %s", symbol)
	}

	// Scan conditionals to find matching positions (both directions)
	// We might have Outgoing (Our Collateral) and Incoming (Their Collateral)
	var outgoingKey, incomingKey ed25519.PublicKey
	var outgoingCond, incomingCond *conditionals.ConditionalResolvable

	all, err := ch.Our.Data.Conditionals.LoadAll()
	if err != nil {
		return fmt.Errorf("failed to load conditionals: %w", err)
	}

	for _, kv := range all {
		code := kv.Value.MustToCell()
		cnd, err := payments.CodeToConditional(ctx, code, s.core)
		if err != nil {
			continue
		}
		res, ok := cnd.(*conditionals.ConditionalResolvable)
		if !ok {
			continue
		}

		if res.Details.AssetID == assetID {
			// Check direction via Meta
			meta, err := s.core.GetVirtualChannelMeta(ctx, res.Key)
			if err != nil {
				continue // Skip if no meta known
			}

			if meta.Outgoing != nil {
				outgoingKey = res.Key
				outgoingCond = res
			}
			if meta.Incoming != nil {
				incomingKey = res.Key
				incomingCond = res
			}
		}
	}

	if outgoingCond == nil && incomingCond == nil {
		return fmt.Errorf("position not found")
	}

	// Calculate resolution state
	if typ != "market" {
		return fmt.Errorf("only market close supported for now")
	}

	resolver := oracle.PriceResolvers[assetID]
	if resolver == nil {
		return fmt.Errorf("no price resolver for asset %d", assetID)
	}
	at, lastPrice, err := resolver.GetLastPrice()
	if err != nil {
		return fmt.Errorf("failed to get current price: %w", err)
	}

	// PnL Logic:
	// We use the details from one of the conditionals (prefer Outgoing if available as it defines "Our" trade view, else Incoming)
	// If Incoming, IsLong might be from Their perspective?
	// Contract "Details" are usually static. If we created the trade, Details reflect the trade parameters.
	// If we are Long, Outgoing Details.IsLong = true.
	// Incoming Details.IsLong? If it mirrors, it might be same?
	// Let's assume standard Details: IsLong means "Initiator is Long".
	// If we see Incoming, We are the Acceptor. IsLong=true means They are Long.

	var refCond *conditionals.ConditionalResolvable
	var isOurLong bool

	if outgoingCond != nil {
		refCond = outgoingCond
		isOurLong = refCond.Details.IsLong
	} else {
		refCond = incomingCond
		// If Incoming, and they are Long (Details.IsLong=true), then We are Short (False).
		isOurLong = !refCond.Details.IsLong
	}

	entryPrice := refCond.Details.EntryPrice.Nano()

	var delta *big.Int
	// Calculate Delta from OUR perspective
	if isOurLong {
		delta = new(big.Int).Sub(lastPrice, entryPrice)
	} else {
		delta = new(big.Int).Sub(entryPrice, lastPrice)
	}

	// PnL = (Collateral * Leverage * PriceDelta) / EntryPrice
	// Base calculation on the reference conditional's Amount (Collateral).
	// If both exist, amounts should match? Or maybe asymmetric.
	// Let's use refCond amount.
	positionSize := new(big.Int).Mul(refCond.Amount, big.NewInt(int64(refCond.Details.Leverage)))
	pnl := new(big.Int).Mul(positionSize, delta)
	pnl.Div(pnl, entryPrice)

	// Determine Settlement Amounts for Each Side

	// Outgoing (Us -> Them): pay Payout if we lose (PnL < 0)
	if outgoingCond != nil {
		var settleAmount *big.Int
		if pnl.Sign() < 0 {
			// Loss. We pay Abs(PnL).
			loss := new(big.Int).Abs(pnl)

			// Cap Logic: If Amount > 0, Cap at Amount. If Amount == 0, Unlimited.
			if outgoingCond.Amount.Sign() > 0 && loss.Cmp(outgoingCond.Amount) > 0 {
				loss.Set(outgoingCond.Amount)
			}
			settleAmount = loss
		} else {
			// Profit. We pay 0.
			settleAmount = big.NewInt(0)
		}

		resState := conditionals.ResolvableState{
			Key:    outgoingKey,
			Amount: settleAmount,
			At:     at,
		}
		st, _ := tlb.ToCell(resState)
		if err := s.core.AddConditionalResolve(ctx, outgoingKey, st); err != nil {
			return fmt.Errorf("failed to add outgoing resolve: %w", err)
		}
		if err := s.core.CloseDerivative(ctx, outgoingKey); err != nil {
			return fmt.Errorf("failed to close outgoing derivative: %w", err)
		}
	}

	// Incoming (Them -> Us): pay Payout if we win (PnL > 0)
	if incomingCond != nil {
		var settleAmount *big.Int
		if pnl.Sign() > 0 {
			// Profit. They pay PnL.
			profit := new(big.Int).Set(pnl)

			// Cap Logic: If Amount > 0, Cap at Amount. If Amount == 0, Unlimited.
			if incomingCond.Amount.Sign() > 0 && profit.Cmp(incomingCond.Amount) > 0 {
				profit.Set(incomingCond.Amount)
			}
			settleAmount = profit
		} else {
			// Loss (for Us) = Win (for Them). They pay 0.
			settleAmount = big.NewInt(0)
		}

		resState := conditionals.ResolvableState{
			Key:    incomingKey,
			Amount: settleAmount,
			At:     at,
		}
		st, _ := tlb.ToCell(resState)
		if err := s.core.AddConditionalResolve(ctx, incomingKey, st); err != nil {
			return fmt.Errorf("failed to add incoming resolve: %w", err)
		}
		if err := s.core.CloseConditional(ctx, incomingKey); err != nil {
			return fmt.Errorf("failed to close incoming derivative: %w", err)
		}
	}

	return nil
}

func trimFloat(f float64) string {
	// format without trailing zeros
	return strconv.FormatFloat(f, 'f', -1, 64)
}
