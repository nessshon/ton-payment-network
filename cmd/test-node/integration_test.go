package testnode

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/pkg/payments/actions"
	"github.com/xssnick/ton-payment-network/pkg/payments/conditionals"
	dbpkg "github.com/xssnick/ton-payment-network/tonpayments/db"
	"github.com/xssnick/ton-payment-network/tonpayments/transport"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/ton/wallet"
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

	waitFor(t, 45*time.Second, 300*time.Millisecond, func() (bool, string) {
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

func TestIntegration_DerivativesOpen_RejectByHedgeServer(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}

	installMockBTCResolver(t, "100")

	hedgeServer := newTestHedgeServer(t)
	hedgeServer.setRejectOpen(true)
	hedgeKey, hedgeSecret := hedgeServer.auth()

	hub := newLoopbackHub()
	node1 := newTestNode(t, hub, 1, 19011)
	node2 := newTestNodeWithOptions(t, hub, 2, 19012, testNodeOptions{
		acceptingDerivatives: true,
		hedgeWebhookURL:      hedgeServer.url(),
		hedgeWebhookKey:      hedgeKey,
		hedgeWebhookSecret:   hedgeSecret,
	})
	defer node1.stop(t)
	defer node2.stop(t)

	node1.start()
	node2.start()

	seedAmount := tlb.MustFromDecimal("5", 9).Nano()
	ch12, ch21 := openAndSeedChannel(t, node1, node2, seedAmount)
	waitActiveChannelReady(t, node1, ch12.Our.Address)
	waitActiveChannelReady(t, node2, ch21.Our.Address)

	derivID, err := openDerivativeForTest(context.Background(), node1, ch12.Our.Address, true, 10, "0.01")
	if err != nil {
		t.Fatalf("queueing derivative open failed: %v", err)
	}

	positionKeyNode1 := decodeDerivativePositionKeyForTest(t, derivID)
	waitFor(t, 10*time.Second, 200*time.Millisecond, func() (bool, string) {
		keys, err := listActiveIncomingDerivativeKeys(node2)
		if err != nil {
			return false, fmt.Sprintf("failed to read node2 incoming derivative index: %v", err)
		}
		if len(keys) != 0 {
			return false, fmt.Sprintf("node2 still has %d active incoming derivative(s)", len(keys))
		}

		meta, metaErr := node1.svc.GetVirtualChannelMeta(context.Background(), positionKeyNode1)
		if metaErr == nil && meta.Status == dbpkg.ConditionalStateActive {
			return false, fmt.Sprintf("node1 derivative is still active with status=%d", meta.Status)
		}

		hedged, closed := hedgeServer.counts()
		if hedged != 0 || closed != 0 {
			return false, fmt.Sprintf("unexpected hedge server counts hedged=%d closed=%d", hedged, closed)
		}
		return true, "derivative open rejected and no incoming derivative was stored"
	})
}

func TestIntegration_DerivativesOpen_RejectByInvalidHedgeSignature(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}

	installMockBTCResolver(t, "100")

	hedgeServer := newTestHedgeServer(t)
	hedgeServer.setTamperResponseSignature(true)
	hedgeKey, hedgeSecret := hedgeServer.auth()

	hub := newLoopbackHub()
	node1 := newTestNode(t, hub, 1, 19016)
	node2 := newTestNodeWithOptions(t, hub, 2, 19017, testNodeOptions{
		acceptingDerivatives: true,
		hedgeWebhookURL:      hedgeServer.url(),
		hedgeWebhookKey:      hedgeKey,
		hedgeWebhookSecret:   hedgeSecret,
	})
	defer node1.stop(t)
	defer node2.stop(t)

	node1.start()
	node2.start()

	seedAmount := tlb.MustFromDecimal("5", 9).Nano()
	ch12, ch21 := openAndSeedChannel(t, node1, node2, seedAmount)
	waitActiveChannelReady(t, node1, ch12.Our.Address)
	waitActiveChannelReady(t, node2, ch21.Our.Address)

	derivID, err := openDerivativeForTest(context.Background(), node1, ch12.Our.Address, true, 10, "0.01")
	if err != nil {
		t.Fatalf("queueing derivative open failed: %v", err)
	}

	positionKeyNode1 := decodeDerivativePositionKeyForTest(t, derivID)
	waitFor(t, 10*time.Second, 200*time.Millisecond, func() (bool, string) {
		keys, err := listActiveIncomingDerivativeKeys(node2)
		if err != nil {
			return false, fmt.Sprintf("failed to read node2 incoming derivative index: %v", err)
		}
		if len(keys) != 0 {
			return false, fmt.Sprintf("node2 still has %d active incoming derivative(s)", len(keys))
		}

		meta, metaErr := node1.svc.GetVirtualChannelMeta(context.Background(), positionKeyNode1)
		if metaErr == nil && meta.Status == dbpkg.ConditionalStateActive {
			return false, fmt.Sprintf("node1 derivative is still active with status=%d", meta.Status)
		}

		hedged, closed := hedgeServer.counts()
		if hedged != 1 || closed != 0 {
			return false, fmt.Sprintf("unexpected hedge server counts hedged=%d closed=%d", hedged, closed)
		}
		return true, "derivative open rejected because hedge response signature was invalid"
	})
}

func TestIntegration_DerivativesHedging_WebhooksOpenAndClose(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}

	installMockBTCResolver(t, "100")

	hedgeServer := newTestHedgeServer(t)
	hedgeKey, hedgeSecret := hedgeServer.auth()

	hub := newLoopbackHub()
	node1 := newTestNode(t, hub, 1, 19021)
	node2 := newTestNodeWithOptions(t, hub, 2, 19022, testNodeOptions{
		acceptingDerivatives: true,
		hedgeWebhookURL:      hedgeServer.url(),
		hedgeWebhookKey:      hedgeKey,
		hedgeWebhookSecret:   hedgeSecret,
	})
	defer node1.stop(t)
	defer node2.stop(t)

	node1.start()
	node2.start()

	seedAmount := tlb.MustFromDecimal("5", 9).Nano()
	ch12, ch21 := openAndSeedChannel(t, node1, node2, seedAmount)
	waitActiveChannelReady(t, node1, ch12.Our.Address)
	waitActiveChannelReady(t, node2, ch21.Our.Address)

	derivID, err := openDerivativeForTest(context.Background(), node1, ch12.Our.Address, true, 10, "0.01")
	if err != nil {
		t.Fatalf("open derivative position failed: %v", err)
	}

	positionKeyNode1 := decodeDerivativePositionKeyForTest(t, derivID)
	metaOutNode1 := waitVirtualMeta(t, node1, positionKeyNode1, false, true)
	_, incomingKeyNode1, err := resolveDerivativePairKeysForTest(metaOutNode1, positionKeyNode1)
	if err != nil {
		t.Fatalf("resolve derivative pair keys failed: %v", err)
	}
	_ = waitVirtualMeta(t, node1, incomingKeyNode1, true, false)

	var incomingKeyNode2 ed25519.PublicKey
	waitFor(t, 35*time.Second, 300*time.Millisecond, func() (bool, string) {
		keys, err := listActiveIncomingDerivativeKeys(node2)
		if err != nil {
			return false, fmt.Sprintf("failed to list node2 incoming derivative index: %v", err)
		}
		if len(keys) == 0 {
			return false, "waiting for node2 incoming derivative in index"
		}
		incomingKeyNode2 = keys[0]
		return true, fmt.Sprintf("node2 has %d active incoming derivative(s)", len(keys))
	})
	metaInNode2 := waitVirtualMeta(t, node2, incomingKeyNode2, true, false)
	outgoingKeyNode2, _, err := resolveDerivativePairKeysForTest(metaInNode2, incomingKeyNode2)
	if err != nil {
		t.Fatalf("resolve node2 derivative pair keys failed: %v", err)
	}

	waitFor(t, 45*time.Second, 300*time.Millisecond, func() (bool, string) {
		meta, err := node2.svc.GetVirtualChannelMeta(context.Background(), incomingKeyNode2)
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

	waitHedgeCounts(t, hedgeServer, 1, 0)
	events := hedgeServer.snapshot()
	if len(events) == 0 || events[0].Event != "open" {
		t.Fatalf("expected first hedge webhook to be open, got %+v", events)
	}
	wantOrderID := base64.StdEncoding.EncodeToString(outgoingKeyNode2)
	if events[0].OrderID != wantOrderID {
		t.Fatalf("unexpected hedge order id: got %s want %s", events[0].OrderID, wantOrderID)
	}

	if err = node1.derivatives.ClosePosition(context.Background(), ch12.Our.Address, derivID, "market"); err != nil {
		t.Fatalf("close derivative position failed: %v", err)
	}

	waitDerivativeMetaClosedWithResolveForTest(t, node1, positionKeyNode1)
	waitDerivativeMetaClosedWithResolveForTest(t, node1, incomingKeyNode1)
	waitHedgeCounts(t, hedgeServer, 1, 1)

	events = hedgeServer.snapshot()
	if len(events) < 2 || events[len(events)-1].Event != "close" {
		t.Fatalf("expected close hedge webhook, got %+v", events)
	}
	if events[len(events)-1].OrderID != wantOrderID {
		t.Fatalf("unexpected close hedge order id: got %s want %s", events[len(events)-1].OrderID, wantOrderID)
	}
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

func TestIntegration_DerivativesMarketClose_ByCounterparty(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}

	mockResolver := installMockBTCResolver(t, "100")

	hub := newLoopbackHub()
	node1 := newTestNode(t, hub, 1, 19311)
	node2 := newTestNode(t, hub, 2, 19312)
	defer node1.stop(t)
	defer node2.stop(t)

	node1.start()
	node2.start()

	seedAmount := tlb.MustFromDecimal("5", 9).Nano()
	ch12, ch21 := openAndSeedChannel(t, node1, node2, seedAmount)

	derivID, err := openDerivativeForTest(context.Background(), node1, ch12.Our.Address, true, 10, "0.01")
	if err != nil {
		t.Fatalf("open derivative position failed: %v", err)
	}

	positionKeyNode1 := decodeDerivativePositionKeyForTest(t, derivID)
	metaOutNode1 := waitVirtualMeta(t, node1, positionKeyNode1, false, true)
	_, incomingKeyNode1, err := resolveDerivativePairKeysForTest(metaOutNode1, positionKeyNode1)
	if err != nil {
		t.Fatalf("resolve node1 derivative pair keys failed: %v", err)
	}
	metaInNode1 := waitVirtualMeta(t, node1, incomingKeyNode1, true, false)

	var incomingKeyNode2 ed25519.PublicKey
	waitFor(t, 25*time.Second, 300*time.Millisecond, func() (bool, string) {
		keys, err := listActiveIncomingDerivativeKeys(node2)
		if err != nil {
			return false, fmt.Sprintf("failed to list node2 incoming derivative index: %v", err)
		}
		if len(keys) == 0 {
			return false, "waiting for node2 incoming derivative in index"
		}
		incomingKeyNode2 = keys[0]
		return true, fmt.Sprintf("node2 has %d active incoming derivative(s)", len(keys))
	})

	metaInNode2 := waitVirtualMeta(t, node2, incomingKeyNode2, true, false)
	outgoingKeyNode2, _, err := resolveDerivativePairKeysForTest(metaInNode2, incomingKeyNode2)
	if err != nil {
		t.Fatalf("resolve node2 derivative pair keys failed: %v", err)
	}
	metaOutNode2 := waitVirtualMeta(t, node2, outgoingKeyNode2, false, true)

	waitFor(t, 25*time.Second, 300*time.Millisecond, func() (bool, string) {
		meta, err := node2.svc.GetVirtualChannelMeta(context.Background(), incomingKeyNode2)
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

	outCondNode1 := loadResolvableFromMetaForTest(t, node1, metaOutNode1, true)
	inCondNode1 := loadResolvableFromMetaForTest(t, node1, metaInNode1, false)
	outCondNode2 := loadResolvableFromMetaForTest(t, node2, metaOutNode2, true)
	inCondNode2 := loadResolvableFromMetaForTest(t, node2, metaInNode2, false)

	chBeforeNode1, err := node1.svc.GetActiveChannel(context.Background(), ch12.Our.Address)
	if err != nil {
		t.Fatalf("get node1 channel before close failed: %v", err)
	}
	chBeforeNode2, err := node2.svc.GetActiveChannel(context.Background(), ch21.Our.Address)
	if err != nil {
		t.Fatalf("get node2 channel before close failed: %v", err)
	}

	outBeforeNode1 := loadSendActionAmountForMetaSide(t, chBeforeNode1, outCondNode1, true)
	inBeforeNode1 := loadSendActionAmountForMetaSide(t, chBeforeNode1, inCondNode1, false)
	outBeforeNode2 := loadSendActionAmountForMetaSide(t, chBeforeNode2, outCondNode2, true)
	inBeforeNode2 := loadSendActionAmountForMetaSide(t, chBeforeNode2, inCondNode2, false)

	targetPrice := tlb.MustFromDecimal("105", 9).Nano()
	if err = mockResolver.SetPrice(targetPrice); err != nil {
		t.Fatalf("set mock price failed: %v", err)
	}

	if err = node2.derivatives.ClosePosition(context.Background(), ch21.Our.Address, base64.StdEncoding.EncodeToString(incomingKeyNode2), "market"); err != nil {
		t.Fatalf("counterparty close derivative position failed: %v", err)
	}

	waitDerivativeMetaInactiveForTest(t, node1, positionKeyNode1)
	waitDerivativeMetaInactiveForTest(t, node1, incomingKeyNode1)
	metaOutClosedNode2 := waitDerivativeMetaClosedWithResolveForTest(t, node2, outgoingKeyNode2)
	metaInClosedNode2 := waitDerivativeMetaClosedWithResolveForTest(t, node2, incomingKeyNode2)

	pnl := calculateDerivativePnLForTest(outCondNode1, targetPrice)
	wantOutSettle := expectedOutgoingDerivativeSettleForTest(pnl, outCondNode1.Amount)
	wantInSettle := expectedIncomingDerivativeSettleForTest(pnl, inCondNode1.Amount)

	assertDerivativeResolveAmountForTest(t, metaOutClosedNode2, positionKeyNode1, incomingKeyNode1, wantOutSettle, wantInSettle)
	assertDerivativeResolveAmountForTest(t, metaInClosedNode2, positionKeyNode1, incomingKeyNode1, wantOutSettle, wantInSettle)

	chAfterNode1, err := node1.svc.GetActiveChannel(context.Background(), ch12.Our.Address)
	if err != nil {
		t.Fatalf("get node1 channel after close failed: %v", err)
	}
	chAfterNode2, err := node2.svc.GetActiveChannel(context.Background(), ch21.Our.Address)
	if err != nil {
		t.Fatalf("get node2 channel after close failed: %v", err)
	}

	assertDerivativeAmountDeltaForTest(t, chAfterNode1, outCondNode1, inCondNode1, outBeforeNode1, inBeforeNode1,
		applyDerivativeFeeForTransferForTest(wantOutSettle, outCondNode1.Fee, outCondNode1.IsInitiator),
		applyDerivativeFeeForTransferForTest(wantInSettle, inCondNode1.Fee, inCondNode1.IsInitiator),
	)
	assertDerivativeAmountDeltaForTest(t, chAfterNode2, outCondNode2, inCondNode2, outBeforeNode2, inBeforeNode2,
		applyDerivativeFeeForTransferForTest(loadResolvableStateFromMetaForTest(t, metaOutClosedNode2).Amount, outCondNode2.Fee, outCondNode2.IsInitiator),
		applyDerivativeFeeForTransferForTest(loadResolvableStateFromMetaForTest(t, metaInClosedNode2).Amount, inCondNode2.Fee, inCondNode2.IsInitiator),
	)
	assertMirroredChannelPairStateForTest(t, chAfterNode1, chAfterNode2)

	waitFor(t, 40*time.Second, 300*time.Millisecond, func() (bool, string) {
		keys, err := listActiveIncomingDerivativeKeys(node1)
		if err != nil {
			return false, fmt.Sprintf("failed to read node1 incoming derivative index: %v", err)
		}
		if len(keys) != 0 {
			return false, fmt.Sprintf("node1 still has %d active incoming derivative(s)", len(keys))
		}
		keys, err = listActiveIncomingDerivativeKeys(node2)
		if err != nil {
			return false, fmt.Sprintf("failed to read node2 incoming derivative index: %v", err)
		}
		if len(keys) != 0 {
			return false, fmt.Sprintf("node2 still has %d active incoming derivative(s)", len(keys))
		}
		return true, "both nodes cleared incoming derivative indexes"
	})
}

func TestIntegration_DerivativesCancelBeforeOpen(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}

	installMockBTCResolver(t, "100")

	hub := newLoopbackHub()
	node1 := newTestNode(t, hub, 1, 19321)
	node2 := newTestNode(t, hub, 2, 19322)
	defer node1.stop(t)
	defer node2.stop(t)

	node1.start()
	node2.start()

	seedAmount := tlb.MustFromDecimal("5", 9).Nano()
	ch12, _ := openAndSeedChannel(t, node1, node2, seedAmount)

	derivID, err := node1.derivatives.OpenPosition(context.Background(), ch12.Our.Address, "BTCUSDT", "long", 10, "0.01", "limit", "80")
	if err != nil {
		t.Fatalf("open limit derivative position failed: %v", err)
	}

	positionKeyNode1 := decodeDerivativePositionKeyForTest(t, derivID)
	waitVirtualMeta(t, node1, positionKeyNode1, false, true)
	var incomingKeyNode2 ed25519.PublicKey
	waitFor(t, 25*time.Second, 300*time.Millisecond, func() (bool, string) {
		keys, err := listActiveIncomingDerivativeKeys(node2)
		if err != nil {
			return false, fmt.Sprintf("failed to list node2 incoming derivative index: %v", err)
		}
		if len(keys) == 0 {
			return false, "waiting for node2 incoming derivative in index"
		}
		incomingKeyNode2 = keys[0]
		return true, fmt.Sprintf("node2 has %d active incoming derivative(s)", len(keys))
	})

	metaInNode2 := waitVirtualMeta(t, node2, incomingKeyNode2, true, false)
	waitFor(t, 25*time.Second, 300*time.Millisecond, func() (bool, string) {
		meta, err := node2.svc.GetVirtualChannelMeta(context.Background(), incomingKeyNode2)
		if err != nil {
			return false, fmt.Sprintf("failed to read incoming derivative meta: %v", err)
		}
		monitor, ok := derivativeMonitor(meta)
		if !ok {
			return false, "waiting derivative monitor initialization"
		}
		if monitor.EntryCrossed {
			return false, "derivative must stay unopened"
		}
		return true, fmt.Sprintf("monitor initialized without entry cross, last_checked_at=%d", monitor.LastCheckedAt)
	})

	if err = node1.derivatives.ClosePosition(context.Background(), ch12.Our.Address, derivID, "cancel"); err != nil {
		t.Fatalf("cancel derivative position failed: %v", err)
	}

	waitDerivativeMetaInactiveForTest(t, node2, incomingKeyNode2)

	outgoingKeyNode2, _, err := resolveDerivativePairKeysForTest(metaInNode2, incomingKeyNode2)
	if err != nil {
		t.Fatalf("resolve node2 derivative pair keys failed: %v", err)
	}
	waitDerivativeMetaInactiveForTest(t, node2, outgoingKeyNode2)

	chAfterNode1, err := node1.svc.GetActiveChannel(context.Background(), ch12.Our.Address)
	if err != nil {
		t.Fatalf("get node1 channel after cancel failed: %v", err)
	}
	chAfterNode2, err := node2.svc.GetActiveChannel(context.Background(), metaInNode2.Incoming.ChannelAddress)
	if err != nil {
		t.Fatalf("get node2 channel after cancel failed: %v", err)
	}
	assertMirroredChannelPairStateForTest(t, chAfterNode1, chAfterNode2)

	waitFor(t, 40*time.Second, 300*time.Millisecond, func() (bool, string) {
		keys, err := listActiveIncomingDerivativeKeys(node1)
		if err != nil {
			return false, fmt.Sprintf("failed to read node1 incoming derivative index: %v", err)
		}
		if len(keys) != 0 {
			return false, fmt.Sprintf("node1 still has %d active incoming derivative(s)", len(keys))
		}
		keys, err = listActiveIncomingDerivativeKeys(node2)
		if err != nil {
			return false, fmt.Sprintf("failed to read node2 incoming derivative index: %v", err)
		}
		if len(keys) != 0 {
			return false, fmt.Sprintf("node2 still has %d active incoming derivative(s)", len(keys))
		}
		return true, "both nodes cleared incoming derivative indexes"
	})
}

func TestIntegration_DerivativesCancelBeforeOpen_HedgingFallback(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}

	runCase := func(t *testing.T, hedged bool) {
		t.Helper()

		installMockBTCResolver(t, "100")

		hedgeServer := newTestHedgeServer(t)
		hedgeKey, hedgeSecret := hedgeServer.auth()

		hub := newLoopbackHub()
		node1 := newTestNode(t, hub, 1, 19331)
		node2 := newTestNodeWithOptions(t, hub, 2, 19332, testNodeOptions{
			acceptingDerivatives: true,
			hedgeWebhookURL:      hedgeServer.url(),
			hedgeWebhookKey:      hedgeKey,
			hedgeWebhookSecret:   hedgeSecret,
		})
		defer node1.stop(t)
		defer node2.stop(t)

		node1.start()
		node2.start()

		seedAmount := tlb.MustFromDecimal("5", 9).Nano()
		ch12, ch21 := openAndSeedChannel(t, node1, node2, seedAmount)
		waitActiveChannelReady(t, node1, ch12.Our.Address)
		waitActiveChannelReady(t, node2, ch21.Our.Address)

		derivID, err := node1.derivatives.OpenPosition(context.Background(), ch12.Our.Address, "BTCUSDT", "long", 10, "0.01", "limit", "80")
		if err != nil {
			t.Fatalf("open limit derivative position failed: %v", err)
		}

		positionKeyNode1 := decodeDerivativePositionKeyForTest(t, derivID)
		metaOutNode1 := waitVirtualMeta(t, node1, positionKeyNode1, false, true)
		_, incomingKeyNode1, err := resolveDerivativePairKeysForTest(metaOutNode1, positionKeyNode1)
		if err != nil {
			t.Fatalf("resolve derivative pair keys failed: %v", err)
		}
		_ = waitVirtualMeta(t, node1, incomingKeyNode1, true, false)

		var incomingKeyNode2 ed25519.PublicKey
		waitFor(t, 35*time.Second, 300*time.Millisecond, func() (bool, string) {
			keys, err := listActiveIncomingDerivativeKeys(node2)
			if err != nil {
				return false, fmt.Sprintf("failed to list node2 incoming derivative index: %v", err)
			}
			if len(keys) == 0 {
				return false, "waiting for node2 incoming derivative in index"
			}
			incomingKeyNode2 = keys[0]
			return true, fmt.Sprintf("node2 has %d active incoming derivative(s)", len(keys))
		})
		metaInNode2 := waitVirtualMeta(t, node2, incomingKeyNode2, true, false)
		outgoingKeyNode2, _, err := resolveDerivativePairKeysForTest(metaInNode2, incomingKeyNode2)
		if err != nil {
			t.Fatalf("resolve node2 derivative pair keys failed: %v", err)
		}

		waitFor(t, 45*time.Second, 300*time.Millisecond, func() (bool, string) {
			meta, err := node2.svc.GetVirtualChannelMeta(context.Background(), incomingKeyNode2)
			if err != nil {
				return false, fmt.Sprintf("failed to read incoming derivative meta: %v", err)
			}
			monitor, ok := derivativeMonitor(meta)
			if !ok {
				return false, "waiting derivative monitor initialization"
			}
			if monitor.EntryCrossed {
				return false, "derivative must stay unopened"
			}
			return true, fmt.Sprintf("monitor ready for pending derivative (last_checked_at=%d)", monitor.LastCheckedAt)
		})

		waitHedgeCounts(t, hedgeServer, 1, 0)
		forceDerivativeHistoryTooOldForTest(t, node2, incomingKeyNode2)

		if hedged {
			if err = node2.derivatives.SetPositionHedged(context.Background(), base64.StdEncoding.EncodeToString(outgoingKeyNode2), true); err != nil {
				t.Fatalf("mark derivative hedged failed: %v", err)
			}
		}

		err = node1.derivatives.ClosePosition(context.Background(), ch12.Our.Address, derivID, "cancel")
		if hedged {
			if err != nil {
				t.Fatalf("queueing hedged cancel derivative position failed: %v", err)
			}
			waitFor(t, 5*time.Second, 200*time.Millisecond, func() (bool, string) {
				metaIn, err := node2.svc.GetVirtualChannelMeta(context.Background(), incomingKeyNode2)
				if err != nil {
					return false, fmt.Sprintf("failed to get node2 incoming derivative meta: %v", err)
				}
				if metaIn.Status != dbpkg.ConditionalStateActive {
					return false, fmt.Sprintf("hedged cancel should keep node2 incoming derivative active, got status=%d", metaIn.Status)
				}
				hedgedCount, closedCount := hedgeServer.counts()
				if hedgedCount != 1 || closedCount != 0 {
					return false, fmt.Sprintf("unexpected hedge counts hedged=%d closed=%d", hedgedCount, closedCount)
				}
				return true, "hedged cancel kept derivative active"
			})

			if err = node2.derivatives.SetPositionHedged(context.Background(), base64.StdEncoding.EncodeToString(outgoingKeyNode2), false); err != nil {
				t.Fatalf("clear derivative hedged failed: %v", err)
			}
			if err = node1.derivatives.ClosePosition(context.Background(), ch12.Our.Address, derivID, "cancel"); err != nil {
				t.Fatalf("cleanup cancel derivative position failed: %v", err)
			}
		} else if err != nil {
			t.Fatalf("cancel derivative position failed: %v", err)
		}

		waitHedgeCounts(t, hedgeServer, 1, 1)
	}

	t.Run("unhedged_allows_cancel", func(t *testing.T) {
		runCase(t, false)
	})
	t.Run("hedged_blocks_cancel", func(t *testing.T) {
		runCase(t, true)
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

func TestIntegration_DerivativesUncooperativeClose_TwoNodes(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}

	mockResolver := installMockBTCResolver(t, "100")

	hub := newLoopbackHub()
	hedgeServer := newTestHedgeServer(t)
	hedgeKey, hedgeSecret := hedgeServer.auth()
	node1 := newTestNode(t, hub, 1, 19501)
	node2 := newTestNodeWithOptions(t, hub, 2, 19502, testNodeOptions{
		acceptingDerivatives: true,
		hedgeWebhookURL:      hedgeServer.url(),
		hedgeWebhookKey:      hedgeKey,
		hedgeWebhookSecret:   hedgeSecret,
	})
	defer node1.stop(t)
	defer node2.stop(t)

	node1.start()
	node2.start()

	seedAmount := tlb.MustFromDecimal("5", 9).Nano()
	ch12, ch21 := openAndSeedChannel(t, node1, node2, seedAmount)

	derivID, err := openDerivativeForTest(context.Background(), node1, ch12.Our.Address, true, 10, "0.01")
	if err != nil {
		t.Fatalf("open derivative position failed: %v", err)
	}

	positionKeyNode1 := decodeDerivativePositionKeyForTest(t, derivID)
	metaOutNode1 := waitVirtualMeta(t, node1, positionKeyNode1, false, true)
	_, incomingKeyNode1, err := resolveDerivativePairKeysForTest(metaOutNode1, positionKeyNode1)
	if err != nil {
		t.Fatalf("resolve derivative pair keys failed: %v", err)
	}
	waitVirtualMeta(t, node1, incomingKeyNode1, true, false)

	var incomingKeyNode2 ed25519.PublicKey
	waitFor(t, 35*time.Second, 300*time.Millisecond, func() (bool, string) {
		keys, err := listActiveIncomingDerivativeKeys(node2)
		if err != nil {
			return false, fmt.Sprintf("failed to list node2 incoming derivative index: %v", err)
		}
		if len(keys) == 0 {
			return false, "waiting for node2 incoming derivative in index"
		}
		incomingKeyNode2 = keys[0]
		return true, fmt.Sprintf("node2 has %d active incoming derivative(s)", len(keys))
	})
	metaInNode2 := waitVirtualMeta(t, node2, incomingKeyNode2, true, false)
	outgoingKeyNode2, _, err := resolveDerivativePairKeysForTest(metaInNode2, incomingKeyNode2)
	if err != nil {
		t.Fatalf("resolve node2 derivative pair keys failed: %v", err)
	}
	waitVirtualMeta(t, node2, outgoingKeyNode2, false, true)
	waitHedgeCounts(t, hedgeServer, 1, 0)

	waitFor(t, 25*time.Second, 300*time.Millisecond, func() (bool, string) {
		meta, err := node2.svc.GetVirtualChannelMeta(context.Background(), incomingKeyNode2)
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

	targetPrice := tlb.MustFromDecimal("105", 9).Nano()
	if err = mockResolver.SetPrice(targetPrice); err != nil {
		t.Fatalf("set mock price failed: %v", err)
	}

	syncChannelPairFromDB(t, node1, node2, ch12.Our.Address, ch21.Our.Address)

	if err = node1.svc.RequestUncooperativeClose(context.Background(), ch12.Our.Address); err != nil {
		t.Fatalf("request uncooperative close failed: %v", err)
	}

	waitChannelInactiveForTest(t, node1, ch12.Our.Address)
	waitChannelInactiveForTest(t, node2, ch21.Our.Address)
	waitDerivativeMetaTerminalForTest(t, node2, outgoingKeyNode2)
	waitDerivativeMetaTerminalForTest(t, node2, incomingKeyNode2)
	waitHedgeCounts(t, hedgeServer, 1, 1)

	chAfterNode1, err := node1.db.GetChannel(context.Background(), ch12.Our.Address)
	if err != nil {
		t.Fatalf("get node1 channel after uncoop close failed: %v", err)
	}
	chAfterNode2, err := node2.db.GetChannel(context.Background(), ch21.Our.Address)
	if err != nil {
		t.Fatalf("get node2 channel after uncoop close failed: %v", err)
	}

	assertMirroredChannelPairStateForTest(t, chAfterNode1, chAfterNode2)

	t.Logf("[two-node-uncoop-flow] %s", strings.Join(node1.chain.flowSnapshot(), " -> "))
}

func TestIntegration_DerivativesUncooperativeClose_TwoNodes_Testnet(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}
	if os.Getenv("PAYMENTS_TESTNET_TWO_NODE") == "" {
		t.Skip("set PAYMENTS_TESTNET_TWO_NODE=1 to run real testnet two-node flow")
	}

	installMockBTCResolver(t, "100")

	hub := newTestnetLoopbackHub(t)
	hedgeServer := newTestHedgeServer(t)
	hedgeKey, hedgeSecret := hedgeServer.auth()
	node1 := newTestnetNode(t, hub, 1, 19601)
	node2 := newTestnetNodeWithOptions(t, hub, 2, 19602, testNodeOptions{
		acceptingDerivatives: true,
		hedgeWebhookURL:      hedgeServer.url(),
		hedgeWebhookKey:      hedgeKey,
		hedgeWebhookSecret:   hedgeSecret,
	})
	defer node1.stop(t)
	defer node2.stop(t)

	node1.start()
	node2.start()

	t.Logf("[wallet][shared] %s", node1.wallet.WalletAddress().String())

	ch12, ch21 := openAndFundChannelTestnet(t, node1, node2, "0.6")

	mockResolver := installMockBTCResolver(t, "100")

	waitDerivativeMarketPriceForTest(t, node1, "BTCUSDT", 30*time.Second)

	derivID, err := openDerivativeForTest(context.Background(), node1, ch12.Our.Address, true, 10, "0.01")
	if err != nil {
		t.Fatalf("open derivative position failed: %v", err)
	}

	positionKeyNode1 := decodeDerivativePositionKeyForTest(t, derivID)
	metaOutNode1 := waitVirtualMeta(t, node1, positionKeyNode1, false, true)
	_, incomingKeyNode1, err := resolveDerivativePairKeysForTest(metaOutNode1, positionKeyNode1)
	if err != nil {
		t.Fatalf("resolve derivative pair keys failed: %v", err)
	}
	metaInNode1 := waitVirtualMeta(t, node1, incomingKeyNode1, true, false)

	var incomingKeyNode2 ed25519.PublicKey
	waitFor(t, 90*time.Second, time.Second, func() (bool, string) {
		keys, err := listActiveIncomingDerivativeKeys(node2)
		if err != nil {
			return false, fmt.Sprintf("failed to list node2 incoming derivative index: %v", err)
		}
		if len(keys) == 0 {
			return false, "waiting for node2 incoming derivative in index"
		}
		incomingKeyNode2 = keys[0]
		return true, fmt.Sprintf("node2 has %d active incoming derivative(s)", len(keys))
	})
	metaInNode2 := waitVirtualMeta(t, node2, incomingKeyNode2, true, false)
	outgoingKeyNode2, _, err := resolveDerivativePairKeysForTest(metaInNode2, incomingKeyNode2)
	if err != nil {
		t.Fatalf("resolve node2 derivative pair keys failed: %v", err)
	}
	waitHedgeCounts(t, hedgeServer, 1, 0)

	waitFor(t, 90*time.Second, time.Second, func() (bool, string) {
		meta, err := node2.svc.GetVirtualChannelMeta(context.Background(), incomingKeyNode2)
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

	outCondNode1 := loadResolvableFromMetaForTest(t, node1, metaOutNode1, true)
	inCondNode1 := loadResolvableFromMetaForTest(t, node1, metaInNode1, false)
	if outCondNode1.ResolverAddr == nil {
		t.Fatalf("derivative resolver address is nil")
	}

	targetPrice := tlb.MustFromDecimal("105", 9).Nano()
	if err = mockResolver.SetPrice(targetPrice); err != nil {
		t.Fatalf("set mock price failed: %v", err)
	}
	pnl := calculateDerivativePnLForTest(outCondNode1, targetPrice)
	wantOutSettle := expectedOutgoingDerivativeSettleForTest(pnl, outCondNode1.Amount)
	wantInSettle := expectedIncomingDerivativeSettleForTest(pnl, inCondNode1.Amount)

	if err = node1.svc.RequestUncooperativeClose(context.Background(), ch12.Our.Address); err != nil {
		t.Fatalf("request uncooperative close failed: %v", err)
	}

	waitFor(t, 90*time.Second, time.Second, func() (bool, string) {
		acc, err := hub.live.chain.GetAccount(context.Background(), outCondNode1.ResolverAddr, time.Time{})
		if err != nil {
			return false, fmt.Sprintf("failed to fetch resolver account: %v", err)
		}
		if acc != nil && acc.IsActive && acc.HasState {
			return true, fmt.Sprintf("resolver %s is active on testnet", outCondNode1.ResolverAddr.String())
		}
		return false, "waiting resolver deployment on testnet"
	})

	waitFor(t, 180*time.Second, 2*time.Second, func() (bool, string) {
		flow := strings.Join(hub.live.flow.snapshot(), " | ")
		need := []string{"uncoop-start:", "deploy-resolver:", "commit-resolver:", "finish-close:"}
		for _, part := range need {
			if !strings.Contains(flow, part) {
				return false, "waiting flow step " + part
			}
		}
		return true, flow
	})

	waitChannelInactiveWithinForTest(t, node1, ch12.Our.Address, 240*time.Second)
	waitChannelInactiveWithinForTest(t, node2, ch21.Our.Address, 240*time.Second)

	metaOutClosedNode2 := waitDerivativeMetaTerminalForTest(t, node2, outgoingKeyNode2)
	metaInClosedNode2 := waitDerivativeMetaTerminalForTest(t, node2, incomingKeyNode2)
	if metaOutClosedNode2.LastKnownResolve != nil {
		assertDerivativeResolveAmountForTest(t, metaOutClosedNode2, positionKeyNode1, incomingKeyNode1, wantOutSettle, wantInSettle)
	}
	if metaInClosedNode2.LastKnownResolve != nil {
		assertDerivativeResolveAmountForTest(t, metaInClosedNode2, positionKeyNode1, incomingKeyNode1, wantOutSettle, wantInSettle)
	}

	chAfterNode1, err := node1.db.GetChannel(context.Background(), ch12.Our.Address)
	if err != nil {
		t.Fatalf("get node1 channel after uncoop close failed: %v", err)
	}
	chAfterNode2, err := node2.db.GetChannel(context.Background(), ch21.Our.Address)
	if err != nil {
		t.Fatalf("get node2 channel after uncoop close failed: %v", err)
	}

	assertMirroredChannelPairStateForTest(t, chAfterNode1, chAfterNode2)
	waitHedgeCounts(t, hedgeServer, 1, 1)

	t.Logf("[two-node-testnet-addresses] wallet=%s channel_a=%s channel_b=%s resolver=%s",
		node1.wallet.WalletAddress().String(), ch12.Our.Address, ch21.Our.Address, outCondNode1.ResolverAddr.String())
	t.Logf("[two-node-testnet-flow] %s", strings.Join(hub.live.flow.snapshot(), " -> "))
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
	nodeA.chain.syncChannelPair(t, chAB, chBA)

	return chAB, chBA
}

func syncChannelPairFromDB(t *testing.T, nodeA, nodeB *testNode, addrA, addrB string) (*dbpkg.Channel, *dbpkg.Channel) {
	t.Helper()

	chA, err := nodeA.db.GetChannel(context.Background(), addrA)
	if err != nil {
		t.Fatalf("node %d get channel %s failed: %v", nodeA.idx, addrA, err)
	}
	chB, err := nodeB.db.GetChannel(context.Background(), addrB)
	if err != nil {
		t.Fatalf("node %d get channel %s failed: %v", nodeB.idx, addrB, err)
	}

	nodeA.chain.syncChannelPair(t, chA, chB)
	return chA, chB
}

func waitChannelInactiveForTest(t *testing.T, node *testNode, channelAddr string) {
	t.Helper()

	waitChannelInactiveWithinForTest(t, node, channelAddr, 30*time.Second)
}

func waitChannelInactiveWithinForTest(t *testing.T, node *testNode, channelAddr string, timeout time.Duration) {
	t.Helper()

	waitFor(t, timeout, 300*time.Millisecond, func() (bool, string) {
		ch, err := node.db.GetChannel(context.Background(), channelAddr)
		if err != nil {
			return false, fmt.Sprintf("node %d get channel %s failed: %v", node.idx, channelAddr, err)
		}
		if ch.Status == dbpkg.ChannelStateInactive {
			return true, fmt.Sprintf("node %d channel %s is inactive", node.idx, channelAddr)
		}
		return false, fmt.Sprintf("waiting node %d channel %s inactive, status=%d", node.idx, channelAddr, ch.Status)
	})
}

func openAndFundChannelTestnet(t *testing.T, nodeA, nodeB *testNode, amount string) (*dbpkg.Channel, *dbpkg.Channel) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	opened, err := nodeA.svc.OpenChannelWithNode(ctx, nodeB.pub)
	if err != nil {
		t.Fatalf("open channel %d->%d failed: %v", nodeA.idx, nodeB.idx, err)
	}
	t.Logf("offchain channel opened: %s", opened.String())

	chAB := waitChannelByPeer(t, nodeA, nodeB.pub)
	chBA := waitChannelByPeer(t, nodeB, nodeA.pub)

	waitActiveChannelReady(t, nodeA, chAB.Our.Address)
	waitActiveChannelReady(t, nodeB, chBA.Our.Address)

	if chAB.Their.Address != chBA.Our.Address {
		t.Fatalf("unexpected pair mapping: node %d their=%s node %d our=%s", nodeA.idx, chAB.Their.Address, nodeB.idx, chBA.Our.Address)
	}
	if chBA.Their.Address != chAB.Our.Address {
		t.Fatalf("unexpected pair mapping: node %d their=%s node %d our=%s", nodeB.idx, chBA.Their.Address, nodeA.idx, chAB.Our.Address)
	}

	topup := tlb.MustFromDecimal(amount, 9)
	deployChannelContractForTestnet(t, nodeA, chAB, topup)

	waitChannelOnchainReadyForTestnet(t, nodeA, chAB.Our.Address, topup.Nano())
	waitChannelOnchainReadyForTestnet(t, nodeB, chBA.Our.Address, big.NewInt(1))
	fundChannelAddressForTestnet(t, nodeA, chAB.Our.Address, tlb.MustFromTON("0.25"))
	fundChannelAddressForTestnet(t, nodeA, chBA.Our.Address, tlb.MustFromTON("0.25"))
	waitChannelBalancesAtLeastForTestnet(t, nodeA, chAB.Our.Address, big.NewInt(200_000_000), big.NewInt(200_000_000))
	waitChannelBalancesAtLeastForTestnet(t, nodeB, chBA.Our.Address, big.NewInt(200_000_000), big.NewInt(200_000_000))
	return chAB, chBA
}

func deployChannelContractForTestnet(t *testing.T, node *testNode, ch *dbpkg.Channel, amount tlb.Coins) {
	t.Helper()

	if node.walletRaw == nil {
		t.Fatalf("node %d has no raw wallet for testnet deploy", node.idx)
	}

	var code *cell.Cell
	for _, candidate := range payments.PaymentChannelCodes {
		if bytes.Equal(candidate.Hash(), ch.CodeHash) {
			code = candidate
			break
		}
	}
	if code == nil {
		t.Fatalf("node %d cannot resolve channel code for %s", node.idx, ch.Our.Address)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	addr, tx, _, err := node.walletRaw.DeployContractWaitTransaction(ctx, amount, ch.InitMessageBody, code, ch.InitialData)
	if err != nil {
		t.Fatalf("node %d deploy contract %s failed: %v", node.idx, ch.Our.Address, err)
	}

	t.Logf("[testnet-deploy][node=%d] channel=%s tx=%s", node.idx, addr.String(), base64.StdEncoding.EncodeToString(tx.Hash))
}

func fundChannelAddressForTestnet(t *testing.T, node *testNode, channelAddr string, amount tlb.Coins) {
	t.Helper()

	if node.walletRaw == nil {
		t.Fatalf("node %d has no raw wallet for testnet funding", node.idx)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	tx, _, err := node.walletRaw.SendWaitTransaction(ctx, wallet.SimpleMessage(address.MustParseAddr(channelAddr), amount, cell.BeginCell().EndCell()))
	if err != nil {
		t.Fatalf("node %d fund channel %s failed: %v", node.idx, channelAddr, err)
	}

	t.Logf("[testnet-topup][node=%d] channel=%s amount=%s tx=%s", node.idx, channelAddr, amount.String(), base64.StdEncoding.EncodeToString(tx.Hash))
}

func waitChannelOnchainReadyForTestnet(t *testing.T, node *testNode, channelAddr string, _ *big.Int) {
	t.Helper()

	waitFor(t, 180*time.Second, time.Second, func() (bool, string) {
		ch, err := node.db.GetChannel(context.Background(), channelAddr)
		if err != nil {
			return false, fmt.Sprintf("node %d get channel %s failed: %v", node.idx, channelAddr, err)
		}

		if !ch.Our.ActiveOnchain || !ch.Their.ActiveOnchain {
			return false, fmt.Sprintf("waiting node %d channel %s onchain activation our=%t their=%t", node.idx, channelAddr, ch.Our.ActiveOnchain, ch.Their.ActiveOnchain)
		}

		ourBal := ch.Our.OnchainBalances[payments.GetTONBalanceID()]
		theirBal := ch.Their.OnchainBalances[payments.GetTONBalanceID()]
		if ourBal == nil || theirBal == nil {
			return false, fmt.Sprintf("waiting node %d channel %s onchain balances", node.idx, channelAddr)
		}
		if ourBal.Sign() <= 0 || theirBal.Sign() <= 0 {
			return false, fmt.Sprintf("waiting node %d channel %s positive balances our=%s their=%s", node.idx, channelAddr, ourBal.String(), theirBal.String())
		}

		return true, fmt.Sprintf("node %d channel %s is active onchain with balances our=%s their=%s", node.idx, channelAddr, ourBal.String(), theirBal.String())
	})
}

func waitChannelBalancesAtLeastForTestnet(t *testing.T, node *testNode, channelAddr string, ourMin, theirMin *big.Int) {
	t.Helper()

	waitFor(t, 180*time.Second, time.Second, func() (bool, string) {
		ch, err := node.db.GetChannel(context.Background(), channelAddr)
		if err != nil {
			return false, fmt.Sprintf("node %d get channel %s failed: %v", node.idx, channelAddr, err)
		}

		ourBal := ch.Our.OnchainBalances[payments.GetTONBalanceID()]
		theirBal := ch.Their.OnchainBalances[payments.GetTONBalanceID()]
		if ourBal == nil || theirBal == nil {
			return false, fmt.Sprintf("waiting node %d channel %s onchain balances", node.idx, channelAddr)
		}
		if ourBal.Cmp(ourMin) < 0 || theirBal.Cmp(theirMin) < 0 {
			return false, fmt.Sprintf("waiting node %d channel %s balances our=%s their=%s", node.idx, channelAddr, ourBal.String(), theirBal.String())
		}

		return true, fmt.Sprintf("node %d channel %s funded our=%s their=%s", node.idx, channelAddr, ourBal.String(), theirBal.String())
	})
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

func waitDerivativeMarketPriceForTest(t *testing.T, node *testNode, symbol string, timeout time.Duration) {
	t.Helper()

	waitFor(t, timeout, time.Second, func() (bool, string) {
		quote, err := node.derivatives.GetMarketPrice(context.Background(), symbol)
		if err != nil {
			return false, fmt.Sprintf("waiting market price for %s: %v", symbol, err)
		}
		return true, fmt.Sprintf("market price for %s is ready at %d", symbol, quote.At)
	})
}

func waitDerivativeMetaInactiveForTest(t *testing.T, node *testNode, key ed25519.PublicKey) *dbpkg.ConditionalMeta {
	t.Helper()

	var meta *dbpkg.ConditionalMeta
	waitFor(t, 40*time.Second, 300*time.Millisecond, func() (bool, string) {
		got, err := node.svc.GetVirtualChannelMeta(context.Background(), key)
		if err != nil {
			return false, fmt.Sprintf("failed to get derivative meta: %v", err)
		}
		if got.Status == dbpkg.ConditionalStateActive || got.Status == dbpkg.ConditionalStatePending {
			return false, fmt.Sprintf("waiting derivative inactive, status=%d", got.Status)
		}
		meta = got
		return true, fmt.Sprintf("derivative inactive, status=%d", got.Status)
	})

	return meta
}

func waitDerivativeMetaTerminalForTest(t *testing.T, node *testNode, key ed25519.PublicKey) *dbpkg.ConditionalMeta {
	t.Helper()

	var meta *dbpkg.ConditionalMeta
	waitFor(t, 40*time.Second, 300*time.Millisecond, func() (bool, string) {
		got, err := node.svc.GetVirtualChannelMeta(context.Background(), key)
		if err != nil {
			return false, fmt.Sprintf("failed to get derivative meta: %v", err)
		}
		if got.Status != dbpkg.ConditionalStateClosed && got.Status != dbpkg.ConditionalStateRemoved {
			return false, fmt.Sprintf("waiting derivative terminal status, got=%d", got.Status)
		}
		meta = got
		return true, fmt.Sprintf("derivative terminal, status=%d", got.Status)
	})

	return meta
}

func waitDerivativeMetaResolvedForTest(t *testing.T, node *testNode, key ed25519.PublicKey) *dbpkg.ConditionalMeta {
	t.Helper()

	var meta *dbpkg.ConditionalMeta
	waitFor(t, 40*time.Second, 300*time.Millisecond, func() (bool, string) {
		got, err := node.svc.GetVirtualChannelMeta(context.Background(), key)
		if err != nil {
			return false, fmt.Sprintf("failed to get derivative meta: %v", err)
		}
		if got.LastKnownResolve == nil {
			return false, fmt.Sprintf("waiting derivative resolve propagation, status=%d", got.Status)
		}
		meta = got
		return true, fmt.Sprintf("derivative resolved, status=%d", got.Status)
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

func assertDerivativeResolveAmountForTest(t *testing.T, meta *dbpkg.ConditionalMeta, outgoingKey, incomingKey ed25519.PublicKey, wantOutgoing, wantIncoming *big.Int) {
	t.Helper()

	state := loadResolvableStateFromMetaForTest(t, meta)
	want := expectedDerivativeSettleForKeyForTest(t, meta.Key, outgoingKey, incomingKey, wantOutgoing, wantIncoming)

	if state.Amount.Cmp(want) != 0 {
		t.Fatalf("unexpected derivative resolve amount for key %s: got %s want %s",
			base64.StdEncoding.EncodeToString(meta.Key), state.Amount.String(), want.String())
	}
}

func expectedDerivativeSettleForKeyForTest(t *testing.T, key, outgoingKey, incomingKey ed25519.PublicKey, wantOutgoing, wantIncoming *big.Int) *big.Int {
	t.Helper()

	switch {
	case bytes.Equal(key, outgoingKey):
		return new(big.Int).Set(wantOutgoing)
	case bytes.Equal(key, incomingKey):
		return new(big.Int).Set(wantIncoming)
	default:
		t.Fatalf("unexpected derivative key %s", base64.StdEncoding.EncodeToString(key))
		return nil
	}
}

func assertDerivativeAmountDeltaForTest(t *testing.T, ch *dbpkg.Channel, outCond, inCond *conditionals.ConditionalResolvable, outBefore, inBefore, wantOutTransfer, wantInTransfer *big.Int) {
	t.Helper()

	outAfter := loadSendActionAmountForMetaSide(t, ch, outCond, true)
	inAfter := loadSendActionAmountForMetaSide(t, ch, inCond, false)
	outDiff := new(big.Int).Sub(outAfter, outBefore)
	inDiff := new(big.Int).Sub(inAfter, inBefore)

	if outDiff.Cmp(wantOutTransfer) != 0 {
		t.Fatalf("unexpected outgoing transfer delta: got %s want %s", outDiff.String(), wantOutTransfer.String())
	}
	if inDiff.Cmp(wantInTransfer) != 0 {
		t.Fatalf("unexpected incoming transfer delta: got %s want %s", inDiff.String(), wantInTransfer.String())
	}
}

func assertMirroredChannelPairStateForTest(t *testing.T, chA, chB *dbpkg.Channel) {
	t.Helper()

	hashOrNil := func(c *cell.Cell) []byte {
		if c == nil {
			return nil
		}
		return c.Hash()
	}

	if !bytes.Equal(hashOrNil(chA.Our.Data.ActionStates.AsCell()), hashOrNil(chB.Their.Data.ActionStates.AsCell())) {
		t.Fatalf("mirrored action states hash mismatch: nodeA.our=%x nodeB.their=%x",
			hashOrNil(chA.Our.Data.ActionStates.AsCell()), hashOrNil(chB.Their.Data.ActionStates.AsCell()))
	}
	if !bytes.Equal(hashOrNil(chA.Their.Data.ActionStates.AsCell()), hashOrNil(chB.Our.Data.ActionStates.AsCell())) {
		t.Fatalf("mirrored action states hash mismatch: nodeA.their=%x nodeB.our=%x",
			hashOrNil(chA.Their.Data.ActionStates.AsCell()), hashOrNil(chB.Our.Data.ActionStates.AsCell()))
	}
	if !bytes.Equal(hashOrNil(chA.Our.Data.Conditionals.AsCell()), hashOrNil(chB.Their.Data.Conditionals.AsCell())) {
		t.Fatalf("mirrored conditionals hash mismatch: nodeA.our=%x nodeB.their=%x",
			hashOrNil(chA.Our.Data.Conditionals.AsCell()), hashOrNil(chB.Their.Data.Conditionals.AsCell()))
	}
	if !bytes.Equal(hashOrNil(chA.Their.Data.Conditionals.AsCell()), hashOrNil(chB.Our.Data.Conditionals.AsCell())) {
		t.Fatalf("mirrored conditionals hash mismatch: nodeA.their=%x nodeB.our=%x",
			hashOrNil(chA.Their.Data.Conditionals.AsCell()), hashOrNil(chB.Our.Data.Conditionals.AsCell()))
	}
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
