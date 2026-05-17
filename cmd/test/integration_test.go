package test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/pkg/payments/actions"
	"github.com/xssnick/ton-payment-network/pkg/payments/conditionals"
	condcontracts "github.com/xssnick/ton-payment-network/pkg/payments/conditionals/contracts"
	"github.com/xssnick/ton-payment-network/pkg/payments/conditionals/oracle"
	client2 "github.com/xssnick/ton-payment-network/tonpayments/chain/client"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/liteclient"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/ton"
	"github.com/xssnick/tonutils-go/ton/wallet"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"log"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"
)

var api = func() ton.APIClientWrapped {
	client := liteclient.NewConnectionPool()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	//err := client.AddConnectionsFromConfigUrl(ctx, "https://ton-blockchain.github.io/testnet-global.config.json")
	err := client.AddConnection(ctx, "109.236.80.69:49913", "AxFZRHVD1qIO9Fyva52P4vC3tRvk8ac1KKOG0c6IVio=")
	if err != nil {
		panic(err)
	}

	return ton.NewAPIClient(client).WithRetry()
}()

var code = payments.PaymentChannelCodes[0]

var _seed = strings.Split(os.Getenv("WALLET_SEED"), " ")

func TestClient_AsyncChannelFullFlow(t *testing.T) {
	if len(_seed) < 12 || strings.TrimSpace(os.Getenv("WALLET_SEED")) == "" {
		t.Skip("WALLET_SEED is required for testnet integration flow")
	}

	chainClient := client2.NewTON(api)
	client := payments.NewPaymentChannelClient(chainClient)
	ctx := api.Client().StickyContext(context.Background())

	chID, err := payments.RandomChannelID()
	if err != nil {
		t.Fatal(err)
	}

	aPubKey, aKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	bPubKey, bKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}

	w, err := wallet.FromSeed(api, _seed, wallet.HighloadV2R2)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to init wallet: %w", err))
	}
	log.Println("wallet:", w.Address().String())

	closeConfig := payments.ClosingConfig{
		QuarantineDuration:             25,
		ReplicationMessageAttachAmount: tlb.MustFromTON("0.047"),
		ConditionalCloseDuration:       50,
		ActionsDuration:                40,
	}

	_, _, bSig, err := client.GetDeployAsyncChannelParams(chID, false, 0, bKey, aPubKey, nil, closeConfig)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to build deploy channel params: %w", err))
	}

	body, data, _, err := client.GetDeployAsyncChannelParams(chID, true, 0, aKey, bPubKey, bSig, closeConfig)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to build deploy channel params: %w", err))
	}

	channelAddr, _, block, err := w.DeployContractWaitTransaction(ctx, tlb.MustFromTON("0.6"), body, code, data)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to deploy channel: %w", err))
	}
	log.Println("channel deployed:", channelAddr.String())

	waitBlock := func(delta uint32) {
		block, err = api.WaitForBlock(block.SeqNo + delta).GetMasterchainInfo(ctx)
		if err != nil {
			t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
		}
	}

	waitNextBlock := func() {
		waitBlock(1)
	}

	sendExternalRetry := func(dst *address.Address, body *cell.Cell, errMsg string) *tlb.Transaction {
		for {
			tx, _, _, sendErr := api.SendExternalMessageWaitTransaction(ctx, &tlb.ExternalMessage{
				DstAddr: dst,
				Body:    body,
			})
			if sendErr == nil {
				return tx
			}
			t.Log(fmt.Errorf("%s: %w", errMsg, sendErr))
			waitNextBlock()
		}
	}

	sendExternalOnce := func(dst *address.Address, body *cell.Cell, errMsg string) *tlb.Transaction {
		tx, _, _, sendErr := api.SendExternalMessageWaitTransaction(ctx, &tlb.ExternalMessage{
			DstAddr: dst,
			Body:    body,
		})
		if sendErr != nil {
			t.Fatal(fmt.Errorf("%s: %w", errMsg, sendErr))
		}
		return tx
	}

	sendWalletRetry := func(msg *wallet.Message, errMsg string) *tlb.Transaction {
		for {
			tx, _, sendErr := w.SendWaitTransaction(ctx, msg)
			if sendErr == nil {
				return tx
			}
			t.Log(fmt.Errorf("%s: %w", errMsg, sendErr))
			waitNextBlock()
		}
	}

	getChannelRetry := func(addr *address.Address, errMsg string) *payments.ChannelContract {
		for {
			ch, getErr := client.GetChannel(ctx, addr, true, time.Time{})
			if getErr == nil {
				return ch
			}
			t.Log(fmt.Errorf("%s: %w, retrying", errMsg, getErr))
			waitNextBlock()
		}
	}

	getBalanceRetry := func(addr *address.Address, errMsg string) *big.Int {
		for {
			acc, getErr := chainClient.GetAccount(ctx, addr, time.Time{})
			if getErr == nil {
				return acc.Balance.Nano()
			}
			t.Log(fmt.Errorf("%s: %w, retrying", errMsg, getErr))
			waitNextBlock()
		}
	}

	waitChannel := func(addr *address.Address, fetchErrMsg string, onWait func(*payments.ChannelContract), ready func(*payments.ChannelContract) bool) *payments.ChannelContract {
		for {
			ch := getChannelRetry(addr, fetchErrMsg)
			if ready(ch) {
				return ch
			}
			if onWait != nil {
				onWait(ch)
			}
			waitNextBlock()
		}
	}

	ch := getChannelRetry(channelAddr, "failed to get channel")

	log.Println("party channel addr:", ch.Storage.PartyAddress.String())

	ch2 := getChannelRetry(ch.Storage.PartyAddress, "failed to get party channel")

	if ch.Status != payments.ChannelStatusOpen {
		t.Fatal("channel status incorrect")
	}
	if ch2.Status != payments.ChannelStatusOpen {
		t.Fatal("party channel status incorrect")
	}

	log.Println("channel is active", ch.Storage.Initialized)

	until := uint32(time.Now().Add(90 * time.Second).Unix())
	msg := wallet.SimpleMessage(ch.Storage.PartyAddress, tlb.MustFromTON("0.1"), cell.BeginCell().EndCell())
	_, bSig, err = ch.PrepareDoubleExternalMessage(bKey, nil, []payments.WalletMessage{convertMsg(msg)}, until)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to prepare double signed message b: %w", err))
	}

	body, _, err = ch.PrepareDoubleExternalMessage(aKey, bSig, []payments.WalletMessage{convertMsg(msg)}, until)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to prepare double signed message b: %w", err))
	}

	tx := sendExternalRetry(channelAddr, body, "failed to send tx")
	log.Println("double signed tx sent:", base64.StdEncoding.EncodeToString(tx.Hash))

	_, bSig, err = ch.PrepareCoopCommitMessage(bKey, nil, 1, nil, true)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to prepare double signed message: %w", err))
	}

	body, _, err = ch.PrepareCoopCommitMessage(aKey, bSig, 1, nil, true)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to prepare double signed message: %w", err))
	}

	tx = sendExternalOnce(channelAddr, body, "failed to send tx")
	log.Println("commit tx sent:", base64.StdEncoding.EncodeToString(tx.Hash))

	ch = waitChannel(channelAddr, "failed to get channel", func(_ *payments.ChannelContract) {
		t.Log("commit not yet updated")
	}, func(cur *payments.ChannelContract) bool {
		return cur.Storage.CommittedSeqno == 1
	})

	ch2 = waitChannel(ch.Storage.PartyAddress, "failed to get party channel", func(_ *payments.ChannelContract) {
		t.Log("commit not yet updated")
	}, func(cur *payments.ChannelContract) bool {
		return cur.Storage.CommittedSeqno == 1
	})
	log.Println("commit updated")

	_, bSig, err = ch.PrepareCoopCloseMessage(bKey, nil, 2, true)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to prepare double signed message: %w", err))
	}

	body, _, err = ch.PrepareCoopCloseMessage(aKey, bSig, 2, true)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to prepare double signed message: %w", err))
	}

	tx = sendExternalOnce(channelAddr, body, "failed to send tx")
	log.Println("close tx sent:", base64.StdEncoding.EncodeToString(tx.Hash))

	ch = waitChannel(channelAddr, "failed to get channel", func(_ *payments.ChannelContract) {
		t.Log("close not yet updated")
	}, func(cur *payments.ChannelContract) bool {
		return !cur.Storage.Initialized
	})

	ch2 = waitChannel(ch.Storage.PartyAddress, "failed to get party channel", func(_ *payments.ChannelContract) {
		t.Log("close not yet updated")
	}, func(cur *payments.ChannelContract) bool {
		return !cur.Storage.Initialized
	})
	log.Println("close updated")

	until = uint32(time.Now().Add(90 * time.Second).Unix())
	text, _ := wallet.CreateCommentCell("респект тем кто с нами делится чудесами")
	msg = wallet.SimpleMessage(ch.Address, tlb.MustFromTON("0.08"), text)
	body, err = ch.PrepareOwnerExternalMessage(aKey, []payments.WalletMessage{convertMsg(msg)}, until)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to prepare double signed message b: %w", err))
	}

	tx = sendExternalOnce(ch.Address, body, "failed to send tx")
	log.Println("owner tx sent:", base64.StdEncoding.EncodeToString(tx.Hash))

	prevWSeq := ch.Storage.WalletSeqno
	ch = waitChannel(ch.Address, "failed to get channel", func(cur *payments.ChannelContract) {
		t.Log("wallet seqno not yet updated", cur.Storage.WalletSeqno)
	}, func(cur *payments.ChannelContract) bool {
		return cur.Storage.WalletSeqno == prevWSeq+1
	})

	_, _, bSig, err = client.GetDeployAsyncChannelParams(chID, false, 3, bKey, aPubKey, nil, closeConfig)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to build deploy channel params: %w", err))
	}

	body, _, _, err = client.GetDeployAsyncChannelParams(chID, true, 3, aKey, bPubKey, bSig, closeConfig)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to build deploy channel params: %w", err))
	}

	until = uint32(time.Now().Add(90 * time.Second).Unix())
	msg = wallet.SimpleMessage(ch.Address, tlb.MustFromTON("0.1"), body)
	body, err = ch.PrepareOwnerExternalMessage(aKey, []payments.WalletMessage{convertMsg(msg)}, until)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to prepare double signed message b: %w", err))
	}

	tx = sendExternalOnce(ch.Address, body, "failed to send tx")
	log.Println("init external tx sent:", base64.StdEncoding.EncodeToString(tx.Hash))

	ch = waitChannel(channelAddr, "failed to get channel", func(_ *payments.ChannelContract) {
		t.Log("init not yet updated")
	}, func(cur *payments.ChannelContract) bool {
		return cur.Storage.Initialized
	})

	ch2 = waitChannel(ch.Storage.PartyAddress, "failed to get party channel", func(_ *payments.ChannelContract) {
		t.Log("init not yet updated")
	}, func(cur *payments.ChannelContract) bool {
		return cur.Storage.Initialized
	})
	log.Println("init updated")

	vPubKey, vKey, _ := ed25519.GenerateKey(nil)
	_ = vKey

	condA := cell.NewDict(256)
	condB := cell.NewDict(256)
	_ = condB

	actA := cell.NewDict(256)
	actB := cell.NewDict(256)

	a1 := actions.ActionSendTonInsured{
		AddressA: ch.Address,
		AddressB: ch2.Address,
	}
	actC := a1.Serialize()
	actStateA, _ := tlb.ToCell(actions.StateActionSend{
		Amount:        actions.Coins{Val: tlb.MustFromTON("0.009999").Nano()},
		Commited:      actions.Coins{Val: tlb.MustFromTON("0.00").Nano()},
		CommitedSeqno: 0,
	})

	actStateB, _ := tlb.ToCell(actions.StateActionSend{
		Amount:        actions.Coins{Val: tlb.MustFromTON("0.00").Nano()},
		Commited:      actions.Coins{Val: tlb.MustFromTON("0.00").Nano()},
		CommitedSeqno: 0,
	})

	_ = actA.SetIntKey(new(big.Int).SetBytes(actC.Hash()), actStateA)
	_ = actB.SetIntKey(new(big.Int).SetBytes(actC.Hash()), actStateB)

	loadActionAmount := func(dict *cell.Dictionary, label string) *big.Int {
		slice, err := dict.LoadValueByIntKey(new(big.Int).SetBytes(actC.Hash()))
		if err != nil {
			t.Fatal(fmt.Errorf("failed to load %s action state: %w", label, err))
		}

		var state actions.StateActionSend
		if err = payments.LoadState(&state, slice.MustToCell()); err != nil {
			t.Fatal(fmt.Errorf("failed to parse %s action state: %w", label, err))
		}

		t.Logf("[balance-flow][%s] action_amount=%s", label, tlb.MustFromNano(state.Amount.Nano(), 9).String())
		return state.Amount.Nano()
	}

	initialActionAmount := loadActionAmount(actA, "initial")

	vch := conditionals.ConditionalVirtualChannel{
		Action:   &a1,
		Key:      vPubKey,
		Capacity: tlb.MustFromTON("0.03").Nano(),
		Fee:      tlb.MustFromTON("0.01").Nano(),
		Prepay:   tlb.MustFromTON("0.00").Nano(),
		Deadline: time.Now().Add(5 * time.Minute).Unix(),
	}
	_ = condA.SetIntKey(big.NewInt(0), vch.Serialize())
	vch2 := conditionals.ConditionalVirtualChannel{
		Action:   &a1,
		Key:      vPubKey,
		Capacity: tlb.MustFromTON("0.04").Nano(),
		Fee:      tlb.MustFromTON("0.00").Nano(),
		Prepay:   tlb.MustFromTON("0.01").Nano(),
		Deadline: time.Now().Add(5 * time.Minute).Unix(),
	}
	_ = condA.SetIntKey(big.NewInt(5), vch2.Serialize())

	// Prepare derivative resolver contract and conditional
	oraclePub, oracleKey, _ := ed25519.GenerateKey(nil)
	entryPrice := tlb.MustFromTON("100").Nano()
	finalPrice := tlb.MustFromTON("110").Nano()

	cfg := condcontracts.DerivativeConfig{
		OracleKey:          oraclePub,
		QuarantineDuration: 1,
		AcceptionWindow:    10,
		AddressA:           ch.Address,
		AddressB:           ch2.Address,
	}
	stor, err := condcontracts.BuildDerivativeStorage(vPubKey, cfg, tlb.MustFromTON("0.1"), 1, false, tlb.MustFromTON("100"), uint32(time.Now().Unix()-30))
	if err != nil {
		t.Fatal(fmt.Errorf("failed to build derivative storage: %w", err))
	}
	si, err := condcontracts.BuildDerivativeStateInit(stor)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to build derivative state init: %w", err))
	}
	resolverAddr, err := condcontracts.CalcDerivativeAddress(si)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to calc derivative address: %w", err))
	}
	// deploy resolver contract
	_, _, block, err = w.DeployContractWaitTransaction(ctx, tlb.MustFromTON("0.05"), nil, si.Code, si.Data)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to deploy derivative resolver: %w", err))
	}
	log.Println("derivative resolver deployed:", resolverAddr.String())

	// Setup mock price resolver (+10% increase)
	mock := oracle.NewMockProvider(finalPrice)
	priceResolver := oracle.NewResolver(mock)
	defer priceResolver.Close()

	der := conditionals.ConditionalResolvable{
		Key:          vPubKey,
		Amount:       tlb.MustFromTON("0.1").Nano(),
		Fee:          big.NewInt(0),
		IsInitiator:  true,
		ResolverAddr: resolverAddr,
		Details: conditionals.ConditionalResolvableDetails{
			AssetID:    0,
			IsLong:     false,
			Leverage:   1,
			EntryPrice: actions.Coins{Val: entryPrice},
		},
		PriceResolver: priceResolver,
		Action:        &a1,
	}
	_ = condA.SetIntKey(big.NewInt(7), der.Serialize())

	// Commit price to resolver with oracle signature and wait quarantine
	ppEntry := condcontracts.PriceProof{
		At:    uint32(time.Now().Unix() - 10),
		Price: tlb.MustFromNano(entryPrice, 9),
	}
	pp := condcontracts.PriceProof{
		At:    uint32(time.Now().Unix()),
		Price: tlb.MustFromNano(finalPrice, 9),
	}
	ppEntryCell, _ := tlb.ToCell(ppEntry)
	ppExitCell, _ := tlb.ToCell(pp)

	cm := condcontracts.Commit{
		Entry: condcontracts.PriceInner{
			SignedBody: ppEntryCell,
		},
		Exit: condcontracts.PriceInner{
			SignedBody: ppExitCell,
		},
	}
	cm.Entry.Signature.V = ppEntryCell.Sign(oracleKey)
	cm.Exit.Signature.V = ppExitCell.Sign(oracleKey)
	cmBody, _ := tlb.ToCell(cm)

	tx = sendWalletRetry(wallet.SimpleMessage(resolverAddr, tlb.MustFromTON("0.03"), cmBody), "failed to send commit")
	log.Println("commit price external tx sent:", base64.StdEncoding.EncodeToString(tx.Hash))
	// Wait resolver quarantine (1s)
	time.Sleep(2000 * time.Millisecond)

	xBody := payments.StateBody{
		ChannelID: chID,
		Seqno:     4,
		A: payments.StateSide{
			ConditionalsHash: condA.AsCell().Hash(),
			ActionStatesHash: actA.AsCell().Hash(),
		},
		B: payments.StateSide{
			ConditionalsHash: make([]byte, 32), //condB.AsCell().Hash(),
			ActionStatesHash: actB.AsCell().Hash(),
		},
	}

	aState, err := signState(xBody, aKey, bKey)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to sign state: %w", err))
	}

	body, err = ch.PrepareUncoopCloseMessage(aKey, &aState)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to build deploy channel params: %w", err))
	}

	xBody.Seqno = 5

	aState2, err := signState(xBody, aKey, bKey)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to sign state: %w", err))
	}

	body2, err := ch2.PrepareUncoopCloseMessage(bKey, &aState2)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to build deploy channel params: %w", err))
	}

	type asyncTxRes struct {
		side string
		tx   *tlb.Transaction
		err  error
	}
	txRes := make(chan asyncTxRes, 2)
	go func() {
		txA, _, _, errA := api.SendExternalMessageWaitTransaction(ctx, &tlb.ExternalMessage{
			DstAddr: ch.Address,
			Body:    body,
		})
		txRes <- asyncTxRes{side: "A", tx: txA, err: errA}
	}()
	go func() {
		txB, _, _, errB := api.SendExternalMessageWaitTransaction(ctx, &tlb.ExternalMessage{
			DstAddr: ch2.Address,
			Body:    body2,
		})
		txRes <- asyncTxRes{side: "B", tx: txB, err: errB}
	}()
	for i := 0; i < 2; i++ {
		res := <-txRes
		if res.err != nil {
			t.Fatal(fmt.Errorf("failed to send uncoop start %s tx: %w", res.side, res.err))
		}
		log.Println("uncoop start "+res.side+" external tx sent:", base64.StdEncoding.EncodeToString(res.tx.Hash))
	}

	ch = waitChannel(channelAddr, "failed to get channel", func(_ *payments.ChannelContract) {
		t.Log("quarantine seqno not yet updated")
	}, func(cur *payments.ChannelContract) bool {
		return cur.Storage.Quarantine != nil && cur.Storage.Quarantine.Seqno == 5 && cur.Storage.Quarantine.CommittedByOwner
	})

	ch2 = waitChannel(ch.Storage.PartyAddress, "failed to get party channel", func(cur *payments.ChannelContract) {
		t.Log("seqno not yet updated", cur.Storage.CommittedSeqno)
	}, func(cur *payments.ChannelContract) bool {
		return cur.Storage.Quarantine != nil && cur.Storage.Quarantine.Seqno == 5 && cur.Storage.Quarantine.CommittedByOwner
	})
	log.Println("seqno updated")

	ch = waitChannel(channelAddr, "failed to get channel", func(_ *payments.ChannelContract) {
		t.Log("waiting for quarantine end")
	}, func(cur *payments.ChannelContract) bool {
		return cur.Status == payments.ChannelStatusSettlingConditionals
	})

	ch2 = waitChannel(ch.Storage.PartyAddress, "failed to get party channel", func(_ *payments.ChannelContract) {
		t.Log("waiting for quarantine end")
	}, func(cur *payments.ChannelContract) bool {
		return cur.Status == payments.ChannelStatusSettlingConditionals
	})
	log.Println("ready to settle")

	waitBlock(5)

	res, err := api.RunGetMethod(ctx, block, ch2.Address, "get_channel_state")
	if err != nil {
		t.Fatal(fmt.Errorf("failed to get channel state: %w", err))
	}

	println("GET STATE", res.MustInt(0).Uint64())
	t.Log("cur actions hash", hex.EncodeToString(ch2.Storage.Quarantine.TheirState.ActionStatesHash))

	condAProof, err := payments.CreateFullCellUsageProof(condA.AsCell())
	if err != nil {
		t.Fatal(fmt.Errorf("failed to build conditionals proof: %w", err))
	}
	actAProof, err := payments.CreateFullCellUsageProof(actA.AsCell())
	if err != nil {
		t.Fatal(fmt.Errorf("failed to build actions proof: %w", err))
	}

	state := conditionals.VirtualChannelState{
		Amount: tlb.MustFromTON("0.03").Nano(),
	}
	state.Sign(vKey)
	condInput, _ := state.ToCell()

	// PHASE 1: derivative-only settle via resolver proxy
	toSettleDrv := cell.NewDict(256)
	// Derivative resolve: add 10% of 0.1 = 0.01 to action, at committed timestamp
	drv := condcontracts.DerivativeResolve{Key: vPubKey, Amount: tlb.MustFromTON("0.01"), At: pp.At}
	drvInput, _ := tlb.ToCell(drv)
	toSettleDrv.SetIntKey(big.NewInt(7), drvInput)

	if !bytes.Equal(der.Key, drv.Key) {
		t.Fatal("derivation key mismatch")
	}

	// Prepare settle, requiring it to be sent by resolver contract
	body, err = ch2.PrepareSettleMessage(bKey, toSettleDrv, condAProof, actAProof, resolverAddr)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to build settle message (phase 1): %w", err))
	}

	// Wrap settle into resolver proxy and send to resolver contract
	px := condcontracts.ProxySettle{ToA: false, Msg: body}
	pxCell, _ := tlb.ToCell(px)

	tx = sendWalletRetry(wallet.SimpleMessage(resolverAddr, tlb.MustFromTON("0.03"), pxCell), "failed to send proxy settle to resolver")
	log.Println("proxy settle to resolver sent:", base64.StdEncoding.EncodeToString(tx.Hash))
	log.Println("act hash before:", ch2.Address.String(), hex.EncodeToString(ch2.Storage.Quarantine.TheirState.ActionStatesHash), hex.EncodeToString(ch2.Storage.Quarantine.TheirState.ConditionalsHash))

	// After phase 1, only derivative should be applied: 0.009999 + 0.01 = 0.019999
	actStateA1, _ := tlb.ToCell(actions.StateActionSend{
		Amount:        actions.Coins{Val: tlb.MustFromTON("0.019999").Nano()},
		Commited:      actions.Coins{Val: tlb.MustFromTON("0.00").Nano()},
		CommitedSeqno: 0,
	})
	actAfterDrv := cell.NewDict(256)
	_ = actAfterDrv.SetIntKey(new(big.Int).SetBytes(actC.Hash()), actStateA1)

	// Verify conditionals updated after derivative-only settle: mark key 7 as removed via empty cell
	condAfterDrv := cell.NewDict(256)
	condAfterDrv.SetIntKey(big.NewInt(0), vch.Serialize())
	condAfterDrv.SetIntKey(big.NewInt(5), vch2.Serialize())
	condAfterDrv.SetIntKey(big.NewInt(7), cell.BeginCell().EndCell())

	ch2 = waitChannel(ch.Storage.PartyAddress, "failed to get party channel", func(cur *payments.ChannelContract) {
		if !bytes.Equal(cur.Storage.Quarantine.TheirState.ActionStatesHash, actAfterDrv.AsCell().Hash()) {
			t.Log("waiting for actions updated after derivative-only settle, cur hash", hex.EncodeToString(cur.Storage.Quarantine.TheirState.ActionStatesHash),
				hex.EncodeToString(cur.Storage.Quarantine.TheirState.ConditionalsHash), hex.EncodeToString(actAfterDrv.AsCell().Hash()), hex.EncodeToString(condAfterDrv.AsCell().Hash()))
			return
		}
		t.Log("waiting for conditionals updated after derivative-only settle, cur hash", hex.EncodeToString(cur.Storage.Quarantine.TheirState.ConditionalsHash), hex.EncodeToString(condAfterDrv.AsCell().Hash()))
	}, func(cur *payments.ChannelContract) bool {
		return bytes.Equal(cur.Storage.Quarantine.TheirState.ActionStatesHash, actAfterDrv.AsCell().Hash()) &&
			bytes.Equal(cur.Storage.Quarantine.TheirState.ConditionalsHash, condAfterDrv.AsCell().Hash())
	})
	log.Println("phase 1 settled, updated (derivative applied)")
	afterDerivativeAmount := loadActionAmount(actAfterDrv, "after-derivative-settle")
	if new(big.Int).Sub(afterDerivativeAmount, initialActionAmount).Cmp(tlb.MustFromTON("0.01").Nano()) != 0 {
		t.Fatalf("unexpected derivative settle delta: initial=%s after=%s", initialActionAmount.String(), afterDerivativeAmount.String())
	}

	// PHASE 2: normal resolves (virtual channels) sent directly to channel
	toSettleNorm := cell.NewDict(256)
	toSettleNorm.SetIntKey(big.NewInt(0), condInput)
	toSettleNorm.SetIntKey(big.NewInt(5), condInput)

	// Rebuild proof of actions input against the updated action state after phase 1
	actAProof2, err := payments.CreateFullCellUsageProof(actAfterDrv.AsCell())
	if err != nil {
		t.Fatal(fmt.Errorf("failed to build actions proof after derivative settle: %w", err))
	}

	// Rebuild conditionals proof after phase 1: mark derivative key (7) as removed by empty cell
	condAAfterDrv := cell.NewDict(256)
	condAAfterDrv.SetIntKey(big.NewInt(0), vch.Serialize())
	condAAfterDrv.SetIntKey(big.NewInt(5), vch2.Serialize())
	// empty cell value denotes removed key in proof semantics
	condAAfterDrv.SetIntKey(big.NewInt(7), cell.BeginCell().EndCell())
	condAProof2, err := payments.CreateFullCellUsageProof(condAAfterDrv.AsCell())
	if err != nil {
		t.Fatal(fmt.Errorf("failed to build conditionals proof after derivative settle: %w", err))
	}

	// ExpectedSender = addr_none for normal resolves
	body, err = ch2.PrepareSettleMessage(bKey, toSettleNorm, condAProof2, actAProof2, nil)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to build settle message (phase 2): %w", err))
	}

	tx = sendExternalRetry(ch2.Address, body, "failed to send normal settle")
	log.Println("normal settle external tx sent:", base64.StdEncoding.EncodeToString(tx.Hash))

	// Final expected action after phase 2: add both virtual channels 0.03 + 0.03 => 0.079999 total
	actStateAFinal, _ := tlb.ToCell(actions.StateActionSend{
		Amount:        actions.Coins{Val: tlb.MustFromTON("0.079999").Nano()},
		Commited:      actions.Coins{Val: tlb.MustFromTON("0.00").Nano()},
		CommitedSeqno: 0,
	})
	actFinal := cell.NewDict(256)
	_ = actFinal.SetIntKey(new(big.Int).SetBytes(actC.Hash()), actStateAFinal)

	ch2 = waitChannel(ch.Storage.PartyAddress, "failed to get party channel", func(cur *payments.ChannelContract) {
		t.Log("waiting for actions updated after normal resolves, cur hash", hex.EncodeToString(cur.Storage.Quarantine.TheirState.ActionStatesHash), hex.EncodeToString(actFinal.AsCell().Hash()))
	}, func(cur *payments.ChannelContract) bool {
		return bytes.Equal(cur.Storage.Quarantine.TheirState.ActionStatesHash, actFinal.AsCell().Hash())
	})
	log.Println("phase 2 settled, actions updated (normal resolves applied)")
	finalActionAmount := loadActionAmount(actFinal, "after-normal-settle")
	t.Logf("[balance-flow][channel-action-deltas] derivative_delta=%s normal_delta=%s total_delta=%s",
		tlb.MustFromNano(new(big.Int).Sub(afterDerivativeAmount, initialActionAmount), 9).String(),
		tlb.MustFromNano(new(big.Int).Sub(finalActionAmount, afterDerivativeAmount), 9).String(),
		tlb.MustFromNano(new(big.Int).Sub(finalActionAmount, initialActionAmount), 9).String(),
	)

	// Use final actions for subsequent steps
	actA = actFinal

	body, err = ch2.PrepareFinalizeSettleMessage(bKey, actFinal.AsCell().Hash())
	if err != nil {
		t.Fatal(fmt.Errorf("failed to build finalize settle message: %w", err))
	}

	tx = sendExternalRetry(ch2.Address, body, "failed to send finalize settle")
	log.Println("finalize settle B external tx sent:", base64.StdEncoding.EncodeToString(tx.Hash))

	ch2 = waitChannel(ch.Storage.PartyAddress, "failed to get party channel", func(_ *payments.ChannelContract) {
		t.Log("waiting for settlement finalization")
	}, func(cur *payments.ChannelContract) bool {
		return cur.Storage.Quarantine.OurSettlementFinalized
	})

	ch = waitChannel(channelAddr, "failed to get channel", func(_ *payments.ChannelContract) {
		t.Log("waiting for actions hash replication")
	}, func(cur *payments.ChannelContract) bool {
		return bytes.Equal(cur.Storage.Quarantine.ActionsToExecuteHash, actA.AsCell().Hash())
	})
	if ch.Storage.Quarantine.OurSettlementFinalized {
		t.Fatal("A side settlement should be not finalized")
	}
	log.Println("settlement finalized, action hash replicated")

	ch = waitChannel(ch.Storage.PartyAddress, "failed to get party channel", func(_ *payments.ChannelContract) {
		t.Log("waiting for settlement period end")
	}, func(cur *payments.ChannelContract) bool {
		return cur.Status == payments.ChannelStatusExecutingActions
	})
	log.Println("ready for action")

	waitBlock(4)

	actAProof, err = payments.CreateFullCellUsageProof(actA.AsCell())
	if err != nil {
		t.Fatal(fmt.Errorf("failed to build final A actions proof: %w", err))
	}
	actBProof, err := payments.CreateFullCellUsageProof(actB.AsCell())
	if err != nil {
		t.Fatal(fmt.Errorf("failed to build final B actions proof: %w", err))
	}

	body, err = ch2.PrepareProxyExecuteActionsMessage(bKey, actC, actAProof, actBProof)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to build deploy channel params: %w", err))
	}

	execFromAddr := ch2.Storage.PartyAddress // action executes on this side (A)
	execToAddr := ch2.Address                // and sends value to this side (B)
	execFromBefore := getBalanceRetry(execFromAddr, "failed to get execute-source balance before action")
	execToBefore := getBalanceRetry(execToAddr, "failed to get execute-destination balance before action")
	t.Logf("[balance-flow][wallet-before-execute] source=%s destination=%s",
		tlb.MustFromNano(execFromBefore, 9).String(),
		tlb.MustFromNano(execToBefore, 9).String(),
	)

	tx = sendExternalRetry(ch2.Address, body, "failed to send tx")
	log.Println("executed action B external on contract A:", base64.StdEncoding.EncodeToString(tx.Hash))

	execAddr := ch2.Address
	oldSeqno := ch2.Storage.WalletSeqno
	ch2 = waitChannel(execAddr, "failed to get execute side channel", func(cur *payments.ChannelContract) {
		t.Log("waiting for execute period end and w seqno update", cur.Storage.WalletSeqno, oldSeqno+1, cur.Status, payments.ChannelStatusAwaitingFinalization)
	}, func(cur *payments.ChannelContract) bool {
		return cur.Storage.WalletSeqno > oldSeqno && cur.Status == payments.ChannelStatusAwaitingFinalization
	})
	log.Println("ready for finalize")

	execFromAfter := getBalanceRetry(execFromAddr, "failed to get execute-source balance after action")
	execToAfter := getBalanceRetry(execToAddr, "failed to get execute-destination balance after action")
	deltaFrom := new(big.Int).Sub(execFromAfter, execFromBefore)
	deltaTo := new(big.Int).Sub(execToAfter, execToBefore)
	spreadBefore := new(big.Int).Sub(execToBefore, execFromBefore)
	spreadAfter := new(big.Int).Sub(execToAfter, execFromAfter)
	spreadShift := new(big.Int).Sub(spreadAfter, spreadBefore)
	t.Logf("[balance-flow][wallet-after-execute] source=%s destination=%s delta_source=%s delta_destination=%s spread_shift=%s",
		tlb.MustFromNano(execFromAfter, 9).String(),
		tlb.MustFromNano(execToAfter, 9).String(),
		tlb.MustFromNano(deltaFrom, 9).String(),
		tlb.MustFromNano(deltaTo, 9).String(),
		tlb.MustFromNano(spreadShift, 9).String(),
	)

	if deltaFrom.Sign() >= 0 {
		t.Fatalf("unexpected action direction: source side balance did not decrease, delta=%s", deltaFrom.String())
	}
	if spreadAfter.Cmp(spreadBefore) <= 0 {
		t.Fatalf("unexpected action direction: destination/source spread did not grow, before=%s after=%s", spreadBefore.String(), spreadAfter.String())
	}

	// We don't assert exact TON deltas because gas varies, but directional shift must be meaningful.
	minExpectedShift := tlb.MustFromTON("0.02").Nano()
	if spreadShift.Cmp(minExpectedShift) < 0 {
		t.Fatalf("unexpected weak transfer effect: spread shift is too small, shift=%s min=%s delta_to=%s delta_from=%s", spreadShift.String(), minExpectedShift.String(), deltaTo.String(), deltaFrom.String())
	}

	waitBlock(4)

	body, _ = tlb.ToCell(payments.FinishUncooperativeClose{})
	tx = sendExternalRetry(ch2.Storage.PartyAddress, body, "failed to send tx")
	_ = tx

	ch = waitChannel(channelAddr, "failed to get channel", func(_ *payments.ChannelContract) {
		t.Log("waiting for uninit")
	}, func(cur *payments.ChannelContract) bool {
		return !cur.Storage.Initialized
	})

	ch2 = waitChannel(ch.Storage.PartyAddress, "failed to get party channel", func(_ *payments.ChannelContract) {
		t.Log("waiting for uninit")
	}, func(cur *payments.ChannelContract) bool {
		return !cur.Storage.Initialized
	})

	if ch.Storage.Quarantine != nil || ch2.Storage.Quarantine != nil {
		t.Fatal("quarantine must be cleared after final close on both sides")
	}
	if ch.Storage.CommittedSeqno != 6 || ch2.Storage.CommittedSeqno != 6 {
		t.Fatalf("unexpected final committed seqno, got A=%d B=%d, expected 6", ch.Storage.CommittedSeqno, ch2.Storage.CommittedSeqno)
	}

	log.Println("done", channelAddr.String())
}

func signState(body payments.StateBody, keyA, keyB ed25519.PrivateKey) (payments.StateBodySigned, error) {
	cl, err := tlb.ToCell(body)
	if err != nil {
		return payments.StateBodySigned{}, fmt.Errorf("failed to serialize signed: %w", err)
	}

	return payments.StateBodySigned{
		SignatureA: payments.Signature{
			Value: cl.Sign(keyA),
		},
		SignatureB: payments.Signature{
			Value: cl.Sign(keyB),
		},
		Body: body,
	}, nil
}

func convertMsg(msg *wallet.Message) payments.WalletMessage {
	return payments.WalletMessage{
		Mode:            msg.Mode,
		InternalMessage: msg.InternalMessage,
	}
}
