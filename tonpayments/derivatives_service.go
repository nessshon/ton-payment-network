package tonpayments

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	"crypto/ed25519"

	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/pkg/payments/actions"
	"github.com/xssnick/ton-payment-network/pkg/payments/conditionals"
	"github.com/xssnick/ton-payment-network/pkg/payments/conditionals/oracle"
	"github.com/xssnick/ton-payment-network/tonpayments/db"
	deriv "github.com/xssnick/ton-payment-network/tonpayments/derivatives"
	"github.com/xssnick/ton-payment-network/tonpayments/transport"
	"github.com/xssnick/tonutils-go/tlb"
)

// DerivativesService provides APIs for position state (stubbed open/close).
type DerivativesService struct {
	core *Service
}

const defaultDerivativesCollateralSymbol = "TON"
const maxSupportedLeverage = 20

const (
	derivativePositionStatusOpen        = "open"
	derivativePositionStatusPendingOpen = "pending_open"
)

// TODO: make dynamic if needed
var supportedSymbols = []struct {
	Provider string
	Symbol   string
}{
	{"binance", "BTCUSDT"},
	{"binance", "TONUSDT"},
}

func NewDerivativesService(core *Service) *DerivativesService {
	return &DerivativesService{core: core}
}

func (s *DerivativesService) GetSymbolByID(id uint32) string {
	return derivativeSymbolByID(id)
}

func canonicalPositionID(key []byte, linked []byte) string {
	id := base64.StdEncoding.EncodeToString(key)
	if len(linked) != ed25519.PublicKeySize {
		return id
	}

	linkedID := base64.StdEncoding.EncodeToString(linked)
	if linkedID < id {
		return linkedID + ":" + id
	}
	return id + ":" + linkedID
}

func formatDerivativePrice(price *big.Int) string {
	if price == nil {
		return "0"
	}
	return tlb.MustFromNano(price, 9).String()
}

func decodeDerivativePositionID(raw string) (ed25519.PublicKey, bool) {
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(raw))
	if err != nil || len(decoded) != ed25519.PublicKeySize {
		return nil, false
	}
	return ed25519.PublicKey(decoded), true
}

func (s *DerivativesService) findResolvableByKey(ctx context.Context, ch *db.Channel, key ed25519.PublicKey) (*conditionals.ConditionalResolvable, error) {
	if len(key) != ed25519.PublicKeySize {
		return nil, nil
	}

	all, err := ch.Our.Data.Conditionals.LoadAll()
	if err != nil {
		return nil, fmt.Errorf("failed to load conditionals: %w", err)
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
		if bytes.Equal(res.Key, key) {
			return res, nil
		}
	}
	return nil, nil
}

func resolveDerivativePositionKeys(meta *db.ConditionalMeta, positionID ed25519.PublicKey) (ed25519.PublicKey, ed25519.PublicKey, error) {
	if meta == nil {
		return nil, nil, fmt.Errorf("position metadata is missing")
	}

	var outgoingKey, incomingKey ed25519.PublicKey
	if meta.Outgoing != nil {
		outgoingKey = append(ed25519.PublicKey(nil), positionID...)
		if len(meta.Outgoing.LinkedKey) == ed25519.PublicKeySize {
			incomingKey = append(ed25519.PublicKey(nil), meta.Outgoing.LinkedKey...)
		}
	}
	if meta.Incoming != nil {
		incomingKey = append(ed25519.PublicKey(nil), positionID...)
		if len(meta.Incoming.LinkedKey) == ed25519.PublicKeySize {
			outgoingKey = append(ed25519.PublicKey(nil), meta.Incoming.LinkedKey...)
		}
	}
	if outgoingKey == nil && incomingKey == nil {
		return nil, nil, fmt.Errorf("position metadata is inconsistent")
	}
	return outgoingKey, incomingKey, nil
}

func (s *DerivativesService) derivativePositionOpened(ctx context.Context, incomingKey ed25519.PublicKey) (bool, int64, error) {
	// Legacy records may miss linked incoming key; treat them as already opened.
	if len(incomingKey) != ed25519.PublicKeySize {
		return true, 0, nil
	}

	refreshCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	metaFresh, condFresh, monitor, _, err := s.core.refreshIncomingDerivativeMeta(refreshCtx, incomingKey)
	if err == nil && monitor != nil {
		return derivativeOrderOpenedFromMonitor(metaFresh, condFresh, monitor), monitor.EntryCrossedAt, nil
	}

	meta, metaErr := s.core.GetVirtualChannelMeta(ctx, incomingKey)
	if metaErr != nil {
		if err != nil {
			return false, 0, fmt.Errorf("failed to refresh derivative monitor: %w", err)
		}
		return false, 0, fmt.Errorf("failed to load incoming derivative metadata: %w", metaErr)
	}

	monitor, _, parseErr := parseDerivativeMonitor(meta)
	if parseErr != nil {
		if err != nil {
			return false, 0, fmt.Errorf("failed to refresh derivative monitor: %w", err)
		}
		return false, 0, fmt.Errorf("failed to parse derivative monitor: %w", parseErr)
	}

	opened := monitor.EntryCrossed
	if meta.Incoming != nil && meta.Incoming.Conditional != nil {
		condRaw, condErr := payments.CodeToConditional(ctx, meta.Incoming.Conditional, s.core)
		if condErr == nil {
			if cond, ok := condRaw.(*conditionals.ConditionalResolvable); ok {
				opened = derivativeOrderOpenedFromMonitor(meta, cond, monitor)
			}
		}
	}

	return opened, monitor.EntryCrossedAt, nil
}

func (s *DerivativesService) closePositionByID(ctx context.Context, ch *db.Channel, positionID ed25519.PublicKey) error {
	meta, err := s.core.GetVirtualChannelMeta(ctx, positionID)
	if err != nil {
		return fmt.Errorf("failed to load position metadata: %w", err)
	}
	if meta.Status != db.ConditionalStateActive {
		return fmt.Errorf("position is not active")
	}

	outgoingKey, incomingKey, err := resolveDerivativePositionKeys(meta, positionID)
	if err != nil {
		return err
	}

	opened, _, err := s.derivativePositionOpened(ctx, incomingKey)
	if err != nil {
		return fmt.Errorf("failed to check derivative open status: %w", err)
	}
	if !opened {
		return fmt.Errorf("position is not opened yet, cancel it instead")
	}

	outgoingCond, err := s.findResolvableByKey(ctx, ch, outgoingKey)
	if err != nil {
		return err
	}
	incomingCond, err := s.findResolvableByKey(ctx, ch, incomingKey)
	if err != nil {
		return err
	}
	if outgoingCond == nil && incomingCond == nil {
		return fmt.Errorf("position not found")
	}

	refCond := outgoingCond
	if refCond == nil {
		refCond = incomingCond
	}

	if refCond == nil || refCond.Details.Leverage == 0 {
		return fmt.Errorf("position details are invalid")
	}
	entryPrice := refCond.Details.EntryPrice.Nano()
	if entryPrice == nil || entryPrice.Sign() <= 0 {
		return fmt.Errorf("position entry price is invalid")
	}

	resolver := oracle.PriceResolvers[refCond.Details.AssetID]
	if resolver == nil {
		return fmt.Errorf("no price resolver for asset %d", refCond.Details.AssetID)
	}

	at, lastPrice, err := resolver.GetLastPrice()
	if err != nil || lastPrice == nil || lastPrice.Sign() <= 0 {
		return fmt.Errorf("failed to get current price: %w", err)
	}

	calcPnLForCond := func(cond *conditionals.ConditionalResolvable, isLong bool) *big.Int {
		var delta *big.Int
		if isLong {
			delta = new(big.Int).Sub(lastPrice, entryPrice)
		} else {
			delta = new(big.Int).Sub(entryPrice, lastPrice)
		}

		positionSize := new(big.Int).Mul(cond.Amount, big.NewInt(int64(cond.Details.Leverage)))
		p := new(big.Int).Mul(positionSize, delta)
		p.Div(p, entryPrice)
		return p
	}

	var pnlOur *big.Int
	if outgoingCond != nil {
		pnlOur = calcPnLForCond(outgoingCond, outgoingCond.Details.IsLong)
	} else {
		// If outgoing doesn't exist, we evaluate our PnL purely via the inverse of their incoming
		pnlOur = calcPnLForCond(incomingCond, !incomingCond.Details.IsLong)
	}

	if outgoingCond != nil {
		settleAmount := big.NewInt(0)
		if pnlOur.Sign() < 0 {
			loss := new(big.Int).Abs(pnlOur)
			if outgoingCond.Amount.Sign() > 0 && loss.Cmp(outgoingCond.Amount) > 0 {
				loss.Set(outgoingCond.Amount)
			}
			settleAmount = loss
		}

		resState := conditionals.ResolvableState{
			Key:    outgoingKey,
			Amount: settleAmount,
			At:     at,
		}
		st, _ := tlb.ToCell(resState)
		if err = s.core.AddConditionalResolve(ctx, outgoingKey, st); err != nil {
			return fmt.Errorf("failed to add outgoing resolve: %w", err)
		}
		if err = s.core.CloseDerivative(ctx, outgoingKey); err != nil {
			return fmt.Errorf("failed to close outgoing derivative: %w", err)
		}
	}

	if incomingCond != nil {
		pnlTheir := calcPnLForCond(incomingCond, incomingCond.Details.IsLong)

		settleAmount := big.NewInt(0)
		if pnlTheir.Sign() < 0 {
			loss := new(big.Int).Abs(pnlTheir)
			if incomingCond.Amount.Sign() > 0 && loss.Cmp(incomingCond.Amount) > 0 {
				loss.Set(incomingCond.Amount)
			}
			settleAmount = loss
		}

		resState := conditionals.ResolvableState{
			Key:    incomingKey,
			Amount: settleAmount,
			At:     at,
		}
		st, _ := tlb.ToCell(resState)
		if err = s.core.AddConditionalResolve(ctx, incomingKey, st); err != nil {
			return fmt.Errorf("failed to add incoming resolve: %w", err)
		}
		if err = s.core.CloseConditional(ctx, incomingKey); err != nil {
			return fmt.Errorf("failed to close incoming derivative: %w", err)
		}
	}

	return nil
}

func (s *DerivativesService) cancelPositionByID(ctx context.Context, positionID ed25519.PublicKey) error {
	meta, err := s.core.GetVirtualChannelMeta(ctx, positionID)
	if err != nil {
		return fmt.Errorf("failed to load position metadata: %w", err)
	}
	if meta.Status != db.ConditionalStateActive {
		return fmt.Errorf("position is not active")
	}

	_, incomingKey, err := resolveDerivativePositionKeys(meta, positionID)
	if err != nil {
		return err
	}

	opened, _, err := s.derivativePositionOpened(ctx, incomingKey)
	if err != nil {
		return fmt.Errorf("failed to check derivative open status: %w", err)
	}
	if opened {
		return fmt.Errorf("position is already opened, use market close")
	}

	if len(incomingKey) == ed25519.PublicKeySize {
		if err = s.core.RemoveConditional(ctx, incomingKey); err != nil {
			return fmt.Errorf("failed to cancel derivative via remove conditional: %w", err)
		}
		return nil
	}

	return fmt.Errorf("position metadata is inconsistent: incoming side is missing")
}

func (s *DerivativesService) ListDerivativesPositions(ctx context.Context, channelAddr string, symbol string) ([]deriv.PositionView, error) {
	ch, err := s.core.GetChannel(ctx, channelAddr)
	if err != nil {
		return nil, err
	}

	all, err := ch.Our.Data.Conditionals.LoadAll()
	if err != nil {
		return nil, err
	}

	seen := map[string]struct{}{}
	positions := make([]deriv.PositionView, 0)

	// Filter by symbol if provided
	var bufID uint32
	if symbol != "" {
		// Only supporting binance source for now
		bufID = oracle.GetResolverID("binance", symbol)
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

		if bufID != 0 && res.Details.AssetID != bufID {
			continue
		}

		assetSymbol := s.GetSymbolByID(res.Details.AssetID)
		if assetSymbol == "" {
			continue
		}

		meta, err := s.core.GetVirtualChannelMeta(ctx, res.Key)
		if err != nil || meta == nil {
			continue
		}
		if meta.Status != db.ConditionalStateActive {
			continue
		}
		if meta.Outgoing == nil && meta.Incoming == nil {
			continue
		}

		var linked []byte
		switch {
		case meta.Outgoing != nil && len(meta.Outgoing.LinkedKey) == ed25519.PublicKeySize:
			linked = meta.Outgoing.LinkedKey
		case meta.Incoming != nil && len(meta.Incoming.LinkedKey) == ed25519.PublicKeySize:
			linked = meta.Incoming.LinkedKey
		}

		posID := canonicalPositionID(res.Key, linked)
		if _, already := seen[posID]; already {
			continue
		}
		seen[posID] = struct{}{}

		resolver := oracle.PriceResolvers[res.Details.AssetID]
		if resolver == nil {
			continue
		}
		_, lastPrice, err := resolver.GetLastPrice()
		if err != nil || lastPrice == nil {
			continue
		}

		isOurLong := res.Details.IsLong
		if meta.Outgoing == nil && meta.Incoming != nil {
			// Incoming only means initiator is on the other side.
			isOurLong = !isOurLong
		}

		entryStr := formatDerivativePrice(res.Details.EntryPrice.Nano())
		currentStr := formatDerivativePrice(lastPrice)

		incomingKey := ed25519.PublicKey(nil)
		if meta.Incoming != nil {
			incomingKey = append(ed25519.PublicKey(nil), res.Key...)
		} else if meta.Outgoing != nil && len(meta.Outgoing.LinkedKey) == ed25519.PublicKeySize {
			incomingKey = append(ed25519.PublicKey(nil), meta.Outgoing.LinkedKey...)
		}

		opened, openedAt, openedErr := s.derivativePositionOpened(ctx, incomingKey)
		if openedErr != nil {
			// Listing should be resilient; strict validation is done on close/cancel.
			opened = false
			openedAt = 0
		}

		status := derivativePositionStatusPendingOpen
		pnl := 0.0
		liqStr := ""
		if opened {
			status = derivativePositionStatusOpen
			entryF, _ := strconv.ParseFloat(entryStr, 64)
			currF, _ := strconv.ParseFloat(currentStr, 64)
			pnl = deriv.ComputePnLPercent(entryF, currF, int(res.Details.Leverage), isOurLong)
			liqStr = trimFloat(deriv.ComputeLiquidationPrice(entryF, int(res.Details.Leverage), isOurLong))
		}

		positions = append(positions, deriv.PositionView{
			ID:               base64.StdEncoding.EncodeToString(res.Key),
			Symbol:           assetSymbol,
			ChannelAddress:   channelAddr,
			Collateral:       collateralFormatter(res.Amount, s),
			Fee:              collateralFormatter(res.Fee, s),
			IsLong:           isOurLong,
			Leverage:         int(res.Details.Leverage),
			Status:           status,
			Opened:           opened,
			Hedged:           derivativeMetaHedged(meta),
			OpenedAt:         openedAt,
			EntryAt:          meta.CreatedAt.Unix(),
			EntryPrice:       entryStr,
			CurrentPrice:     currentStr,
			PnLPercent:       pnl,
			LiquidationPrice: liqStr,
		})
	}

	return positions, nil
}

func collateralFormatter(amount *big.Int, s *DerivativesService) string {
	if amount == nil {
		return "0"
	}
	cc, err := s.core.ResolveCoinConfigBySymbol(defaultDerivativesCollateralSymbol)
	if err != nil {
		return amount.String()
	}
	return cc.MustAmount(amount).String()
}

func (s *DerivativesService) GetMarketPrice(_ context.Context, symbol string) (*deriv.QuoteView, error) {
	// Only supporting binance source for now
	assetID := oracle.GetResolverID("binance", symbol)

	resolver := oracle.PriceResolvers[assetID]
	if resolver == nil {
		// If explicit symbol not found, try to see if it's a known symbol from our list
		// but maybe user passed just "BTCUSDT" without checks
		// Let's rely on PriceResolvers being populated.
		return nil, fmt.Errorf("no price resolver for %s", symbol)
	}

	at, lastPrice, err := resolver.GetLastPrice()
	if err != nil {
		return nil, fmt.Errorf("failed to get price: %w", err)
	}
	if lastPrice == nil {
		return nil, errors.New("no price available")
	}

	return &deriv.QuoteView{
		Symbol:   symbol,
		Price:    formatDerivativePrice(lastPrice),
		RawPrice: lastPrice.String(),
		At:       at,
	}, nil
}

func (s *DerivativesService) GetPriceHistory(_ context.Context, symbol string) ([]deriv.PriceHistoryPoint, error) {
	assetID := oracle.GetResolverID("binance", symbol)

	resolver := oracle.PriceResolvers[assetID]
	if resolver == nil {
		return nil, fmt.Errorf("no price resolver for %s", symbol)
	}

	rawPrices := resolver.GetPricesSince(0)
	result := make([]deriv.PriceHistoryPoint, 0, len(rawPrices))
	for _, p := range rawPrices {
		result = append(result, deriv.PriceHistoryPoint{
			At:    p.At,
			Price: formatDerivativePrice(p.Price),
		})
	}
	return result, nil
}

// GetDerivativesPosition scans Our-side conditionals and returns one active view.
func (s *DerivativesService) GetDerivativesPosition(ctx context.Context, channelAddr string, symbol string) (any, error) {
	list, err := s.ListDerivativesPositions(ctx, channelAddr, symbol)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, errors.New("no active position found")
	}
	return list[0], nil
}

func (s *DerivativesService) OpenPosition(ctx context.Context, channelAddr string, symbolToOpen string, side string, leverage int, amount string, typ string, price string) (string, error) {
	if side != "long" && side != "short" {
		return "", fmt.Errorf("side must be long or short")
	}
	if typ != "market" && typ != "limit" {
		return "", fmt.Errorf("type must be limit or market")
	}
	if leverage <= 0 || leverage > maxSupportedLeverage {
		return "", fmt.Errorf("leverage must be in range 1..%d", maxSupportedLeverage)
	}

	ch, err := s.core.GetActiveChannel(ctx, channelAddr)
	if err != nil {
		return "", fmt.Errorf("failed to get channel: %w", err)
	}

	// Resolve symbol to ID and CoinConfig
	// For now assuming we use TON as collateral for everything
	cc, err := s.core.ResolveCoinConfigBySymbol(defaultDerivativesCollateralSymbol)
	if err != nil {
		return "", fmt.Errorf("failed to resolve collateral config: %w", err)
	}

	amt, err := tlb.FromDecimal(amount, int(cc.Decimals))
	if err != nil {
		return "", fmt.Errorf("failed to parse amount: %w", err)
	}
	if amt.Nano().Sign() <= 0 {
		return "", fmt.Errorf("amount must be greater than zero")
	}
	feeNano := payments.CalcPercentFeeCeil(amt.Nano(), cc.VirtualTunnelConfig.DerivativeFeePercent)
	feeNano.Mul(feeNano, big.NewInt(int64(leverage)))

	// Only supporting binance source for now
	assetID := oracle.GetResolverID("binance", symbolToOpen)

	// Determine entry price
	var entryPrice tlb.Coins
	if typ == "limit" {
		// parse price
		entryPrice, err = tlb.FromDecimal(price, 9)
		if err != nil {
			return "", fmt.Errorf("failed to parse limit price: %w", err)
		}
		if entryPrice.Nano().Sign() <= 0 {
			return "", fmt.Errorf("limit price must be greater than zero")
		}
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
		if lastPrice == nil || lastPrice.Sign() <= 0 {
			return "", fmt.Errorf("failed to get valid current price")
		}

		// TODO: add slippage
		entryPrice = tlb.MustFromNano(lastPrice, 9)
	}

	deadline := time.Now().Add(24 * time.Hour) // default deadline?

	// Ephemeral key for the condition
	_, vPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return "", fmt.Errorf("failed to generate key: %w", err)
	}

	firstPart := transport.TunnelChainPart{
		Target:   ch.Their.OnchainInfo.Key,
		Capacity: amt.Nano(),
		Fee:      big.NewInt(0),
		Deadline: deadline,
	}

	resDetails := conditionals.ConditionalResolvableDetails{
		AssetID:    assetID,
		IsLong:     side == "long",
		Leverage:   uint16(leverage),
		EntryPrice: actions.Coins{Val: entryPrice.Nano()},
	}

	_, resolverAddr, resInstDetails, err := s.core.buildDerivativeResolverContract(
		ch,
		vPriv.Public().(ed25519.PublicKey),
		amt.Nano(),
		resDetails,
	)
	if err != nil {
		return "", fmt.Errorf("failed to build derivative resolver contract params: %w", err)
	}

	detailsCell, err := tlb.ToCell(resInstDetails)
	if err != nil {
		return "", fmt.Errorf("failed to serialize instruction details: %w", err)
	}

	instructionKey, chain, err := transport.GenerateTunnel(s.core.key, []transport.TunnelChainPart{firstPart}, 0, false, nil, cc)
	if err != nil {
		return "", fmt.Errorf("failed to generate derivative tunnel instruction: %w", err)
	}
	if len(chain) != 1 {
		return "", fmt.Errorf("unexpected generated instruction count: %d", len(chain))
	}
	instruction := chain[0]
	instruction.ExpectedDeadline = time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
	instruction.Details = detailsCell

	err = s.core.CreateDerivativeCond(ctx, instructionKey, vPriv, firstPart, instruction, cc, amt.Nano(), feeNano, resDetails, resolverAddr)
	if err != nil {
		return "", fmt.Errorf("failed to create derivative condition: %w", err)
	}

	return base64.StdEncoding.EncodeToString(vPriv.Public().(ed25519.PublicKey)), nil
}

func (s *DerivativesService) ClosePosition(ctx context.Context, channelAddr string, symbolOrPositionID string, typ string) error {
	if typ != "market" && typ != "cancel" {
		return fmt.Errorf("type must be market or cancel")
	}

	ch, err := s.core.GetActiveChannel(ctx, channelAddr)
	if err != nil {
		return fmt.Errorf("failed to get channel: %w", err)
	}

	if key, ok := decodeDerivativePositionID(symbolOrPositionID); ok {
		if typ == "cancel" {
			return s.cancelPositionByID(ctx, key)
		}
		return s.closePositionByID(ctx, ch, key)
	}

	positions, err := s.ListDerivativesPositions(ctx, channelAddr, symbolOrPositionID)
	if err != nil {
		return err
	}
	if len(positions) == 0 {
		return fmt.Errorf("position not found")
	}
	if len(positions) > 1 {
		return fmt.Errorf("multiple positions found for symbol %s, %s by position id", symbolOrPositionID, typ)
	}

	key, ok := decodeDerivativePositionID(positions[0].ID)
	if !ok {
		return fmt.Errorf("position id is malformed")
	}

	if typ == "cancel" {
		return s.cancelPositionByID(ctx, key)
	}
	return s.closePositionByID(ctx, ch, key)
}

func (s *DerivativesService) SetPositionHedged(ctx context.Context, orderID string, hedged bool) error {
	key, ok := decodeDerivativePositionID(orderID)
	if !ok {
		return fmt.Errorf("order id is malformed")
	}
	return s.core.SetDerivativeOrderHedged(ctx, key, hedged)
}

func trimFloat(f float64) string {
	// format without trailing zeros
	return strconv.FormatFloat(f, 'f', -1, 64)
}
