package tonpayments

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xssnick/ton-payment-network/tonpayments/db"
	dblevel "github.com/xssnick/ton-payment-network/tonpayments/db/leveldb"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

func TestCloseConditionalRejectsOutgoingOnlyMeta(t *testing.T) {
	database := newTestPaymentsDB(t)

	key := ed25519.PublicKey(bytes.Repeat([]byte{7}, ed25519.PublicKeySize))
	meta := &db.ConditionalMeta{
		Key:    key,
		Status: db.ConditionalStateActive,
		Outgoing: &db.ConditionalMetaSide{
			ChannelAddress: "outgoing-channel",
			Conditional:    cell.BeginCell().EndCell(),
		},
		LastKnownResolve: cell.BeginCell().EndCell(),
	}
	if err := database.CreateVirtualChannelMeta(context.Background(), meta); err != nil {
		t.Fatalf("failed to create meta: %v", err)
	}

	svc := &Service{db: database}
	err := svc.CloseConditional(context.Background(), key)
	if !errors.Is(err, ErrCannotCloseOutgoingVirtual) {
		t.Fatalf("expected ErrCannotCloseOutgoingVirtual, got %v", err)
	}

	got, err := database.GetVirtualChannelMeta(context.Background(), key)
	if err != nil {
		t.Fatalf("failed to reload meta: %v", err)
	}
	if got.Status != db.ConditionalStateActive {
		t.Fatalf("outgoing close must not update status, got %d", got.Status)
	}

	tasks, err := database.ListActiveTasks(context.Background(), PaymentsTaskPool)
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}
	if len(tasks) != 0 {
		t.Fatalf("outgoing close must not create tasks, got %d", len(tasks))
	}
}

func TestCloseDerivativeRejectsMissingLinkedIncomingMeta(t *testing.T) {
	database := newTestPaymentsDB(t)

	key := testPublicKey(8)
	meta := testOutgoingDerivativeMeta(key, nil)
	if err := database.CreateVirtualChannelMeta(context.Background(), meta); err != nil {
		t.Fatalf("failed to create meta: %v", err)
	}

	svc := &Service{db: database}
	err := svc.CloseDerivative(context.Background(), key)
	if err == nil || !strings.Contains(err.Error(), "no linked incoming side") {
		t.Fatalf("expected linked incoming error, got %v", err)
	}

	assertMetaStatus(t, database, key, db.ConditionalStateActive)
	assertNoPaymentTasks(t, database)
}

func TestCloseDerivativeRejectsLinkedMetaWithoutIncomingSide(t *testing.T) {
	database := newTestPaymentsDB(t)

	key := testPublicKey(9)
	linkedKey := testPublicKey(10)
	meta := testOutgoingDerivativeMeta(key, linkedKey)
	linkedMeta := testOutgoingDerivativeMeta(linkedKey, key)

	if err := database.CreateVirtualChannelMeta(context.Background(), meta); err != nil {
		t.Fatalf("failed to create meta: %v", err)
	}
	if err := database.CreateVirtualChannelMeta(context.Background(), linkedMeta); err != nil {
		t.Fatalf("failed to create linked meta: %v", err)
	}

	svc := &Service{db: database}
	err := svc.CloseDerivative(context.Background(), key)
	if err == nil || !strings.Contains(err.Error(), "not incoming") {
		t.Fatalf("expected linked incoming error, got %v", err)
	}

	assertMetaStatus(t, database, key, db.ConditionalStateActive)
	assertMetaStatus(t, database, linkedKey, db.ConditionalStateActive)
	assertNoPaymentTasks(t, database)
}

func newTestPaymentsDB(t *testing.T) *db.DB {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "db")
	storage, _, err := dblevel.NewLevelDB(dbPath)
	if err != nil {
		t.Fatalf("failed to open leveldb: %v", err)
	}

	database := db.NewDB(storage, nil)
	t.Cleanup(database.Close)
	return database
}

func testOutgoingDerivativeMeta(key, linkedKey ed25519.PublicKey) *db.ConditionalMeta {
	return &db.ConditionalMeta{
		Key:    key,
		Status: db.ConditionalStateActive,
		Outgoing: &db.ConditionalMetaSide{
			ChannelAddress: "derivative-channel",
			Conditional:    cell.BeginCell().EndCell(),
			LinkedKey:      linkedKey,
		},
		LastKnownResolve: cell.BeginCell().EndCell(),
	}
}

func testPublicKey(seed byte) ed25519.PublicKey {
	return ed25519.PublicKey(bytes.Repeat([]byte{seed}, ed25519.PublicKeySize))
}

func assertMetaStatus(t *testing.T, database *db.DB, key ed25519.PublicKey, want db.ConditionalStatus) {
	t.Helper()

	got, err := database.GetVirtualChannelMeta(context.Background(), key)
	if err != nil {
		t.Fatalf("failed to reload meta: %v", err)
	}
	if got.Status != want {
		t.Fatalf("unexpected meta status: got %d, want %d", got.Status, want)
	}
}

func assertNoPaymentTasks(t *testing.T, database *db.DB) {
	t.Helper()

	tasks, err := database.ListActiveTasks(context.Background(), PaymentsTaskPool)
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}
	if len(tasks) != 0 {
		t.Fatalf("must not create tasks, got %d", len(tasks))
	}
}
