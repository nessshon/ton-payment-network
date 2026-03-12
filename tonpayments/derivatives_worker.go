package tonpayments

import (
	"bytes"
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

			// Liquidation reached on incoming derivative monitor.
			// Beneficiary side must trigger liquidation of the linked outgoing side.
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
	if _, ok := condRaw.(*conditionals.ConditionalResolvable); !ok {
		return nil
	}

	monitor, _, err := parseDerivativeMonitor(meta)
	if err != nil {
		return err
	}
	if !monitor.Liquidated {
		return nil
	}

	// Liquidation is detected by the incoming monitor, but settle must be posted
	// on the linked outgoing derivative (losing side). Incoming side gets zero
	// settle via CloseDerivative propagation.
	outgoingKey, ok := derivativeOutgoingKeyForLiquidation(meta)
	if !ok {
		return fmt.Errorf("incoming derivative has no linked outgoing key")
	}

	outgoingMeta, err := s.db.GetVirtualChannelMeta(ctx, outgoingKey)
	if err != nil {
		return fmt.Errorf("failed to load linked outgoing derivative meta: %w", err)
	}
	if outgoingMeta.Status != db.ConditionalStateActive || outgoingMeta.Outgoing == nil || outgoingMeta.Outgoing.Conditional == nil {
		return nil
	}

	outRaw, err := payments.CodeToConditional(ctx, outgoingMeta.Outgoing.Conditional, s)
	if err != nil {
		return fmt.Errorf("failed to parse linked outgoing derivative conditional: %w", err)
	}
	outCond, ok := outRaw.(*conditionals.ConditionalResolvable)
	if !ok {
		return nil
	}

	amount := parseBigIntString(monitor.LiquidationPayout)
	if amount == nil || amount.Sign() <= 0 {
		if outCond.Amount != nil {
			amount = new(big.Int).Set(outCond.Amount)
		}
	}
	if amount == nil || amount.Sign() <= 0 {
		return nil
	}
	if outCond.Amount != nil && outCond.Amount.Sign() > 0 && amount.Cmp(outCond.Amount) > 0 {
		amount = new(big.Int).Set(outCond.Amount)
	}

	if outgoingMeta.LastKnownResolve == nil {
		at := monitor.LiquidatedAt
		if at == 0 {
			at = time.Now().UTC().Unix()
		}

		state, err := tlb.ToCell(conditionals.ResolvableState{
			Key:    outCond.GetKey(),
			Amount: amount,
			At:     at,
		})
		if err != nil {
			return fmt.Errorf("failed to serialize derivative resolve state: %w", err)
		}

		if err = s.AddConditionalResolve(ctx, outCond.GetKey(), state); err != nil && !errors.Is(err, payments.ErrNewerConditionalStateIsKnown) {
			return fmt.Errorf("failed to add derivative liquidation resolve: %w", err)
		}
	}

	if err = s.CloseDerivative(ctx, outCond.GetKey()); err != nil {
		return fmt.Errorf("failed to trigger derivative liquidation close: %w", err)
	}

	return nil
}

func (s *Service) ensureDerivativeRemovable(ctx context.Context, outgoingMeta *db.ConditionalMeta) error {
	incomingKey, ok := derivativeIncomingKeyForRemove(outgoingMeta)
	if !ok {
		return fmt.Errorf("incoming derivative key is required to validate remove")
	}

	checkCtx, cancel := context.WithTimeout(ctx, derivativeRemoveRefreshTimeout)
	defer cancel()

	meta, cond, monitor, _, err := s.refreshIncomingDerivativeMeta(checkCtx, incomingKey)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) || errors.Is(err, errNotIncomingDerivative) {
			if derivativeRemoveTerminalMeta(outgoingMeta) {
				return nil
			}

			incomingMeta, loadErr := s.db.GetVirtualChannelMeta(ctx, incomingKey)
			switch {
			case errors.Is(loadErr, db.ErrNotFound):
				return nil
			case loadErr == nil && derivativeRemoveTerminalMeta(incomingMeta):
				return nil
			}
		}

		return fmt.Errorf("failed to refresh derivative monitor before remove: %w", err)
	}

	if derivativeOrderOpenedFromMonitor(meta, cond, monitor) {
		return fmt.Errorf("derivative order already opened, remove is denied")
	}

	return nil
}

func derivativeRemoveTerminalMeta(meta *db.ConditionalMeta) bool {
	if meta == nil {
		return false
	}

	switch meta.Status {
	case db.ConditionalStateWantClose, db.ConditionalStateClosed, db.ConditionalStateWantRemove, db.ConditionalStateRemoved:
		return true
	default:
		return false
	}
}

func derivativeIncomingKeyForRemove(meta *db.ConditionalMeta) (ed25519.PublicKey, bool) {
	if meta == nil {
		return nil, false
	}

	// Incoming derivative meta keeps monitor directly under its own key.
	if meta.Incoming != nil && len(meta.Key) == ed25519.PublicKeySize {
		return append(ed25519.PublicKey(nil), meta.Key...), true
	}

	// Outgoing derivative meta references incoming side via linked key.
	if meta.Outgoing != nil && len(meta.Outgoing.LinkedKey) == ed25519.PublicKeySize {
		return append(ed25519.PublicKey(nil), meta.Outgoing.LinkedKey...), true
	}

	return nil, false
}

func derivativeOutgoingKeyForLiquidation(meta *db.ConditionalMeta) (ed25519.PublicKey, bool) {
	if meta == nil || meta.Incoming == nil || len(meta.Incoming.LinkedKey) != ed25519.PublicKeySize {
		return nil, false
	}
	return append(ed25519.PublicKey(nil), meta.Incoming.LinkedKey...), true
}

func derivativeOrderOpenedFromMonitor(meta *db.ConditionalMeta, cond *conditionals.ConditionalResolvable, monitor *derivativeMonitorState) bool {
	if monitor == nil || cond == nil {
		return false
	}

	entry := cond.Details.EntryPrice.Nano()
	if entry == nil || entry.Sign() <= 0 {
		// Malformed records should keep legacy conservative behavior.
		return monitor.EntryCrossed
	}

	isOrderLong := derivativeCanonicalIsLong(meta, cond)
	min := parseBigIntString(monitor.MinPrice)
	max := parseBigIntString(monitor.MaxPrice)

	if isOrderLong {
		if min != nil {
			return min.Cmp(entry) <= 0
		}
	} else {
		if max != nil {
			return max.Cmp(entry) >= 0
		}
	}

	// If price history is incomplete (e.g. too old), keep conservative fallback.
	return monitor.EntryCrossed
}

func derivativeCanonicalIsLong(meta *db.ConditionalMeta, cond *conditionals.ConditionalResolvable) bool {
	if cond == nil {
		return false
	}

	isLong := cond.Details.IsLong
	if meta == nil {
		return isLong
	}

	var linked []byte
	if meta.Incoming != nil && len(meta.Incoming.LinkedKey) == ed25519.PublicKeySize {
		linked = meta.Incoming.LinkedKey
	} else if meta.Outgoing != nil && len(meta.Outgoing.LinkedKey) == ed25519.PublicKeySize {
		linked = meta.Outgoing.LinkedKey
	} else {
		return isLong
	}

	key := cond.GetKey()
	if len(key) != ed25519.PublicKeySize {
		return isLong
	}

	// Linked side keeps inverted IsLong; normalize back to canonical order side.
	if bytes.Equal(key, derivativeLinkedKey(linked)) {
		return !isLong
	}

	return isLong
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
		crossed := false
		if cond.Details.IsLong {
			// Incoming long corresponds to our short order:
			// it opens once price reaches entry or higher.
			crossed = max != nil && max.Cmp(entry) >= 0
		} else {
			// Incoming short corresponds to our long order:
			// it opens once price reaches entry or lower.
			crossed = min != nil && min.Cmp(entry) <= 0
		}
		if crossed {
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
