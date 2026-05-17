package actions

import (
	"context"
	"crypto/sha256"
	"math/big"
	"testing"
	"time"

	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/tonutils-go/address"
)

type testBalanceResolver struct {
	byID map[string]*payments.CoinConfig
}

func (t testBalanceResolver) ResolveBalanceType(id string) (*payments.CoinConfig, error) {
	return t.byID[id], nil
}

func (t testBalanceResolver) GetKnownBalanceTypes() []*payments.CoinConfig {
	res := make([]*payments.CoinConfig, 0, len(t.byID))
	for _, coin := range t.byID {
		res = append(res, coin)
	}
	return res
}

type testVaultResolver struct {
	vaultA *payments.VaultData
	vaultB *payments.VaultData
}

func (t testVaultResolver) ResolveVaults(_ context.Context, _ *address.Address, _ *address.Address) (*payments.VaultData, *payments.VaultData, error) {
	return t.vaultA, t.vaultB, nil
}

type testJettonClient struct {
	root    *address.Address
	wallets map[string]*address.Address
}

func (t testJettonClient) GetRootAddress() *address.Address {
	return t.root
}

func (t testJettonClient) GetWalletAddress(_ context.Context, addr *address.Address) (*address.Address, error) {
	return t.wallets[addr.String()], nil
}

func (t testJettonClient) GetBalance(_ context.Context, _ *address.Address, _ time.Time) (*big.Int, error) {
	return nil, nil
}

func TestNewSendActionFromBalanceIDSelectsVault(t *testing.T) {
	addrA := testAddr("channel-a")
	addrB := testAddr("channel-b")
	vaultAddrA := testAddr("vault-a")
	vaultAddrB := testAddr("vault-b")

	vaultA := &payments.VaultData{
		Target:    addrB,
		Address:   vaultAddrA,
		Signature: make([]byte, 64),
	}
	vaultB := &payments.VaultData{
		Target:    addrA,
		Address:   vaultAddrB,
		Signature: make([]byte, 64),
	}

	t.Run("ton", func(t *testing.T) {
		cc := &payments.CoinConfig{
			Symbol:        "TON",
			Decimals:      9,
			BalanceID:     payments.GetTONBalanceID(),
			VaultResolver: testVaultResolver{vaultA: vaultA},
		}

		action, err := NewSendActionFromBalanceID(context.Background(), cc, addrA.String(), addrB.String())
		if err != nil {
			t.Fatalf("new send action failed: %v", err)
		}
		if _, ok := action.(*ActionSendTonVault); !ok {
			t.Fatalf("expected ton vault action, got %T", action)
		}
	})

	t.Run("jetton", func(t *testing.T) {
		root := testAddr("jetton-root")
		jettonWalletA := testAddr("vault-jetton-wallet-a")
		cc := &payments.CoinConfig{
			Symbol:    "USDT",
			Decimals:  6,
			BalanceID: payments.GetJettonBalanceID(root),
			JettonClient: testJettonClient{
				root: root,
				wallets: map[string]*address.Address{
					vaultAddrA.String(): jettonWalletA,
				},
			},
			VaultResolver: testVaultResolver{vaultA: vaultA},
		}

		action, err := NewSendActionFromBalanceID(context.Background(), cc, addrA.String(), addrB.String())
		if err != nil {
			t.Fatalf("new send action failed: %v", err)
		}
		vaultAction, ok := action.(*ActionSendJettonVault)
		if !ok {
			t.Fatalf("expected jetton vault action, got %T", action)
		}
		if vaultAction.WalletA == nil || !vaultAction.WalletA.Equals(jettonWalletA) {
			t.Fatalf("jetton vault action must resolve vault jetton wallet")
		}
		if vaultAction.WalletB != nil {
			t.Fatalf("wallet B must stay nil when vault B is absent")
		}
	})

	t.Run("ec", func(t *testing.T) {
		cc := &payments.CoinConfig{
			Symbol:        "USDX",
			Decimals:      4,
			BalanceID:     payments.GetECBalanceID(7),
			VaultResolver: testVaultResolver{vaultA: vaultA},
		}

		action, err := NewSendActionFromBalanceID(context.Background(), cc, addrA.String(), addrB.String())
		if err != nil {
			t.Fatalf("new send action failed: %v", err)
		}
		if _, ok := action.(*ActionSendECVault); !ok {
			t.Fatalf("expected ec vault action, got %T", action)
		}
	})

	t.Run("both-sides", func(t *testing.T) {
		cc := &payments.CoinConfig{
			Symbol:        "TON",
			Decimals:      9,
			BalanceID:     payments.GetTONBalanceID(),
			VaultResolver: testVaultResolver{vaultA: vaultA, vaultB: vaultB},
		}

		action, err := NewSendActionFromBalanceID(context.Background(), cc, addrA.String(), addrB.String())
		if err != nil {
			t.Fatalf("new send action failed: %v", err)
		}
		vaultAction := action.(*ActionSendTonVault)
		if vaultAction.VaultA == nil || vaultAction.VaultB == nil {
			t.Fatalf("both vault sides must be preserved")
		}
	})
}

func TestVaultActionsSerializeRoundTrip(t *testing.T) {
	addrA := testAddr("channel-a")
	addrB := testAddr("channel-b")
	vaultAddrA := testAddr("vault-a")
	vaultAddrB := testAddr("vault-b")
	root := testAddr("jetton-root")
	jettonWalletA := testAddr("vault-jetton-wallet-a")
	jettonWalletB := testAddr("vault-jetton-wallet-b")

	vaultA := &payments.VaultData{
		Target:    addrB,
		Address:   vaultAddrA,
		Signature: make([]byte, 64),
	}
	vaultB := &payments.VaultData{
		Target:    addrA,
		Address:   vaultAddrB,
		Signature: make([]byte, 64),
	}

	jettonClient := testJettonClient{
		root: root,
		wallets: map[string]*address.Address{
			vaultAddrA.String(): jettonWalletA,
			vaultAddrB.String(): jettonWalletB,
		},
	}

	tests := []struct {
		name   string
		action payments.Action
		coin   *payments.CoinConfig
		check  func(t *testing.T, action payments.Action)
	}{
		{
			name: "ton",
			action: &ActionSendTonVault{
				Coin:   &payments.CoinConfig{Symbol: "TON", Decimals: 9, BalanceID: payments.GetTONBalanceID()},
				VaultA: vaultA,
				VaultB: vaultB,
			},
			coin: &payments.CoinConfig{Symbol: "TON", Decimals: 9, BalanceID: payments.GetTONBalanceID()},
			check: func(t *testing.T, action payments.Action) {
				if _, ok := action.(*ActionSendTonVault); !ok {
					t.Fatalf("expected ton vault action, got %T", action)
				}
			},
		},
		{
			name: "jetton",
			action: &ActionSendJettonVault{
				Coin:     &payments.CoinConfig{Symbol: "USDT", Decimals: 6, BalanceID: payments.GetJettonBalanceID(root), JettonClient: jettonClient},
				VaultA:   vaultA,
				VaultB:   vaultB,
				RootAddr: root,
				WalletA:  jettonWalletA,
				WalletB:  jettonWalletB,
			},
			coin: &payments.CoinConfig{Symbol: "USDT", Decimals: 6, BalanceID: payments.GetJettonBalanceID(root), JettonClient: jettonClient},
			check: func(t *testing.T, action payments.Action) {
				if _, ok := action.(*ActionSendJettonVault); !ok {
					t.Fatalf("expected jetton vault action, got %T", action)
				}
			},
		},
		{
			name: "ec",
			action: &ActionSendECVault{
				Coin:   &payments.CoinConfig{Symbol: "USDX", Decimals: 4, BalanceID: payments.GetECBalanceID(7)},
				VaultA: vaultA,
				VaultB: vaultB,
				EC:     7,
			},
			coin: &payments.CoinConfig{Symbol: "USDX", Decimals: 4, BalanceID: payments.GetECBalanceID(7)},
			check: func(t *testing.T, action payments.Action) {
				if _, ok := action.(*ActionSendECVault); !ok {
					t.Fatalf("expected ec vault action, got %T", action)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolver := testBalanceResolver{byID: map[string]*payments.CoinConfig{
				test.coin.BalanceID: test.coin,
			}}
			parsed, err := payments.CodeToAction(context.Background(), test.action.Serialize(), resolver)
			if err != nil {
				t.Fatalf("round trip parse failed: %v", err)
			}
			test.check(t, parsed)
		})
	}
}

func testAddr(seed string) *address.Address {
	hash := sha256.Sum256([]byte(seed))
	return address.NewAddress(0, 0, hash[:])
}
