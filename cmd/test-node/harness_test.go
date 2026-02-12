package testnode

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/pkg/payments/conditionals/oracle"
	"github.com/xssnick/ton-payment-network/tonpayments"
	chainclient "github.com/xssnick/ton-payment-network/tonpayments/chain/client"
	cfgpkg "github.com/xssnick/ton-payment-network/tonpayments/config"
	dbpkg "github.com/xssnick/ton-payment-network/tonpayments/db"
	"github.com/xssnick/ton-payment-network/tonpayments/db/leveldb"
	"github.com/xssnick/ton-payment-network/tonpayments/transport"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tl"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

type testNode struct {
	idx         int
	port        int
	key         ed25519.PrivateKey
	pub         ed25519.PublicKey
	transport   *transport.Transport
	svc         *tonpayments.Service
	derivatives *tonpayments.DerivativesService
	db          *dbpkg.DB
	wallet      *stubWallet
	done        chan struct{}
}

func newTestNode(t *testing.T, hub *loopbackHub, idx, port int) *testNode {
	t.Helper()

	cfg, err := cfgpkg.Generate()
	if err != nil {
		t.Fatalf("node %d: generate config failed: %v", idx, err)
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("node %d: generate node key failed: %v", idx, err)
	}

	dbPath := filepath.Join(t.TempDir(), fmt.Sprintf("node-%d-db", idx))
	storage, fresh, err := leveldb.NewLevelDB(dbPath)
	if err != nil {
		t.Fatalf("node %d: open db failed: %v", idx, err)
	}

	database := dbpkg.NewDB(storage, pub)
	if fresh {
		if err = database.SetMigrationVersion(context.Background(), len(dbpkg.Migrations)); err != nil {
			t.Fatalf("node %d: set migration version failed: %v", idx, err)
		}
	} else {
		if err = dbpkg.RunMigrations(database); err != nil {
			t.Fatalf("node %d: run migrations failed: %v", idx, err)
		}
	}

	net := newLoopbackNet(hub, pub, port)
	tr := transport.NewTransport(priv, net, false)

	wallet := newStubWallet(pub)
	updates := make(chan any, 64)

	svc, err := tonpayments.NewService(&stubChainAPI{}, database, tr, nil, wallet, updates, priv, cfg.ChannelConfig, false)
	if err != nil {
		t.Fatalf("node %d: init service failed: %v", idx, err)
	}
	tr.SetService(svc)

	return &testNode{
		idx:         idx,
		port:        port,
		key:         priv,
		pub:         pub,
		transport:   tr,
		svc:         svc,
		derivatives: tonpayments.NewDerivativesService(svc),
		db:          database,
		wallet:      wallet,
		done:        make(chan struct{}),
	}
}

func (n *testNode) start() {
	go func() {
		defer close(n.done)
		n.svc.Start()
	}()
}

func (n *testNode) stop(t *testing.T) {
	t.Helper()

	n.svc.Stop()
	n.transport.Stop()

	select {
	case <-n.done:
	case <-time.After(4 * time.Second):
		t.Logf("node %d stop timeout; continuing cleanup", n.idx)
	}

	n.db.Close()
}

func waitChannelByPeer(t *testing.T, node *testNode, peerKey ed25519.PublicKey) *dbpkg.Channel {
	t.Helper()

	var out *dbpkg.Channel
	waitFor(t, 20*time.Second, 250*time.Millisecond, func() (bool, string) {
		list, err := node.svc.ListChannels(context.Background(), nil, dbpkg.ChannelStateAny)
		if err != nil {
			return false, fmt.Sprintf("node %d list channels failed: %v", node.idx, err)
		}

		for _, ch := range list {
			if bytes.Equal(ch.Their.OnchainInfo.Key, peerKey) {
				out = ch
				return true, fmt.Sprintf("node %d channel with peer found: %s", node.idx, ch.Our.Address)
			}
		}
		return false, fmt.Sprintf("node %d has no channel with target peer yet", node.idx)
	})
	return out
}

func waitActiveChannelReady(t *testing.T, node *testNode, channelAddr string) {
	t.Helper()

	waitFor(t, 20*time.Second, 250*time.Millisecond, func() (bool, string) {
		_, err := node.svc.GetActiveChannel(context.Background(), channelAddr)
		if err != nil {
			return false, fmt.Sprintf("node %d waiting active channel %s: %v", node.idx, channelAddr, err)
		}
		return true, fmt.Sprintf("node %d active channel %s is ready", node.idx, channelAddr)
	})
}

func openDerivativeForTest(ctx context.Context, node *testNode, channelAddr string, isLong bool, leverage int, amountDecimal string) (string, error) {
	side := "short"
	if isLong {
		side = "long"
	}

	return node.derivatives.OpenPosition(ctx, channelAddr, "BTC", side, leverage, amountDecimal, "market", "")
}

func seedChannelTONBalance(t *testing.T, node *testNode, channelAddr string, amount *big.Int) {
	t.Helper()

	deadline := time.Now().Add(6 * time.Second)
	tonBalanceID := payments.GetTONBalanceID()

	for {
		ch, err := node.db.GetChannel(context.Background(), channelAddr)
		if err != nil {
			t.Fatalf("node %d get channel %s failed: %v", node.idx, channelAddr, err)
		}

		if ch.Our.OnchainBalances == nil {
			ch.Our.OnchainBalances = map[string]*big.Int{}
		}
		if ch.Their.OnchainBalances == nil {
			ch.Their.OnchainBalances = map[string]*big.Int{}
		}

		ch.Our.ActiveOnchain = true
		ch.Their.ActiveOnchain = true
		ch.Our.OnchainBalances[tonBalanceID] = new(big.Int).Set(amount)
		ch.Their.OnchainBalances[tonBalanceID] = new(big.Int).Set(amount)

		err = node.db.UpdateChannel(context.Background(), ch)
		if err == nil {
			return
		}

		if strings.Contains(err.Error(), "version mismatch") && time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		t.Fatalf("node %d update channel %s failed: %v", node.idx, channelAddr, err)
	}
}

func listActiveIncomingDerivativeKeys(node *testNode) ([]ed25519.PublicKey, error) {
	keys := make([]ed25519.PublicKey, 0, 4)
	err := node.db.ForEachActiveSpecialMetaKey(context.Background(), func(key ed25519.PublicKey) error {
		keys = append(keys, append(ed25519.PublicKey{}, key...))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return keys, nil
}

type derivativeMonitorStateJSON struct {
	LastCheckedAt int64 `json:"last_checked_at"`
	EntryCrossed  bool  `json:"entry_crossed"`
	Liquidated    bool  `json:"liquidated"`
}

func derivativeMonitor(meta *dbpkg.ConditionalMeta) (derivativeMonitorStateJSON, bool) {
	if meta == nil || meta.SpecialDetails == nil {
		return derivativeMonitorStateJSON{}, false
	}

	raw, err := json.Marshal(meta.SpecialDetails)
	if err != nil {
		return derivativeMonitorStateJSON{}, false
	}

	var state struct {
		Monitor derivativeMonitorStateJSON `json:"monitor"`
	}
	if err = json.Unmarshal(raw, &state); err != nil {
		return derivativeMonitorStateJSON{}, false
	}

	return state.Monitor, true
}

func isDerivativeMonitorLiquidated(meta *dbpkg.ConditionalMeta) bool {
	monitor, ok := derivativeMonitor(meta)
	return ok && monitor.Liquidated
}

type signedMockProvider struct {
	mu    sync.RWMutex
	price *big.Int
	key   ed25519.PrivateKey
}

func newSignedMockProvider(initial *big.Int) *signedMockProvider {
	seed := sha256.Sum256([]byte("testnode-mock-btc-proof-signer"))

	p := &signedMockProvider{
		key: ed25519.NewKeyFromSeed(seed[:]),
	}
	if initial != nil {
		p.price = new(big.Int).Set(initial)
	} else {
		p.price = big.NewInt(0)
	}
	return p
}

func (m *signedMockProvider) Fetch(_ context.Context, at int64) (int64, *big.Int, error) {
	m.mu.RLock()
	price := new(big.Int).Set(m.price)
	m.mu.RUnlock()
	return at, price, nil
}

func (m *signedMockProvider) SetPrice(p *big.Int) {
	if p == nil {
		return
	}

	m.mu.Lock()
	m.price = new(big.Int).Set(p)
	m.mu.Unlock()
}

func (m *signedMockProvider) ProofPublicKey() ed25519.PublicKey {
	return append(ed25519.PublicKey(nil), m.key.Public().(ed25519.PublicKey)...)
}

func (m *signedMockProvider) SignProofCell(proof *cell.Cell) ([]byte, error) {
	if proof == nil {
		return nil, fmt.Errorf("proof cell is nil")
	}
	return proof.Sign(m.key), nil
}

func installMockBTCResolver(t *testing.T, initial string) *oracle.Resolver {
	t.Helper()

	initialCoins, err := tlb.FromDecimal(initial, 9)
	if err != nil {
		t.Fatalf("parse initial mock price %q failed: %v", initial, err)
	}

	resolverID := oracle.GetResolverID("binance", "BTCUSDT")
	prev, hadPrev := oracle.PriceResolvers[resolverID]

	resolver := oracle.NewResolver(newSignedMockProvider(initialCoins.Nano()))
	oracle.PriceResolvers[resolverID] = resolver

	t.Cleanup(func() {
		resolver.Close()
		if hadPrev {
			oracle.PriceResolvers[resolverID] = prev
		} else {
			delete(oracle.PriceResolvers, resolverID)
		}
	})

	return resolver
}

func waitFor(t *testing.T, timeout, interval time.Duration, check func() (bool, string)) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	var lastMsg string

	for time.Now().Before(deadline) {
		ok, msg := check()
		if msg != "" {
			lastMsg = msg
		}
		if ok {
			if lastMsg != "" {
				t.Log(lastMsg)
			}
			return
		}
		time.Sleep(interval)
	}

	t.Fatalf("timeout: %s", lastMsg)
}

type loopbackHub struct {
	mu       sync.RWMutex
	byPubKey map[string]*loopbackNet
}

func newLoopbackHub() *loopbackHub {
	return &loopbackHub{byPubKey: map[string]*loopbackNet{}}
}

func (h *loopbackHub) register(net *loopbackNet) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.byPubKey[string(net.pub)] = net
}

func (h *loopbackHub) get(pub ed25519.PublicKey) *loopbackNet {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.byPubKey[string(pub)]
}

type loopbackNet struct {
	hub *loopbackHub
	pub ed25519.PublicKey
	id  [32]byte

	mu                sync.RWMutex
	queryHandler      func(ctx context.Context, peer *transport.Peer, msg any) (any, error)
	disconnectHandler func(ctx context.Context, peer *transport.Peer) error
}

func newLoopbackNet(hub *loopbackHub, pub ed25519.PublicKey, port int) *loopbackNet {
	net := &loopbackNet{
		hub: hub,
		pub: append(ed25519.PublicKey{}, pub...),
		id:  sha256.Sum256([]byte(fmt.Sprintf("loopback-%d", port))),
	}
	hub.register(net)
	return net
}

func (n *loopbackNet) GetOurID() []byte {
	return append([]byte{}, n.id[:]...)
}

func (n *loopbackNet) SetHandlers(q func(ctx context.Context, peer *transport.Peer, msg any) (any, error), d func(ctx context.Context, peer *transport.Peer) error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.queryHandler = q
	n.disconnectHandler = d
}

func (n *loopbackNet) Connect(_ context.Context, channelKey ed25519.PublicKey) (*transport.Peer, error) {
	remote := n.hub.get(channelKey)
	if remote == nil {
		return nil, fmt.Errorf("peer %s not found in loopback hub", base64.StdEncoding.EncodeToString(channelKey))
	}

	localPeer := &transport.Peer{ID: remote.GetOurID()}
	remotePeer := &transport.Peer{ID: n.GetOurID()}

	forward := &loopbackConn{
		remote:       remote,
		peerAtRemote: remotePeer,
	}
	backward := &loopbackConn{
		remote:       n,
		peerAtRemote: localPeer,
	}

	localPeer.Conn = forward
	remotePeer.Conn = backward
	return localPeer, nil
}

type loopbackConn struct {
	remote       *loopbackNet
	peerAtRemote *transport.Peer
}

func (c *loopbackConn) Query(ctx context.Context, msg, res tl.Serializable) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	c.remote.mu.RLock()
	h := c.remote.queryHandler
	c.remote.mu.RUnlock()
	if h == nil {
		return fmt.Errorf("remote loopback query handler is not set")
	}

	out, err := h(ctx, c.peerAtRemote, msg)
	if err != nil {
		return err
	}

	if res == nil || out == nil {
		return nil
	}

	dst := reflect.ValueOf(res)
	if dst.Kind() != reflect.Ptr || dst.IsNil() {
		return fmt.Errorf("response must be a non-nil pointer")
	}

	src := reflect.ValueOf(out)
	if !src.Type().AssignableTo(dst.Elem().Type()) {
		return fmt.Errorf("response type mismatch: got %T, want %s", out, dst.Elem().Type())
	}
	dst.Elem().Set(src)
	return nil
}

type stubChainAPI struct{}

func (s *stubChainAPI) GetAccount(_ context.Context, addr *address.Address, _ time.Time) (*chainclient.Account, error) {
	return &chainclient.Account{
		Address:         addr,
		Balance:         tlb.MustFromTON("1000"),
		ExtraCurrencies: cell.NewDict(32),
		HasState:        true,
		IsActive:        true,
	}, nil
}

func (s *stubChainAPI) GetJettonWalletAddress(_ context.Context, root, addr *address.Address) (*address.Address, error) {
	seed := append(append([]byte{}, root.Data()...), addr.Data()...)
	h := sha256.Sum256(seed)
	return address.NewAddress(0, 0, h[:]), nil
}

func (s *stubChainAPI) GetJettonBalance(_ context.Context, _ *address.Address, _ *address.Address, _ time.Time) (*big.Int, error) {
	return big.NewInt(0), nil
}

func (s *stubChainAPI) GetLastTransaction(_ context.Context, addr *address.Address, _ time.Time) (*chainclient.Transaction, *chainclient.Account, error) {
	acc, _ := s.GetAccount(context.Background(), addr, time.Time{})
	return nil, acc, nil
}

func (s *stubChainAPI) GetTransactionByInMsgHash(_ context.Context, _ *address.Address, _ []byte, _ time.Time) (*chainclient.Transaction, error) {
	return nil, nil
}

func (s *stubChainAPI) SendWaitExternalMessage(_ context.Context, to *address.Address, body *cell.Cell) ([]byte, error) {
	payload := append([]byte{}, to.Data()...)
	if body != nil {
		payload = append(payload, body.Hash()...)
	}
	h := sha256.Sum256(payload)
	return h[:], nil
}

type stubWallet struct {
	addr *address.Address
}

func newStubWallet(seed ed25519.PublicKey) *stubWallet {
	h := sha256.Sum256(seed)
	return &stubWallet{addr: address.NewAddress(0, 0, h[:])}
}

func (w *stubWallet) WalletAddress() *address.Address {
	return w.addr
}

func (w *stubWallet) DoTransactionMany(_ context.Context, reason string, messages []tonpayments.WalletMessage) ([]byte, error) {
	hash := sha256.New()
	hash.Write([]byte(reason))
	for _, msg := range messages {
		if msg.To != nil {
			hash.Write(msg.To.Data())
		}
		hash.Write(msg.Amount.Nano().Bytes())
	}
	sum := hash.Sum(nil)
	return append([]byte{}, sum[:32]...), nil
}

func (w *stubWallet) DoTransaction(ctx context.Context, reason string, to *address.Address, amt tlb.Coins, body *cell.Cell) ([]byte, error) {
	return w.DoTransactionMany(ctx, reason, []tonpayments.WalletMessage{{
		To:     to,
		Amount: amt,
		Body:   body,
	}})
}
