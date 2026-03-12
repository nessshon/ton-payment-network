package tonpayments

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"math/big"
	"path/filepath"
	"testing"
	"time"

	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/tonpayments/db"
	dblevel "github.com/xssnick/ton-payment-network/tonpayments/db/leveldb"
	"github.com/xssnick/ton-payment-network/tonpayments/transport"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
)

func TestProcessActionRequestExecuteTransactionAction_IdempotentRetryForSameWalletSeqno(t *testing.T) {
	svc, theirKey, channelAddr := newExecuteTxActionRequestTestService(t, 11, 11)

	res, err := svc.ProcessActionRequest(context.Background(), theirKey, channelAddr, testExecuteTransactionAction(t, 11, channelAddr.String(), svc.db))
	if err != nil {
		t.Fatalf("expected idempotent retry to succeed, got error: %v", err)
	}
	if res != nil {
		t.Fatalf("expected no extra signature for idempotent retry")
	}
}

func TestProcessActionRequestExecuteTransactionAction_RejectsDifferentPendingWalletSeqno(t *testing.T) {
	svc, theirKey, channelAddr := newExecuteTxActionRequestTestService(t, 11, 12)

	_, err := svc.ProcessActionRequest(context.Background(), theirKey, channelAddr, testExecuteTransactionAction(t, 11, channelAddr.String(), svc.db))
	if err == nil {
		t.Fatal("expected request with different pending wallet seqno to fail")
	}
	if err.Error() != "to execute action must be no pending onchain transfers" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func newExecuteTxActionRequestTestService(t *testing.T, walletSeqno uint32, pendingWalletSeqno uint32) (*Service, ed25519.PublicKey, *address.Address) {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "db")
	storage, _, err := dblevel.NewLevelDB(dbPath)
	if err != nil {
		t.Fatalf("failed to open leveldb: %v", err)
	}

	database := db.NewDB(storage, nil)
	t.Cleanup(database.Close)

	theirPriv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, ed25519.SeedSize))
	theirKey := theirPriv.Public().(ed25519.PublicKey)

	ourAddr := testAddress(1)
	theirAddr := testAddress(2)
	channelID := payments.ChannelID(bytes.Repeat([]byte{9}, 16))

	signedState, err := tlb.ToCell(payments.StateBodySigned{
		SignatureA: payments.Signature{Value: bytes.Repeat([]byte{1}, 64)},
		SignatureB: payments.Signature{Value: make([]byte, 64)},
		Body: payments.StateBody{
			ChannelID: channelID,
			Seqno:     1,
			A: payments.StateSide{
				ConditionalsHash: bytes.Repeat([]byte{1}, 32),
				ActionStatesHash: make([]byte, 32),
			},
			B: payments.StateSide{
				ConditionalsHash: make([]byte, 32),
				ActionStatesHash: make([]byte, 32),
			},
		},
	})
	if err != nil {
		t.Fatalf("failed to encode signed state: %v", err)
	}

	channel := &db.Channel{
		ID:               channelID,
		Status:           db.ChannelStateActive,
		AcceptingActions: true,
		WeLeft:           false,
		SignedState:      signedState,
		Our: db.Side{
			Address:                 ourAddr.String(),
			Data:                    db.NewAgreedData(),
			OnchainBalances:         map[string]*big.Int{},
			LockedDeposits:          map[string]*payments.LockedDepositInfo{},
			PendingOnchainTransfers: map[string]*payments.PendingMessageInfo{},
		},
		Their: db.Side{
			Address:           theirAddr.String(),
			Data:              db.NewAgreedData(),
			OnchainBalances:   map[string]*big.Int{},
			LockedDeposits:    map[string]*payments.LockedDepositInfo{},
			LatestWalletSeqno: walletSeqno,
			OnchainInfo:       db.OnchainState{Key: theirKey},
			PendingOnchainTransfers: map[string]*payments.PendingMessageInfo{
				pendingIDWallet(pendingWalletSeqno): &payments.PendingMessageInfo{
					Amounts: map[string]*big.Int{
						payments.GetTONBalanceID(): big.NewInt(1),
					},
				},
			},
		},
	}

	if err = database.CreateChannel(context.Background(), channel); err != nil {
		t.Fatalf("failed to create channel: %v", err)
	}

	return &Service{db: database}, theirKey, ourAddr
}

func testExecuteTransactionAction(t *testing.T, walletSeqno uint32, channelAddr string, database DB) transport.ExecuteTransactionAction {
	t.Helper()

	channel, err := database.GetChannel(context.Background(), channelAddr)
	if err != nil {
		t.Fatalf("failed to load test channel: %v", err)
	}

	var req payments.ExternalMsgDoubleSigned
	req.SignatureA = payments.Signature{Value: make([]byte, 64)}
	req.SignatureB = payments.Signature{Value: make([]byte, 64)}
	req.Signed.ChannelID = payments.ChannelID(channel.ID)
	req.Signed.SideA = true
	req.Signed.ValidUntil = uint32(time.Now().Add(2 * time.Minute).Unix())
	req.Signed.WalletSeqno = walletSeqno

	body, err := tlb.ToCell(req)
	if err != nil {
		t.Fatalf("failed to encode external request: %v", err)
	}

	return transport.ExecuteTransactionAction{
		ExternalBody: body,
	}
}

func testAddress(seed byte) *address.Address {
	return address.NewAddress(0, 0, bytes.Repeat([]byte{seed}, 32))
}
