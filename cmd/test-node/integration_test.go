package testnode

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"math/big"
	"testing"
	"time"

	dbpkg "github.com/xssnick/ton-payment-network/tonpayments/db"
	"github.com/xssnick/ton-payment-network/tonpayments/transport"
	"github.com/xssnick/tonutils-go/tlb"
)

func TestIntegration_DerivativesInProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}

	mockResolver := installMockBTCResolver(t, "100")

	hub := newLoopbackHub()
	node1 := newTestNode(t, hub, 1, 19001)
	node2 := newTestNode(t, hub, 2, 19002)
	defer node1.stop(t)
	defer node2.stop(t)

	node1.start()
	node2.start()

	t.Logf("[wallet][node=%d][port=%d] %s", node1.idx, node1.port, node1.wallet.WalletAddress().String())
	t.Logf("[wallet][node=%d][port=%d] %s", node2.idx, node2.port, node2.wallet.WalletAddress().String())

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	opened, err := node1.svc.OpenChannelWithNode(ctx, node2.pub)
	if err != nil {
		t.Fatalf("open channel 1->2 failed: %v", err)
	}
	t.Logf("offchain channel opened: %s", opened.String())

	ch12 := waitChannelByPeer(t, node1, node2.pub)
	ch21 := waitChannelByPeer(t, node2, node1.pub)

	seedAmount := tlb.MustFromDecimal("5", 9).Nano()
	seedChannelTONBalance(t, node1, ch12.Our.Address, seedAmount)
	seedChannelTONBalance(t, node2, ch21.Our.Address, seedAmount)
	waitActiveChannelReady(t, node1, ch12.Our.Address)
	waitActiveChannelReady(t, node2, ch21.Our.Address)

	derivID, err := openDerivativeForTest(context.Background(), node1, ch12.Our.Address, true, 10, "0.01")
	if err != nil {
		t.Fatalf("open derivative position failed: %v", err)
	}
	t.Logf("derivative accepted id=%s", derivID)

	var incomingKey ed25519.PublicKey
	waitFor(t, 35*time.Second, 300*time.Millisecond, func() (bool, string) {
		keys, err := listActiveIncomingDerivativeKeys(node2)
		if err != nil {
			return false, fmt.Sprintf("failed to list node2 incoming derivative index: %v", err)
		}
		if len(keys) == 0 {
			return false, "waiting for node2 incoming derivative in index"
		}
		incomingKey = keys[0]
		return true, fmt.Sprintf("node2 has %d active incoming derivative(s)", len(keys))
	})
	t.Logf("node2 incoming derivative key=%s", base64.StdEncoding.EncodeToString(incomingKey))

	waitFor(t, 25*time.Second, 300*time.Millisecond, func() (bool, string) {
		meta, err := node2.svc.GetVirtualChannelMeta(context.Background(), incomingKey)
		if err != nil {
			return false, fmt.Sprintf("failed to read incoming derivative meta: %v", err)
		}

		monitor, ok := derivativeMonitor(meta)
		if !ok {
			return false, "waiting derivative monitor initialization"
		}
		if monitor.EntryCrossed {
			return true, fmt.Sprintf("monitor ready (entry crossed, last_checked_at=%d)", monitor.LastCheckedAt)
		}
		return false, fmt.Sprintf("waiting entry-cross detection (last_checked_at=%d)", monitor.LastCheckedAt)
	})

	targetPrice := tlb.MustFromDecimal("120", 9).Nano()
	if err = mockResolver.SetPrice(targetPrice); err != nil {
		t.Fatalf("set mock price failed: %v", err)
	}
	t.Log("mock BTCUSDT price moved to 120")

	waitFor(t, 40*time.Second, 300*time.Millisecond, func() (bool, string) {
		meta, err := node2.svc.GetVirtualChannelMeta(context.Background(), incomingKey)
		if err != nil {
			return false, fmt.Sprintf("failed to read incoming derivative meta: %v", err)
		}

		if meta.LastKnownResolve != nil && meta.Status != dbpkg.ConditionalStateActive {
			return true, fmt.Sprintf("incoming derivative resolved, status=%d", meta.Status)
		}

		if isDerivativeMonitorLiquidated(meta) {
			if meta.LastKnownResolve != nil {
				return true, "monitor marks liquidated and resolve is known"
			}
			return false, "monitor marks liquidated, waiting resolve propagation"
		}

		return false, "waiting liquidation to be detected by background worker"
	})

	waitFor(t, 40*time.Second, 300*time.Millisecond, func() (bool, string) {
		keys, err := listActiveIncomingDerivativeKeys(node2)
		if err != nil {
			return false, fmt.Sprintf("failed to read incoming derivative index: %v", err)
		}
		if len(keys) == 0 {
			return true, "node2 incoming derivative index is empty"
		}
		return false, fmt.Sprintf("node2 still has %d active incoming derivative(s)", len(keys))
	})
}

func TestIntegration_VirtualChannelDirect(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}

	hub := newLoopbackHub()
	node1 := newTestNode(t, hub, 1, 19101)
	node2 := newTestNode(t, hub, 2, 19102)
	defer node1.stop(t)
	defer node2.stop(t)

	node1.start()
	node2.start()

	t.Logf("[wallet][node=%d][port=%d] %s", node1.idx, node1.port, node1.wallet.WalletAddress().String())
	t.Logf("[wallet][node=%d][port=%d] %s", node2.idx, node2.port, node2.wallet.WalletAddress().String())

	seedAmount := tlb.MustFromDecimal("5", 9).Nano()
	ch12, ch21 := openAndSeedChannel(t, node1, node2, seedAmount)

	cc, err := node1.svc.ResolveCoinConfigBySymbol("TON")
	if err != nil {
		t.Fatalf("resolve coin config failed: %v", err)
	}

	capacity := tlb.MustFromDecimal("1", int(cc.Decimals)).Nano()
	fee := tlb.MustFromDecimal("0.01", int(cc.Decimals)).Nano()
	deadline := time.Now().Add(24 * time.Hour)

	tunnel := []transport.TunnelChainPart{{
		Target:   node2.pub,
		Capacity: capacity,
		Fee:      fee,
		Deadline: deadline,
	}}

	_, vPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate virtual key failed: %v", err)
	}

	instructionKey, instructions, err := transport.GenerateTunnel(vPriv, tunnel, 0, false, nil, cc)
	if err != nil {
		t.Fatalf("generate tunnel instructions failed: %v", err)
	}

	if err = node1.svc.CreateSendConditional(context.Background(), instructionKey, vPriv, tunnel[0], tunnel[len(tunnel)-1], instructions, cc); err != nil {
		t.Fatalf("create send conditional failed: %v", err)
	}

	vKey := vPriv.Public().(ed25519.PublicKey)

	metaSender := waitVirtualMeta(t, node1, vKey, false, true)
	if metaSender.Outgoing.ChannelAddress != ch12.Our.Address {
		t.Fatalf("sender outgoing channel mismatch: got %s, want %s", metaSender.Outgoing.ChannelAddress, ch12.Our.Address)
	}

	metaReceiver := waitVirtualMeta(t, node2, vKey, true, false)
	if metaReceiver.Incoming.ChannelAddress != ch21.Our.Address {
		t.Fatalf("receiver incoming channel mismatch: got %s, want %s", metaReceiver.Incoming.ChannelAddress, ch21.Our.Address)
	}
}

func TestIntegration_VirtualChannelTunnel3Nodes(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}

	hub := newLoopbackHub()
	node1 := newTestNode(t, hub, 1, 19201)
	node2 := newTestNode(t, hub, 2, 19202)
	node3 := newTestNode(t, hub, 3, 19203)
	defer node1.stop(t)
	defer node2.stop(t)
	defer node3.stop(t)

	node1.start()
	node2.start()
	node3.start()

	t.Logf("[wallet][node=%d][port=%d] %s", node1.idx, node1.port, node1.wallet.WalletAddress().String())
	t.Logf("[wallet][node=%d][port=%d] %s", node2.idx, node2.port, node2.wallet.WalletAddress().String())
	t.Logf("[wallet][node=%d][port=%d] %s", node3.idx, node3.port, node3.wallet.WalletAddress().String())

	seedAmount := tlb.MustFromDecimal("5", 9).Nano()
	ch12, ch21 := openAndSeedChannel(t, node1, node2, seedAmount)
	ch23, ch32 := openAndSeedChannel(t, node2, node3, seedAmount)

	cc, err := node1.svc.ResolveCoinConfigBySymbol("TON")
	if err != nil {
		t.Fatalf("resolve coin config failed: %v", err)
	}

	capacity := tlb.MustFromDecimal("1", int(cc.Decimals)).Nano()
	feeFirst := tlb.MustFromDecimal("0.02", int(cc.Decimals)).Nano()
	feeFinal := tlb.MustFromDecimal("0.001", int(cc.Decimals)).Nano()

	now := time.Now()
	deadlineFirst := now.Add(48 * time.Hour)
	deadlineFinal := now.Add(24 * time.Hour)

	tunnel := []transport.TunnelChainPart{
		{
			Target:   node2.pub,
			Capacity: capacity,
			Fee:      feeFirst,
			Deadline: deadlineFirst,
		},
		{
			Target:   node3.pub,
			Capacity: capacity,
			Fee:      feeFinal,
			Deadline: deadlineFinal,
		},
	}

	_, vPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate virtual key failed: %v", err)
	}

	instructionKey, instructions, err := transport.GenerateTunnel(vPriv, tunnel, 0, false, nil, cc)
	if err != nil {
		t.Fatalf("generate tunnel instructions failed: %v", err)
	}

	if err = node1.svc.CreateSendConditional(context.Background(), instructionKey, vPriv, tunnel[0], tunnel[len(tunnel)-1], instructions, cc); err != nil {
		t.Fatalf("create send conditional failed: %v", err)
	}

	vKey := vPriv.Public().(ed25519.PublicKey)

	metaSender := waitVirtualMeta(t, node1, vKey, false, true)
	if metaSender.Outgoing.ChannelAddress != ch12.Our.Address {
		t.Fatalf("sender outgoing channel mismatch: got %s, want %s", metaSender.Outgoing.ChannelAddress, ch12.Our.Address)
	}

	metaRelay := waitVirtualMeta(t, node2, vKey, true, true)
	if metaRelay.Incoming.ChannelAddress != ch21.Our.Address {
		t.Fatalf("relay incoming channel mismatch: got %s, want %s", metaRelay.Incoming.ChannelAddress, ch21.Our.Address)
	}
	if metaRelay.Outgoing.ChannelAddress != ch23.Our.Address {
		t.Fatalf("relay outgoing channel mismatch: got %s, want %s", metaRelay.Outgoing.ChannelAddress, ch23.Our.Address)
	}

	metaReceiver := waitVirtualMeta(t, node3, vKey, true, false)
	if metaReceiver.Incoming.ChannelAddress != ch32.Our.Address {
		t.Fatalf("receiver incoming channel mismatch: got %s, want %s", metaReceiver.Incoming.ChannelAddress, ch32.Our.Address)
	}
}

func openAndSeedChannel(t *testing.T, nodeA, nodeB *testNode, seedAmount *big.Int) (*dbpkg.Channel, *dbpkg.Channel) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	opened, err := nodeA.svc.OpenChannelWithNode(ctx, nodeB.pub)
	if err != nil {
		t.Fatalf("open channel %d->%d failed: %v", nodeA.idx, nodeB.idx, err)
	}
	t.Logf("offchain channel opened: %s", opened.String())

	chAB := waitChannelByPeer(t, nodeA, nodeB.pub)
	chBA := waitChannelByPeer(t, nodeB, nodeA.pub)

	seedChannelTONBalance(t, nodeA, chAB.Our.Address, seedAmount)
	seedChannelTONBalance(t, nodeB, chBA.Our.Address, seedAmount)
	waitActiveChannelReady(t, nodeA, chAB.Our.Address)
	waitActiveChannelReady(t, nodeB, chBA.Our.Address)

	return chAB, chBA
}

func waitVirtualMeta(t *testing.T, node *testNode, key ed25519.PublicKey, wantIncoming, wantOutgoing bool) *dbpkg.ConditionalMeta {
	t.Helper()

	var meta *dbpkg.ConditionalMeta
	waitFor(t, 35*time.Second, 300*time.Millisecond, func() (bool, string) {
		got, err := node.svc.GetVirtualChannelMeta(context.Background(), key)
		if err != nil {
			return false, fmt.Sprintf("node %d waiting for virtual meta: %v", node.idx, err)
		}
		if got.Status != dbpkg.ConditionalStateActive {
			return false, fmt.Sprintf("node %d waiting for active meta, status=%d", node.idx, got.Status)
		}
		if wantIncoming && got.Incoming == nil {
			return false, fmt.Sprintf("node %d waiting for incoming meta", node.idx)
		}
		if wantOutgoing && got.Outgoing == nil {
			return false, fmt.Sprintf("node %d waiting for outgoing meta", node.idx)
		}

		meta = got
		return true, fmt.Sprintf("node %d virtual meta ready", node.idx)
	})
	return meta
}
