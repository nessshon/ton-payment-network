package tonpayments

import (
	"crypto/ed25519"
	"math/big"
	"testing"
	"time"

	"github.com/xssnick/ton-payment-network/pkg/payments/actions"
	"github.com/xssnick/ton-payment-network/pkg/payments/conditionals"
	"github.com/xssnick/ton-payment-network/tonpayments/db"
)

func TestApplyDerivativePriceSample_Long(t *testing.T) {
	cond := &conditionals.ConditionalResolvable{
		Amount: big.NewInt(1000),
		Details: conditionals.ConditionalResolvableDetails{
			IsLong:     true,
			Leverage:   10,
			EntryPrice: actions.Coins{Val: big.NewInt(100)},
		},
	}
	monitor := &derivativeMonitorState{}

	if changed, liq := applyDerivativePriceSample(monitor, cond, 1, big.NewInt(95)); !changed || liq {
		t.Fatalf("expected changed=true, liquidated=false, got changed=%v liquidated=%v", changed, liq)
	}
	if monitor.EntryCrossed {
		t.Fatalf("entry should not be crossed yet")
	}

	if changed, liq := applyDerivativePriceSample(monitor, cond, 2, big.NewInt(100)); !changed || liq {
		t.Fatalf("expected changed=true, liquidated=false at entry, got changed=%v liquidated=%v", changed, liq)
	}
	if !monitor.EntryCrossed {
		t.Fatalf("entry must be crossed")
	}

	if changed, liq := applyDerivativePriceSample(monitor, cond, 3, big.NewInt(111)); !changed || !liq {
		t.Fatalf("expected liquidation on 111, got changed=%v liquidated=%v", changed, liq)
	}
	if !monitor.Liquidated {
		t.Fatalf("monitor must be liquidated")
	}
}

func TestApplyDerivativePriceSample_Short(t *testing.T) {
	cond := &conditionals.ConditionalResolvable{
		Amount: big.NewInt(2000),
		Details: conditionals.ConditionalResolvableDetails{
			IsLong:     false,
			Leverage:   20,
			EntryPrice: actions.Coins{Val: big.NewInt(100)},
		},
	}
	monitor := &derivativeMonitorState{}

	applyDerivativePriceSample(monitor, cond, 1, big.NewInt(105))
	applyDerivativePriceSample(monitor, cond, 2, big.NewInt(100))
	if !monitor.EntryCrossed {
		t.Fatalf("entry must be crossed for short")
	}

	if _, liq := applyDerivativePriceSample(monitor, cond, 3, big.NewInt(94)); !liq {
		t.Fatalf("expected liquidation for short")
	}
}

func TestApplyDerivativePriceSample_EntryCrossedImmediatelyForMarketableLong(t *testing.T) {
	cond := &conditionals.ConditionalResolvable{
		Amount: big.NewInt(1000),
		Details: conditionals.ConditionalResolvableDetails{
			IsLong:     true,
			Leverage:   10,
			EntryPrice: actions.Coins{Val: big.NewInt(100)},
		},
	}
	monitor := &derivativeMonitorState{}

	changed, liq := applyDerivativePriceSample(monitor, cond, 1, big.NewInt(105))
	if !changed || liq {
		t.Fatalf("expected changed=true, liquidated=false, got changed=%v liquidated=%v", changed, liq)
	}
	if !monitor.EntryCrossed {
		t.Fatalf("entry must be crossed immediately for price above entry")
	}
	if monitor.EntryCrossedAt != 1 {
		t.Fatalf("unexpected entry crossed at: %d", monitor.EntryCrossedAt)
	}
}

func TestApplyDerivativePriceSample_EntryCrossedImmediatelyForMarketableShort(t *testing.T) {
	cond := &conditionals.ConditionalResolvable{
		Amount: big.NewInt(1000),
		Details: conditionals.ConditionalResolvableDetails{
			IsLong:     false,
			Leverage:   10,
			EntryPrice: actions.Coins{Val: big.NewInt(100)},
		},
	}
	monitor := &derivativeMonitorState{}

	changed, liq := applyDerivativePriceSample(monitor, cond, 1, big.NewInt(95))
	if !changed || liq {
		t.Fatalf("expected changed=true, liquidated=false, got changed=%v liquidated=%v", changed, liq)
	}
	if !monitor.EntryCrossed {
		t.Fatalf("entry must be crossed immediately for price below entry")
	}
	if monitor.EntryCrossedAt != 1 {
		t.Fatalf("unexpected entry crossed at: %d", monitor.EntryCrossedAt)
	}
}

func TestParseDerivativeMonitor_LegacyAny(t *testing.T) {
	meta := &db.ConditionalMeta{
		CreatedAt: time.Unix(1000, 0).UTC(),
		SpecialDetails: conditionals.ConditionalResolvableDetails{
			AssetID:  1,
			Leverage: 10,
		},
	}

	monitor, needsWrap, err := parseDerivativeMonitor(meta)
	if err != nil {
		t.Fatalf("parse monitor failed: %v", err)
	}
	if !needsWrap {
		t.Fatalf("legacy Any must require wrap")
	}
	if monitor.LastCheckedAt != 999 {
		t.Fatalf("unexpected last checked: %d", monitor.LastCheckedAt)
	}
}

func TestDerivativeIncomingKeyForRemove_FromIncomingMeta(t *testing.T) {
	key := make([]byte, ed25519.PublicKeySize)
	for i := range key {
		key[i] = byte(i + 1)
	}

	meta := &db.ConditionalMeta{
		Key:      key,
		Incoming: &db.ConditionalMetaSide{},
	}

	got, ok := derivativeIncomingKeyForRemove(meta)
	if !ok {
		t.Fatalf("expected key to be resolved")
	}
	if string(got) != string(key) {
		t.Fatalf("unexpected key resolved")
	}
}

func TestDerivativeIncomingKeyForRemove_FromOutgoingLinked(t *testing.T) {
	linked := make([]byte, ed25519.PublicKeySize)
	for i := range linked {
		linked[i] = byte(255 - i)
	}

	meta := &db.ConditionalMeta{
		Outgoing: &db.ConditionalMetaSide{
			LinkedKey: linked,
		},
	}

	got, ok := derivativeIncomingKeyForRemove(meta)
	if !ok {
		t.Fatalf("expected linked key to be resolved")
	}
	if string(got) != string(linked) {
		t.Fatalf("unexpected linked key resolved")
	}
}

func TestDerivativeIncomingKeyForRemove_Missing(t *testing.T) {
	meta := &db.ConditionalMeta{}
	if _, ok := derivativeIncomingKeyForRemove(meta); ok {
		t.Fatalf("expected key resolution to fail")
	}
}

func TestDerivativeRemoveTerminalMeta(t *testing.T) {
	trueStatuses := []db.ConditionalStatus{
		db.ConditionalStateWantClose,
		db.ConditionalStateClosed,
		db.ConditionalStateWantRemove,
		db.ConditionalStateRemoved,
	}
	for _, status := range trueStatuses {
		if !derivativeRemoveTerminalMeta(&db.ConditionalMeta{Status: status}) {
			t.Fatalf("status %d must be treated as terminal for remove", status)
		}
	}

	falseStatuses := []db.ConditionalStatus{
		db.ConditionalStateActive,
		db.ConditionalStatePending,
	}
	for _, status := range falseStatuses {
		if derivativeRemoveTerminalMeta(&db.ConditionalMeta{Status: status}) {
			t.Fatalf("status %d must not be treated as terminal for remove", status)
		}
	}
}

func TestDerivativeOutgoingKeyForLiquidation_FromIncomingLinked(t *testing.T) {
	linked := make([]byte, ed25519.PublicKeySize)
	for i := range linked {
		linked[i] = byte(100 + i)
	}

	meta := &db.ConditionalMeta{
		Incoming: &db.ConditionalMetaSide{
			LinkedKey: linked,
		},
	}

	got, ok := derivativeOutgoingKeyForLiquidation(meta)
	if !ok {
		t.Fatalf("expected outgoing linked key to be resolved")
	}
	if string(got) != string(linked) {
		t.Fatalf("unexpected outgoing linked key resolved")
	}
}

func TestDerivativeOutgoingKeyForLiquidation_Missing(t *testing.T) {
	meta := &db.ConditionalMeta{}
	if _, ok := derivativeOutgoingKeyForLiquidation(meta); ok {
		t.Fatalf("expected outgoing linked key resolution to fail")
	}
}

func TestDerivativeOrderOpenedFromMonitor_NormalizesLinkedSide(t *testing.T) {
	base := make([]byte, ed25519.PublicKeySize)
	for i := range base {
		base[i] = byte(i + 1)
	}
	linked := derivativeLinkedKey(base)

	meta := &db.ConditionalMeta{
		Key: linked,
		Incoming: &db.ConditionalMetaSide{
			LinkedKey: base,
		},
	}
	cond := &conditionals.ConditionalResolvable{
		Key: linked,
		Details: conditionals.ConditionalResolvableDetails{
			// Linked side is opposite of canonical side.
			IsLong:     true,
			EntryPrice: actions.Coins{Val: big.NewInt(100)},
		},
	}
	monitor := &derivativeMonitorState{
		MinPrice:     "95",
		MaxPrice:     "95",
		EntryCrossed: true, // Legacy marker may still be set with old logic.
	}

	if derivativeCanonicalIsLong(meta, cond) {
		t.Fatalf("linked side must normalize to canonical short")
	}

	if derivativeOrderOpenedFromMonitor(meta, cond, monitor) {
		t.Fatalf("position should stay pending when price is below short entry")
	}
}

func TestDerivativeOrderOpenedFromMonitor_MarketableShortOpens(t *testing.T) {
	base := make([]byte, ed25519.PublicKeySize)
	for i := range base {
		base[i] = byte(200 + i)
	}
	linked := derivativeLinkedKey(base)

	meta := &db.ConditionalMeta{
		Key: linked,
		Incoming: &db.ConditionalMetaSide{
			LinkedKey: base,
		},
	}
	cond := &conditionals.ConditionalResolvable{
		Key: linked,
		Details: conditionals.ConditionalResolvableDetails{
			IsLong:     true, // linked side, canonical is short
			EntryPrice: actions.Coins{Val: big.NewInt(100)},
		},
	}
	monitor := &derivativeMonitorState{
		MinPrice: "105",
		MaxPrice: "105",
	}

	if !derivativeOrderOpenedFromMonitor(meta, cond, monitor) {
		t.Fatalf("marketable short must be considered opened")
	}
}

func TestDerivativeOrderOpenedFromMonitor_HistoryTooOldIsConservative(t *testing.T) {
	meta := &db.ConditionalMeta{}
	cond := &conditionals.ConditionalResolvable{
		Details: conditionals.ConditionalResolvableDetails{
			EntryPrice: actions.Coins{Val: big.NewInt(100)},
		},
	}
	monitor := &derivativeMonitorState{HistoryTooOld: true}

	if !derivativeOrderOpenedFromMonitor(meta, cond, monitor) {
		t.Fatalf("history-too-old derivatives must stay conservatively opened")
	}
}

func TestDerivativeCanRemoveWithoutPriceHistory_Unhedged(t *testing.T) {
	meta := &db.ConditionalMeta{
		SpecialDetails: map[string]any{
			"details": map[string]any{
				"AssetID": 1,
			},
			"hedged": false,
		},
	}

	if !derivativeCanRemoveWithoutPriceHistory(meta, &derivativeMonitorState{HistoryTooOld: true}) {
		t.Fatalf("unhedged derivative should be removable when price history is too old")
	}
}

func TestDerivativeCanRemoveWithoutPriceHistory_Hedged(t *testing.T) {
	meta := &db.ConditionalMeta{
		SpecialDetails: map[string]any{
			"details": map[string]any{
				"AssetID": 1,
			},
			"hedged": true,
		},
	}

	if derivativeCanRemoveWithoutPriceHistory(meta, &derivativeMonitorState{HistoryTooOld: true}) {
		t.Fatalf("hedged derivative must stay protected when price history is too old")
	}
}
