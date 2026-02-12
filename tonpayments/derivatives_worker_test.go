package tonpayments

import (
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
