package tonpayments

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"math/big"
	"testing"

	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/pkg/payments/actions"
	"github.com/xssnick/ton-payment-network/pkg/payments/conditionals"
	"github.com/xssnick/ton-payment-network/pkg/payments/conditionals/oracle"
	"github.com/xssnick/tonutils-go/tlb"
)

type testLinkedPriceResolver struct {
	price       *big.Int
	getPriceErr error
	called      int
}

func (r *testLinkedPriceResolver) GetPriceAt(_ context.Context, _ int64) (*big.Int, error) {
	r.called++
	if r.getPriceErr != nil {
		return nil, r.getPriceErr
	}
	return new(big.Int).Set(r.price), nil
}

func (r *testLinkedPriceResolver) GetLastPrice() (int64, *big.Int, error) {
	if r.price == nil {
		return 0, nil, r.getPriceErr
	}
	return 0, new(big.Int).Set(r.price), nil
}

func (r *testLinkedPriceResolver) GetPricesSince(int64) []oracle.RangePrice {
	return nil
}

func TestComputeLinkedDerivativeSettle_LossProducesSettle(t *testing.T) {
	key := sha256.Sum256([]byte("linked-loss"))
	at := int64(1700000000)

	resolve, err := tlb.ToCell(conditionals.ResolvableState{
		Key:    key[:],
		Amount: big.NewInt(0),
		At:     at,
	})
	if err != nil {
		t.Fatalf("failed to build primary resolve: %v", err)
	}

	pr := &testLinkedPriceResolver{price: big.NewInt(90)}
	linked := &conditionals.ConditionalResolvable{
		Key:    ed25519.PublicKey(key[:]),
		Amount: big.NewInt(1000),
		Details: conditionals.ConditionalResolvableDetails{
			IsLong:     true,
			Leverage:   10,
			EntryPrice: actions.Coins{Val: big.NewInt(100)},
		},
		PriceResolver: pr,
	}

	got, err := computeLinkedDerivativeSettle(context.Background(), resolve, linked)
	if err != nil {
		t.Fatalf("compute linked settle failed: %v", err)
	}
	if got == nil {
		t.Fatalf("expected linked settle cell for loss case")
	}

	var st conditionals.ResolvableState
	if err = payments.LoadState(&st, got); err != nil {
		t.Fatalf("failed to parse linked settle: %v", err)
	}
	if st.At != at {
		t.Fatalf("unexpected settle time: got %d want %d", st.At, at)
	}
	if string(st.Key) != string(key[:]) {
		t.Fatalf("unexpected settle key")
	}
	if st.Amount.Cmp(big.NewInt(1000)) != 0 {
		t.Fatalf("unexpected settle amount: got %s want %s", st.Amount.String(), "1000")
	}
}

func TestComputeLinkedDerivativeSettle_ProfitSkipsSettle(t *testing.T) {
	key := sha256.Sum256([]byte("linked-profit"))

	resolve, err := tlb.ToCell(conditionals.ResolvableState{
		Key:    key[:],
		Amount: big.NewInt(0),
		At:     1700000001,
	})
	if err != nil {
		t.Fatalf("failed to build primary resolve: %v", err)
	}

	linked := &conditionals.ConditionalResolvable{
		Key:    ed25519.PublicKey(key[:]),
		Amount: big.NewInt(1000),
		Details: conditionals.ConditionalResolvableDetails{
			IsLong:     true,
			Leverage:   10,
			EntryPrice: actions.Coins{Val: big.NewInt(100)},
		},
		PriceResolver: &testLinkedPriceResolver{price: big.NewInt(110)},
	}

	got, err := computeLinkedDerivativeSettle(context.Background(), resolve, linked)
	if err != nil {
		t.Fatalf("compute linked settle failed: %v", err)
	}
	if got != nil {
		t.Fatalf("expected no linked settle for profit case")
	}
}

func TestComputeLinkedDerivativeSettle_PrimaryAlreadySettledSkipsPriceLookup(t *testing.T) {
	key := sha256.Sum256([]byte("primary-settle"))

	resolve, err := tlb.ToCell(conditionals.ResolvableState{
		Key:    key[:],
		Amount: big.NewInt(1),
		At:     1700000002,
	})
	if err != nil {
		t.Fatalf("failed to build primary resolve: %v", err)
	}

	pr := &testLinkedPriceResolver{getPriceErr: errors.New("must not be called")}
	linked := &conditionals.ConditionalResolvable{
		Key:    ed25519.PublicKey(key[:]),
		Amount: big.NewInt(1000),
		Details: conditionals.ConditionalResolvableDetails{
			IsLong:     true,
			Leverage:   10,
			EntryPrice: actions.Coins{Val: big.NewInt(100)},
		},
		PriceResolver: pr,
	}

	got, err := computeLinkedDerivativeSettle(context.Background(), resolve, linked)
	if err != nil {
		t.Fatalf("compute linked settle failed: %v", err)
	}
	if got != nil {
		t.Fatalf("expected no linked settle when primary settle already positive")
	}
	if pr.called != 0 {
		t.Fatalf("price resolver should not be called when primary settle is already positive")
	}
}
