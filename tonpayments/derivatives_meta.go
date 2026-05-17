package tonpayments

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/xssnick/ton-payment-network/pkg/log"
	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/pkg/payments/conditionals"
	"github.com/xssnick/ton-payment-network/pkg/payments/conditionals/oracle"
	"github.com/xssnick/ton-payment-network/tonpayments/db"
	"github.com/xssnick/ton-payment-network/tonpayments/hedgeauth"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

type derivativeMetaAny struct {
	Details  conditionals.ConditionalResolvableDetails `json:"details"`
	Resolver *derivativeResolverMeta                   `json:"resolver,omitempty"`
	Monitor  *derivativeMonitorState                   `json:"monitor,omitempty"`
	Hedged   bool                                      `json:"hedged,omitempty"`
}

func derivativeSymbolByID(id uint32) string {
	for _, sym := range supportedSymbols {
		if oracle.GetResolverID(sym.Provider, sym.Symbol) == id {
			return sym.Symbol
		}
	}
	return ""
}

func derivativeMetaPack(meta *db.ConditionalMeta) derivativeMetaAny {
	if meta == nil || meta.SpecialDetails == nil {
		return derivativeMetaAny{}
	}

	var packed derivativeMetaAny
	if err := recodeJSON(meta.SpecialDetails, &packed); err == nil {
		if packed.Details.AssetID != 0 || packed.Details.Leverage != 0 || packed.Resolver != nil || packed.Monitor != nil || packed.Hedged {
			return packed
		}
	}

	var details conditionals.ConditionalResolvableDetails
	if err := recodeJSON(meta.SpecialDetails, &details); err == nil {
		packed.Details = details
	}
	return packed
}

func derivativeMetaHedged(meta *db.ConditionalMeta) bool {
	return derivativeMetaPack(meta).Hedged
}

func derivativeMetaSetHedged(meta *db.ConditionalMeta, hedged bool) {
	packed := derivativeMetaPack(meta)
	packed.Hedged = hedged
	meta.SpecialDetails = packed
}

func derivativeMetaSetMonitor(meta *db.ConditionalMeta, monitor *derivativeMonitorState) {
	packed := derivativeMetaPack(meta)
	packed.Monitor = monitor
	meta.SpecialDetails = packed
}

func derivativeStatusString(status db.ConditionalStatus) string {
	switch status {
	case db.ConditionalStateClosed:
		return "closed"
	case db.ConditionalStateRemoved:
		return "removed"
	case db.ConditionalStateWantClose:
		return "want_close"
	case db.ConditionalStateWantRemove:
		return "want_remove"
	case db.ConditionalStatePending:
		return "pending"
	default:
		return "active"
	}
}

func (s *Service) formatDerivativeCollateralAmount(amount *big.Int) string {
	if amount == nil {
		return "0"
	}

	cc, err := s.ResolveCoinConfigBySymbol(defaultDerivativesCollateralSymbol)
	if err != nil {
		return amount.String()
	}
	return cc.MustAmount(amount).String()
}

func (s *Service) resolveDerivativeOrderMeta(ctx context.Context, meta *db.ConditionalMeta) (*db.ConditionalMeta, error) {
	if meta == nil {
		return nil, nil
	}
	if meta.Outgoing != nil {
		return meta, nil
	}
	if meta.Incoming == nil || len(meta.Incoming.LinkedKey) != ed25519.PublicKeySize {
		return nil, nil
	}

	linkedMeta, err := s.db.GetVirtualChannelMeta(ctx, meta.Incoming.LinkedKey)
	if err != nil {
		return nil, err
	}
	if linkedMeta.Outgoing == nil {
		return nil, nil
	}
	return linkedMeta, nil
}

func (s *Service) derivativesHedgingEnabled() bool {
	return strings.TrimSpace(s.cfg.DerivativesHedge.WebhookURL) != ""
}

func derivativeCanRemoveWithoutPriceHistory(meta *db.ConditionalMeta, monitor *derivativeMonitorState) bool {
	return monitor != nil && monitor.HistoryTooOld && !derivativeMetaHedged(meta)
}

type derivativeHedgeWebhookEvent string

const (
	derivativeHedgeWebhookEventOpen  derivativeHedgeWebhookEvent = "open"
	derivativeHedgeWebhookEventClose derivativeHedgeWebhookEvent = "close"
)

type derivativeHedgeWebhookRequest struct {
	Event          derivativeHedgeWebhookEvent `json:"event"`
	OrderID        string                      `json:"order_id"`
	LinkedOrderID  string                      `json:"linked_order_id,omitempty"`
	Status         string                      `json:"status,omitempty"`
	ChannelAddress string                      `json:"channel_address"`
	Symbol         string                      `json:"symbol,omitempty"`
	IsLong         bool                        `json:"is_long"`
	Leverage       int                         `json:"leverage"`
	Collateral     string                      `json:"collateral"`
	Fee            string                      `json:"fee"`
	EntryPrice     string                      `json:"entry_price"`
	Hedged         bool                        `json:"hedged"`
	CreatedAt      int64                       `json:"created_at"`
	ClosedAt       int64                       `json:"closed_at,omitempty"`
}

type derivativeHedgeWebhookTask struct {
	Request derivativeHedgeWebhookRequest `json:"request"`
}

type derivativeHedgeWebhookResponse struct {
	Success bool `json:"success"`
}

const (
	derivativeHedgeWebhookOpenTimeout  = 2 * time.Second
	derivativeHedgeWebhookCloseTimeout = 5 * time.Second
	derivativeHedgeWebhookMaxResponse  = 8 * 1024
)

func (s *Service) derivativeHedgeAuth() (string, []byte, error) {
	key := strings.TrimSpace(s.cfg.DerivativesHedge.WebhookKey)
	if key == "" {
		return "", nil, fmt.Errorf("derivatives hedge webhook key is not configured")
	}

	secret, err := hedgeauth.DecodeBase64Key(strings.TrimSpace(s.cfg.DerivativesHedge.WebhookSignatureHMACSHA256Key))
	if err != nil {
		return "", nil, err
	}
	return key, secret, nil
}

func (s *Service) buildDerivativeHedgeOpenWebhook(channel *db.Channel, incoming, outgoing *conditionals.ConditionalResolvable) (*derivativeHedgeWebhookRequest, error) {
	if channel == nil || incoming == nil || outgoing == nil {
		return nil, fmt.Errorf("derivative hedge webhook requires full derivative pair")
	}

	return &derivativeHedgeWebhookRequest{
		Event:          derivativeHedgeWebhookEventOpen,
		OrderID:        base64.StdEncoding.EncodeToString(outgoing.GetKey()),
		LinkedOrderID:  base64.StdEncoding.EncodeToString(incoming.GetKey()),
		ChannelAddress: channel.Our.Address,
		Symbol:         derivativeSymbolByID(outgoing.Details.AssetID),
		IsLong:         outgoing.Details.IsLong,
		Leverage:       int(outgoing.Details.Leverage),
		Collateral:     s.formatDerivativeCollateralAmount(outgoing.Amount),
		Fee:            s.formatDerivativeCollateralAmount(outgoing.Fee),
		EntryPrice:     formatDerivativePrice(outgoing.Details.EntryPrice.Nano()),
		Hedged:         false,
		CreatedAt:      time.Now().UTC().Unix(),
	}, nil
}

func (s *Service) buildDerivativeHedgeCloseWebhook(ctx context.Context, meta *db.ConditionalMeta, status db.ConditionalStatus) (*derivativeHedgeWebhookRequest, error) {
	orderMeta, err := s.resolveDerivativeOrderMeta(ctx, meta)
	if err != nil {
		return nil, err
	}
	if orderMeta == nil || orderMeta.Outgoing == nil || orderMeta.Outgoing.Conditional == nil {
		return nil, nil
	}

	condRaw, err := payments.CodeToConditional(ctx, orderMeta.Outgoing.Conditional, s)
	if err != nil {
		return nil, fmt.Errorf("failed to parse derivative for hedge close webhook: %w", err)
	}
	cond, ok := condRaw.(*conditionals.ConditionalResolvable)
	if !ok {
		return nil, nil
	}

	req := &derivativeHedgeWebhookRequest{
		Event:          derivativeHedgeWebhookEventClose,
		OrderID:        base64.StdEncoding.EncodeToString(orderMeta.Key),
		ChannelAddress: orderMeta.Outgoing.ChannelAddress,
		Status:         derivativeStatusString(status),
		Symbol:         derivativeSymbolByID(cond.Details.AssetID),
		IsLong:         cond.Details.IsLong,
		Leverage:       int(cond.Details.Leverage),
		Collateral:     s.formatDerivativeCollateralAmount(cond.Amount),
		Fee:            s.formatDerivativeCollateralAmount(cond.Fee),
		EntryPrice:     formatDerivativePrice(cond.Details.EntryPrice.Nano()),
		Hedged:         derivativeMetaHedged(orderMeta),
		CreatedAt:      orderMeta.CreatedAt.UTC().Unix(),
		ClosedAt:       time.Now().UTC().Unix(),
	}
	if len(orderMeta.Outgoing.LinkedKey) == ed25519.PublicKeySize {
		req.LinkedOrderID = base64.StdEncoding.EncodeToString(orderMeta.Outgoing.LinkedKey)
	}
	return req, nil
}

func (s *Service) sendDerivativeHedgeWebhook(ctx context.Context, req derivativeHedgeWebhookRequest, timeout time.Duration) error {
	return s.sendDerivativeHedgeWebhookImpl(ctx, req, timeout)
}

func (s *Service) requestDerivativeHedgeOpen(ctx context.Context, channel *db.Channel, incoming, outgoing *conditionals.ConditionalResolvable) error {
	req, err := s.buildDerivativeHedgeOpenWebhook(channel, incoming, outgoing)
	if err != nil {
		return err
	}
	return s.sendDerivativeHedgeWebhook(ctx, *req, derivativeHedgeWebhookOpenTimeout)
}

func (s *Service) scheduleDerivativeHedgeClose(ctx context.Context, meta *db.ConditionalMeta, status db.ConditionalStatus) error {
	if !s.derivativesHedgingEnabled() {
		return nil
	}

	req, err := s.buildDerivativeHedgeCloseWebhook(ctx, meta, status)
	if err != nil || req == nil {
		return err
	}

	err = s.db.CreateTask(
		ctx,
		PaymentsTaskPool,
		"derivative-hedge-webhook",
		req.OrderID,
		fmt.Sprintf("derivative-hedge-webhook-%s-%s", req.Event, req.OrderID),
		derivativeHedgeWebhookTask{Request: *req},
		nil,
		nil,
	)
	if err != nil && !errors.Is(err, db.ErrAlreadyExists) {
		return err
	}
	return nil
}

func (s *Service) SetDerivativeOrderHedged(ctx context.Context, key ed25519.PublicKey, hedged bool) error {
	if len(key) != ed25519.PublicKeySize {
		return fmt.Errorf("incorrect order id")
	}

	return s.db.Transaction(ctx, func(ctx context.Context) error {
		meta, err := s.db.GetVirtualChannelMeta(ctx, key)
		if err != nil {
			return err
		}

		orderMeta, err := s.resolveDerivativeOrderMeta(ctx, meta)
		if err != nil {
			return err
		}
		if orderMeta == nil {
			return fmt.Errorf("derivative order is not found")
		}

		derivativeMetaSetHedged(orderMeta, hedged)
		orderMeta.UpdatedAt = time.Now()
		if err = s.db.UpdateVirtualChannelMeta(ctx, orderMeta); err != nil {
			return err
		}

		var linkedKey ed25519.PublicKey
		if orderMeta.Outgoing != nil && len(orderMeta.Outgoing.LinkedKey) == ed25519.PublicKeySize {
			linkedKey = orderMeta.Outgoing.LinkedKey
		}
		if len(linkedKey) != ed25519.PublicKeySize {
			return nil
		}

		linkedMeta, err := s.db.GetVirtualChannelMeta(ctx, linkedKey)
		if err != nil {
			return err
		}
		derivativeMetaSetHedged(linkedMeta, hedged)
		linkedMeta.UpdatedAt = time.Now()
		return s.db.UpdateVirtualChannelMeta(ctx, linkedMeta)
	})
}

func derivativeMetaTouchesChannel(meta *db.ConditionalMeta, channelAddr string) bool {
	if meta == nil {
		return false
	}
	if meta.Incoming != nil && meta.Incoming.ChannelAddress == channelAddr {
		return true
	}
	return meta.Outgoing != nil && meta.Outgoing.ChannelAddress == channelAddr
}

func derivativeMetaResolved(meta *db.ConditionalMeta) bool {
	return meta != nil && meta.LastKnownResolve != nil
}

func derivativeMetaTerminal(status db.ConditionalStatus) bool {
	return status == db.ConditionalStateClosed || status == db.ConditionalStateRemoved
}

func derivativeFinalStatusOnInactive(orderMeta, linkedMeta *db.ConditionalMeta) db.ConditionalStatus {
	if derivativeMetaResolved(orderMeta) || derivativeMetaResolved(linkedMeta) {
		return db.ConditionalStateClosed
	}
	return db.ConditionalStateRemoved
}

func (s *Service) finalizeInactiveChannelDerivatives(ctx context.Context, channel *db.Channel) error {
	if channel == nil {
		return nil
	}

	handled := map[string]struct{}{}

	finalizeKey := func(key ed25519.PublicKey) error {
		if len(key) != ed25519.PublicKeySize {
			return nil
		}

		meta, err := s.db.GetVirtualChannelMeta(ctx, key)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return nil
			}
			return err
		}
		if !derivativeMetaTouchesChannel(meta, channel.Our.Address) {
			return nil
		}

		orderMeta, err := s.resolveDerivativeOrderMeta(ctx, meta)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return nil
			}
			return err
		}
		if orderMeta == nil || orderMeta.Outgoing == nil {
			return nil
		}

		orderID := base64.StdEncoding.EncodeToString(orderMeta.Key)
		if _, ok := handled[orderID]; ok {
			return nil
		}
		handled[orderID] = struct{}{}

		var linkedMeta *db.ConditionalMeta
		if len(orderMeta.Outgoing.LinkedKey) == ed25519.PublicKeySize {
			linkedMeta, err = s.db.GetVirtualChannelMeta(ctx, orderMeta.Outgoing.LinkedKey)
			if err != nil && !errors.Is(err, db.ErrNotFound) {
				return err
			}
		}

		status := derivativeFinalStatusOnInactive(orderMeta, linkedMeta)
		if !derivativeMetaTerminal(orderMeta.Status) {
			if err = s.db.ClosePairMeta(ctx, orderMeta.Key, status); err != nil {
				return err
			}
			orderMeta.Status = status
			orderMeta.UpdatedAt = time.Now()
		}

		if err = s.scheduleDerivativeHedgeClose(ctx, orderMeta, status); err != nil {
			return err
		}
		return nil
	}

	finalizeDict := func(side string, dict *cell.Dictionary) error {
		all, err := dict.LoadAll()
		if err != nil {
			return fmt.Errorf("failed to load %s channel conditionals: %w", side, err)
		}

		for _, kv := range all {
			if kv.Value.RefsNum() == 0 && kv.Value.BitsLeft() == 0 {
				continue
			}

			cond, err := payments.CodeToConditional(ctx, kv.Value.MustToCell(), s)
			if err != nil {
				log.Warn().Err(err).Str("channel", channel.Our.Address).Str("side", side).
					Msg("failed to parse conditional while finalizing inactive derivatives")
				continue
			}
			if _, ok := cond.(*conditionals.ConditionalResolvable); !ok {
				continue
			}

			if err = finalizeKey(cond.GetKey()); err != nil {
				return err
			}
		}
		return nil
	}

	if err := finalizeDict("our", channel.Our.Data.Conditionals); err != nil {
		return err
	}
	return finalizeDict("their", channel.Their.Data.Conditionals)
}
