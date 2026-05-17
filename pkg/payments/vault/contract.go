package vault

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

var DefaultExternalValidity = 5 * time.Minute

var Code = func() *cell.Cell {
	data, err := hex.DecodeString("b5ee9c7241020501000109000114ff00f4a413f4bcf2c80b0102012002030082d2f891f240ed44d0d37f31d31f31d3ffd101d72c228d20b4648e23f892f828c8cf91c6905a3212cecec9018308d718d74c02f9004013f910f2e08771fb00e0f23f01b2f2ed44d0d37fd31fd3ffd103d72c218d20b4648ec18308d71820f901541025f910f2e087d37fd31fd31ff4055135baf2e07223baf2e085f823bcf2e088f80001a402c8cb7f12cb1f12cbffc9ed54f80f206e9130e30ee0f23f0400ac8b027022d739308e4220d74bc002f2e093c028f2e093d72c20761e436cf2e093d4d307d74c0172b0f2e089d0d2000191309fd30231fa4031fa403023c705f2d093e2d7393001a421c70012e630318407bbf2e093ed559a6e526c")
	if err != nil {
		panic(err.Error())
	}

	code, err := cell.FromBOC(data)
	if err != nil {
		panic(err.Error())
	}
	return code
}()

type Storage struct {
	ID          []byte            `tlb:"bits 128"`
	WalletSeqno uint32            `tlb:"## 32"`
	Key         ed25519.PublicKey `tlb:"bits 256"`
}

type PairToSign struct {
	_        tlb.Magic        `tlb:"#71a4168c"`
	Sender   *address.Address `tlb:"addr"`
	Receiver *address.Address `tlb:"addr"`
}

type InternalSignedSenderRequest struct {
	_         tlb.Magic  `tlb:"#51a4168c"`
	Signature []byte     `tlb:"bits 512"`
	Message   *cell.Cell `tlb:"^"`
}

type ExternalSignedRequest struct {
	_          tlb.Magic  `tlb:"#31a4168c"`
	Signature  []byte     `tlb:"bits 512"`
	SignedBody *cell.Cell `tlb:"."`
}

type ExternalSignedSendBody struct {
	ID         []byte     `tlb:"bits 128"`
	ValidUntil uint32     `tlb:"## 32"`
	Seqno      uint32     `tlb:"## 32"`
	OutActions *cell.Cell `tlb:"maybe ^"`
}

func ParsePrivateKey(raw []byte) (ed25519.PrivateKey, error) {
	switch len(raw) {
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(raw), nil
	case ed25519.PrivateKeySize:
		key := ed25519.PrivateKey(append([]byte(nil), raw...))
		if pub := key.Public().(ed25519.PublicKey); len(pub) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("invalid ed25519 private key")
		}
		return key, nil
	default:
		return nil, fmt.Errorf("invalid vault private key length %d", len(raw))
	}
}

func DeriveID(pub ed25519.PublicKey) []byte {
	hash := sha256.Sum256(pub)
	return append([]byte(nil), hash[:16]...)
}

func BuildStateInit(pub ed25519.PublicKey) (*tlb.StateInit, error) {
	if len(pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("invalid vault public key length %d", len(pub))
	}

	data, err := tlb.ToCell(Storage{
		ID:          DeriveID(pub),
		WalletSeqno: 0,
		Key:         pub,
	})
	if err != nil {
		return nil, fmt.Errorf("serialize vault storage: %w", err)
	}

	return &tlb.StateInit{
		Code: Code,
		Data: data,
	}, nil
}

func BuildStateInitFromPrivateKey(key ed25519.PrivateKey) (*tlb.StateInit, error) {
	if len(key) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("invalid vault private key length %d", len(key))
	}
	return BuildStateInit(key.Public().(ed25519.PublicKey))
}

func AddressFromStateInit(stateInit *tlb.StateInit) (*address.Address, error) {
	if stateInit == nil {
		return nil, fmt.Errorf("vault state init is nil")
	}

	stateCell, err := tlb.ToCell(*stateInit)
	if err != nil {
		return nil, fmt.Errorf("serialize vault state init: %w", err)
	}

	return address.NewAddress(0, 0, stateCell.Hash()), nil
}

func AddressFromPublicKey(pub ed25519.PublicKey) (*address.Address, error) {
	stateInit, err := BuildStateInit(pub)
	if err != nil {
		return nil, err
	}
	return AddressFromStateInit(stateInit)
}

func AddressFromPrivateKey(key ed25519.PrivateKey) (*address.Address, error) {
	stateInit, err := BuildStateInitFromPrivateKey(key)
	if err != nil {
		return nil, err
	}
	return AddressFromStateInit(stateInit)
}

func LoadStorage(data *cell.Cell) (*Storage, error) {
	if data == nil {
		return nil, fmt.Errorf("vault storage data is nil")
	}

	var storage Storage
	if err := tlb.Parse(&storage, data); err != nil {
		return nil, fmt.Errorf("parse vault storage: %w", err)
	}
	return &storage, nil
}

func SignPair(privateKey ed25519.PrivateKey, sender, receiver *address.Address) ([]byte, error) {
	if sender == nil {
		return nil, fmt.Errorf("sender is nil")
	}
	if receiver == nil {
		return nil, fmt.Errorf("receiver is nil")
	}

	body, err := tlb.ToCell(PairToSign{
		Sender:   sender,
		Receiver: receiver,
	})
	if err != nil {
		return nil, fmt.Errorf("serialize vault pair to sign: %w", err)
	}

	return body.Sign(privateKey), nil
}

func BuildVaultData(privateKey ed25519.PrivateKey, sender, target *address.Address) (*payments.VaultData, error) {
	if target == nil {
		return nil, fmt.Errorf("target is nil")
	}

	addr, err := AddressFromPrivateKey(privateKey)
	if err != nil {
		return nil, err
	}

	signature, err := SignPair(privateKey, sender, addr)
	if err != nil {
		return nil, err
	}

	return &payments.VaultData{
		Target:    target,
		Address:   addr,
		Signature: signature,
	}, nil
}

func BuildInternalSignedRequest(signature []byte, message *cell.Cell) (*cell.Cell, error) {
	if len(signature) != ed25519.SignatureSize {
		return nil, fmt.Errorf("invalid internal request signature length %d", len(signature))
	}
	if message == nil {
		return nil, fmt.Errorf("internal request message is nil")
	}

	body, err := tlb.ToCell(InternalSignedSenderRequest{
		Signature: append([]byte(nil), signature...),
		Message:   message,
	})
	if err != nil {
		return nil, fmt.Errorf("serialize internal signed request: %w", err)
	}
	return body, nil
}

func BuildExternalSignedBody(privateKey ed25519.PrivateKey, storage *Storage, outActions *cell.Cell, validUntil time.Time) (*cell.Cell, error) {
	if storage == nil {
		return nil, fmt.Errorf("vault storage is nil")
	}
	if len(storage.ID) != 16 {
		return nil, fmt.Errorf("invalid vault id length %d", len(storage.ID))
	}

	if validUntil.IsZero() {
		validUntil = time.Now().Add(DefaultExternalValidity)
	}
	if !validUntil.After(time.Now()) {
		return nil, fmt.Errorf("valid until must be in the future")
	}

	signedBody, err := tlb.ToCell(ExternalSignedSendBody{
		ID:         append([]byte(nil), storage.ID...),
		ValidUntil: uint32(validUntil.Unix()),
		Seqno:      storage.WalletSeqno,
		OutActions: outActions,
	})
	if err != nil {
		return nil, fmt.Errorf("serialize external signed body: %w", err)
	}

	body, err := tlb.ToCell(ExternalSignedRequest{
		Signature:  signedBody.Sign(privateKey),
		SignedBody: signedBody,
	})
	if err != nil {
		return nil, fmt.Errorf("serialize external signed request: %w", err)
	}
	return body, nil
}

func BuildTransferBody(privateKey ed25519.PrivateKey, storage *Storage, messages []payments.WalletMessage, validUntil time.Time) (*cell.Cell, error) {
	outActions, err := payments.PackOutActions(messages)
	if err != nil {
		return nil, fmt.Errorf("pack vault out actions: %w", err)
	}
	return BuildExternalSignedBody(privateKey, storage, outActions, validUntil)
}

func StorageMatchesKey(storage *Storage, pub ed25519.PublicKey) bool {
	return storage != nil && len(storage.Key) == ed25519.PublicKeySize && string(storage.Key) == string(pub)
}

func StorageMatchesID(storage *Storage, pub ed25519.PublicKey) bool {
	return storage != nil && bytes.Equal(storage.ID, DeriveID(pub))
}
