package tonpayments

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/pkg/payments/actions"
	paymentvault "github.com/xssnick/ton-payment-network/pkg/payments/vault"
	chainclient "github.com/xssnick/ton-payment-network/tonpayments/chain/client"
	cfgpkg "github.com/xssnick/ton-payment-network/tonpayments/config"
	dbpkg "github.com/xssnick/ton-payment-network/tonpayments/db"
	"github.com/xssnick/ton-payment-network/tonpayments/db/leveldb"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

type vaultServiceChain struct {
	mx       sync.Mutex
	accounts map[string]*chainclient.Account
}

func newVaultServiceChain() *vaultServiceChain {
	return &vaultServiceChain{accounts: map[string]*chainclient.Account{}}
}

func (v *vaultServiceChain) GetAccount(_ context.Context, addr *address.Address, _ time.Time) (*chainclient.Account, error) {
	v.mx.Lock()
	defer v.mx.Unlock()

	acc := v.accounts[addr.String()]
	if acc == nil {
		return &chainclient.Account{
			Address:         addr,
			Balance:         tlb.ZeroCoins,
			ExtraCurrencies: cell.NewDict(32),
			HasState:        false,
			IsActive:        false,
		}, nil
	}

	cp := *acc
	return &cp, nil
}

func (v *vaultServiceChain) GetJettonWalletAddress(_ context.Context, root, addr *address.Address) (*address.Address, error) {
	return deriveTestAddr("jetton-wallet-" + root.String() + "-" + addr.String()), nil
}

func (v *vaultServiceChain) GetJettonBalance(_ context.Context, _ *address.Address, _ *address.Address, _ time.Time) (*big.Int, error) {
	return big.NewInt(0), nil
}

func (v *vaultServiceChain) GetLastTransaction(context.Context, *address.Address, time.Time) (*chainclient.Transaction, *chainclient.Account, error) {
	return nil, nil, nil
}

func (v *vaultServiceChain) GetTransactionByInMsgHash(context.Context, *address.Address, []byte, time.Time) (*chainclient.Transaction, error) {
	return nil, nil
}

func (v *vaultServiceChain) SendWaitExternalMessage(_ context.Context, _ *address.Address, body *cell.Cell) ([]byte, error) {
	return body.Hash(), nil
}

func (v *vaultServiceChain) deploy(stateInit *tlb.StateInit, amount tlb.Coins) (*address.Address, error) {
	addr, err := paymentvault.AddressFromStateInit(stateInit)
	if err != nil {
		return nil, err
	}

	v.mx.Lock()
	v.accounts[addr.String()] = &chainclient.Account{
		Address:         addr,
		Balance:         amount,
		ExtraCurrencies: cell.NewDict(32),
		HasState:        true,
		IsActive:        true,
		Code:            stateInit.Code,
		Data:            stateInit.Data,
	}
	v.mx.Unlock()
	return addr, nil
}

func (v *vaultServiceChain) addTON(addr *address.Address, amount tlb.Coins) {
	v.mx.Lock()
	defer v.mx.Unlock()

	acc := v.accounts[addr.String()]
	if acc == nil {
		acc = &chainclient.Account{
			Address:         addr,
			Balance:         tlb.ZeroCoins,
			ExtraCurrencies: cell.NewDict(32),
			HasState:        false,
			IsActive:        false,
		}
		v.accounts[addr.String()] = acc
	}
	acc.Balance = tlb.FromNanoTON(new(big.Int).Add(acc.Balance.Nano(), amount.Nano()))
}

type vaultServiceWallet struct {
	addr  *address.Address
	chain *vaultServiceChain

	mx       sync.Mutex
	requests [][]WalletMessage
}

func (v *vaultServiceWallet) WalletAddress() *address.Address {
	return v.addr
}

func (v *vaultServiceWallet) DoTransactionMany(_ context.Context, _ string, messages []WalletMessage) ([]byte, error) {
	v.mx.Lock()
	cp := make([]WalletMessage, len(messages))
	copy(cp, messages)
	v.requests = append(v.requests, cp)
	v.mx.Unlock()

	for _, message := range messages {
		if message.StateInit != nil {
			if _, err := v.chain.deploy(message.StateInit, message.Amount); err != nil {
				return nil, err
			}
			continue
		}
		if message.To != nil && message.EC == nil {
			v.chain.addTON(message.To, message.Amount)
		}
		if message.To != nil && message.EC != nil {
			v.chain.addTON(message.To, message.Amount)
		}
	}

	if len(messages) == 0 {
		return []byte("wallet"), nil
	}
	return messages[0].Amount.Nano().Bytes(), nil
}

func (v *vaultServiceWallet) DoTransaction(ctx context.Context, reason string, to *address.Address, amt tlb.Coins, body *cell.Cell) ([]byte, error) {
	return v.DoTransactionMany(ctx, reason, []WalletMessage{{
		To:     to,
		Amount: amt,
		Body:   body,
	}})
}

func (v *vaultServiceWallet) requestCount() int {
	v.mx.Lock()
	defer v.mx.Unlock()
	return len(v.requests)
}

func (v *vaultServiceWallet) requestAt(i int) []WalletMessage {
	v.mx.Lock()
	defer v.mx.Unlock()
	return append([]WalletMessage(nil), v.requests[i]...)
}

func TestServiceStartDeploysAndTopupsVault(t *testing.T) {
	cfg, chain, wallet, service, cleanup := newVaultService(t, func(cfg *cfgpkg.Config) {
		cfg.Vault.UseOnOurSide = true
		cfg.Vault.Coins = map[string]cfgpkg.VaultCoinBalanceConfig{
			"TON": {
				MinBalance: "0.5",
				MaxBalance: "1",
			},
		}
	})
	defer cleanup()

	restore := VaultWorkerInterval
	VaultWorkerInterval = 20 * time.Millisecond
	defer func() { VaultWorkerInterval = restore }()

	done := make(chan struct{})
	go func() {
		defer close(done)
		service.Start()
	}()

	waitForTest(t, 2*time.Second, func() bool {
		return wallet.requestCount() >= 2
	})

	service.Stop()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("service stop timeout")
	}

	if got := wallet.requestCount(); got < 2 {
		t.Fatalf("expected at least 2 wallet requests, got %d", got)
	}

	first := wallet.requestAt(0)
	if len(first) != 1 || first[0].StateInit == nil {
		t.Fatalf("first wallet request must deploy vault")
	}

	second := wallet.requestAt(1)
	if len(second) != 1 || second[0].To == nil {
		t.Fatalf("second wallet request must top up vault")
	}
	if !second[0].To.Equals(service.LocalVaultAddress()) {
		t.Fatalf("vault top up target mismatch")
	}
	if second[0].Body == nil {
		t.Fatalf("vault top up body must be signed for active vault")
	}
	if second[0].Amount.String() != "0.95" {
		t.Fatalf("unexpected vault top up amount %s", second[0].Amount.String())
	}

	acc, err := chain.GetAccount(context.Background(), service.LocalVaultAddress(), time.Time{})
	if err != nil {
		t.Fatalf("load vault account failed: %v", err)
	}
	if acc.Balance.String() != "1" {
		t.Fatalf("unexpected vault balance after reconcile %s", acc.Balance.String())
	}

	if cfg.Vault.AllowOnTheirSide {
		t.Fatalf("regular config must keep allow-on-their-side disabled by default")
	}
}

func TestServiceResolveVaultsUsesLocalSide(t *testing.T) {
	_, _, _, service, cleanup := newVaultService(t, func(cfg *cfgpkg.Config) {
		cfg.Vault.UseOnOurSide = true
	})
	defer cleanup()

	addrA := deriveTestAddr("channel-a")
	addrB := deriveTestAddr("channel-b")
	channel := &dbpkg.Channel{
		ID:          bytes.Repeat([]byte{1}, 16),
		Status:      dbpkg.ChannelStateActive,
		SignedState: mustSignedStateCell(t),
		Our: dbpkg.Side{
			Address:                 addrA.String(),
			OnchainBalances:         map[string]*big.Int{},
			PendingOnchainTransfers: map[string]*payments.PendingMessageInfo{},
			Data:                    dbpkg.NewAgreedData(),
		},
		Their: dbpkg.Side{
			Address:                 addrB.String(),
			OnchainBalances:         map[string]*big.Int{},
			PendingOnchainTransfers: map[string]*payments.PendingMessageInfo{},
			Data:                    dbpkg.NewAgreedData(),
		},
	}

	dbImpl := service.db.(*dbpkg.DB)
	if err := dbImpl.CreateChannel(context.Background(), channel); err != nil {
		t.Fatalf("create channel failed: %v", err)
	}

	vaultA, vaultB, err := service.ResolveVaults(context.Background(), addrA, addrB)
	if err != nil {
		t.Fatalf("resolve vaults failed: %v", err)
	}
	if vaultA == nil || vaultB != nil {
		t.Fatalf("expected only A-side vault data")
	}
	if !vaultA.Target.Equals(addrB) {
		t.Fatalf("vault target mismatch for A-side")
	}

	vaultA, vaultB, err = service.ResolveVaults(context.Background(), addrB, addrA)
	if err != nil {
		t.Fatalf("resolve reversed vaults failed: %v", err)
	}
	if vaultA != nil || vaultB == nil {
		t.Fatalf("expected only B-side vault data when our channel is second argument")
	}
	if !vaultB.Target.Equals(addrB) {
		t.Fatalf("vault target mismatch for B-side")
	}
}

func TestServiceValidateIncomingVaultAction(t *testing.T) {
	service := &Service{}
	action := &actions.ActionSendTonVault{
		Coin: &payments.CoinConfig{
			Symbol:    "TON",
			Decimals:  9,
			BalanceID: payments.GetTONBalanceID(),
		},
	}

	service.vaultCfg.AllowOnTheirSide = false
	if err := service.validateIncomingAction(action); err == nil {
		t.Fatalf("vault action must be rejected when disabled")
	}

	service.vaultCfg.AllowOnTheirSide = true
	if err := service.validateIncomingAction(action); err != nil {
		t.Fatalf("vault action must be accepted when enabled: %v", err)
	}
}

func TestServiceStartSkipsVaultWorkerWhenUseOnOurSideDisabled(t *testing.T) {
	_, _, wallet, service, cleanup := newVaultService(t, func(cfg *cfgpkg.Config) {
		cfg.Vault.AllowOnTheirSide = true
		cfg.Vault.Coins = map[string]cfgpkg.VaultCoinBalanceConfig{
			"TON": {
				MinBalance: "0.5",
				MaxBalance: "1",
			},
		}
	})
	defer cleanup()

	done := make(chan struct{})
	go func() {
		defer close(done)
		service.Start()
	}()

	time.Sleep(100 * time.Millisecond)
	service.Stop()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("service stop timeout")
	}

	if got := wallet.requestCount(); got != 0 {
		t.Fatalf("vault worker must stay idle when UseOnOurSide=false, got %d wallet requests", got)
	}
}

func newVaultService(t *testing.T, mutate func(cfg *cfgpkg.Config)) (*cfgpkg.Config, *vaultServiceChain, *vaultServiceWallet, *Service, func()) {
	t.Helper()

	cfg, err := cfgpkg.Generate()
	if err != nil {
		t.Fatalf("generate config failed: %v", err)
	}
	cfg.ChannelConfig.SupportedCoins.Ton.BalanceControl = nil
	if mutate != nil {
		mutate(cfg)
	}

	storage, fresh, err := leveldb.NewLevelDB(t.TempDir())
	if err != nil {
		t.Fatalf("open leveldb failed: %v", err)
	}

	pub := ed25519.NewKeyFromSeed(cfg.PaymentNodePrivateKey).Public().(ed25519.PublicKey)
	database := dbpkg.NewDB(storage, pub)
	if fresh {
		if err = database.SetMigrationVersion(context.Background(), len(dbpkg.Migrations)); err != nil {
			t.Fatalf("set migration version failed: %v", err)
		}
	}

	chain := newVaultServiceChain()
	wallet := &vaultServiceWallet{
		addr:  deriveTestAddr("wallet"),
		chain: chain,
	}
	service, err := NewService(chain, database, nil, nil, wallet, make(chan any, 4), ed25519.NewKeyFromSeed(cfg.PaymentNodePrivateKey), cfg.ChannelConfig, cfg.Vault, false)
	if err != nil {
		t.Fatalf("new service failed: %v", err)
	}

	cleanup := func() {
		service.Stop()
		database.Close()
	}
	return cfg, chain, wallet, service, cleanup
}

func mustSignedStateCell(t *testing.T) *cell.Cell {
	t.Helper()

	state, err := tlb.ToCell(payments.StateBodySigned{
		SignatureA: payments.Signature{Value: bytes.Repeat([]byte{1}, 64)},
		SignatureB: payments.Signature{Value: make([]byte, 64)},
		Body: payments.StateBody{
			ChannelID: bytes.Repeat([]byte{1}, 16),
			Seqno:     0,
			A: payments.StateSide{
				ConditionalsHash: make([]byte, 32),
				ActionStatesHash: make([]byte, 32),
			},
			B: payments.StateSide{
				ConditionalsHash: make([]byte, 32),
				ActionStatesHash: make([]byte, 32),
			},
		},
	})
	if err != nil {
		t.Fatalf("build signed state failed: %v", err)
	}
	return state
}

func deriveTestAddr(seed string) *address.Address {
	hash := sha256.Sum256([]byte(seed))
	return address.NewAddress(0, 0, hash[:])
}

func waitForTest(t *testing.T, timeout time.Duration, ready func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition was not met within %s", timeout)
}
