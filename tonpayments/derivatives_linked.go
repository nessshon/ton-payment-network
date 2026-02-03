package tonpayments

import (
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/big"

	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/pkg/payments/actions"
	"github.com/xssnick/ton-payment-network/pkg/payments/conditionals"
	"github.com/xssnick/ton-payment-network/tonpayments/db"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

var derivativeLinkedKeySalt = []byte("derivative-linked-v1")

func derivativeLinkedKey(base []byte) ed25519.PublicKey {
	payload := make([]byte, 0, len(derivativeLinkedKeySalt)+len(base))
	payload = append(payload, derivativeLinkedKeySalt...)
	payload = append(payload, base...)

	hash := sha256.Sum256(payload)
	return ed25519.PublicKey(hash[:])
}

func reverseDerivativeAction(act payments.Action) (payments.Action, error) {
	switch a := act.(type) {
	case *actions.ActionSendTon:
		return &actions.ActionSendTon{
			Coin:     a.Coin,
			AddressA: a.AddressB,
			AddressB: a.AddressA,
		}, nil
	case *actions.ActionSendJetton:
		return &actions.ActionSendJetton{
			Coin:     a.Coin,
			AddressA: a.AddressB,
			AddressB: a.AddressA,
			RootAddr: a.RootAddr,
			WalletA:  a.WalletB,
			WalletB:  a.WalletA,
		}, nil
	case *actions.ActionSendEC:
		return &actions.ActionSendEC{
			Coin:     a.Coin,
			AddressA: a.AddressB,
			AddressB: a.AddressA,
			EC:       a.EC,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported derivative action type: %T", act)
	}
}

func (s *Service) buildLinkedDerivativeConditional(base *conditionals.ConditionalResolvable) (*conditionals.ConditionalResolvable, error) {
	if base == nil {
		return nil, fmt.Errorf("base derivative conditional is nil")
	}

	if len(base.GetKey()) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("invalid derivative key size: %d", len(base.GetKey()))
	}

	reversedAction, err := reverseDerivativeAction(base.GetAction())
	if err != nil {
		return nil, err
	}

	details := base.Details
	details.IsLong = !details.IsLong

	return &conditionals.ConditionalResolvable{
		Key:           derivativeLinkedKey(base.GetKey()),
		Amount:        big.NewInt(0), // reciprocal side has no hard cap
		ResolverAddr:  base.ResolverAddr,
		Details:       details,
		PriceResolver: base.PriceResolver,
		Action:        reversedAction,
	}, nil
}

func ensureConditionalOnSide(side *db.Side, cond payments.Conditional) (bool, error) {
	serialized := cond.Serialize()
	key := cell.BeginCell().MustStoreSlice(serialized.Hash(), 256).EndCell()

	_, err := side.Data.Conditionals.LoadValue(key)
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, cell.ErrNoSuchKeyInDict) {
		return false, fmt.Errorf("failed to check conditional existence: %w", err)
	}

	if err = side.Data.Conditionals.Set(key, serialized); err != nil {
		return false, fmt.Errorf("failed to add conditional: %w", err)
	}
	return true, nil
}

func ensureActionStateOnSide(side *db.Side, act payments.Action) (bool, error) {
	actID := act.IDCell()

	_, err := side.Data.ActionStates.LoadValue(actID)
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, cell.ErrNoSuchKeyInDict) {
		return false, fmt.Errorf("failed to check action state existence: %w", err)
	}

	if err = side.Data.ActionStates.Set(actID, act.GetEmptyState()); err != nil {
		return false, fmt.Errorf("failed to init action state: %w", err)
	}
	return true, nil
}

func remapDerivativeStateKey(state *cell.Cell, key []byte) (*cell.Cell, error) {
	var st conditionals.ResolvableState
	if err := payments.LoadState(&st, state); err != nil {
		return nil, fmt.Errorf("failed to parse derivative resolve state: %w", err)
	}

	st.Key = key
	mapped, err := tlb.ToCell(st)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize derivative resolve state: %w", err)
	}
	return mapped, nil
}
