package tonpayments

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/xssnick/ton-payment-network/pkg/log"
	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/pkg/payments/conditionals"
	"github.com/xssnick/ton-payment-network/pkg/payments/conditionals/oracle"
	"github.com/xssnick/ton-payment-network/tonpayments/db"
	"github.com/xssnick/tonutils-go/tlb"
)

var errNotIncomingDerivative = errors.New("meta is not active incoming derivative")

const derivativeRemoveRefreshTimeout = 3 * time.Second

type derivativeMetaAny struct {
	Details  conditionals.ConditionalResolvableDetails `json:"details"`
	Resolver *derivativeResolverMeta                   `json:"resolver,omitempty"`
	Monitor  *derivativeMonitorState                   `json:"monitor,omitempty"`
}

type derivativeResolverMeta struct {
	CodeHash string `json:"code_hash,omitempty"`
	DataBOC  string `json:"data_boc,omitempty"`
}

type derivativeMonitorState struct {
	LastCheckedAt     int64  `json:"last_checked_at"`
	MinPrice          string `json:"min_price,omitempty"`
	MaxPrice          string `json:"max_price,omitempty"`
	EntryCrossed      bool   `json:"entry_crossed"`
	EntryCrossedAt    int64  `json:"entry_crossed_at,omitempty"`
	Liquidated        bool   `json:"liquidated"`
	LiquidatedAt      int64  `json:"liquidated_at,omitempty"`
	LiquidatedPrice   string `json:"liquidated_price,omitempty"`
	LiquidationPayout string `json:"liquidation_payout,omitempty"`
}

func (s *Service) derivativePriceWorker() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-s.globalCtx.Done():
			return
		case <-ticker.C:
		}

		err := s.db.ForEachActiveSpecialMetaKey(s.globalCtx, func(key ed25519.PublicKey) error {
			_, _, _, becameLiquidated, err := s.refreshIncomingDerivativeMeta(s.globalCtx, key)
			if err != nil {
				if errors.Is(err, errNotIncomingDerivative) {
					return nil
				}
				log.Debug().Err(err).Msg("failed to refresh incoming derivative monitor")
				return nil
			}

			if !becameLiquidated {
				return nil
			}

			// Liquidation reached on our incoming derivative condition:
			// request execution to pull payout from the counterparty.
			if err = s.triggerIncomingDerivativeLiquidation(s.globalCtx, key); err != nil {
				log.Warn().Err(err).Msg("failed to trigger incoming derivative liquidation")
			}
			return nil
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Error().Err(err).Msg("failed to iterate active special metas index")
		}
	}
}

func (s *Service) refreshIncomingDerivativeMeta(ctx context.Context, key ed25519.PublicKey) (*db.ConditionalMeta, *conditionals.ConditionalResolvable, *derivativeMonitorState, bool, error) {
	meta, err := s.db.GetVirtualChannelMeta(ctx, key)
	if err != nil {
		return nil, nil, nil, false, err
	}
	if (meta.Status != db.ConditionalStateActive && meta.Status != db.ConditionalStatePending) || meta.Incoming == nil || meta.Incoming.Conditional == nil {
		return nil, nil, nil, false, errNotIncomingDerivative
	}

	condRaw, err := payments.CodeToConditional(ctx, meta.Incoming.Conditional, s)
	if err != nil {
		return nil, nil, nil, false, fmt.Errorf("failed to parse incoming derivative conditional: %w", err)
	}

	cond, ok := condRaw.(*conditionals.ConditionalResolvable)
	if !ok {
		return nil, nil, nil, false, errNotIncomingDerivative
	}
	if cond.PriceResolver == nil {
		return nil, nil, nil, false, fmt.Errorf("price resolver for asset %d is not configured", cond.Details.AssetID)
	}

	monitor, needsWrap, err := parseDerivativeMonitor(meta)
	if err != nil {
		return nil, nil, nil, false, err
	}

	now := time.Now().UTC().Unix()
	from := monitor.LastCheckedAt + 1
	createdAt := meta.CreatedAt.UTC().Unix()
	if from < createdAt {
		from = createdAt
	}

	changed := needsWrap
	becameLiquidated := false
	lastChecked := monitor.LastCheckedAt

	for sec := from; sec <= now; sec++ {
		select {
		case <-ctx.Done():
			return nil, nil, nil, false, ctx.Err()
		default:
		}

		price, err := cond.PriceResolver.GetPriceAt(ctx, sec)
		if err != nil {
			switch {
			case errors.Is(err, oracle.ErrTooNew):
				// We reached the freshest available slot.
				sec = now + 1
				continue
			case errors.Is(err, oracle.ErrNoData):
				// Resolver is still warming up.
				sec = now + 1
				continue
			case errors.Is(err, oracle.ErrTooOld):
				// Safety-first fallback: if we cannot reconstruct the full history anymore,
				// we treat the order as opened to prevent unsafe removal.
				if !monitor.EntryCrossed {
					monitor.EntryCrossed = true
					if monitor.EntryCrossedAt == 0 {
						monitor.EntryCrossedAt = sec
					}
					changed = true
				}
				lastChecked = now
				sec = now + 1
				continue
			case errors.Is(err, oracle.ErrUnavailable):
				// This second has no trade sample; still considered checked.
				lastChecked = sec
				continue
			default:
				return nil, nil, nil, false, fmt.Errorf("failed to get price at %d: %w", sec, err)
			}
		}

		priceChanged, liquidated := applyDerivativePriceSample(monitor, cond, sec, price)
		if priceChanged {
			changed = true
		}
		if liquidated {
			becameLiquidated = true
		}
		lastChecked = sec
	}

	if lastChecked > monitor.LastCheckedAt {
		monitor.LastCheckedAt = lastChecked
		changed = true
	}

	if changed {
		latestMeta, err := s.db.GetVirtualChannelMeta(ctx, key)
		if err != nil {
			return nil, nil, nil, false, err
		}
		latestMeta.SpecialDetails = derivativeMetaAny{
			Details: cond.Details,
			Monitor: monitor,
		}
		if err = s.db.UpdateVirtualChannelMeta(ctx, latestMeta); err != nil {
			return nil, nil, nil, false, err
		}
		meta = latestMeta
	}

	return meta, cond, monitor, becameLiquidated, nil
}

func (s *Service) triggerIncomingDerivativeLiquidation(ctx context.Context, key ed25519.PublicKey) error {
	meta, err := s.db.GetVirtualChannelMeta(ctx, key)
	if err != nil {
		return err
	}
	if meta.Status != db.ConditionalStateActive || meta.Incoming == nil || meta.Incoming.Conditional == nil {
		return nil
	}

	condRaw, err := payments.CodeToConditional(ctx, meta.Incoming.Conditional, s)
	if err != nil {
		return fmt.Errorf("failed to parse derivative conditional: %w", err)
	}
	cond, ok := condRaw.(*conditionals.ConditionalResolvable)
	if !ok {
		return nil
	}

	monitor, _, err := parseDerivativeMonitor(meta)
	if err != nil {
		return err
	}
	if !monitor.Liquidated {
		return nil
	}

	amount := parseBigIntString(monitor.LiquidationPayout)
	if amount == nil || amount.Sign() <= 0 {
		amount = new(big.Int).Set(cond.Amount)
	}
	if amount == nil || amount.Sign() <= 0 {
		return nil
	}

	if meta.LastKnownResolve == nil {
		at := monitor.LiquidatedAt
		if at == 0 {
			at = time.Now().UTC().Unix()
		}

		state, err := tlb.ToCell(conditionals.ResolvableState{
			Key:    cond.GetKey(),
			Amount: amount,
			At:     at,
		})
		if err != nil {
			return fmt.Errorf("failed to serialize derivative resolve state: %w", err)
		}

		if err = s.AddConditionalResolve(ctx, cond.GetKey(), state); err != nil && !errors.Is(err, payments.ErrNewerConditionalStateIsKnown) {
			return fmt.Errorf("failed to add derivative liquidation resolve: %w", err)
		}
	}

	if err = s.CloseConditional(ctx, cond.GetKey()); err != nil {
		return fmt.Errorf("failed to trigger derivative liquidation close: %w", err)
	}

	return nil
}

func (s *Service) ensureDerivativeRemovable(ctx context.Context, outgoingMeta *db.ConditionalMeta) error {
	if outgoingMeta == nil || outgoingMeta.Outgoing == nil || len(outgoingMeta.Outgoing.LinkedKey) == 0 {
		return fmt.Errorf("linked incoming derivative meta is required to validate remove")
	}

	checkCtx, cancel := context.WithTimeout(ctx, derivativeRemoveRefreshTimeout)
	defer cancel()

	_, _, monitor, _, err := s.refreshIncomingDerivativeMeta(checkCtx, outgoingMeta.Outgoing.LinkedKey)
	if err != nil {
		return fmt.Errorf("failed to refresh derivative monitor before remove: %w", err)
	}

	if monitor.EntryCrossed {
		return fmt.Errorf("derivative order already opened, remove is denied")
	}

	return nil
}

func parseDerivativeMonitor(meta *db.ConditionalMeta) (*derivativeMonitorState, bool, error) {
	baseLastCheck := meta.CreatedAt.UTC().Unix() - 1
	if baseLastCheck < 0 {
		baseLastCheck = 0
	}

	if meta.SpecialDetails == nil {
		return &derivativeMonitorState{LastCheckedAt: baseLastCheck}, true, nil
	}

	var packed derivativeMetaAny
	if err := recodeJSON(meta.SpecialDetails, &packed); err != nil {
		// Keep the worker resilient if the metadata shape changed in older records.
		return &derivativeMonitorState{LastCheckedAt: baseLastCheck}, true, nil
	}

	if packed.Monitor == nil {
		return &derivativeMonitorState{LastCheckedAt: baseLastCheck}, true, nil
	}
	if packed.Monitor.LastCheckedAt < baseLastCheck {
		packed.Monitor.LastCheckedAt = baseLastCheck
		return packed.Monitor, true, nil
	}

	return packed.Monitor, false, nil
}

func applyDerivativePriceSample(m *derivativeMonitorState, cond *conditionals.ConditionalResolvable, at int64, price *big.Int) (changed bool, becameLiquidated bool) {
	if m == nil || cond == nil || price == nil {
		return false, false
	}

	if min := parseBigIntString(m.MinPrice); min == nil || price.Cmp(min) < 0 {
		m.MinPrice = price.String()
		changed = true
	}
	if max := parseBigIntString(m.MaxPrice); max == nil || price.Cmp(max) > 0 {
		m.MaxPrice = price.String()
		changed = true
	}

	entry := cond.Details.EntryPrice.Nano()
	if !m.EntryCrossed && entry != nil && entry.Sign() > 0 {
		min := parseBigIntString(m.MinPrice)
		max := parseBigIntString(m.MaxPrice)
		if min != nil && max != nil && min.Cmp(entry) <= 0 && max.Cmp(entry) >= 0 {
			m.EntryCrossed = true
			if m.EntryCrossedAt == 0 {
				m.EntryCrossedAt = at
			}
			changed = true
		}
	}

	if m.EntryCrossed && !m.Liquidated && cond.Amount != nil && cond.Amount.Sign() > 0 {
		roi := calcDerivativeROI(cond, price)
		if roi.Cmp(cond.Amount) >= 0 {
			m.Liquidated = true
			m.LiquidatedAt = at
			m.LiquidatedPrice = price.String()
			m.LiquidationPayout = cond.Amount.String()
			changed = true
			becameLiquidated = true
		}
	}

	return changed, becameLiquidated
}

func calcDerivativeROI(cond *conditionals.ConditionalResolvable, price *big.Int) *big.Int {
	if cond == nil || price == nil || cond.Amount == nil || cond.Amount.Sign() <= 0 || cond.Details.Leverage == 0 {
		return big.NewInt(0)
	}

	entry := cond.Details.EntryPrice.Nano()
	if entry == nil || entry.Sign() <= 0 {
		return big.NewInt(0)
	}

	var delta *big.Int
	if cond.Details.IsLong {
		delta = new(big.Int).Sub(price, entry)
	} else {
		delta = new(big.Int).Sub(entry, price)
	}
	if delta.Sign() <= 0 {
		return big.NewInt(0)
	}

	roi := new(big.Int).Mul(delta, cond.Amount)
	roi.Mul(roi, big.NewInt(int64(cond.Details.Leverage)))
	roi.Div(roi, entry)
	if roi.Sign() < 0 {
		return big.NewInt(0)
	}
	return roi
}

func parseBigIntString(v string) *big.Int {
	if v == "" {
		return nil
	}
	n, ok := new(big.Int).SetString(v, 10)
	if !ok {
		return nil
	}
	return n
}

func recodeJSON(src any, dst any) error {
	raw, err := json.Marshal(src)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, dst)
}
