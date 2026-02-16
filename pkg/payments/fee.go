package payments

import (
	"math"
	"math/big"
)

// CalcPercentFeeCeil returns ceil(amount * percent / 100).
func CalcPercentFeeCeil(amount *big.Int, percent float64) *big.Int {
	if amount == nil || amount.Sign() <= 0 {
		return big.NewInt(0)
	}
	if percent <= 0 || math.IsNaN(percent) || math.IsInf(percent, 0) {
		return big.NewInt(0)
	}

	percentRat := new(big.Rat).SetFloat64(percent)
	if percentRat == nil || percentRat.Sign() <= 0 {
		return big.NewInt(0)
	}

	feeRat := new(big.Rat).Mul(new(big.Rat).SetInt(amount), percentRat)
	feeRat.Quo(feeRat, big.NewRat(100, 1))

	num := feeRat.Num()
	den := feeRat.Denom()
	if num.Sign() <= 0 {
		return big.NewInt(0)
	}

	// ceil(num/den) = (num + den - 1) / den
	numAdd := new(big.Int).Add(num, new(big.Int).Sub(den, big.NewInt(1)))
	return new(big.Int).Quo(numAdd, den)
}
