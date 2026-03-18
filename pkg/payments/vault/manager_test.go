package vault

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"math/big"
	"testing"
	"time"

	"github.com/xssnick/ton-payment-network/pkg/payments"
	chainclient "github.com/xssnick/ton-payment-network/tonpayments/chain/client"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

type testJettonClient struct {
	root *address.Address
}

func (t testJettonClient) GetRootAddress() *address.Address {
	return t.root
}

func (t testJettonClient) GetWalletAddress(_ context.Context, addr *address.Address) (*address.Address, error) {
	return mustDerivedAddr("jetton-wallet-" + addr.String()), nil
}

func (t testJettonClient) GetBalance(_ context.Context, _ *address.Address, _ time.Time) (*big.Int, error) {
	return big.NewInt(0), nil
}

type sentExternal struct {
	to   *address.Address
	body *cell.Cell
}

type testChain struct {
	accounts       map[string]*chainclient.Account
	jettonBalances map[string]*big.Int
	sentExternal   []sentExternal
}

func newTestChain() *testChain {
	return &testChain{
		accounts:       map[string]*chainclient.Account{},
		jettonBalances: map[string]*big.Int{},
	}
}

func (t *testChain) GetAccount(_ context.Context, addr *address.Address, _ time.Time) (*chainclient.Account, error) {
	if acc := t.accounts[addr.String()]; acc != nil {
		return cloneAccount(acc), nil
	}
	return &chainclient.Account{
		Address:         addr,
		Balance:         tlb.ZeroCoins,
		ExtraCurrencies: cell.NewDict(32),
		HasState:        false,
		IsActive:        false,
	}, nil
}

func (t *testChain) GetJettonWalletAddress(_ context.Context, root, addr *address.Address) (*address.Address, error) {
	return mustDerivedAddr("jetton-wallet-" + root.String() + "-" + addr.String()), nil
}

func (t *testChain) GetJettonBalance(_ context.Context, root, addr *address.Address, _ time.Time) (*big.Int, error) {
	balance := t.jettonBalances[root.String()+"|"+addr.String()]
	if balance == nil {
		return big.NewInt(0), nil
	}
	return new(big.Int).Set(balance), nil
}

func (t *testChain) SendWaitExternalMessage(_ context.Context, to *address.Address, body *cell.Cell) ([]byte, error) {
	t.sentExternal = append(t.sentExternal, sentExternal{
		to:   to,
		body: body,
	})
	return append([]byte(nil), body.Hash()...), nil
}

type testWallet struct {
	addr     *address.Address
	requests [][]WalletMessage
	onSend   func([]WalletMessage)
}

func (t *testWallet) WalletAddress() *address.Address {
	return t.addr
}

func (t *testWallet) DoTransactionMany(_ context.Context, _ string, messages []WalletMessage) ([]byte, error) {
	cp := make([]WalletMessage, len(messages))
	copy(cp, messages)
	t.requests = append(t.requests, cp)
	if t.onSend != nil {
		t.onSend(cp)
	}
	if len(messages) == 0 {
		return []byte("wallet"), nil
	}
	return append([]byte(nil), messages[0].Amount.Nano().Bytes()...), nil
}

func TestBuildStateInitAndVaultData(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key failed: %v", err)
	}

	stateInit, err := BuildStateInitFromPrivateKey(key)
	if err != nil {
		t.Fatalf("build state init failed: %v", err)
	}
	addr, err := AddressFromStateInit(stateInit)
	if err != nil {
		t.Fatalf("build address failed: %v", err)
	}

	storage, err := LoadStorage(stateInit.Data)
	if err != nil {
		t.Fatalf("load storage failed: %v", err)
	}
	if !StorageMatchesKey(storage, key.Public().(ed25519.PublicKey)) {
		t.Fatalf("vault storage key mismatch")
	}
	if !StorageMatchesID(storage, key.Public().(ed25519.PublicKey)) {
		t.Fatalf("vault storage id mismatch")
	}

	sender := mustDerivedAddr("sender")
	target := mustDerivedAddr("target")
	data, err := BuildVaultData(key, sender, target)
	if err != nil {
		t.Fatalf("build vault data failed: %v", err)
	}

	if !data.Address.Equals(addr) {
		t.Fatalf("vault address mismatch")
	}
	if !data.Target.Equals(target) {
		t.Fatalf("vault target mismatch")
	}

	pairCell, err := tlb.ToCell(PairToSign{
		Sender:   sender,
		Receiver: addr,
	})
	if err != nil {
		t.Fatalf("serialize pair failed: %v", err)
	}
	if !ed25519.Verify(key.Public().(ed25519.PublicKey), pairCell.Hash(), data.Signature) {
		t.Fatalf("vault pair signature is invalid")
	}
}

func TestBuildTransferBodyRoundTrip(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key failed: %v", err)
	}

	stateInit, err := BuildStateInitFromPrivateKey(key)
	if err != nil {
		t.Fatalf("build state init failed: %v", err)
	}
	storage, err := LoadStorage(stateInit.Data)
	if err != nil {
		t.Fatalf("load storage failed: %v", err)
	}

	target := mustDerivedAddr("target")
	messages := []payments.WalletMessage{{
		Mode: 1 + 2,
		InternalMessage: &tlb.InternalMessage{
			IHRDisabled: true,
			Bounce:      target.IsBounceable(),
			DstAddr:     target,
			Amount:      tlb.MustFromTON("0.25"),
			Body:        cell.BeginCell().EndCell(),
		},
	}}

	body, err := BuildTransferBody(key, storage, messages, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("build transfer body failed: %v", err)
	}

	var req ExternalSignedRequest
	if err = tlb.LoadFromCell(&req, body.BeginParse()); err != nil {
		t.Fatalf("parse external request failed: %v", err)
	}
	if !ed25519.Verify(key.Public().(ed25519.PublicKey), req.SignedBody.Hash(), req.Signature) {
		t.Fatalf("external signature is invalid")
	}

	var signed ExternalSignedSendBody
	if err = tlb.LoadFromCell(&signed, req.SignedBody.BeginParse()); err != nil {
		t.Fatalf("parse signed body failed: %v", err)
	}
	if hex.EncodeToString(signed.ID) != hex.EncodeToString(storage.ID) {
		t.Fatalf("vault id mismatch")
	}

	unpacked, err := payments.UnpackOutActions(signed.OutActions)
	if err != nil {
		t.Fatalf("unpack out actions failed: %v", err)
	}
	if len(unpacked) != 1 || !unpacked[0].InternalMessage.DstAddr.Equals(target) {
		t.Fatalf("unexpected unpacked out actions")
	}
}

func TestManagerEnsureDeployed(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key failed: %v", err)
	}

	chain := newTestChain()
	wallet := &testWallet{addr: mustDerivedAddr("wallet")}

	manager, err := NewManager(chain, wallet, key, map[string]*payments.CoinConfig{}, nil)
	if err != nil {
		t.Fatalf("new manager failed: %v", err)
	}

	if err = manager.EnsureDeployed(context.Background()); err != nil {
		t.Fatalf("ensure deployed failed: %v", err)
	}
	if len(wallet.requests) != 1 {
		t.Fatalf("expected 1 wallet request, got %d", len(wallet.requests))
	}
	if wallet.requests[0][0].StateInit == nil {
		t.Fatalf("expected deploy state init in first request")
	}
}

func TestManagerReconcileTopUpTONAndLock(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key failed: %v", err)
	}

	chain := newTestChain()
	wallet := &testWallet{addr: mustDerivedAddr("wallet")}

	manager, err := NewManager(chain, wallet, key, map[string]*payments.CoinConfig{
		"TON": {
			Symbol:    "TON",
			Decimals:  9,
			BalanceID: payments.GetTONBalanceID(),
		},
	}, map[string]CoinLimits{
		"TON": {
			MinBalance: tlb.MustFromTON("0.5"),
			MaxBalance: tlb.MustFromTON("1.0"),
		},
	})
	if err != nil {
		t.Fatalf("new manager failed: %v", err)
	}

	chain.accounts[manager.Address().String()] = mustVaultAccount(t, key, tlb.MustFromTON("0.2"), nil)

	if err = manager.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}
	if len(wallet.requests) != 1 {
		t.Fatalf("expected 1 wallet top up, got %d", len(wallet.requests))
	}
	if got := wallet.requests[0][0].Amount.String(); got != "0.8" {
		t.Fatalf("unexpected top up amount %s", got)
	}
	if wallet.requests[0][0].Body == nil {
		t.Fatalf("ton top up body must be signed for active vault")
	}
	assertSignedTopUpBody(t, wallet.requests[0][0].Body, wallet.addr, manager.Address(), key.Public().(ed25519.PublicKey), 1)

	if err = manager.Reconcile(context.Background()); err != nil {
		t.Fatalf("second reconcile failed: %v", err)
	}
	if len(wallet.requests) != 1 {
		t.Fatalf("action lock must suppress duplicate top up")
	}
}

func TestManagerReconcileWithdrawTON(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key failed: %v", err)
	}

	chain := newTestChain()
	wallet := &testWallet{addr: mustDerivedAddr("wallet")}

	manager, err := NewManager(chain, wallet, key, map[string]*payments.CoinConfig{
		"TON": {
			Symbol:    "TON",
			Decimals:  9,
			BalanceID: payments.GetTONBalanceID(),
		},
	}, map[string]CoinLimits{
		"TON": {
			MinBalance: tlb.MustFromTON("0.5"),
			MaxBalance: tlb.MustFromTON("1.0"),
		},
	})
	if err != nil {
		t.Fatalf("new manager failed: %v", err)
	}

	chain.accounts[manager.Address().String()] = mustVaultAccount(t, key, tlb.MustFromTON("2.0"), nil)

	if err = manager.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}
	if len(chain.sentExternal) != 1 {
		t.Fatalf("expected 1 external withdraw, got %d", len(chain.sentExternal))
	}

	var req ExternalSignedRequest
	if err = tlb.LoadFromCell(&req, chain.sentExternal[0].body.BeginParse()); err != nil {
		t.Fatalf("parse external request failed: %v", err)
	}
	var signed ExternalSignedSendBody
	if err = tlb.LoadFromCell(&signed, req.SignedBody.BeginParse()); err != nil {
		t.Fatalf("parse external signed body failed: %v", err)
	}
	msgs, err := payments.UnpackOutActions(signed.OutActions)
	if err != nil {
		t.Fatalf("unpack out actions failed: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("unexpected external messages count %d", len(msgs))
	}
	if !msgs[0].InternalMessage.DstAddr.Equals(wallet.addr) {
		t.Fatalf("withdraw target mismatch")
	}
	if msgs[0].InternalMessage.Amount.String() != "1" {
		t.Fatalf("unexpected withdraw amount %s", msgs[0].InternalMessage.Amount.String())
	}
}

func TestManagerReconcileTopUpJetton(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key failed: %v", err)
	}

	root := mustDerivedAddr("jetton-root")
	jetton := testJettonClient{root: root}
	chain := newTestChain()
	wallet := &testWallet{addr: mustDerivedAddr("wallet")}

	manager, err := NewManager(chain, wallet, key, map[string]*payments.CoinConfig{
		"USDT": {
			Symbol:       "USDT",
			Decimals:     6,
			BalanceID:    payments.GetJettonBalanceID(root),
			JettonClient: jetton,
		},
	}, map[string]CoinLimits{
		"USDT": {
			MinBalance: tlb.MustFromDecimal("20", 6),
			MaxBalance: tlb.MustFromDecimal("30", 6),
		},
	})
	if err != nil {
		t.Fatalf("new manager failed: %v", err)
	}

	chain.accounts[manager.Address().String()] = mustVaultAccount(t, key, tlb.MustFromTON("0.2"), nil)
	chain.jettonBalances[root.String()+"|"+manager.Address().String()] = tlb.MustFromDecimal("10", 6).Nano()

	if err = manager.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}
	if len(wallet.requests) != 1 {
		t.Fatalf("expected 1 jetton top up, got %d", len(wallet.requests))
	}

	msg := wallet.requests[0][0]
	if msg.Body == nil {
		t.Fatalf("jetton top up body is nil")
	}
	var payload jettonTransferPayload
	if err = tlb.LoadFromCell(&payload, msg.Body.BeginParse()); err != nil {
		t.Fatalf("parse jetton payload failed: %v", err)
	}
	if !payload.Destination.Equals(manager.Address()) {
		t.Fatalf("jetton destination mismatch")
	}
	if payload.Amount.Nano().Cmp(tlb.MustFromDecimal("20", 6).Nano()) != 0 {
		t.Fatalf("unexpected jetton top up amount %s", payload.Amount.String())
	}
}

func TestManagerTopUpECUsesSignedBody(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key failed: %v", err)
	}

	wallet := &testWallet{addr: mustDerivedAddr("wallet")}
	manager, err := NewManager(newTestChain(), wallet, key, map[string]*payments.CoinConfig{
		"USDX": {
			Symbol:    "USDX",
			Decimals:  4,
			BalanceID: payments.GetECBalanceID(7),
		},
	}, nil)
	if err != nil {
		t.Fatalf("new manager failed: %v", err)
	}

	coin := &payments.CoinConfig{
		Symbol:    "USDX",
		Decimals:  4,
		BalanceID: payments.GetECBalanceID(7),
	}
	if _, err = manager.TopUp(context.Background(), coin, tlb.MustFromDecimal("12.5", 4).Nano()); err != nil {
		t.Fatalf("ec top up failed: %v", err)
	}
	if len(wallet.requests) != 1 {
		t.Fatalf("expected single wallet request, got %d", len(wallet.requests))
	}

	msg := wallet.requests[0][0]
	if msg.To == nil || !msg.To.Equals(manager.Address()) {
		t.Fatalf("ec top up target mismatch")
	}
	if msg.Body == nil {
		t.Fatalf("ec top up body must be signed for active vault")
	}
	assertSignedTopUpBody(t, msg.Body, wallet.addr, manager.Address(), key.Public().(ed25519.PublicKey), 1)
	if msg.EC == nil || msg.EC[7].Nano().Cmp(tlb.MustFromDecimal("12.5", 4).Nano()) != 0 {
		t.Fatalf("unexpected ec amount in top up")
	}
}

func TestManagerReconcileWithdrawEC(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key failed: %v", err)
	}

	const ecID = 7

	chain := newTestChain()
	wallet := &testWallet{addr: mustDerivedAddr("wallet")}

	manager, err := NewManager(chain, wallet, key, map[string]*payments.CoinConfig{
		"USDX": {
			Symbol:    "USDX",
			Decimals:  4,
			BalanceID: payments.GetECBalanceID(ecID),
		},
	}, map[string]CoinLimits{
		"USDX": {
			MinBalance: tlb.MustFromDecimal("10", 4),
			MaxBalance: tlb.MustFromDecimal("20", 4),
		},
	})
	if err != nil {
		t.Fatalf("new manager failed: %v", err)
	}

	chain.accounts[manager.Address().String()] = mustVaultAccount(t, key, tlb.MustFromTON("0.2"), map[uint32]*big.Int{
		ecID: tlb.MustFromDecimal("35", 4).Nano(),
	})

	if err = manager.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}
	if len(chain.sentExternal) != 1 {
		t.Fatalf("expected 1 extra currency withdraw, got %d", len(chain.sentExternal))
	}

	var req ExternalSignedRequest
	if err = tlb.LoadFromCell(&req, chain.sentExternal[0].body.BeginParse()); err != nil {
		t.Fatalf("parse external request failed: %v", err)
	}
	var signed ExternalSignedSendBody
	if err = tlb.LoadFromCell(&signed, req.SignedBody.BeginParse()); err != nil {
		t.Fatalf("parse external signed body failed: %v", err)
	}
	msgs, err := payments.UnpackOutActions(signed.OutActions)
	if err != nil {
		t.Fatalf("unpack out actions failed: %v", err)
	}
	ecs, err := msgs[0].InternalMessage.ExtraCurrencies.LoadAll()
	if err != nil {
		t.Fatalf("load extra currencies failed: %v", err)
	}
	if len(ecs) != 1 {
		t.Fatalf("expected 1 extra currency entry, got %d", len(ecs))
	}
	if amount := ecs[0].Value.MustLoadVarUInt(32); amount.Cmp(tlb.MustFromDecimal("15", 4).Nano()) != 0 {
		t.Fatalf("unexpected extra currency withdraw amount %s", amount.String())
	}
}

func mustVaultAccount(t *testing.T, key ed25519.PrivateKey, tonBalance tlb.Coins, ecs map[uint32]*big.Int) *chainclient.Account {
	t.Helper()

	stateInit, err := BuildStateInitFromPrivateKey(key)
	if err != nil {
		t.Fatalf("build state init failed: %v", err)
	}
	addr, err := AddressFromStateInit(stateInit)
	if err != nil {
		t.Fatalf("build address failed: %v", err)
	}

	extraCurrencies := cell.NewDict(32)
	for id, amount := range ecs {
		if err = extraCurrencies.SetIntKey(big.NewInt(int64(id)), cell.BeginCell().MustStoreBigVarUInt(amount, 32).EndCell()); err != nil {
			t.Fatalf("set extra currency failed: %v", err)
		}
	}

	return &chainclient.Account{
		Address:         addr,
		Balance:         tonBalance,
		ExtraCurrencies: extraCurrencies,
		HasState:        true,
		IsActive:        true,
		Code:            Code,
		Data:            stateInit.Data,
	}
}

func assertSignedTopUpBody(t *testing.T, body *cell.Cell, sender, vault *address.Address, pub ed25519.PublicKey, expectedNano uint64) {
	t.Helper()

	var req InternalSignedSenderRequest
	if err := tlb.LoadFromCell(&req, body.BeginParse()); err != nil {
		t.Fatalf("parse internal signed request failed: %v", err)
	}
	if len(req.Signature) != ed25519.SignatureSize {
		t.Fatalf("unexpected internal signature length %d", len(req.Signature))
	}

	var msg tlb.InternalMessage
	if err := tlb.LoadFromCell(&msg, req.Message.BeginParse()); err != nil {
		t.Fatalf("parse inner top up message failed: %v", err)
	}
	if msg.DstAddr == nil || !msg.DstAddr.Equals(sender) {
		t.Fatalf("inner top up target mismatch")
	}
	if msg.Amount.Nano().Cmp(new(big.Int).SetUint64(expectedNano)) != 0 {
		t.Fatalf("unexpected inner top up amount %s", msg.Amount.String())
	}

	pairCell, err := tlb.ToCell(PairToSign{
		Sender:   sender,
		Receiver: vault,
	})
	if err != nil {
		t.Fatalf("serialize signed pair failed: %v", err)
	}
	if !ed25519.Verify(pub, pairCell.Hash(), req.Signature) {
		t.Fatalf("invalid internal top up signature")
	}
}

func cloneAccount(acc *chainclient.Account) *chainclient.Account {
	if acc == nil {
		return nil
	}

	clone := *acc
	return &clone
}

func mustDerivedAddr(seed string) *address.Address {
	hash := sha256.Sum256([]byte(seed))
	return address.NewAddress(0, 0, hash[:])
}
