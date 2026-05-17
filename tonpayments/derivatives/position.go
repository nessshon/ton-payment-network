package derivatives

import "math"

// ComputePnLPercent computes PnL% for a position.
// For a long: ((curr/entry)-1)*leverage*100
// For a short: ((entry/curr)-1)*leverage*100
func ComputePnLPercent(entry, current float64, leverage int, isLong bool) float64 {
	if leverage <= 0 || entry <= 0 || current <= 0 {
		return 0
	}
	l := float64(leverage)
	var pnl float64
	if isLong {
		pnl = ((current/entry)-1.0)*l*100.0
	} else {
		pnl = ((entry/current)-1.0)*l*100.0
	}
	// clamp to sane bounds
	if pnl > 10000 {
		pnl = 10000
	}
	if pnl < -10000 {
		pnl = -10000
	}
	if math.IsNaN(pnl) || math.IsInf(pnl, 0) {
		return 0
	}
	return pnl
}

// ComputeLiquidationPrice computes approximate isolated liquidation price.
// Long: entry * (1 - 1/leverage)
// Short: entry * (1 + 1/leverage)
func ComputeLiquidationPrice(entry float64, leverage int, isLong bool) float64 {
	if leverage <= 0 || entry <= 0 {
		return 0
	}
	l := float64(leverage)
	if isLong {
		return entry * (1.0 - 1.0/l)
	}
	return entry * (1.0 + 1.0/l)
}
