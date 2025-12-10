package payments

import (
	"math/big"
	"testing"

	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/ton/wallet"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

func testMsg(toStr string, nano int64, body *cell.Cell) WalletMessage {
	to := address.MustParseAddr(toStr)
	coins := tlb.MustFromNano(big.NewInt(nano), 9)
	m := wallet.SimpleMessage(to, coins, body)
	return WalletMessage{InternalMessage: m.InternalMessage, Mode: m.Mode}
}

func hashInternal(m *tlb.InternalMessage) []byte {
	c, _ := tlb.ToCell(m)
	return c.Hash()
}

func TestPackUnpackOutActions_RoundTrip(t *testing.T) {
	body1 := cell.BeginCell().MustStoreUInt(0xAA, 8).EndCell()
	body2 := cell.BeginCell().MustStoreUInt(0xBB, 8).EndCell()

	m1 := testMsg("EQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAM9c", 123456789, body1)
	m2 := testMsg("EQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAM9c", 42, body2)

	// set some explicit modes
	m1.Mode = 3
	m2.Mode = 128

	packed, err := PackOutActions([]WalletMessage{m1, m2})
	if err != nil {
		t.Fatalf("PackOutActions failed: %v", err)
	}

	unpacked, err := UnpackOutActions(packed)
	if err != nil {
		t.Fatalf("UnpackOutActions failed: %v", err)
	}

	if len(unpacked) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(unpacked))
	}

	// order should be preserved
	if unpacked[0].Mode != m1.Mode || unpacked[1].Mode != m2.Mode {
		t.Fatalf("modes mismatch: got %d,%d want %d,%d", unpacked[0].Mode, unpacked[1].Mode, m1.Mode, m2.Mode)
	}

	if h1, h1e := hashInternal(unpacked[0].InternalMessage), hashInternal(m1.InternalMessage); string(h1) != string(h1e) {
		t.Fatalf("internal message 1 mismatch")
	}
	if h2, h2e := hashInternal(unpacked[1].InternalMessage), hashInternal(m2.InternalMessage); string(h2) != string(h2e) {
		t.Fatalf("internal message 2 mismatch")
	}
}

func TestPackUnpackOutActions_Empty(t *testing.T) {
	packed, err := PackOutActions(nil)
	if err != nil {
		t.Fatalf("PackOutActions(nil) failed: %v", err)
	}
	unpacked, err := UnpackOutActions(packed)
	if err != nil {
		t.Fatalf("UnpackOutActions failed: %v", err)
	}
	if len(unpacked) != 0 {
		t.Fatalf("expected 0 messages, got %d", len(unpacked))
	}
}

func TestUnpackOutActions_InvalidTag(t *testing.T) {
	// Build a list node with wrong tag
	body := cell.BeginCell().MustStoreUInt(0xAA, 8).EndCell()
	m := testMsg("EQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAM9c", 1, body)
	outMsg, _ := tlb.ToCell(m.InternalMessage)

	wrongAction := cell.BeginCell().
		MustStoreUInt(0xDEADBEEF, 32). // wrong tag
		MustStoreUInt(0, 8). // mode
		MustStoreRef(outMsg).
		EndCell()

	list := cell.BeginCell().MustStoreRef(cell.BeginCell().EndCell()).MustStoreRef(wrongAction).EndCell()

	if _, err := UnpackOutActions(list); err == nil {
		t.Fatalf("expected error for invalid tag, got nil")
	}
}
