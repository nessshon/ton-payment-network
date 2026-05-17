package tonpayments

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math/big"
	"path/filepath"
	"testing"
	"time"

	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/pkg/payments/actions"
	"github.com/xssnick/ton-payment-network/pkg/payments/conditionals"
	condcontracts "github.com/xssnick/ton-payment-network/pkg/payments/conditionals/contracts"
	"github.com/xssnick/ton-payment-network/pkg/payments/conditionals/oracle"
	"github.com/xssnick/ton-payment-network/tonpayments/chain/client"
	"github.com/xssnick/ton-payment-network/tonpayments/db"
	dblevel "github.com/xssnick/ton-payment-network/tonpayments/db/leveldb"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

type testDerivativeChainAPI struct {
	accounts       map[string]*client.Account
	lastExternalTo *address.Address
	lastExternal   *cell.Cell
}

func (t *testDerivativeChainAPI) GetAccount(_ context.Context, addr *address.Address, _ time.Time) (*client.Account, error) {
	acc := t.accounts[addr.String()]
	if acc == nil {
		return &client.Account{
			Address:  addr,
			Balance:  tlb.MustFromTON("1"),
			HasState: true,
			IsActive: true,
		}, nil
	}
	cp := *acc
	return &cp, nil
}

func (t *testDerivativeChainAPI) GetJettonWalletAddress(context.Context, *address.Address, *address.Address) (*address.Address, error) {
	return nil, nil
}

func (t *testDerivativeChainAPI) GetJettonBalance(context.Context, *address.Address, *address.Address, time.Time) (*big.Int, error) {
	return nil, nil
}

func (t *testDerivativeChainAPI) GetLastTransaction(context.Context, *address.Address, time.Time) (*client.Transaction, *client.Account, error) {
	return nil, nil, nil
}

func (t *testDerivativeChainAPI) GetTransactionByInMsgHash(context.Context, *address.Address, []byte, time.Time) (*client.Transaction, error) {
	return nil, nil
}

func (t *testDerivativeChainAPI) SendWaitExternalMessage(_ context.Context, to *address.Address, body *cell.Cell) ([]byte, error) {
	t.lastExternalTo = to
	t.lastExternal = body
	return []byte("external"), nil
}

type testDerivativeWalletCall struct {
	Reason   string
	Messages []WalletMessage
}

type testDerivativeWallet struct {
	addr  *address.Address
	calls []testDerivativeWalletCall
}

func (t *testDerivativeWallet) WalletAddress() *address.Address {
	return t.addr
}

func (t *testDerivativeWallet) DoTransactionMany(_ context.Context, reason string, messages []WalletMessage) ([]byte, error) {
	cp := make([]WalletMessage, len(messages))
	copy(cp, messages)
	t.calls = append(t.calls, testDerivativeWalletCall{
		Reason:   reason,
		Messages: cp,
	})
	return []byte(reason), nil
}

func (t *testDerivativeWallet) DoTransaction(ctx context.Context, reason string, to *address.Address, amt tlb.Coins, body *cell.Cell) ([]byte, error) {
	return t.DoTransactionMany(ctx, reason, []WalletMessage{{
		To:     to,
		Amount: amt,
		Body:   body,
	}})
}

type testSignedFixedPriceProvider struct {
	key   ed25519.PrivateKey
	price *big.Int
}

func (t *testSignedFixedPriceProvider) Fetch(_ context.Context, at int64) (int64, *big.Int, error) {
	return at, new(big.Int).Set(t.price), nil
}

func (t *testSignedFixedPriceProvider) ProofPublicKey() ed25519.PublicKey {
	return append(ed25519.PublicKey(nil), t.key.Public().(ed25519.PublicKey)...)
}

func (t *testSignedFixedPriceProvider) SignProofCell(proof *cell.Cell) ([]byte, error) {
	return proof.Sign(t.key), nil
}

func (t *testSignedFixedPriceProvider) SetPrice(p *big.Int) {
	t.price = new(big.Int).Set(p)
}

func installTestSignedResolverForAsset(t *testing.T, assetID uint32, price *big.Int) {
	t.Helper()

	seed := sha256.Sum256([]byte("signed-resolver-test"))
	provider := &testSignedFixedPriceProvider{
		key:   ed25519.NewKeyFromSeed(seed[:]),
		price: new(big.Int).Set(price),
	}
	resolver := oracle.NewResolver(provider)
	if err := resolver.SetPrice(price); err != nil {
		t.Fatalf("failed to prime resolver price: %v", err)
	}

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

func buildTestChannelAccount(t *testing.T, storage payments.ChannelStorageData) (*address.Address, *client.Account) {
	t.Helper()

	data, err := tlb.ToCell(storage)
	if err != nil {
		t.Fatalf("failed to serialize channel storage: %v", err)
	}

	baseStorage := payments.ChannelStorageData{
		IsA:            storage.IsA,
		Initialized:    false,
		CommittedSeqno: 0,
		WalletSeqno:    0,
		KeyA:           storage.KeyA,
		KeyB:           storage.KeyB,
		ChannelID:      storage.ChannelID,
		ClosingConfig:  storage.ClosingConfig,
		PartyAddress:   nil,
		Quarantine:     nil,
	}
	baseData, err := tlb.ToCell(baseStorage)
	if err != nil {
		t.Fatalf("failed to serialize base channel storage: %v", err)
	}

	stateInit := tlb.StateInit{
		Code: payments.PaymentChannelCodes[0],
		Data: baseData,
	}
	stateCell, err := tlb.ToCell(stateInit)
	if err != nil {
		t.Fatalf("failed to serialize state init: %v", err)
	}

	addr := address.NewAddress(0, 0, stateCell.Hash())
	return addr, &client.Account{
		Address:  addr,
		Balance:  tlb.MustFromTON("1"),
		HasState: true,
		IsActive: true,
		Code:     stateInit.Code,
		Data:     data,
	}
}

func markTestResolverCommitted(t *testing.T, chainAPI *testDerivativeChainAPI, resolverAddr *address.Address, exitAt uint32, exitPrice *big.Int) {
	t.Helper()

	acc := chainAPI.accounts[resolverAddr.String()]
	if acc == nil || acc.Data == nil {
		t.Fatalf("resolver account is missing")
	}

	storage, err := condcontracts.LoadDerivativeStorage(acc.Data)
	if err != nil {
		t.Fatalf("failed to parse resolver storage: %v", err)
	}

	exitCoins, err := tlb.FromNano(exitPrice, 9)
	if err != nil {
		t.Fatalf("failed to convert exit price: %v", err)
	}

	storage.ExitAt = exitAt
	storage.ExitPrice = exitCoins
	if storage.QuarantineTill < exitAt {
		storage.QuarantineTill = exitAt
	}

	data, err := tlb.ToCell(storage)
	if err != nil {
		t.Fatalf("failed to serialize committed resolver storage: %v", err)
	}

	stateInit, err := condcontracts.BuildDerivativeStateInit(data)
	if err != nil {
		t.Fatalf("failed to build committed resolver state init: %v", err)
	}

	acc.Data = stateInit.Data
	acc.Code = stateInit.Code
}

func newDerivativeUncoopTestService(t *testing.T, isA bool) (*Service, *db.DB, *testDerivativeChainAPI, *testDerivativeWallet, *db.Channel, *conditionals.ConditionalResolvable, *db.ConditionalMeta, *address.Address) {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "db")
	storage, _, err := dblevel.NewLevelDB(dbPath)
	if err != nil {
		t.Fatalf("failed to open leveldb: %v", err)
	}

	database := db.NewDB(storage, nil)
	t.Cleanup(database.Close)

	walletAddr := testAddress(77)
	wallet := &testDerivativeWallet{addr: walletAddr}
	chainAPI := &testDerivativeChainAPI{accounts: map[string]*client.Account{
		walletAddr.String(): {
			Address:  walletAddr,
			Balance:  tlb.MustFromTON("2"),
			HasState: true,
			IsActive: true,
		},
	}}

	svc := &Service{
		db:                database,
		ton:               chainAPI,
		wallet:            wallet,
		channelClient:     payments.NewPaymentChannelClient(chainAPI),
		key:               ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, ed25519.SeedSize)),
		actionsCache:      map[string]payments.Action{},
		knownBalanceTypes: map[string]*payments.CoinConfig{},
	}

	coinCfg := &payments.CoinConfig{
		Enabled:   true,
		Decimals:  9,
		BalanceID: payments.GetTONBalanceID(),
	}
	svc.knownBalanceTypes[coinCfg.BalanceID] = coinCfg

	channelID := payments.ChannelID(bytes.Repeat([]byte{4}, 16))
	ourKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, ed25519.SeedSize))
	theirKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{2}, ed25519.SeedSize))
	partyAddr := testAddress(78)

	channelStorage := payments.ChannelStorageData{
		IsA:         isA,
		Initialized: true,
		WalletSeqno: 17,
		KeyA:        ourKey.Public().(ed25519.PublicKey),
		KeyB:        theirKey.Public().(ed25519.PublicKey),
		ChannelID:   channelID,
		ClosingConfig: payments.ClosingConfig{
			QuarantineDuration:             1,
			ConditionalCloseDuration:       120,
			ActionsDuration:                120,
			ReplicationMessageAttachAmount: tlb.ZeroCoins,
		},
		PartyAddress: partyAddr,
		Quarantine: &payments.QuarantinedState{
			Seqno:            1,
			QuarantineStarts: uint32(time.Now().Add(-5 * time.Second).Unix()),
			TheirState: &payments.StateSide{
				ConditionalsHash: make([]byte, 32),
				ActionStatesHash: make([]byte, 32),
			},
			ActionsToExecuteHash: make([]byte, 32),
		},
	}
	channelAddr, channelAccount := buildTestChannelAccount(t, channelStorage)
	chainAPI.accounts[channelAddr.String()] = channelAccount

	channel := &db.Channel{
		ID:                     channelID,
		Status:                 db.ChannelStateActive,
		WeLeft:                 true,
		SafeOnchainClosePeriod: 60,
		AcceptingActions:       true,
		InitAt:                 time.Now().UTC(),
		CreatedAt:              time.Now().UTC(),
		Our: db.Side{
			Address:                 channelAddr.String(),
			Data:                    db.NewAgreedData(),
			OnchainBalances:         map[string]*big.Int{},
			LockedDeposits:          map[string]*payments.LockedDepositInfo{},
			PendingOnchainTransfers: map[string]*payments.PendingMessageInfo{},
		},
		Their: db.Side{
			Address:                 partyAddr.String(),
			Data:                    db.NewAgreedData(),
			OnchainBalances:         map[string]*big.Int{},
			LockedDeposits:          map[string]*payments.LockedDepositInfo{},
			PendingOnchainTransfers: map[string]*payments.PendingMessageInfo{},
		},
	}

	act, err := actions.NewSendActionFromBalanceID(context.Background(), coinCfg, channelAddr.String(), partyAddr.String())
	if err != nil {
		t.Fatalf("failed to build action: %v", err)
	}
	if err = svc.SaveAction(context.Background(), act); err != nil {
		t.Fatalf("failed to save action: %v", err)
	}

	actionState, err := tlb.ToCell(actions.StateActionSend{
		Amount:        actions.Coins{Val: big.NewInt(0)},
		Commited:      actions.Coins{Val: big.NewInt(0)},
		CommitedSeqno: 0,
	})
	if err != nil {
		t.Fatalf("failed to serialize action state: %v", err)
	}
	if err = channel.Their.Data.ActionStates.Set(act.IDCell(), actionState); err != nil {
		t.Fatalf("failed to set action state: %v", err)
	}

	assetID := uint32(9123)
	installTestSignedResolverForAsset(t, assetID, big.NewInt(1_000_000_000))

	key := sha256.Sum256([]byte("uncoop-derivative"))
	details := conditionals.ConditionalResolvableDetails{
		AssetID:    assetID,
		IsLong:     true,
		Leverage:   2,
		EntryPrice: actions.Coins{Val: big.NewInt(2_000_000_000)},
	}
	amount := big.NewInt(1_000_000_000)
	stateInit, resolverAddr, _, err := svc.buildDerivativeResolverContract(channel, key[:], amount, details)
	if err != nil {
		t.Fatalf("failed to build derivative resolver contract: %v", err)
	}

	chainAPI.accounts[resolverAddr.String()] = &client.Account{
		Address:  resolverAddr,
		Balance:  tlb.MustFromTON("0.05"),
		HasState: true,
		IsActive: true,
		Code:     stateInit.Code,
		Data:     stateInit.Data,
	}

	cond := &conditionals.ConditionalResolvable{
		Key:          append([]byte(nil), key[:]...),
		Amount:       amount,
		Fee:          big.NewInt(0),
		IsInitiator:  true,
		ResolverAddr: resolverAddr,
		Details:      details,
		Action:       act,
	}
	if err = channel.Their.Data.Conditionals.SetIntKey(big.NewInt(7), cond.Serialize()); err != nil {
		t.Fatalf("failed to set conditional: %v", err)
	}

	meta := &db.ConditionalMeta{
		Key:       cond.GetKey(),
		Status:    db.ConditionalStateActive,
		CreatedAt: time.Now().Add(-10 * time.Second).UTC(),
		UpdatedAt: time.Now().Add(-10 * time.Second).UTC(),
	}

	if err = database.CreateChannel(context.Background(), channel); err != nil {
		t.Fatalf("failed to create channel: %v", err)
	}
	if err = database.CreateVirtualChannelMeta(context.Background(), meta); err != nil {
		t.Fatalf("failed to create conditional meta: %v", err)
	}

	return svc, database, chainAPI, wallet, channel, cond, meta, resolverAddr
}

func TestSettleChannelConditionals_PreparesDerivativeResolveAndExpectedSender(t *testing.T) {
	svc, database, _, wallet, channel, cond, _, resolverAddr := newDerivativeUncoopTestService(t, false)

	if err := svc.settleChannelConditionals(context.Background(), channel.Our.Address); err != nil {
		t.Fatalf("settle conditionals failed: %v", err)
	}

	if len(wallet.calls) != 1 {
		t.Fatalf("expected one resolver commit transaction, got %d", len(wallet.calls))
	}
	if len(wallet.calls[0].Messages) != 1 {
		t.Fatalf("expected one commit message, got %d", len(wallet.calls[0].Messages))
	}
	if !wallet.calls[0].Messages[0].To.Equals(resolverAddr) {
		t.Fatalf("resolver commit target mismatch")
	}

	var commit condcontracts.Commit
	if err := tlb.LoadFromCell(&commit, wallet.calls[0].Messages[0].Body.MustBeginParse()); err != nil {
		t.Fatalf("failed to parse resolver commit body: %v", err)
	}
	if commit.Entry.SignedBody == nil || commit.Exit.SignedBody == nil {
		t.Fatalf("resolver commit must contain entry and exit proofs")
	}

	proofKey := oracle.PriceResolvers[cond.Details.AssetID].GetProofPublicKey()
	entryProof, err := tlb.ToCell(commit.Entry)
	if err != nil {
		t.Fatalf("failed to serialize entry proof: %v", err)
	}
	exitProof, err := tlb.ToCell(commit.Exit)
	if err != nil {
		t.Fatalf("failed to serialize exit proof: %v", err)
	}
	if !oracle.VerifyProofCell(entryProof, proofKey) {
		t.Fatalf("entry proof signature is invalid")
	}
	if !oracle.VerifyProofCell(exitProof, proofKey) {
		t.Fatalf("exit proof signature is invalid")
	}

	meta, err := database.GetVirtualChannelMeta(context.Background(), cond.GetKey())
	if err != nil {
		t.Fatalf("failed to reload conditional meta: %v", err)
	}
	if meta.LastKnownResolve == nil {
		t.Fatalf("expected derivative resolve to be stored")
	}

	time.Sleep(time.Duration(derivativeResolverQuarantineDuration+1) * time.Second)

	tasks, err := database.ListActiveTasks(context.Background(), PaymentsTaskPool)
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}

	var settleTask *db.SettleConditionalStepTask
	for _, task := range tasks {
		if task.Type != "settle-step" {
			continue
		}
		var decoded db.SettleConditionalStepTask
		if err = json.Unmarshal(task.Data, &decoded); err != nil {
			t.Fatalf("failed to decode settle step task: %v", err)
		}
		settleTask = &decoded
		break
	}

	if settleTask == nil {
		t.Fatal("settle step task was not created")
	}

	var settleMsg payments.SettleMsg
	if err = tlb.LoadFromCell(&settleMsg, settleTask.Message.MustBeginParse()); err != nil {
		t.Fatalf("failed to parse settle message: %v", err)
	}
	if settleMsg.Signed.ExpectedSender == nil || !settleMsg.Signed.ExpectedSender.Equals(resolverAddr) {
		t.Fatalf("settle message expected sender mismatch")
	}
	if settleMsg.Signed.ToSettle == nil || settleMsg.Signed.ToSettle.IsEmpty() {
		t.Fatalf("settle message must contain derivative resolve")
	}
}

func TestExecuteSettleStep_ProxiesDerivativeViaResolver(t *testing.T) {
	svc, _, chainAPI, wallet, channel, _, _, resolverAddr := newDerivativeUncoopTestService(t, false)
	markTestResolverCommitted(t, chainAPI, resolverAddr, uint32(time.Now().Unix()), big.NewInt(1_000_000_000))

	msg := payments.SettleMsg{}
	msg.Signature.Value = make([]byte, ed25519.SignatureSize)
	msg.Signed.ChannelID = channel.ID
	msg.Signed.ExpectedSender = resolverAddr
	msg.Signed.ToSettle = cell.NewDict(256)
	msg.Signed.ConditionalsProof = cell.BeginCell().EndCell()
	msg.Signed.ActionsInputProof = cell.BeginCell().EndCell()

	raw, err := tlb.ToCell(msg)
	if err != nil {
		t.Fatalf("failed to serialize settle message: %v", err)
	}

	if err = svc.executeSettleStep(context.Background(), channel.Our.Address, raw, 0); err != nil {
		t.Fatalf("execute settle step failed: %v", err)
	}

	if len(wallet.calls) != 1 {
		t.Fatalf("expected one proxied settle transaction, got %d", len(wallet.calls))
	}
	if chainAPI.lastExternalTo != nil {
		t.Fatalf("derivative settle must not use direct external message")
	}

	call := wallet.calls[0]
	if len(call.Messages) != 1 {
		t.Fatalf("expected one wallet message, got %d", len(call.Messages))
	}
	if !call.Messages[0].To.Equals(resolverAddr) {
		t.Fatalf("proxy settle target mismatch")
	}

	var proxy condcontracts.ProxySettle
	if err = tlb.LoadFromCell(&proxy, call.Messages[0].Body.MustBeginParse()); err != nil {
		t.Fatalf("failed to parse proxy settle body: %v", err)
	}
	if proxy.ToA {
		t.Fatalf("expected proxy settle to target side B")
	}

	var proxied payments.SettleMsg
	if err = tlb.LoadFromCell(&proxied, proxy.Msg.MustBeginParse()); err != nil {
		t.Fatalf("failed to parse proxied settle message: %v", err)
	}
	if proxied.Signed.ExpectedSender == nil || !proxied.Signed.ExpectedSender.Equals(resolverAddr) {
		t.Fatalf("proxied settle message lost expected sender")
	}
	if proxied.Signed.WalletSeqno != 17 {
		t.Fatalf("wallet seqno was not refreshed, got %d", proxied.Signed.WalletSeqno)
	}
}

func TestExecuteSettleStep_DerivativeWaitsForCommittedResolverState(t *testing.T) {
	svc, _, chainAPI, wallet, channel, _, _, resolverAddr := newDerivativeUncoopTestService(t, false)

	msg := payments.SettleMsg{}
	msg.Signature.Value = make([]byte, ed25519.SignatureSize)
	msg.Signed.ChannelID = channel.ID
	msg.Signed.ExpectedSender = resolverAddr
	msg.Signed.ToSettle = cell.NewDict(256)
	msg.Signed.ConditionalsProof = cell.BeginCell().EndCell()
	msg.Signed.ActionsInputProof = cell.BeginCell().EndCell()

	raw, err := tlb.ToCell(msg)
	if err != nil {
		t.Fatalf("failed to serialize settle message: %v", err)
	}

	err = svc.executeSettleStep(context.Background(), channel.Our.Address, raw, 0)
	if err == nil || !errors.Is(err, ErrStillPending) {
		t.Fatalf("expected ErrStillPending, got %v", err)
	}
	if len(wallet.calls) != 0 {
		t.Fatalf("wallet must not send proxy settle while resolver is pending")
	}
	if chainAPI.lastExternalTo != nil {
		t.Fatalf("direct external send must not happen while resolver is pending")
	}
}

func TestExecuteSettleFinalize_WaitsForOnchainActionsHash(t *testing.T) {
	svc, _, chainAPI, _, channel, _, _, _ := newDerivativeUncoopTestService(t, false)

	expectedHash := bytes.Repeat([]byte{0xAB}, 32)

	err := svc.executeSettleFinalize(context.Background(), channel.Our.Address, expectedHash)
	if err == nil || !errors.Is(err, ErrStillPending) {
		t.Fatalf("expected ErrStillPending, got %v", err)
	}
	if chainAPI.lastExternalTo != nil {
		t.Fatalf("finalize must not be sent before onchain actions hash matches")
	}
}
