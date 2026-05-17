package tonpayments

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"fmt"
	"math/big"
	"testing"

	"github.com/xssnick/ton-payment-network/pkg/payments/actions"
	"github.com/xssnick/ton-payment-network/pkg/payments/conditionals"
	condcontracts "github.com/xssnick/ton-payment-network/pkg/payments/conditionals/contracts"
	"github.com/xssnick/ton-payment-network/pkg/payments/conditionals/oracle"
	"github.com/xssnick/ton-payment-network/tonpayments/db"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
)

func testAddr(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return address.NewAddress(0, 0, sum[:]).String()
}

type staticSignedPriceProvider struct {
	key ed25519.PrivateKey
}

func (s *staticSignedPriceProvider) Fetch(_ context.Context, at int64) (int64, *big.Int, error) {
	return at, big.NewInt(1_000_000_000), nil
}

func (s *staticSignedPriceProvider) ProofPublicKey() ed25519.PublicKey {
	return append(ed25519.PublicKey(nil), s.key.Public().(ed25519.PublicKey)...)
}

func installSignedResolverForAsset(t *testing.T, assetID uint32) {
	t.Helper()

	seed := sha256.Sum256([]byte(fmt.Sprintf("resolver-seed-%d", assetID)))
	provider := &staticSignedPriceProvider{
		key: ed25519.NewKeyFromSeed(seed[:]),
	}
	resolver := oracle.NewResolver(provider)

	prev, hadPrev := oracle.PriceResolvers[assetID]
	oracle.PriceResolvers[assetID] = resolver
	t.Cleanup(func() {
		resolver.Close()
		if hadPrev {
			oracle.PriceResolvers[assetID] = prev
		} else {
			delete(oracle.PriceResolvers, assetID)
		}
	})
}

func TestBuildDerivativeResolverContract(t *testing.T) {
	s := &Service{}
	key := sha256.Sum256([]byte("derivative-key"))
	installSignedResolverForAsset(t, 123)

	channel := &db.Channel{
		WeLeft: true,
		Our: db.Side{
			Address: testAddr("our"),
		},
		Their: db.Side{
			Address: testAddr("their"),
		},
	}

	amount := tlb.MustFromDecimal("0.1", 9).Nano()
	entry := tlb.MustFromDecimal("100", 9).Nano()
	details := conditionals.ConditionalResolvableDetails{
		AssetID:    123,
		IsLong:     true,
		Leverage:   10,
		EntryPrice: actions.Coins{Val: entry},
	}

	stateInit, resolverAddr, instruction, err := s.buildDerivativeResolverContract(channel, key[:], amount, details)
	if err != nil {
		t.Fatalf("build resolver contract failed: %v", err)
	}

	if stateInit == nil || stateInit.Code == nil || stateInit.Data == nil {
		t.Fatalf("unexpected nil state init")
	}
	if resolverAddr == nil {
		t.Fatalf("resolver address is nil")
	}
	if !bytes.Equal(instruction.ResolverContractCodeHash, stateInit.Code.Hash()) {
		t.Fatalf("resolver code hash mismatch")
	}
	if instruction.ResolverContractData == nil || !bytes.Equal(instruction.ResolverContractData.Hash(), stateInit.Data.Hash()) {
		t.Fatalf("resolver data mismatch")
	}

	calcAddr, err := condcontracts.CalcDerivativeAddress(stateInit)
	if err != nil {
		t.Fatalf("calc resolver address failed: %v", err)
	}
	if !calcAddr.Equals(resolverAddr) {
		t.Fatalf("calculated resolver address mismatch")
	}
}

func TestBuildDerivativeMetaAnyStoresResolverData(t *testing.T) {
	s := &Service{}
	key := sha256.Sum256([]byte("derivative-key-meta"))
	installSignedResolverForAsset(t, 555)
	channel := &db.Channel{
		WeLeft: true,
		Our: db.Side{
			Address: testAddr("our-meta"),
		},
		Their: db.Side{
			Address: testAddr("their-meta"),
		},
	}

	amount := tlb.MustFromDecimal("0.2", 9).Nano()
	details := conditionals.ConditionalResolvableDetails{
		AssetID:    555,
		IsLong:     false,
		Leverage:   7,
		EntryPrice: actions.Coins{Val: tlb.MustFromDecimal("42", 9).Nano()},
	}

	stateInit, _, instruction, err := s.buildDerivativeResolverContract(channel, key[:], amount, details)
	if err != nil {
		t.Fatalf("build resolver contract failed: %v", err)
	}

	detailsCell, err := tlb.ToCell(instruction)
	if err != nil {
		t.Fatalf("serialize instruction failed: %v", err)
	}

	meta := &db.ConditionalMeta{
		SpecialDetails: buildDerivativeMetaAny(details, detailsCell),
	}

	loaded, err := loadDerivativeResolverStateInit(meta)
	if err != nil {
		t.Fatalf("load resolver from meta failed: %v", err)
	}
	if !bytes.Equal(loaded.Code.Hash(), stateInit.Code.Hash()) {
		t.Fatalf("loaded resolver code hash mismatch")
	}
	if !bytes.Equal(loaded.Data.Hash(), stateInit.Data.Hash()) {
		t.Fatalf("loaded resolver data hash mismatch")
	}
}
