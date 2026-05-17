package tonpayments

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"path/filepath"
	"testing"

	"github.com/xssnick/ton-payment-network/tonpayments/db"
	dblevel "github.com/xssnick/ton-payment-network/tonpayments/db/leveldb"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

func TestCloseConditionalRejectsOutgoingOnlyMeta(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "db")
	storage, _, err := dblevel.NewLevelDB(dbPath)
	if err != nil {
		t.Fatalf("failed to open leveldb: %v", err)
	}

	database := db.NewDB(storage, nil)
	t.Cleanup(database.Close)

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
	if err = database.CreateVirtualChannelMeta(context.Background(), meta); err != nil {
		t.Fatalf("failed to create meta: %v", err)
	}

	svc := &Service{db: database}
	err = svc.CloseConditional(context.Background(), key)
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
