package vault

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/xssnick/ton-payment-network/pkg/payments"
	chainclient "github.com/xssnick/ton-payment-network/tonpayments/chain/client"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

var (
	DefaultDeployAmount  = tlb.MustFromTON("0.05")
	DefaultWorkerTimeout = 30 * time.Second
	DefaultActionLockTTL = 2 * time.Minute
)

type WalletMessage struct {
	To        *address.Address
	Amount    tlb.Coins
	Body      *cell.Cell
	StateInit *tlb.StateInit
	EC        map[uint32]tlb.Coins
}

type Wallet interface {
	WalletAddress() *address.Address
	DoTransactionMany(ctx context.Context, reason string, messages []WalletMessage) ([]byte, error)
}

type Chain interface {
	GetAccount(ctx context.Context, addr *address.Address, blockAfter time.Time) (*chainclient.Account, error)
	GetJettonWalletAddress(ctx context.Context, root, addr *address.Address) (*address.Address, error)
	GetJettonBalance(ctx context.Context, root, addr *address.Address, blockAfter time.Time) (*big.Int, error)
	SendWaitExternalMessage(ctx context.Context, to *address.Address, body *cell.Cell) ([]byte, error)
}

type CoinLimits struct {
	MinBalance tlb.Coins
	MaxBalance tlb.Coins
}

type Snapshot struct {
	Address     *address.Address
	Account     *chainclient.Account
	Deployed    bool
	Active      bool
	CodeMatches bool
	Storage     *Storage
	Balances    map[string]*big.Int
}

type Manager struct {
	chain      Chain
	wallet     Wallet
	privateKey ed25519.PrivateKey
	publicKey  ed25519.PublicKey
	stateInit  *tlb.StateInit
	address    *address.Address

	coins  map[string]*payments.CoinConfig
	limits map[string]CoinLimits

	actionLockTTL time.Duration

	mx         sync.Mutex
	lastAction map[string]time.Time
}

func NewManager(chain Chain, wallet Wallet, privateKey ed25519.PrivateKey, knownCoins map[string]*payments.CoinConfig, limits map[string]CoinLimits) (*Manager, error) {
	if chain == nil {
		return nil, fmt.Errorf("vault chain is nil")
	}
	if len(privateKey) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("invalid vault private key length %d", len(privateKey))
	}

	stateInit, err := BuildStateInitFromPrivateKey(privateKey)
	if err != nil {
		return nil, err
	}
	addr, err := AddressFromStateInit(stateInit)
	if err != nil {
		return nil, err
	}

	coins := map[string]*payments.CoinConfig{}
	for symbol, coin := range knownCoins {
		if coin == nil {
			continue
		}
		coins[normalizeSymbol(symbol)] = coin
	}

	cleanLimits := map[string]CoinLimits{}
	for symbol, limit := range limits {
		cleanLimits[normalizeSymbol(symbol)] = limit
	}

	return &Manager{
		chain:         chain,
		wallet:        wallet,
		privateKey:    privateKey,
		publicKey:     privateKey.Public().(ed25519.PublicKey),
		stateInit:     stateInit,
		address:       addr,
		coins:         coins,
		limits:        cleanLimits,
		actionLockTTL: DefaultActionLockTTL,
		lastAction:    map[string]time.Time{},
	}, nil
}

func (m *Manager) Address() *address.Address {
	if m == nil {
		return nil
	}
	return m.address
}

func (m *Manager) StateInit() *tlb.StateInit {
	if m == nil {
		return nil
	}
	return m.stateInit
}

func (m *Manager) BuildVaultData(sender, target *address.Address) (*payments.VaultData, error) {
	if m == nil {
		return nil, fmt.Errorf("vault manager is nil")
	}
	return BuildVaultData(m.privateKey, sender, target)
}

func (m *Manager) EnsureDeployed(ctx context.Context) error {
	if m == nil {
		return fmt.Errorf("vault manager is nil")
	}
	if m.wallet == nil {
		return fmt.Errorf("vault wallet is nil")
	}

	snapshot, err := m.Snapshot(ctx, m.address, nil)
	if err != nil {
		return err
	}

	if snapshot.Deployed {
		if !snapshot.CodeMatches {
			return fmt.Errorf("vault address %s is occupied by another contract", m.address.String())
		}
		return nil
	}

	_, err = m.wallet.DoTransactionMany(ctx, "Deploy vault", []WalletMessage{{
		Amount:    DefaultDeployAmount,
		StateInit: m.stateInit,
	}})
	if err != nil {
		return fmt.Errorf("deploy vault: %w", err)
	}

	return nil
}

func (m *Manager) Snapshot(ctx context.Context, addr *address.Address, symbols []string) (*Snapshot, error) {
	if m == nil {
		return nil, fmt.Errorf("vault manager is nil")
	}
	if addr == nil {
		addr = m.address
	}
	if addr == nil {
		return nil, fmt.Errorf("vault address is nil")
	}

	coins, err := m.coinsForSymbols(symbols)
	if err != nil {
		return nil, err
	}

	acc, err := m.chain.GetAccount(ctx, addr, time.Time{})
	if err != nil {
		return nil, fmt.Errorf("load vault account %s: %w", addr.String(), err)
	}

	snapshot := &Snapshot{
		Address:  addr,
		Account:  acc,
		Balances: map[string]*big.Int{},
	}

	for symbol := range coins {
		snapshot.Balances[symbol] = big.NewInt(0)
	}

	if acc == nil {
		return snapshot, nil
	}

	snapshot.Deployed = acc.HasState
	snapshot.Active = acc.IsActive
	snapshot.CodeMatches = acc.Code != nil && string(acc.Code.Hash()) == string(Code.Hash())

	if acc.HasState && acc.Data != nil && snapshot.CodeMatches {
		storage, err := LoadStorage(acc.Data)
		if err != nil {
			return nil, err
		}
		snapshot.Storage = storage
	}

	for symbol, coin := range coins {
		switch {
		case coin.BalanceID == payments.GetTONBalanceID():
			snapshot.Balances[symbol] = new(big.Int).Set(acc.Balance.Nano())
		case coin.JettonClient != nil:
			if !acc.HasState {
				continue
			}
			balance, err := m.chain.GetJettonBalance(ctx, coin.JettonClient.GetRootAddress(), addr, time.Time{})
			if err != nil {
				return nil, fmt.Errorf("load vault jetton balance %s: %w", symbol, err)
			}
			snapshot.Balances[symbol] = new(big.Int).Set(balance)
		default:
			ecID := payments.GetECFromBalanceID(coin.BalanceID)
			value := big.NewInt(0)
			if acc.ExtraCurrencies != nil && !acc.ExtraCurrencies.IsEmpty() {
				item, err := acc.ExtraCurrencies.LoadValueByIntKey(big.NewInt(int64(ecID)))
				if err == nil {
					amount, loadErr := item.LoadVarUInt(32)
					if loadErr != nil {
						return nil, fmt.Errorf("load vault extra currency %s: %w", symbol, loadErr)
					}
					value = amount
				}
			}
			snapshot.Balances[symbol] = value
		}
	}

	return snapshot, nil
}

func (m *Manager) Transfer(ctx context.Context, coin *payments.CoinConfig, to *address.Address, amount *big.Int) ([]byte, error) {
	if m == nil {
		return nil, fmt.Errorf("vault manager is nil")
	}
	if coin == nil {
		return nil, fmt.Errorf("vault coin config is nil")
	}
	if to == nil {
		return nil, fmt.Errorf("vault transfer target is nil")
	}
	if amount == nil || amount.Sign() <= 0 {
		return nil, fmt.Errorf("vault transfer amount must be positive")
	}

	snapshot, err := m.Snapshot(ctx, m.address, []string{coin.Symbol})
	if err != nil {
		return nil, err
	}
	if !snapshot.Deployed || !snapshot.CodeMatches {
		return nil, fmt.Errorf("vault is not deployed")
	}
	if snapshot.Storage == nil {
		return nil, fmt.Errorf("vault storage is unavailable")
	}

	messages, err := m.buildTransferMessages(ctx, coin, to, amount)
	if err != nil {
		return nil, err
	}
	body, err := BuildTransferBody(m.privateKey, snapshot.Storage, messages, time.Now().Add(DefaultExternalValidity))
	if err != nil {
		return nil, err
	}
	hash, err := m.chain.SendWaitExternalMessage(ctx, m.address, body)
	if err != nil {
		return nil, fmt.Errorf("send vault transfer external: %w", err)
	}
	return hash, nil
}

func (m *Manager) TopUp(ctx context.Context, coin *payments.CoinConfig, amount *big.Int) ([]byte, error) {
	if m == nil {
		return nil, fmt.Errorf("vault manager is nil")
	}
	if m.wallet == nil {
		return nil, fmt.Errorf("vault wallet is nil")
	}
	if coin == nil {
		return nil, fmt.Errorf("vault coin config is nil")
	}
	if amount == nil || amount.Sign() <= 0 {
		return nil, fmt.Errorf("vault top up amount must be positive")
	}

	signedInternalBody, err := m.buildSignedTopUpBody()
	if err != nil {
		return nil, err
	}

	var message WalletMessage
	switch {
	case coin.BalanceID == payments.GetTONBalanceID():
		value, err := tlb.FromNano(amount, int(coin.Decimals))
		if err != nil {
			return nil, fmt.Errorf("convert vault ton top up amount: %w", err)
		}
		message = WalletMessage{
			To:     m.address,
			Amount: value,
			Body:   signedInternalBody,
		}
	case coin.JettonClient != nil:
		walletAddr, err := m.chain.GetJettonWalletAddress(ctx, coin.JettonClient.GetRootAddress(), m.wallet.WalletAddress())
		if err != nil {
			return nil, fmt.Errorf("resolve wallet jetton address: %w", err)
		}

		payload, err := buildJettonTransferPayload(m.address, m.wallet.WalletAddress(), coin.MustAmount(amount), tlb.ZeroCoins, nil, nil)
		if err != nil {
			return nil, err
		}

		message = WalletMessage{
			To:     walletAddr,
			Amount: tlb.MustFromTON("0.05"),
			Body:   payload,
		}
	default:
		ec := payments.GetECFromBalanceID(coin.BalanceID)
		message = WalletMessage{
			To:     m.address,
			Amount: tlb.MustFromTON("0.001"),
			Body:   signedInternalBody,
			EC: map[uint32]tlb.Coins{
				ec: coin.MustAmount(amount),
			},
		}
	}

	hash, err := m.wallet.DoTransactionMany(ctx, "Top up vault "+coin.Symbol, []WalletMessage{message})
	if err != nil {
		return nil, fmt.Errorf("top up vault %s: %w", coin.Symbol, err)
	}
	return hash, nil
}

func (m *Manager) buildSignedTopUpBody() (*cell.Cell, error) {
	if m == nil {
		return nil, fmt.Errorf("vault manager is nil")
	}
	if m.wallet == nil {
		return nil, fmt.Errorf("vault wallet is nil")
	}

	signature, err := SignPair(m.privateKey, m.wallet.WalletAddress(), m.address)
	if err != nil {
		return nil, fmt.Errorf("sign vault top up request: %w", err)
	}

	keepAliveMessage, err := tlb.ToCell(tlb.InternalMessage{
		IHRDisabled: true,
		Bounce:      false,
		DstAddr:     m.wallet.WalletAddress(),
		Amount:      tlb.FromNanoTONU(1),
		Body:        cell.BeginCell().EndCell(),
	})
	if err != nil {
		return nil, fmt.Errorf("build vault top up keep-alive message: %w", err)
	}

	body, err := BuildInternalSignedRequest(signature, keepAliveMessage)
	if err != nil {
		return nil, fmt.Errorf("build vault top up signed request: %w", err)
	}
	return body, nil
}

func (m *Manager) Reconcile(ctx context.Context) error {
	if m == nil {
		return fmt.Errorf("vault manager is nil")
	}
	if err := m.EnsureDeployed(ctx); err != nil {
		return err
	}

	snapshot, err := m.Snapshot(ctx, m.address, nil)
	if err != nil {
		return err
	}

	for symbol, limits := range m.limits {
		coin := m.coins[symbol]
		if coin == nil {
			return fmt.Errorf("unknown vault coin %s", symbol)
		}

		balance := snapshot.Balances[symbol]
		if balance == nil {
			balance = big.NewInt(0)
		}

		minBalance, maxBalance := effectiveLimits(coin, limits)
		if maxBalance.Cmp(minBalance) < 0 {
			maxBalance = new(big.Int).Set(minBalance)
		}

		switch {
		case balance.Cmp(minBalance) < 0:
			if !m.canAct(symbol) {
				continue
			}
			topUpAmount := new(big.Int).Sub(maxBalance, balance)
			if topUpAmount.Sign() <= 0 {
				continue
			}
			if _, err = m.TopUp(ctx, coin, topUpAmount); err != nil {
				return err
			}
			m.markActed(symbol)
		case balance.Cmp(maxBalance) > 0:
			if !m.canAct(symbol) {
				continue
			}
			withdrawAmount := new(big.Int).Sub(balance, maxBalance)
			if withdrawAmount.Sign() <= 0 {
				continue
			}
			if _, err = m.Transfer(ctx, coin, m.wallet.WalletAddress(), withdrawAmount); err != nil {
				return err
			}
			m.markActed(symbol)
		}
	}

	return nil
}

func (m *Manager) RunWorker(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultWorkerTimeout
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		if err := m.Reconcile(ctx); err != nil {
			// caller decides how to log errors; monitor callback covers the public monitor helper
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (m *Manager) StartMonitor(ctx context.Context, addr *address.Address, interval time.Duration, symbols []string, fn func(*Snapshot, error)) {
	if interval <= 0 {
		interval = DefaultWorkerTimeout
	}

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			snapshot, err := m.Snapshot(ctx, addr, symbols)
			if fn != nil {
				fn(snapshot, err)
			}

			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (m *Manager) coinsForSymbols(symbols []string) (map[string]*payments.CoinConfig, error) {
	if len(symbols) == 0 {
		res := map[string]*payments.CoinConfig{}
		for symbol, coin := range m.coins {
			if _, ok := m.limits[symbol]; ok {
				res[symbol] = coin
			}
		}
		return res, nil
	}

	res := make(map[string]*payments.CoinConfig, len(symbols))
	for _, symbol := range symbols {
		normalized := normalizeSymbol(symbol)
		coin := m.coins[normalized]
		if coin == nil {
			return nil, fmt.Errorf("unknown vault coin %s", symbol)
		}
		res[normalized] = coin
	}
	return res, nil
}

func (m *Manager) buildTransferMessages(ctx context.Context, coin *payments.CoinConfig, to *address.Address, amount *big.Int) ([]payments.WalletMessage, error) {
	switch {
	case coin.BalanceID == payments.GetTONBalanceID():
		return []payments.WalletMessage{{
			Mode: 1 + 2,
			InternalMessage: &tlb.InternalMessage{
				IHRDisabled: true,
				Bounce:      to.IsBounceable(),
				DstAddr:     to,
				Amount:      coin.MustAmount(amount),
				Body:        cell.BeginCell().EndCell(),
			},
		}}, nil
	case coin.JettonClient != nil:
		jettonWallet, err := m.chain.GetJettonWalletAddress(ctx, coin.JettonClient.GetRootAddress(), m.address)
		if err != nil {
			return nil, fmt.Errorf("resolve vault jetton wallet: %w", err)
		}
		payload, err := buildJettonTransferPayload(to, m.address, coin.MustAmount(amount), tlb.ZeroCoins, nil, nil)
		if err != nil {
			return nil, err
		}
		return []payments.WalletMessage{{
			Mode: 1 + 2,
			InternalMessage: &tlb.InternalMessage{
				IHRDisabled: true,
				Bounce:      true,
				DstAddr:     jettonWallet,
				Amount:      tlb.MustFromTON("0.05"),
				Body:        payload,
			},
		}}, nil
	default:
		ec := payments.GetECFromBalanceID(coin.BalanceID)
		extraCurrencies := cell.NewDict(32)
		if err := extraCurrencies.SetIntKey(big.NewInt(int64(ec)), cell.BeginCell().MustStoreBigVarUInt(amount, 32).EndCell()); err != nil {
			return nil, fmt.Errorf("build vault extra currencies payload: %w", err)
		}
		return []payments.WalletMessage{{
			Mode: 1 + 2,
			InternalMessage: &tlb.InternalMessage{
				IHRDisabled:     true,
				Bounce:          to.IsBounceable(),
				DstAddr:         to,
				Amount:          tlb.MustFromTON("0.02"),
				Body:            cell.BeginCell().EndCell(),
				ExtraCurrencies: extraCurrencies,
			},
		}}, nil
	}
}

func (m *Manager) canAct(symbol string) bool {
	m.mx.Lock()
	defer m.mx.Unlock()

	last := m.lastAction[symbol]
	return last.IsZero() || time.Since(last) >= m.actionLockTTL
}

func (m *Manager) markActed(symbol string) {
	m.mx.Lock()
	m.lastAction[symbol] = time.Now()
	m.mx.Unlock()
}

func effectiveLimits(coin *payments.CoinConfig, limits CoinLimits) (*big.Int, *big.Int) {
	minBalance := new(big.Int).Set(limits.MinBalance.Nano())
	maxBalance := new(big.Int).Set(limits.MaxBalance.Nano())

	if coin != nil && coin.BalanceID == payments.GetTONBalanceID() {
		reserve := new(big.Int).Set(DefaultDeployAmount.Nano())
		if minBalance.Cmp(reserve) < 0 {
			minBalance = reserve
		}
		if maxBalance.Cmp(minBalance) < 0 {
			maxBalance = new(big.Int).Set(minBalance)
		}
	}

	return minBalance, maxBalance
}

func normalizeSymbol(symbol string) string {
	return strings.ToUpper(strings.TrimSpace(symbol))
}

type jettonTransferPayload struct {
	_                   tlb.Magic        `tlb:"#0f8a7ea5"`
	QueryID             uint64           `tlb:"## 64"`
	Amount              tlb.Coins        `tlb:"."`
	Destination         *address.Address `tlb:"addr"`
	ResponseDestination *address.Address `tlb:"addr"`
	CustomPayload       *cell.Cell       `tlb:"maybe ^"`
	ForwardTONAmount    tlb.Coins        `tlb:"."`
	ForwardPayload      *cell.Cell       `tlb:"either . ^"`
}

func buildJettonTransferPayload(to, responseTo *address.Address, amountCoins, amountForwardTON tlb.Coins, payloadForward, customPayload *cell.Cell) (*cell.Cell, error) {
	if payloadForward == nil {
		payloadForward = cell.BeginCell().EndCell()
	}

	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("generate jetton payload id: %w", err)
	}

	body, err := tlb.ToCell(jettonTransferPayload{
		QueryID:             binary.LittleEndian.Uint64(buf),
		Amount:              amountCoins,
		Destination:         to,
		ResponseDestination: responseTo,
		CustomPayload:       customPayload,
		ForwardTONAmount:    amountForwardTON,
		ForwardPayload:      payloadForward,
	})
	if err != nil {
		return nil, fmt.Errorf("serialize jetton transfer payload: %w", err)
	}
	return body, nil
}
