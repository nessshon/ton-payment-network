package payments

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"github.com/xssnick/ton-payment-network/tonpayments/chain/client"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"time"
)

type ChainAPI interface {
	GetAccount(ctx context.Context, addr *address.Address, blockAfter time.Time) (*client.Account, error)
}

type Client struct {
	api ChainAPI
}

type ChannelContract struct {
	Status  ChannelStatus
	Storage ChannelStorageData
	Code    *cell.Cell
	Address *address.Address
	client  *Client
}

type ChannelStatus int8
type ChannelID []byte

var ErrVerificationNotPassed = fmt.Errorf("verification not passed")

const (
	ChannelStatusUninitialized ChannelStatus = iota
	ChannelStatusOpen
	ChannelStatusClosureStarted
	ChannelStatusSettlingConditionals
	ChannelStatusExecutingActions
	ChannelStatusAwaitingFinalization
)

func NewPaymentChannelClient(api ChainAPI) *Client {
	return &Client{
		api: api,
	}
}

func (c *Client) GetDeployAsyncChannelParams(channelId ChannelID, isA bool, seqno uint64, ourKey ed25519.PrivateKey, theirKey ed25519.PublicKey, theirSig []byte, closingConfig ClosingConfig) (body, data *cell.Cell, signature []byte, err error) {
	if len(channelId) != 16 {
		return nil, nil, nil, fmt.Errorf("channelId len should be 16 bytes")
	}

	storageData := ChannelStorageData{
		IsA:           isA,
		KeyA:          ourKey.Public().(ed25519.PublicKey),
		KeyB:          theirKey,
		ChannelID:     channelId,
		ClosingConfig: closingConfig,
	}

	if !isA {
		storageData.KeyA, storageData.KeyB = storageData.KeyB, storageData.KeyA
	}

	data, err = tlb.ToCell(storageData)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to serialize storage data: %w", err)
	}

	initCh := InitChannel{}
	initCh.Signed.ChannelID = channelId
	initCh.Signed.Seqno = seqno
	sig, err := toSignature(initCh.Signed, ourKey)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to sign data: %w", err)
	}

	if theirSig != nil {
		cl, err := tlb.ToCell(initCh.Signed)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("failed to serialize signed: %w", err)
		}

		if !cl.Verify(theirKey, theirSig) {
			return nil, nil, nil, fmt.Errorf("their signature is not match")
		}
	}

	if len(theirSig) == 0 {
		theirSig = make([]byte, ed25519.SignatureSize)
	}

	if isA {
		initCh.SignatureA, initCh.SignatureB = sig, Signature{Value: theirSig}
	} else {
		initCh.SignatureA, initCh.SignatureB = Signature{Value: theirSig}, sig
	}

	body, err = tlb.ToCell(initCh)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to serialize message: %w", err)
	}
	return body, data, sig.Value, nil
}

func (c *Client) GetChannel(ctx context.Context, addr *address.Address, verify bool, blockAfter time.Time) (*ChannelContract, error) {
	acc, err := c.api.GetAccount(ctx, addr, blockAfter)
	if err != nil {
		return nil, fmt.Errorf("failed to get account: %w", err)
	}

	if !acc.IsActive {
		return nil, fmt.Errorf("channel account is not active %s", addr.String())
	}

	return c.ParseChannel(addr, acc.Code, acc.Data, verify)
}

func (c *Client) ParseChannel(addr *address.Address, code, data *cell.Cell, verify bool) (*ChannelContract, error) {
	if verify {
		ok := false
		for _, h := range PaymentChannelCodes {
			if bytes.Equal(code.Hash(), h.Hash()) {
				ok = true
				code = h // optimize mem pointers
				break
			}
		}

		if !ok {
			return nil, ErrVerificationNotPassed
		}
	}

	ch := &ChannelContract{
		Address: addr,
		client:  c,
		Status:  ChannelStatusUninitialized,
		Code:    code,
	}

	err := tlb.LoadFromCell(&ch.Storage, data.BeginParse())
	if err != nil {
		return nil, fmt.Errorf("failed to load storage: %w", err)
	}

	if verify {
		storageData := ChannelStorageData{
			IsA:            ch.Storage.IsA,
			Initialized:    false,
			CommittedSeqno: 0,
			WalletSeqno:    0,
			KeyA:           ch.Storage.KeyA,
			KeyB:           ch.Storage.KeyB,
			ChannelID:      ch.Storage.ChannelID,
			ClosingConfig:  ch.Storage.ClosingConfig,
			PartyAddress:   nil,
			Quarantine:     nil,
		}

		data, err = tlb.ToCell(storageData)
		if err != nil {
			return nil, fmt.Errorf("failed to serialize storage data: %w", err)
		}

		si, err := tlb.ToCell(tlb.StateInit{
			Code: code,
			Data: data,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to serialize state init: %w", err)
		}

		if !bytes.Equal(si.Hash(), ch.Address.Data()) {
			return nil, ErrVerificationNotPassed
		}
	}

	ch.Status = ch.calcState()

	return ch, nil
}

// calcState - it repeats get_channel_state method of contract,
// we do this because we cannot prove method execution for now,
// but can proof contract data and code, so this approach is safe
func (c *ChannelContract) calcState() ChannelStatus {
	if !c.Storage.Initialized {
		return ChannelStatusUninitialized
	}
	if c.Storage.Quarantine == nil {
		return ChannelStatusOpen
	}
	now := time.Now().UTC().Unix()
	quarantineEnds := int64(c.Storage.Quarantine.QuarantineStarts) + int64(c.Storage.ClosingConfig.QuarantineDuration)
	if quarantineEnds > now {
		return ChannelStatusClosureStarted
	}
	if c.Storage.Quarantine.TheirState != nil {
		if quarantineEnds+int64(c.Storage.ClosingConfig.ConditionalCloseDuration) > now {
			return ChannelStatusSettlingConditionals
		}
		if quarantineEnds+int64(c.Storage.ClosingConfig.ConditionalCloseDuration)+int64(c.Storage.ClosingConfig.ActionsDuration) > now {
			return ChannelStatusExecutingActions
		}
	}
	return ChannelStatusAwaitingFinalization
}

func (c *ChannelContract) GetPartyAddr() *address.Address {
	storageData := ChannelStorageData{
		IsA:            !c.Storage.IsA,
		Initialized:    false,
		CommittedSeqno: 0,
		WalletSeqno:    0,
		KeyA:           c.Storage.KeyA,
		KeyB:           c.Storage.KeyB,
		ChannelID:      c.Storage.ChannelID,
		ClosingConfig:  c.Storage.ClosingConfig,
		PartyAddress:   nil,
		Quarantine:     nil,
	}

	data, err := tlb.ToCell(storageData)
	if err != nil {
		panic(err.Error())
	}

	si, err := tlb.ToCell(tlb.StateInit{
		Code: c.Code,
		Data: data,
	})
	if err != nil {
		panic(err.Error())
	}

	return address.NewAddress(0, 0, si.Hash())
}

func (c *ChannelContract) PrepareDoubleExternalMessage(ourKey ed25519.PrivateKey, theirSig []byte, messages []WalletMessage, validUntil uint32) (body *cell.Cell, signature []byte, err error) {
	if !c.Storage.Initialized {
		return nil, nil, fmt.Errorf("channel is in owner sign mode")
	}

	msg := ExternalMsgDoubleSigned{}
	msg.Signed.ChannelID = c.Storage.ChannelID
	msg.Signed.WalletSeqno = c.Storage.WalletSeqno
	msg.Signed.SideA = c.Storage.IsA
	msg.Signed.ValidUntil = validUntil
	out, err := PackOutActions(messages)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to pack out actions: %w", err)
	}
	msg.Signed.OutActions = out

	var newSig Signature
	newSig, msg.SignatureA, msg.SignatureB, err = c.prepareDoubleSignedMessage(ourKey, theirSig, msg.Signed)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to prepare double signed message: %w", err)
	}

	body, err = tlb.ToCell(msg)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to serialize message: %w", err)
	}

	return body, newSig.Value, nil
}

func (c *ChannelContract) PrepareOwnerExternalMessage(ourKey ed25519.PrivateKey, messages []WalletMessage, validUntil uint32) (body *cell.Cell, err error) {
	if c.Storage.Initialized {
		return nil, fmt.Errorf("channel is in double sign mode")
	}

	msg := ExternalMsgOwnerSigned{}
	msg.Signed.ChannelID = c.Storage.ChannelID
	msg.Signed.WalletSeqno = c.Storage.WalletSeqno
	msg.Signed.SideA = c.Storage.IsA
	msg.Signed.ValidUntil = validUntil
	out, err := PackOutActions(messages)
	if err != nil {
		return nil, fmt.Errorf("failed to pack out actions: %w", err)
	}
	msg.Signed.OutActions = out

	expectedKey := c.Storage.KeyB
	if c.Storage.IsA {
		expectedKey = c.Storage.KeyA
	}

	if !ourKey.Public().(ed25519.PublicKey).Equal(expectedKey) {
		return nil, fmt.Errorf("our key is not match")
	}

	msg.Signature, err = toSignature(msg.Signed, ourKey)
	if err != nil {
		return nil, fmt.Errorf("failed to sign data: %w", err)
	}

	body, err = tlb.ToCell(msg)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize message: %w", err)
	}

	return body, nil
}

func (c *ChannelContract) PrepareUncoopCloseMessage(ourKey ed25519.PrivateKey, state *StateBodySigned) (body *cell.Cell, err error) {
	msg := UncoopCloseMsg{}
	msg.Signed.ChannelID = c.Storage.ChannelID
	msg.Signed.State = state

	expectedKey := c.Storage.KeyB
	if c.Storage.IsA {
		expectedKey = c.Storage.KeyA
	}

	if !ourKey.Public().(ed25519.PublicKey).Equal(expectedKey) {
		return nil, fmt.Errorf("our key is not match")
	}

	msg.Signature, err = toSignature(msg.Signed, ourKey)
	if err != nil {
		return nil, fmt.Errorf("failed to sign data: %w", err)
	}

	body, err = tlb.ToCell(msg)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize message: %w", err)
	}

	return body, nil
}

func (c *ChannelContract) PrepareSettleMessage(ourKey ed25519.PrivateKey, toSettle *cell.Dictionary, condProof, actProof *cell.Cell, sender *address.Address) (body *cell.Cell, err error) {
	msg := SettleMsg{}
	msg.Signed.ChannelID = c.Storage.ChannelID
	msg.Signed.ExpectedSender = sender
	msg.Signed.WalletSeqno = c.Storage.WalletSeqno
	msg.Signed.ToSettle = toSettle
	msg.Signed.ConditionalsProof = condProof
	msg.Signed.ActionsInputProof = actProof

	expectedKey := c.Storage.KeyB
	if c.Storage.IsA {
		expectedKey = c.Storage.KeyA
	}

	if !ourKey.Public().(ed25519.PublicKey).Equal(expectedKey) {
		return nil, fmt.Errorf("our key is not match")
	}

	msg.Signature, err = toSignature(msg.Signed, ourKey)
	if err != nil {
		return nil, fmt.Errorf("failed to sign data: %w", err)
	}

	body, err = tlb.ToCell(msg)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize message: %w", err)
	}

	return body, nil
}

func (c *ChannelContract) PrepareFinalizeSettleMessage(ourKey ed25519.PrivateKey, actionsHash []byte) (body *cell.Cell, err error) {
	msg := FinalizeSettleMsg{}
	msg.Signed.ChannelID = c.Storage.ChannelID
	msg.Signed.WalletSeqno = c.Storage.WalletSeqno
	msg.Signed.ActionsInputHash = actionsHash

	expectedKey := c.Storage.KeyB
	if c.Storage.IsA {
		expectedKey = c.Storage.KeyA
	}

	if !ourKey.Public().(ed25519.PublicKey).Equal(expectedKey) {
		return nil, fmt.Errorf("our key is not match")
	}

	msg.Signature, err = toSignature(msg.Signed, ourKey)
	if err != nil {
		return nil, fmt.Errorf("failed to sign data: %w", err)
	}

	body, err = tlb.ToCell(msg)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize message: %w", err)
	}

	return body, nil
}

func (c *ChannelContract) PrepareExecuteActionsMessage(ourKey ed25519.PrivateKey, action *cell.Cell, ourProof *cell.Cell, theirProof *cell.Cell) (body *cell.Cell, err error) {
	msg := ExecuteActionsMsg{}
	msg.Signed.ChannelID = c.Storage.ChannelID
	msg.Signed.Action = action
	msg.Signed.OurActionsInputProof = ourProof
	msg.Signed.TheirActionsInputProof = theirProof

	expectedKey := c.Storage.KeyB
	if c.Storage.IsA {
		expectedKey = c.Storage.KeyA
	}

	if !ourKey.Public().(ed25519.PublicKey).Equal(expectedKey) {
		return nil, fmt.Errorf("our key is not match")
	}

	msg.Signature, err = toSignature(msg.Signed, ourKey)
	if err != nil {
		return nil, fmt.Errorf("failed to sign data: %w", err)
	}

	body, err = tlb.ToCell(msg)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize message: %w", err)
	}

	return body, nil
}

func (c *ChannelContract) PrepareProxyExecuteActionsMessage(ourKey ed25519.PrivateKey, action *cell.Cell, ourProof *cell.Cell, theirProof *cell.Cell) (body *cell.Cell, err error) {
	aMsg := ExecuteActionsMsg{}
	aMsg.Signed.ChannelID = c.Storage.ChannelID
	aMsg.Signed.Action = action
	aMsg.Signed.OurActionsInputProof = ourProof
	aMsg.Signed.TheirActionsInputProof = theirProof

	expectedKey := c.Storage.KeyB
	if c.Storage.IsA {
		expectedKey = c.Storage.KeyA
	}

	if !ourKey.Public().(ed25519.PublicKey).Equal(expectedKey) {
		return nil, fmt.Errorf("our key is not match")
	}

	aMsg.Signature, err = toSignature(aMsg.Signed, ourKey)
	if err != nil {
		return nil, fmt.Errorf("failed to sign data: %w", err)
	}

	msg := ProxyExecuteActionsMsg{}
	msg.Signed.ChannelID = c.Storage.ChannelID
	msg.Signed.WalletSeqno = c.Storage.WalletSeqno
	msg.Signed.Msg = aMsg

	msg.Signature, err = toSignature(msg.Signed, ourKey)
	if err != nil {
		return nil, fmt.Errorf("failed to sign data: %w", err)
	}

	body, err = tlb.ToCell(msg)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize message: %w", err)
	}

	return body, nil
}

func (c *ChannelContract) PrepareCoopCommitMessage(ourKey ed25519.PrivateKey, theirSig []byte, seqno uint64, action *CooperativeCommitAction, fromA bool) (body *cell.Cell, signature []byte, err error) {
	msg := CooperativeCommit{}
	msg.Signed.ChannelID = c.Storage.ChannelID
	msg.Signed.Seqno = seqno
	msg.Signed.FromA = fromA
	msg.Signed.Action = action

	var newSig Signature
	newSig, msg.SignatureA, msg.SignatureB, err = c.prepareDoubleSignedMessage(ourKey, theirSig, msg.Signed)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to prepare double signed message: %w", err)
	}

	body, err = tlb.ToCell(msg)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to serialize message: %w", err)
	}

	return body, newSig.Value, nil
}

func (c *ChannelContract) PrepareCoopCloseMessage(ourKey ed25519.PrivateKey, theirSig []byte, seqno uint64, fromA bool) (body *cell.Cell, signature []byte, err error) {
	msg := CooperativeClose{}
	msg.Signed.ChannelID = c.Storage.ChannelID
	msg.Signed.Seqno = seqno
	msg.Signed.FromA = fromA

	var newSig Signature
	newSig, msg.SignatureA, msg.SignatureB, err = c.prepareDoubleSignedMessage(ourKey, theirSig, msg.Signed)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to prepare double signed message: %w", err)
	}

	body, err = tlb.ToCell(msg)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to serialize message: %w", err)
	}

	return body, newSig.Value, nil
}

func (c *ChannelContract) prepareDoubleSignedMessage(ourKey ed25519.PrivateKey, theirSig []byte, msg any) (newSig, sigA, sigB Signature, err error) {
	sig, err := toSignature(msg, ourKey)
	if err != nil {
		return Signature{}, Signature{}, Signature{}, fmt.Errorf("failed to sign data: %w", err)
	}

	weA := ourKey.Public().(ed25519.PublicKey).Equal(c.Storage.KeyA)

	var theirKey = c.Storage.KeyB
	if !weA {
		theirKey = c.Storage.KeyA
		if !ourKey.Public().(ed25519.PublicKey).Equal(c.Storage.KeyB) {
			return Signature{}, Signature{}, Signature{}, fmt.Errorf("our key is not match %s %s", hex.EncodeToString(c.Storage.KeyB), hex.EncodeToString(ourKey.Public().(ed25519.PublicKey)))
		}
	}

	if theirSig != nil {
		cl, err := tlb.ToCell(msg)
		if err != nil {
			return Signature{}, Signature{}, Signature{}, fmt.Errorf("failed to serialize signed: %w", err)
		}

		if !cl.Verify(theirKey, theirSig) {
			return Signature{}, Signature{}, Signature{}, fmt.Errorf("their signature is not match")
		}
	}

	if len(theirSig) == 0 {
		theirSig = make([]byte, ed25519.SignatureSize)
	}

	if weA {
		sigA, sigB = sig, Signature{Value: theirSig}
	} else {
		sigA, sigB = Signature{Value: theirSig}, sig
	}

	return sig, sigA, sigB, nil
}

func validateMessageFields(messages []WalletMessage) error {
	if len(messages) > 255 {
		return fmt.Errorf("max 255 messages allowed for v5")
	}
	for _, message := range messages {
		if message.InternalMessage == nil {
			return fmt.Errorf("internal message cannot be nil")
		}
	}
	return nil
}

func PackOutActions(messages []WalletMessage) (*cell.Cell, error) {
	if err := validateMessageFields(messages); err != nil {
		return nil, err
	}

	var list = cell.BeginCell().EndCell()
	for _, message := range messages {
		outMsg, err := tlb.ToCell(message.InternalMessage)
		if err != nil {
			return nil, err
		}

		/*
			out_list_empty$_ = OutList 0;
			out_list$_ {n:#} prev:^(OutList n) action:OutAction
			  = OutList (n + 1);
			action_send_msg#0ec3c86d mode:(## 8)
			  out_msg:^(MessageRelaxed Any) = OutAction;
		*/
		msg := cell.BeginCell().MustStoreUInt(0x0ec3c86d, 32). // action_send_msg prefix
									MustStoreUInt(uint64(message.Mode), 8). // mode
									MustStoreRef(outMsg)                    // message reference

		list = cell.BeginCell().MustStoreRef(list).MustStoreBuilder(msg).EndCell()
	}

	return list, nil
}

// UnpackOutActions decodes OutList into wallet messages.
func UnpackOutActions(list *cell.Cell) ([]WalletMessage, error) {
	var resRev []WalletMessage

	for {
		if list == nil {
			break
		}
		// terminator: empty cell (no bits, no refs)
		if list.BitsSize() == 0 && list.RefsNum() == 0 {
			break
		}

		sl := list.BeginParse()
		prev, err := sl.LoadRef()
		if err != nil {
			return nil, fmt.Errorf("failed to load prev ref: %w", err)
		}

		// Action is stored inline in the same cell after prev ref
		tag, err := sl.LoadUInt(32)
		if err != nil {
			return nil, fmt.Errorf("failed to load action tag: %w", err)
		}
		if tag != 0x0ec3c86d {
			return nil, fmt.Errorf("unsupported out action tag: 0x%x", tag)
		}

		mode, err := sl.LoadUInt(8)
		if err != nil {
			return nil, fmt.Errorf("failed to load mode: %w", err)
		}

		outMsgRef, err := sl.LoadRef()
		if err != nil {
			return nil, fmt.Errorf("failed to load out_msg ref: %w", err)
		}

		var internal tlb.InternalMessage
		if err := tlb.LoadFromCell(&internal, outMsgRef); err != nil { // outMsgRef is a *cell.Slice
			return nil, fmt.Errorf("failed to parse internal message: %w", err)
		}

		resRev = append(resRev, WalletMessage{
			Mode:            uint8(mode),
			InternalMessage: &internal,
		})

		next, err := prev.ToCell()
		if err != nil {
			return nil, fmt.Errorf("failed to convert prev to cell: %w", err)
		}
		list = next
	}

	// reverse to restore original order
	for i, j := 0, len(resRev)-1; i < j; i, j = i+1, j-1 {
		resRev[i], resRev[j] = resRev[j], resRev[i]
	}

	return resRev, nil
}

func toSignature(obj any, key ed25519.PrivateKey) (Signature, error) {
	toSign, err := tlb.ToCell(obj)
	if err != nil {
		return Signature{}, fmt.Errorf("failed to serialize body to sign: %w", err)
	}
	return Signature{Value: toSign.Sign(key)}, nil
}

func RandomChannelID() (ChannelID, error) {
	id := make(ChannelID, 16)
	_, err := rand.Read(id)
	if err != nil {
		return nil, err
	}
	return id, nil
}
