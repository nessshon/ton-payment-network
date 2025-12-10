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
	client2 "github.com/xssnick/ton-payment-network/tonpayments/chain/client"
	"github.com/xssnick/tonutils-go/liteclient"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/ton"
	"github.com/xssnick/tonutils-go/ton/wallet"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"log"
	"math/big"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

var api = func() ton.APIClientWrapped {
	client := liteclient.NewConnectionPool()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := client.AddConnectionsFromConfigUrl(ctx, "https://ton-blockchain.github.io/testnet-global.config.json")
	if err != nil {
		panic(err)
	}

	return ton.NewAPIClient(client).WithRetry()
}()

var code = payments.PaymentChannelCodes[0]

var _seed = strings.Split(os.Getenv("WALLET_SEED"), " ")

func TestClient_AsyncChannelFullFlow(t *testing.T) {
	client := payments.NewPaymentChannelClient(client2.NewTON(api))
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
		ReplicationMessageAttachAmount: tlb.MustFromTON("0.05"),
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

	channelAddr, _, block, err := w.DeployContractWaitTransaction(ctx, tlb.MustFromTON("0.5"), body, code, data)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to deploy channel: %w", err))
	}
	log.Println("channel deployed:", channelAddr.String())

reCh:
	ch, err := client.GetChannel(ctx, channelAddr, true, time.Time{})
	if err != nil {
		t.Log(fmt.Errorf("failed to get channel: %w, retrying", err))
		block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(ctx)
		if err != nil {
			t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
		}
		goto reCh
	}

	log.Println("party channel addr:", ch.Storage.PartyAddress.String())

reCh2:
	ch2, err := client.GetChannel(ctx, ch.Storage.PartyAddress, true, time.Time{})
	if err != nil {
		t.Log(fmt.Errorf("failed to get party channel: %w, retrying", err))
		block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(ctx)
		if err != nil {
			t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
		}
		goto reCh2
	}

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

reTx5:
	tx, _, _, err := api.SendExternalMessageWaitTransaction(ctx, &tlb.ExternalMessage{
		DstAddr: channelAddr,
		Body:    body,
	})
	if err != nil {
		t.Log(fmt.Errorf("failed to send tx: %w", err))
		block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(ctx)
		if err != nil {
			t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
		}
		goto reTx5
	}
	log.Println("double signed tx sent:", base64.StdEncoding.EncodeToString(tx.Hash))

	_, bSig, err = ch.PrepareCoopCommitMessage(bKey, nil, 1, nil, true)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to prepare double signed message: %w", err))
	}

	body, _, err = ch.PrepareCoopCommitMessage(aKey, bSig, 1, nil, true)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to prepare double signed message: %w", err))
	}

	tx, _, _, err = api.SendExternalMessageWaitTransaction(ctx, &tlb.ExternalMessage{
		DstAddr: channelAddr,
		Body:    body,
	})
	if err != nil {
		t.Fatal(fmt.Errorf("failed to send tx: %w", err))
	}
	log.Println("commit tx sent:", base64.StdEncoding.EncodeToString(tx.Hash))

reCh3:
	block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(ctx)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}
	ch, err = client.GetChannel(ctx, channelAddr, true, time.Time{})
	if err != nil {
		t.Log(fmt.Errorf("failed to get channel: %w, retrying", err))
		goto reCh3
	}
	if ch.Storage.CommittedSeqno != 1 {
		t.Log("commit not yet updated")
		goto reCh3
	}

reCh4:
	block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(ctx)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}
	ch2, err = client.GetChannel(ctx, ch.Storage.PartyAddress, true, time.Time{})
	if err != nil {
		t.Log(fmt.Errorf("failed to get party channel: %w, retrying", err))
		goto reCh4
	}
	if ch2.Storage.CommittedSeqno != 1 {
		t.Log("commit not yet updated")
		goto reCh4
	}
	log.Println("commit updated")

	_, bSig, err = ch.PrepareCoopCloseMessage(bKey, nil, 2, true)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to prepare double signed message: %w", err))
	}

	body, _, err = ch.PrepareCoopCloseMessage(aKey, bSig, 2, true)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to prepare double signed message: %w", err))
	}

	tx, _, _, err = api.SendExternalMessageWaitTransaction(ctx, &tlb.ExternalMessage{
		DstAddr: channelAddr,
		Body:    body,
	})
	if err != nil {
		t.Fatal(fmt.Errorf("failed to send tx: %w", err))
	}
	log.Println("close tx sent:", base64.StdEncoding.EncodeToString(tx.Hash))

reCh5:
	block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(ctx)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}
	ch, err = client.GetChannel(ctx, channelAddr, true, time.Time{})
	if err != nil {
		t.Log(fmt.Errorf("failed to get channel: %w, retrying", err))
		goto reCh5
	}
	if ch.Storage.Initialized {
		t.Log("close not yet updated")
		goto reCh5
	}

reCh6:
	block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(ctx)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}
	ch2, err = client.GetChannel(ctx, ch.Storage.PartyAddress, true, time.Time{})
	if err != nil {
		t.Log(fmt.Errorf("failed to get party channel: %w, retrying", err))
		goto reCh6
	}
	if ch2.Storage.Initialized {
		t.Log("close not yet updated")
		goto reCh6
	}
	log.Println("close updated")

	until = uint32(time.Now().Add(90 * time.Second).Unix())
	text, _ := wallet.CreateCommentCell("респект тем кто с нами делится чудесами")
	msg = wallet.SimpleMessage(ch.Address, tlb.MustFromTON("0.08"), text)
	body, err = ch.PrepareOwnerExternalMessage(aKey, []payments.WalletMessage{convertMsg(msg)}, until)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to prepare double signed message b: %w", err))
	}

	tx, _, _, err = api.SendExternalMessageWaitTransaction(ctx, &tlb.ExternalMessage{
		DstAddr: ch.Address,
		Body:    body,
	})
	if err != nil {
		t.Fatal(fmt.Errorf("failed to send tx: %w", err))
	}
	log.Println("owner tx sent:", base64.StdEncoding.EncodeToString(tx.Hash))

	prevWSeq := ch.Storage.WalletSeqno
reChW:
	block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(ctx)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}
	ch, err = client.GetChannel(ctx, ch.Address, true, time.Time{})
	if err != nil {
		t.Log(fmt.Errorf("failed to get channel: %w, retrying", err))
		goto reChW
	}
	if ch.Storage.WalletSeqno != prevWSeq+1 {
		t.Log("wallet seqno not yet updated", ch.Storage.WalletSeqno)
		goto reChW
	}

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

	tx, _, _, err = api.SendExternalMessageWaitTransaction(ctx, &tlb.ExternalMessage{
		DstAddr: ch.Address,
		Body:    body,
	})
	if err != nil {
		t.Fatal(fmt.Errorf("failed to send tx: %w", err))
	}
	log.Println("init external tx sent:", base64.StdEncoding.EncodeToString(tx.Hash))

reCh7:
	block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(ctx)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}
	ch, err = client.GetChannel(ctx, channelAddr, true, time.Time{})
	if err != nil {
		t.Log(fmt.Errorf("failed to get channel: %w, retrying", err))
		goto reCh7
	}
	if !ch.Storage.Initialized {
		t.Log("init not yet updated")
		goto reCh7
	}

reCh8:
	block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(ctx)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}
	ch2, err = client.GetChannel(ctx, ch.Storage.PartyAddress, true, time.Time{})
	if err != nil {
		t.Log(fmt.Errorf("failed to get party channel: %w, retrying", err))
		goto reCh8
	}
	if !ch2.Storage.Initialized {
		t.Log("init not yet updated")
		goto reCh8
	}
	log.Println("init updated")

	vPubKey, vKey, _ := ed25519.GenerateKey(nil)
	_ = vKey

	condA := cell.NewDict(256)
	condB := cell.NewDict(256)
	_ = condB

	actA := cell.NewDict(256)
	actB := cell.NewDict(256)

	a1 := actions.ActionSendTon{
		AddressA: ch.Address,
		AddressB: ch2.Address,
	}
	actC := a1.Serialize()
	actStateA, _ := tlb.ToCell(actions.StateActionSend{
		Amount:        tlb.MustFromTON("0.009999"),
		Commited:      tlb.MustFromTON("0.00"),
		CommitedSeqno: 0,
	})

	actStateB, _ := tlb.ToCell(actions.StateActionSend{
		Amount:        tlb.MustFromTON("0.00"),
		Commited:      tlb.MustFromTON("0.00"),
		CommitedSeqno: 0,
	})

	_ = actA.SetIntKey(new(big.Int).SetBytes(actC.Hash()), actStateA)
	_ = actB.SetIntKey(new(big.Int).SetBytes(actC.Hash()), actStateB)

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

	wg := sync.WaitGroup{}
	wg.Add(2)

	var ok = true
	go func() {
		defer wg.Done()

		tx, _, _, err = api.SendExternalMessageWaitTransaction(ctx, &tlb.ExternalMessage{
			DstAddr: ch.Address,
			Body:    body,
		})
		if err != nil {
			ok = false
			t.Fatal(fmt.Errorf("failed to send tx: %w", err))
		}
		log.Println("uncoop start A external tx sent:", base64.StdEncoding.EncodeToString(tx.Hash))
	}()

	go func() {
		defer wg.Done()

		tx, _, _, err = api.SendExternalMessageWaitTransaction(ctx, &tlb.ExternalMessage{
			DstAddr: ch2.Address,
			Body:    body2,
		})
		if err != nil {
			ok = false

			t.Fatal(fmt.Errorf("failed to send tx: %w", err))
		}
		log.Println("uncoop start B external tx sent:", base64.StdEncoding.EncodeToString(tx.Hash))
	}()
	wg.Wait()

	if !ok {
		t.Fatal("failed to execute tx")
	}

reCh9:
	block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(ctx)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}
	ch, err = client.GetChannel(ctx, channelAddr, true, time.Time{})
	if err != nil {
		t.Log(fmt.Errorf("failed to get channel: %w, retrying", err))
		goto reCh9
	}
	if ch.Storage.Quarantine == nil || ch.Storage.Quarantine.Seqno != 5 || !ch.Storage.Quarantine.CommittedByOwner {
		t.Log("quarantine seqno not yet updated")
		goto reCh9
	}

reCh10:
	block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(ctx)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}
	ch2, err = client.GetChannel(ctx, ch.Storage.PartyAddress, true, time.Time{})
	if err != nil {
		t.Log(fmt.Errorf("failed to get party channel: %w, retrying", err))
		goto reCh10
	}
	if ch2.Storage.Quarantine == nil || ch2.Storage.Quarantine.Seqno != 5 || !ch2.Storage.Quarantine.CommittedByOwner {
		t.Log("seqno not yet updated", ch2.Storage.CommittedSeqno)
		goto reCh10
	}
	log.Println("seqno updated")

reCh11:
	block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(ctx)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}
	ch, err = client.GetChannel(ctx, channelAddr, true, time.Time{})
	if err != nil {
		t.Log(fmt.Errorf("failed to get channel: %w, retrying", err))
		goto reCh11
	}
	if ch.Status != payments.ChannelStatusSettlingConditionals {
		t.Log("waiting for quarantine end")
		goto reCh11
	}

reCh12:
	block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(ctx)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}
	ch2, err = client.GetChannel(ctx, ch.Storage.PartyAddress, true, time.Time{})
	if err != nil {
		t.Log(fmt.Errorf("failed to get party channel: %w, retrying", err))
		goto reCh12
	}
	if ch2.Status != payments.ChannelStatusSettlingConditionals {
		t.Log("waiting for quarantine end")
		goto reCh12
	}
	log.Println("ready to settle")

	block, err = api.WaitForBlock(block.SeqNo + 3).GetMasterchainInfo(ctx)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}

	res, err := api.RunGetMethod(ctx, block, ch2.Address, "get_channel_state")
	if err != nil {
		t.Fatal(fmt.Errorf("failed to get channel state: %w", err))
	}

	println("GET STATE", res.MustInt(0).Uint64())
	t.Log("cur actions hash", hex.EncodeToString(ch2.Storage.Quarantine.TheirState.ActionStatesHash))

	var sk = cell.CreateProofSkeleton()
	sk.SetRecursive()
	condAProof, _ := condA.AsCell().CreateProof(sk)
	actAProof, _ := actA.AsCell().CreateProof(sk)

	state := conditionals.VirtualChannelState{
		Amount: tlb.MustFromTON("0.03").Nano(),
	}
	state.Sign(vKey)
	condInput, _ := state.ToCell()

	toSettle := cell.NewDict(256)
	toSettle.SetIntKey(big.NewInt(0), condInput)
	toSettle.SetIntKey(big.NewInt(5), condInput)

	body, err = ch2.PrepareSettleMessage(bKey, toSettle, condAProof, actAProof)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to build deploy channel params: %w", err))
	}

reTx3:
	tx, _, _, err = api.SendExternalMessageWaitTransaction(ctx, &tlb.ExternalMessage{
		DstAddr: ch2.Address,
		Body:    body,
	})
	if err != nil {
		t.Log(fmt.Errorf("failed to send tx: %w", err))
		block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(ctx)
		if err != nil {
			t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
		}
		goto reTx3
	}
	log.Println("settle B external tx sent:", base64.StdEncoding.EncodeToString(tx.Hash))

	actStateA, _ = tlb.ToCell(actions.StateActionSend{
		Amount:        tlb.MustFromTON("0.069999"),
		Commited:      tlb.MustFromTON("0.00"),
		CommitedSeqno: 0,
	})

	_ = actA.SetIntKey(new(big.Int).SetBytes(actC.Hash()), actStateA)

	println("UPDATED ACT", actA.AsCell().Dump())

reCh14:
	block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(ctx)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}
	ch2, err = client.GetChannel(ctx, ch.Storage.PartyAddress, true, time.Time{})
	if err != nil {
		t.Log(fmt.Errorf("failed to get party channel: %w, retrying", err))
		goto reCh14
	}
	if !bytes.Equal(ch2.Storage.Quarantine.TheirState.ActionStatesHash, actA.AsCell().Hash()) {
		t.Log("waiting for actions updated, cur hash", hex.EncodeToString(ch2.Storage.Quarantine.TheirState.ActionStatesHash), hex.EncodeToString(actA.AsCell().Hash()))
		goto reCh14
	}
	log.Println("settled, actions updated")

	body, err = ch2.PrepareFinalizeSettleMessage(bKey, actA.AsCell().Hash())
	if err != nil {
		t.Fatal(fmt.Errorf("failed to build deploy channel params: %w", err))
	}

reTx4:
	tx, _, _, err = api.SendExternalMessageWaitTransaction(ctx, &tlb.ExternalMessage{
		DstAddr: ch2.Address,
		Body:    body,
	})
	if err != nil {
		t.Log(fmt.Errorf("failed to send tx: %w", err))
		block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(ctx)
		if err != nil {
			t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
		}
		goto reTx4
	}
	log.Println("finalize settle B external tx sent:", base64.StdEncoding.EncodeToString(tx.Hash))

reCh15:
	block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(ctx)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}
	ch2, err = client.GetChannel(ctx, ch.Storage.PartyAddress, true, time.Time{})
	if err != nil {
		t.Log(fmt.Errorf("failed to get party channel: %w, retrying", err))
		goto reCh15
	}
	if !ch2.Storage.Quarantine.OurSettlementFinalized {
		t.Log("waiting for settlement finalization")
		goto reCh15
	}

reCh16:
	block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(ctx)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}
	ch, err = client.GetChannel(ctx, channelAddr, true, time.Time{})
	if err != nil {
		t.Log(fmt.Errorf("failed to get channel: %w, retrying", err))
		goto reCh16
	}
	if !bytes.Equal(ch.Storage.Quarantine.ActionsToExecuteHash, actA.AsCell().Hash()) {
		t.Log("waiting for actions hash replication")
		goto reCh16
	}
	if ch.Storage.Quarantine.OurSettlementFinalized {
		t.Fatal("A side settlement should be not finalized")
	}
	log.Println("settlement finalized, action hash replicated")

reCh17:
	block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(ctx)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}
	ch, err = client.GetChannel(ctx, ch.Storage.PartyAddress, true, time.Time{})
	if err != nil {
		t.Log(fmt.Errorf("failed to get party channel: %w, retrying", err))
		goto reCh17
	}
	if ch.Status != payments.ChannelStatusExecutingActions {
		t.Log("waiting for settlement period end")
		goto reCh17
	}
	log.Println("ready for action")

	block, err = api.WaitForBlock(block.SeqNo + 4).GetMasterchainInfo(ctx)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}

	actAProof, _ = actA.AsCell().CreateProof(sk)
	actBProof, _ := actB.AsCell().CreateProof(sk)

	body, err = ch2.PrepareProxyExecuteActionsMessage(bKey, actC, actAProof, actBProof)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to build deploy channel params: %w", err))
	}

reTx1:
	tx, _, _, err = api.SendExternalMessageWaitTransaction(ctx, &tlb.ExternalMessage{
		DstAddr: ch2.Address,
		Body:    body,
	})
	if err != nil {
		t.Log(fmt.Errorf("failed to send tx: %w", err))
		block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(ctx)
		if err != nil {
			t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
		}
		goto reTx1
	}
	log.Println("executed action B external on contract A:", base64.StdEncoding.EncodeToString(tx.Hash))

	oldSeqno := ch2.Storage.WalletSeqno
reCh18:
	block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(ctx)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}
	ch2, err = client.GetChannel(ctx, ch.Storage.PartyAddress, true, time.Time{})
	if err != nil {
		t.Log(fmt.Errorf("failed to get party channel: %w, retrying", err))
		goto reCh18
	}
	if ch2.Storage.WalletSeqno <= oldSeqno || ch2.Status != payments.ChannelStatusAwaitingFinalization {
		t.Log("waiting for execute period end and w seqno update", ch2.Storage.WalletSeqno, oldSeqno+1, ch2.Status, payments.ChannelStatusAwaitingFinalization)
		goto reCh18
	}
	log.Println("ready for finalize")

	block, err = api.WaitForBlock(block.SeqNo + 4).GetMasterchainInfo(ctx)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}

	body, _ = tlb.ToCell(payments.FinishUncooperativeClose{})
reTx2:
	tx, _, _, err = api.SendExternalMessageWaitTransaction(ctx, &tlb.ExternalMessage{
		DstAddr: ch2.Storage.PartyAddress,
		Body:    body,
	})
	if err != nil {
		t.Log(fmt.Errorf("failed to send tx: %w", err))
		block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(ctx)
		if err != nil {
			t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
		}
		goto reTx2
	}

reCh19:
	block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(ctx)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}
	ch, err = client.GetChannel(ctx, channelAddr, true, time.Time{})
	if err != nil {
		t.Log(fmt.Errorf("failed to get channel: %w, retrying", err))
		goto reCh19
	}
	if ch.Storage.Initialized {
		t.Log("waiting for uninit")
		goto reCh19
	}

reCh20:
	block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(ctx)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}
	ch2, err = client.GetChannel(ctx, ch.Storage.PartyAddress, true, time.Time{})
	if err != nil {
		t.Log(fmt.Errorf("failed to get party channel: %w, retrying", err))
		goto reCh20
	}
	if ch2.Storage.Initialized {
		t.Log("waiting for uninit")
		goto reCh20
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
