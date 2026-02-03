package derivatives

import "testing"

func TestComputePnLPercent(t *testing.T) {
	p := ComputePnLPercent(100, 110, 10, true)
	if int(p+0.5) != 100 { // ~100%
		t.Fatalf("expected ~100%%, got %f", p)
	}

	p = ComputePnLPercent(100, 90, 10, true)
	if int(p-0.5) != -100 { // ~-100%
		t.Fatalf("expected ~-100%%, got %f", p)
	}

	p = ComputePnLPercent(100, 90, 5, false) // short profit
	if int(p+0.5) != 55 { // (100/90-1)*5*100 = 55.5
		t.Fatalf("expected ~55%%, got %f", p)
	}
}

func TestComputeLiquidationPrice(t *testing.T) {
	liq := ComputeLiquidationPrice(100, 10, true)
	if liq < 89.9 || liq > 90.1 {
		t.Fatalf("expected ~90, got %f", liq)
	}

	liq = ComputeLiquidationPrice(100, 5, false)
	if liq < 119.9 || liq > 120.1 {
		t.Fatalf("expected ~120, got %f", liq)
	}
}
