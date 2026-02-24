package testnode

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/pkg/payments/actions"
	"github.com/xssnick/ton-payment-network/pkg/payments/conditionals"
	dbpkg "github.com/xssnick/ton-payment-network/tonpayments/db"
	"github.com/xssnick/ton-payment-network/tonpayments/transport"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
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

func TestIntegration_DerivativesMarketClose_SettlementAndFee(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}

	mockResolver := installMockBTCResolver(t, "100")

	hub := newLoopbackHub()
	node1 := newTestNode(t, hub, 1, 19301)
	node2 := newTestNode(t, hub, 2, 19302)
	defer node1.stop(t)
	defer node2.stop(t)

	node1.start()
	node2.start()

	seedAmount := tlb.MustFromDecimal("5", 9).Nano()
	ch12, _ := openAndSeedChannel(t, node1, node2, seedAmount)

	derivID, err := openDerivativeForTest(context.Background(), node1, ch12.Our.Address, true, 10, "0.01")
	if err != nil {
		t.Fatalf("open derivative position failed: %v", err)
	}

	positionKey := decodeDerivativePositionKeyForTest(t, derivID)
	metaOut := waitVirtualMeta(t, node1, positionKey, false, true)

	outgoingKey, incomingKey, err := resolveDerivativePairKeysForTest(metaOut, positionKey)
	if err != nil {
		t.Fatalf("resolve derivative pair keys failed: %v", err)
	}

	metaIn := waitVirtualMeta(t, node1, incomingKey, true, false)

	waitFor(t, 25*time.Second, 300*time.Millisecond, func() (bool, string) {
		meta, err := node1.svc.GetVirtualChannelMeta(context.Background(), incomingKey)
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

	outCond := loadResolvableFromMetaForTest(t, node1, metaOut, true)
	inCond := loadResolvableFromMetaForTest(t, node1, metaIn, false)

	if outCond.Fee.Cmp(inCond.Fee) != 0 {
		t.Fatalf("fee mismatch in derivative pair: outgoing=%s incoming=%s", outCond.Fee.String(), inCond.Fee.String())
	}

	chBefore, err := node1.svc.GetActiveChannel(context.Background(), ch12.Our.Address)
	if err != nil {
		t.Fatalf("get channel before close failed: %v", err)
	}

	outBefore := loadSendActionAmountForMetaSide(t, chBefore, outCond, true)
	inBefore := loadSendActionAmountForMetaSide(t, chBefore, inCond, false)

	targetPrice := tlb.MustFromDecimal("120", 9).Nano()
	if err = mockResolver.SetPrice(targetPrice); err != nil {
		t.Fatalf("set mock price failed: %v", err)
	}

	if err = node1.derivatives.ClosePosition(context.Background(), ch12.Our.Address, derivID, "market"); err != nil {
		t.Fatalf("close derivative position failed: %v", err)
	}

	metaOutClosed := waitDerivativeMetaClosedWithResolveForTest(t, node1, outgoingKey)
	metaInClosed := waitDerivativeMetaClosedWithResolveForTest(t, node1, incomingKey)

	outResolve := loadResolvableStateFromMetaForTest(t, metaOutClosed)
	inResolve := loadResolvableStateFromMetaForTest(t, metaInClosed)

	pnl := calculateDerivativePnLForTest(outCond, targetPrice)
	wantOutSettle := expectedOutgoingDerivativeSettleForTest(pnl, outCond.Amount)
	wantInSettle := expectedIncomingDerivativeSettleForTest(pnl, inCond.Amount)

	if outResolve.Amount.Cmp(wantOutSettle) != 0 {
		t.Fatalf("outgoing settle mismatch: got %s, want %s", outResolve.Amount.String(), wantOutSettle.String())
	}
	if inResolve.Amount.Cmp(wantInSettle) != 0 {
		t.Fatalf("incoming settle mismatch: got %s, want %s", inResolve.Amount.String(), wantInSettle.String())
	}

	chAfter, err := node1.svc.GetActiveChannel(context.Background(), ch12.Our.Address)
	if err != nil {
		t.Fatalf("get channel after close failed: %v", err)
	}

	outAfter := loadSendActionAmountForMetaSide(t, chAfter, outCond, true)
	inAfter := loadSendActionAmountForMetaSide(t, chAfter, inCond, false)

	outDiff := new(big.Int).Sub(outAfter, outBefore)
	inDiff := new(big.Int).Sub(inAfter, inBefore)

	wantOutTransfer := applyDerivativeFeeForTransferForTest(wantOutSettle, outCond.Fee, outCond.IsInitiator)
	wantInTransfer := applyDerivativeFeeForTransferForTest(wantInSettle, inCond.Fee, inCond.IsInitiator)

	waitFor(t, 40*time.Second, 300*time.Millisecond, func() (bool, string) {
		chCur, err := node1.svc.GetActiveChannel(context.Background(), ch12.Our.Address)
		if err != nil {
			return false, fmt.Sprintf("get channel while waiting transfer failed: %v", err)
		}

		outCur := loadSendActionAmountForMetaSide(t, chCur, outCond, true)
		inCur := loadSendActionAmountForMetaSide(t, chCur, inCond, false)

		outDiff = new(big.Int).Sub(outCur, outBefore)
		inDiff = new(big.Int).Sub(inCur, inBefore)

		if outDiff.Cmp(wantOutTransfer) == 0 && inDiff.Cmp(wantInTransfer) == 0 {
			return true, fmt.Sprintf("transfers applied: outgoing=%s incoming=%s", outDiff.String(), inDiff.String())
		}
		return false, fmt.Sprintf("waiting transfers, outgoing=%s/%s incoming=%s/%s", outDiff.String(), wantOutTransfer.String(), inDiff.String(), wantInTransfer.String())
	})

	outHasSettle := wantOutSettle.Sign() > 0
	inHasSettle := wantInSettle.Sign() > 0
	if outHasSettle == inHasSettle {
		t.Fatalf("invalid zero-sum settle state: outgoing=%s incoming=%s", wantOutSettle.String(), wantInSettle.String())
	}

	waitFor(t, 40*time.Second, 300*time.Millisecond, func() (bool, string) {
		keys, err := listActiveIncomingDerivativeKeys(node1)
		if err != nil {
			return false, fmt.Sprintf("failed to read node1 incoming derivative index: %v", err)
		}
		if len(keys) == 0 {
			return true, "node1 incoming derivative index is empty"
		}
		return false, fmt.Sprintf("node1 still has %d active incoming derivative(s)", len(keys))
	})
}

func TestIntegration_DerivativesMarketClose_RejectTamperedResolves(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}

	mockResolver := installMockBTCResolver(t, "100")

	hub := newLoopbackHub()
	node1 := newTestNode(t, hub, 1, 19401)
	node2 := newTestNode(t, hub, 2, 19402)
	defer node1.stop(t)
	defer node2.stop(t)

	node1.start()
	node2.start()

	seedAmount := tlb.MustFromDecimal("5", 9).Nano()
	ch12, _ := openAndSeedChannel(t, node1, node2, seedAmount)

	derivID, err := openDerivativeForTest(context.Background(), node1, ch12.Our.Address, true, 10, "0.01")
	if err != nil {
		t.Fatalf("open derivative position failed: %v", err)
	}

	positionKey := decodeDerivativePositionKeyForTest(t, derivID)
	metaOut := waitVirtualMeta(t, node1, positionKey, false, true)
	outgoingKey, incomingKey, err := resolveDerivativePairKeysForTest(metaOut, positionKey)
	if err != nil {
		t.Fatalf("resolve derivative pair keys failed: %v", err)
	}

	metaIn := waitVirtualMeta(t, node1, incomingKey, true, false)
	inCond := loadResolvableFromMetaForTest(t, node1, metaIn, false)

	waitFor(t, 25*time.Second, 300*time.Millisecond, func() (bool, string) {
		meta, err := node1.svc.GetVirtualChannelMeta(context.Background(), incomingKey)
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

	lossPrice := tlb.MustFromDecimal("95", 9).Nano()
	if err = mockResolver.SetPrice(lossPrice); err != nil {
		t.Fatalf("set loss price failed: %v", err)
	}
	atLoss, _, err := mockResolver.GetLastPrice()
	if err != nil {
		t.Fatalf("read loss price timestamp failed: %v", err)
	}

	underpayLoss := mustResolvableStateCellForTest(t, outgoingKey, big.NewInt(0), atLoss)
	err = node1.svc.AddConditionalResolve(context.Background(), outgoingKey, underpayLoss)
	assertResolveRejectedForTest(t, err, "incorrect amount")

	winPrice := tlb.MustFromDecimal("105", 9).Nano()
	if err = mockResolver.SetPrice(winPrice); err != nil {
		t.Fatalf("set win price failed: %v", err)
	}
	atWin, _, err := mockResolver.GetLastPrice()
	if err != nil {
		t.Fatalf("read win price timestamp failed: %v", err)
	}

	overchargeProfit := mustResolvableStateCellForTest(t, incomingKey, new(big.Int).Add(inCond.Amount, big.NewInt(1_000_000)), atWin)
	err = node1.svc.AddConditionalResolve(context.Background(), incomingKey, overchargeProfit)
	assertResolveRejectedForTest(t, err, "incorrect amount")

	tooOld := mustResolvableStateCellForTest(t, outgoingKey, big.NewInt(0), atWin-100)
	err = node1.svc.AddConditionalResolve(context.Background(), outgoingKey, tooOld)
	assertResolveRejectedForTest(t, err, "too old")

	tooFuture := mustResolvableStateCellForTest(t, outgoingKey, big.NewInt(0), atWin+100)
	err = node1.svc.AddConditionalResolve(context.Background(), outgoingKey, tooFuture)
	assertResolveRejectedForTest(t, err, "too far in the future")

	fakeKey := append(ed25519.PublicKey{}, outgoingKey...)
	fakeKey[0] ^= 0xFF
	wrongKeyState := mustResolvableStateCellForTest(t, fakeKey, big.NewInt(0), atWin)
	err = node1.svc.AddConditionalResolve(context.Background(), outgoingKey, wrongKeyState)
	assertResolveRejectedForTest(t, err, "incorrect key")

	metaAfter, err := node1.svc.GetVirtualChannelMeta(context.Background(), outgoingKey)
	if err != nil {
		t.Fatalf("load outgoing meta failed: %v", err)
	}
	if metaAfter.LastKnownResolve != nil {
		t.Fatalf("tampered resolve should not persist in metadata")
	}

	if err = node1.derivatives.ClosePosition(context.Background(), ch12.Our.Address, derivID, "market"); err != nil {
		t.Fatalf("final close after tamper checks failed: %v", err)
	}

	waitDerivativeMetaClosedWithResolveForTest(t, node1, outgoingKey)
	waitDerivativeMetaClosedWithResolveForTest(t, node1, incomingKey)

	waitFor(t, 40*time.Second, 300*time.Millisecond, func() (bool, string) {
		keys, err := listActiveIncomingDerivativeKeys(node1)
		if err != nil {
			return false, fmt.Sprintf("failed to read node1 incoming derivative index: %v", err)
		}
		if len(keys) == 0 {
			return true, "node1 incoming derivative index is empty"
		}
		return false, fmt.Sprintf("node1 still has %d active incoming derivative(s)", len(keys))
	})
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

func decodeDerivativePositionKeyForTest(t *testing.T, raw string) ed25519.PublicKey {
	t.Helper()

	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		t.Fatalf("decode derivative position id failed: %v", err)
	}
	if len(decoded) != ed25519.PublicKeySize {
		t.Fatalf("invalid derivative key size: got %d, want %d", len(decoded), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(decoded)
}

func resolveDerivativePairKeysForTest(meta *dbpkg.ConditionalMeta, positionID ed25519.PublicKey) (ed25519.PublicKey, ed25519.PublicKey, error) {
	if meta == nil {
		return nil, nil, fmt.Errorf("position metadata is missing")
	}

	var outgoingKey, incomingKey ed25519.PublicKey
	if meta.Outgoing != nil {
		outgoingKey = append(ed25519.PublicKey(nil), positionID...)
		if len(meta.Outgoing.LinkedKey) == ed25519.PublicKeySize {
			incomingKey = append(ed25519.PublicKey(nil), meta.Outgoing.LinkedKey...)
		}
	}
	if meta.Incoming != nil {
		incomingKey = append(ed25519.PublicKey(nil), positionID...)
		if len(meta.Incoming.LinkedKey) == ed25519.PublicKeySize {
			outgoingKey = append(ed25519.PublicKey(nil), meta.Incoming.LinkedKey...)
		}
	}

	if outgoingKey == nil && incomingKey == nil {
		return nil, nil, fmt.Errorf("position metadata is inconsistent")
	}
	return outgoingKey, incomingKey, nil
}

func loadResolvableFromMetaForTest(t *testing.T, node *testNode, meta *dbpkg.ConditionalMeta, outgoing bool) *conditionals.ConditionalResolvable {
	t.Helper()

	var codeCell *cell.Cell
	if outgoing {
		if meta == nil || meta.Outgoing == nil || meta.Outgoing.Conditional == nil {
			t.Fatalf("outgoing conditional meta is missing")
		}
		codeCell = meta.Outgoing.Conditional
	} else {
		if meta == nil || meta.Incoming == nil || meta.Incoming.Conditional == nil {
			t.Fatalf("incoming conditional meta is missing")
		}
		codeCell = meta.Incoming.Conditional
	}

	condRaw, err := payments.CodeToConditional(context.Background(), codeCell, node.svc)
	if err != nil {
		t.Fatalf("decode conditional from meta failed: %v", err)
	}
	cond, ok := condRaw.(*conditionals.ConditionalResolvable)
	if !ok {
		t.Fatalf("meta conditional is not derivative resolvable: %T", condRaw)
	}
	return cond
}

func loadSendActionAmountForMetaSide(t *testing.T, ch *dbpkg.Channel, cond *conditionals.ConditionalResolvable, outgoing bool) *big.Int {
	t.Helper()
	_ = outgoing

	if ch == nil {
		t.Fatalf("channel is nil")
	}
	if cond == nil {
		t.Fatalf("conditional is nil")
	}

	actionID := cond.GetAction().IDCell()
	total := big.NewInt(0)

	loadAndAdd := func(stateCell *cell.Slice, err error) {
		if err != nil {
			if errors.Is(err, cell.ErrNoSuchKeyInDict) {
				return
			}
			t.Fatalf("load action state failed: %v", err)
		}

		var state actions.StateActionSend
		if err = payments.LoadState(&state, stateCell.MustToCell()); err != nil {
			t.Fatalf("decode action state failed: %v", err)
		}
		total.Add(total, state.Amount.Nano())
	}

	loadAndAdd(ch.Our.Data.ActionStates.LoadValue(actionID))
	loadAndAdd(ch.Their.Data.ActionStates.LoadValue(actionID))

	return total
}

func waitDerivativeMetaClosedWithResolveForTest(t *testing.T, node *testNode, key ed25519.PublicKey) *dbpkg.ConditionalMeta {
	t.Helper()

	var meta *dbpkg.ConditionalMeta
	waitFor(t, 40*time.Second, 300*time.Millisecond, func() (bool, string) {
		got, err := node.svc.GetVirtualChannelMeta(context.Background(), key)
		if err != nil {
			return false, fmt.Sprintf("failed to get derivative meta: %v", err)
		}
		if got.Status == dbpkg.ConditionalStateActive || got.Status == dbpkg.ConditionalStatePending {
			return false, fmt.Sprintf("waiting derivative close, status=%d", got.Status)
		}
		if got.LastKnownResolve == nil {
			return false, "waiting derivative resolve propagation"
		}
		meta = got
		return true, fmt.Sprintf("derivative closed, status=%d", got.Status)
	})

	return meta
}

func loadResolvableStateFromMetaForTest(t *testing.T, meta *dbpkg.ConditionalMeta) conditionals.ResolvableState {
	t.Helper()

	if meta == nil || meta.LastKnownResolve == nil {
		t.Fatalf("resolve metadata is missing")
	}

	var state conditionals.ResolvableState
	if err := payments.LoadState(&state, meta.LastKnownResolve); err != nil {
		t.Fatalf("decode resolve state failed: %v", err)
	}
	return state
}

func calculateDerivativePnLForTest(cond *conditionals.ConditionalResolvable, currentPrice *big.Int) *big.Int {
	if cond == nil {
		return big.NewInt(0)
	}
	entryPrice := cond.Details.EntryPrice.Nano()
	if entryPrice == nil || entryPrice.Sign() <= 0 {
		return big.NewInt(0)
	}

	var delta *big.Int
	if cond.Details.IsLong {
		delta = new(big.Int).Sub(currentPrice, entryPrice)
	} else {
		delta = new(big.Int).Sub(entryPrice, currentPrice)
	}

	positionSize := new(big.Int).Mul(cond.Amount, big.NewInt(int64(cond.Details.Leverage)))
	pnl := new(big.Int).Mul(positionSize, delta)
	pnl.Div(pnl, entryPrice)
	return pnl
}

func expectedOutgoingDerivativeSettleForTest(pnl, capAmount *big.Int) *big.Int {
	settle := big.NewInt(0)
	if pnl.Sign() < 0 {
		settle = new(big.Int).Abs(pnl)
		if capAmount != nil && capAmount.Sign() > 0 && settle.Cmp(capAmount) > 0 {
			settle = new(big.Int).Set(capAmount)
		}
	}
	return settle
}

func expectedIncomingDerivativeSettleForTest(pnl, capAmount *big.Int) *big.Int {
	settle := big.NewInt(0)
	if pnl.Sign() > 0 {
		settle = new(big.Int).Set(pnl)
		if capAmount != nil && capAmount.Sign() > 0 && settle.Cmp(capAmount) > 0 {
			settle = new(big.Int).Set(capAmount)
		}
	}
	return settle
}

func applyDerivativeFeeForTransferForTest(settle, fee *big.Int, isInitiator bool) *big.Int {
	out := new(big.Int).Set(settle)
	if out.Sign() <= 0 {
		return big.NewInt(0)
	}

	if isInitiator {
		return out.Add(out, fee)
	}

	out.Sub(out, fee)
	if out.Sign() < 0 {
		out.SetInt64(0)
	}
	return out
}

func mustResolvableStateCellForTest(t *testing.T, key ed25519.PublicKey, amount *big.Int, at int64) *cell.Cell {
	t.Helper()

	st := conditionals.ResolvableState{
		Key:    key,
		Amount: amount,
		At:     at,
	}
	cell, err := tlb.ToCell(st)
	if err != nil {
		t.Fatalf("serialize resolve state failed: %v", err)
	}
	return cell
}

func assertResolveRejectedForTest(t *testing.T, err error, contains string) {
	t.Helper()

	if err == nil {
		t.Fatalf("tampered resolve must be rejected")
	}
	if contains != "" && !strings.Contains(err.Error(), contains) {
		t.Fatalf("unexpected resolve rejection error: got %q, want substring %q", err.Error(), contains)
	}
}
