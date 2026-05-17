package conditionals

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/pkg/payments/actions"
	"github.com/xssnick/ton-payment-network/pkg/payments/conditionals/oracle"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
)

type stepBackResolver struct {
	cutoff int64
	price  *big.Int
}

func (r *stepBackResolver) GetPriceAt(_ context.Context, at int64) (*big.Int, error) {
	if at > r.cutoff {
		return nil, oracle.ErrTooNew
	}
	return new(big.Int).Set(r.price), nil
}

func (r *stepBackResolver) GetLastPrice() (int64, *big.Int, error) {
	return r.cutoff, new(big.Int).Set(r.price), nil
}

func (r *stepBackResolver) GetPricesSince(_ int64) []oracle.RangePrice { return nil }

type latestOnlyResolver struct {
	price *big.Int
}

func (r *latestOnlyResolver) GetPriceAt(_ context.Context, _ int64) (*big.Int, error) {
	return nil, oracle.ErrUnavailable
}

func (r *latestOnlyResolver) GetLastPrice() (int64, *big.Int, error) {
	return time.Now().Unix() - 5, new(big.Int).Set(r.price), nil
}

func (r *latestOnlyResolver) GetPricesSince(_ int64) []oracle.RangePrice { return nil }

type hardErrorResolver struct{}

func (r *hardErrorResolver) GetPriceAt(_ context.Context, _ int64) (*big.Int, error) {
	return nil, errors.New("boom")
}

func (r *hardErrorResolver) GetLastPrice() (int64, *big.Int, error) {
	return 0, nil, errors.New("boom")
}

func (r *hardErrorResolver) GetPricesSince(_ int64) []oracle.RangePrice { return nil }

func testResolvableActionWithFeePercent(percent float64) *actions.ActionSendTonInsured {
	return &actions.ActionSendTonInsured{
		Coin: &payments.CoinConfig{
			Symbol:   "TON",
			Decimals: 9,
			VirtualTunnelConfig: payments.VirtualConfig{
				DerivativeFeePercent: percent,
			},
		},
	}
}

func TestGetBestEffortCurrentPrice_UsesPreviousSecondOnTooNew(t *testing.T) {
	want := big.NewInt(123)
	cond := &ConditionalResolvable{
		PriceResolver: &stepBackResolver{
			cutoff: time.Now().Unix() - 1,
			price:  want,
		},
	}

	got, err := cond.getBestEffortCurrentPrice()
	if err != nil {
		t.Fatalf("expected price, got error: %v", err)
	}
	if got.Cmp(want) != 0 {
		t.Fatalf("unexpected price: got %s want %s", got, want)
	}
}

func TestGetBestEffortCurrentPrice_FallsBackToLastPrice(t *testing.T) {
	want := big.NewInt(777)
	cond := &ConditionalResolvable{PriceResolver: &latestOnlyResolver{price: want}}

	got, err := cond.getBestEffortCurrentPrice()
	if err != nil {
		t.Fatalf("expected fallback price, got error: %v", err)
	}
	if got.Cmp(want) != 0 {
		t.Fatalf("unexpected fallback price: got %s want %s", got, want)
	}
}

func TestGetBestEffortCurrentPrice_StopsOnHardError(t *testing.T) {
	cond := &ConditionalResolvable{PriceResolver: &hardErrorResolver{}}

	_, err := cond.getBestEffortCurrentPrice()
	if err == nil {
		t.Fatal("expected error")
	}
	if err.Error() != "boom" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateState_NegativeROI_AcceptsLossAmount(t *testing.T) {
	now := time.Now().Unix()
	price := big.NewInt(95_000_000_000)  // 95 (9 decimals)
	entry := big.NewInt(100_000_000_000) // 100

	cond := &ConditionalResolvable{
		Key:    make([]byte, 32),
		Amount: big.NewInt(1_000_000_000), // 1 TON collateral
		Details: ConditionalResolvableDetails{
			IsLong:     true,
			Leverage:   10,
			EntryPrice: actions.Coins{Val: new(big.Int).Set(entry)},
		},
		PriceResolver: &stepBackResolver{cutoff: now, price: price},
	}

	// Expected ROI: (95 - 100) * 1e9 * 10 / 100 = -500_000_000 (negative, we're losing)
	// Settle amount should be abs(ROI) = 500_000_000
	settleAmount := big.NewInt(500_000_000)

	state := ResolvableState{
		Key:    make([]byte, 32),
		Amount: settleAmount,
		At:     now,
	}
	stateCell, err := tlb.ToCell(state)
	if err != nil {
		t.Fatalf("failed to serialize state: %v", err)
	}

	if err := cond.ValidateState(context.Background(), nil, stateCell); err != nil {
		t.Fatalf("ValidateState should accept loss amount, got: %v", err)
	}

	// But an amount exceeding abs(ROI) should still be rejected
	overState := ResolvableState{
		Key:    make([]byte, 32),
		Amount: big.NewInt(500_000_002), // > 1 tolerance
		At:     now,
	}
	overCell, _ := tlb.ToCell(overState)
	if err := cond.ValidateState(context.Background(), nil, overCell); err == nil {
		t.Fatal("ValidateState should reject amount exceeding abs(ROI)")
	}
}

func TestValidateState_AcceptsZeroOnWinningSide(t *testing.T) {
	now := time.Now().Unix()
	price := big.NewInt(105_000_000_000) // 105
	entry := big.NewInt(100_000_000_000) // 100

	cond := &ConditionalResolvable{
		Key:    make([]byte, 32),
		Amount: big.NewInt(1_000_000_000),
		Details: ConditionalResolvableDetails{
			IsLong:     true,
			Leverage:   10,
			EntryPrice: actions.Coins{Val: new(big.Int).Set(entry)},
		},
		PriceResolver: &stepBackResolver{cutoff: now, price: price},
	}

	// ROI = (105-100)*1e9*10/100 = 500_000_000 — profitable (winning side)
	// In zero-sum model, winning side sends Amount=0; payment comes via linked conditional.
	state := ResolvableState{
		Key:    make([]byte, 32),
		Amount: big.NewInt(0),
		At:     now,
	}
	stateCell, _ := tlb.ToCell(state)
	if err := cond.ValidateState(context.Background(), nil, stateCell); err != nil {
		t.Fatalf("ValidateState should accept Amount=0 on winning side, got: %v", err)
	}
}

func TestValidateState_RejectsNegativeAmount(t *testing.T) {
	now := time.Now().Unix()
	price := big.NewInt(100_000_000_000)
	entry := big.NewInt(100_000_000_000)

	cond := &ConditionalResolvable{
		Key:    make([]byte, 32),
		Amount: big.NewInt(1_000_000_000),
		Details: ConditionalResolvableDetails{
			IsLong:     true,
			Leverage:   10,
			EntryPrice: actions.Coins{Val: new(big.Int).Set(entry)},
		},
		PriceResolver: &stepBackResolver{cutoff: now, price: price},
	}

	state := ResolvableState{
		Key:    make([]byte, 32),
		Amount: big.NewInt(-100),
		At:     now,
	}
	stateCell, _ := tlb.ToCell(state)
	if err := cond.ValidateState(context.Background(), nil, stateCell); err == nil {
		t.Fatal("ValidateState should reject negative Amount")
	}
}

func TestValidateState_RejectsWhenResolverNotReady(t *testing.T) {
	cond := &ConditionalResolvable{
		Key:    make([]byte, 32),
		Amount: big.NewInt(1_000_000_000),
		Details: ConditionalResolvableDetails{
			IsLong:     true,
			Leverage:   10,
			EntryPrice: actions.Coins{Val: big.NewInt(100_000_000_000)},
		},
		PriceResolver: &hardErrorResolver{},
	}

	state := ResolvableState{
		Key:    make([]byte, 32),
		Amount: big.NewInt(0),
		At:     time.Now().Unix(),
	}
	stateCell, _ := tlb.ToCell(state)
	if err := cond.ValidateState(context.Background(), nil, stateCell); err == nil {
		t.Fatal("ValidateState should reject when price resolver is not ready")
	}
}

func TestValidateOnAdd_RejectsExcessiveLeverage(t *testing.T) {
	cond := &ConditionalResolvable{
		Amount:       big.NewInt(100),
		Fee:          big.NewInt(0),
		Action:       testResolvableActionWithFeePercent(0),
		ResolverAddr: &address.Address{},
		Details: ConditionalResolvableDetails{
			Leverage: 21,
		},
	}
	if err := cond.ValidateOnAdd(); err == nil {
		t.Fatal("ValidateOnAdd should reject leverage > 20")
	}

	cond.Details.Leverage = 0
	if err := cond.ValidateOnAdd(); err == nil {
		t.Fatal("ValidateOnAdd should reject leverage == 0")
	}

	cond.Details.Leverage = 20
	if err := cond.ValidateOnAdd(); err != nil {
		t.Fatalf("ValidateOnAdd should accept leverage == 20, got: %v", err)
	}

	cond.Details.Leverage = 1
	if err := cond.ValidateOnAdd(); err != nil {
		t.Fatalf("ValidateOnAdd should accept leverage == 1, got: %v", err)
	}
}

func TestValidateOnAdd_RejectsFeeBelowConfiguredPercent(t *testing.T) {
	cond := &ConditionalResolvable{
		Amount:       big.NewInt(1_000_000_000),
		Fee:          big.NewInt(9_999_999), // below 1% of amount
		Action:       testResolvableActionWithFeePercent(1),
		ResolverAddr: &address.Address{},
		Details: ConditionalResolvableDetails{
			Leverage: 10,
		},
	}

	if err := cond.ValidateOnAdd(); err == nil {
		t.Fatal("ValidateOnAdd should reject fee below configured percent")
	}

	cond.Fee = big.NewInt(100_000_000)
	if err := cond.ValidateOnAdd(); err != nil {
		t.Fatalf("ValidateOnAdd should accept fee equal to configured percent, got: %v", err)
	}
}

func TestValidateOnAdd_RejectsMissingResolverAddress(t *testing.T) {
	cond := &ConditionalResolvable{
		Amount: big.NewInt(1_000_000_000),
		Fee:    big.NewInt(100_000_000),
		Action: testResolvableActionWithFeePercent(1),
		Details: ConditionalResolvableDetails{
			Leverage: 10,
		},
	}

	if err := cond.ValidateOnAdd(); err == nil {
		t.Fatal("ValidateOnAdd should reject missing resolver address")
	}

	cond.ResolverAddr = &address.Address{}
	if err := cond.ValidateOnAdd(); err != nil {
		t.Fatalf("ValidateOnAdd should accept non-nil resolver address, got: %v", err)
	}
}

func TestValidateState_AcceptsExactROI(t *testing.T) {
	now := time.Now().Unix()
	price := big.NewInt(110_000_000_000) // 110
	entry := big.NewInt(100_000_000_000) // 100

	cond := &ConditionalResolvable{
		Key:    make([]byte, 32),
		Amount: big.NewInt(1_000_000_000),
		Details: ConditionalResolvableDetails{
			IsLong:     true,
			Leverage:   5,
			EntryPrice: actions.Coins{Val: new(big.Int).Set(entry)},
		},
		PriceResolver: &stepBackResolver{cutoff: now, price: price},
	}

	// ROI = (110-100)*1e9*5/100 = 500_000_000
	state := ResolvableState{
		Key:    make([]byte, 32),
		Amount: big.NewInt(500_000_000),
		At:     now,
	}
	stateCell, _ := tlb.ToCell(state)
	if err := cond.ValidateState(context.Background(), nil, stateCell); err != nil {
		t.Fatalf("ValidateState should accept exact ROI, got: %v", err)
	}
}

func TestValidateState_RejectsZeroOnLosingSide(t *testing.T) {
	now := time.Now().Unix()
	price := big.NewInt(90_000_000_000)  // 90 — price went DOWN
	entry := big.NewInt(100_000_000_000) // 100

	cond := &ConditionalResolvable{
		Key:    make([]byte, 32),
		Amount: big.NewInt(1_000_000_000),
		Details: ConditionalResolvableDetails{
			IsLong:     true,
			Leverage:   5,
			EntryPrice: actions.Coins{Val: new(big.Int).Set(entry)},
		},
		PriceResolver: &stepBackResolver{cutoff: now, price: price},
	}

	// ROI = (90-100)*1e9*5/100 = -500_000_000 (losing)
	// Losing side MUST pay |loss|, Amount=0 is the attack vector
	state := ResolvableState{
		Key:    make([]byte, 32),
		Amount: big.NewInt(0),
		At:     now,
	}
	stateCell, _ := tlb.ToCell(state)
	if err := cond.ValidateState(context.Background(), nil, stateCell); err == nil {
		t.Fatal("ValidateState should reject Amount=0 on losing side")
	}
}
