package payments

import (
	"math/big"
	"testing"
)

func TestCalcPercentFeeCeil(t *testing.T) {
	if got := CalcPercentFeeCeil(big.NewInt(1_000_000_000), 1); got.Cmp(big.NewInt(10_000_000)) != 0 {
		t.Fatalf("unexpected fee: got %s, want %s", got.String(), "10000000")
	}

	if got := CalcPercentFeeCeil(big.NewInt(100), 0.5); got.Cmp(big.NewInt(1)) != 0 {
		t.Fatalf("unexpected rounded fee: got %s, want %s", got.String(), "1")
	}

	if got := CalcPercentFeeCeil(big.NewInt(0), 1); got.Sign() != 0 {
		t.Fatalf("unexpected fee for zero amount: %s", got.String())
	}
}
