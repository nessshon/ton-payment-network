package contracts

import (
	"encoding/hex"
	"fmt"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

var ChannelCodeBOCs = []string{
	"b5ee9c7241020b0100025b000114ff00f4a413f4bcf2c80b01020162020a0202cd03090289d7c48f92076a26869ffea7d006987e9007d00698fe98fe98ffd0069006b858f956869ffe98fe98ffd207d20180888eb96103e29fa9a718136196b96103e29fa967181791fc040702fe31363f04d4d74c01d08308d71802d08308d71823f901541035f910f2e06521f9014014f910f2e065d72c2089119a24f2bfd70b1f01d72c2089119a24f2bfd31ffa003026c0005338bef2e0695328bef2e0695323bef2e06929c000917f955393bec300e2f2e06925b393373870e30df2e06920f823a1b60b500fbbf2e0692c0506002e2092377f955228bbc300e292387f955229bbc300e2c30000b2544c302c544c302c544c30545b225615544f3c01f00620c10096b60b29bec300923070e203917f9322c300e2f2e064f82358a00ac8cbff19cc5007fa0215cb0f13ca0001fa0214cb1fcb1f14cb1f01fa0212ca00cb1fc9ed5401fa23917f9f25c2009622f823b9c3009170e2c300e2f2e0802d0a109d108c107b5160106e105d104c4e1350dcf00603d200d74c019135953102a34014e222c2fff2e07624d0d72c212af085b4f2bf8308d71831d31f31d37f31fa40d31f31f405f82812c705f2e0658307f4966fa531216eb392c300923070e2f2e08d016e080068f2e08ad3fffa00d70b1f5026ba955003bac30093303270e29412bac30093303170e2f2e069c8cf8508ce71cf0b6eccc98306fb00003768c142697c14165440917c128f824d448e864d48ce87880aa00aa612005ba0ba0fda89a1a7ffa9f401a61fa401f401a63fa63fa63ff401a401ae163e21563632302e2c2a288660a561e00c034d56fce9",
}

var Codes = func() []*cell.Cell {
	var codes []*cell.Cell
	for _, c := range ChannelCodeBOCs {
		codeBoC, _ := hex.DecodeString(c)
		code, _ := cell.FromBOC(codeBoC)
		codes = append(codes, code)
	}
	return codes
}()

// DerivativeConfig mirrors FunC struct Config:
//
//	struct Config {
//	  oracleKey: int256
//	  quarantineDuration: uint32
//	  acceptionWindow: uint32
//	  addressA: address
//	  addressB: address
//	}
//
// Note: oracleKey represented as 256-bit signed integer; here encoded as 256 bits.
// Addresses are standard workchain addresses.
// All layout and order strictly follow the FunC definition.
type DerivativeConfig struct {
	OracleKey          []byte           `tlb:"bits 256"`
	QuarantineDuration uint32           `tlb:"## 32"`
	AcceptionWindow    uint32           `tlb:"## 32"`
	AddressA           *address.Address `tlb:"addr"`
	AddressB           *address.Address `tlb:"addr"`
}

// DerivativeStorage mirrors FunC struct Storage:
//
//	struct Storage {
//	  key: int256
//	  config: Cell<Config>
//
//	  amount: coins
//	  leverage: uint16
//	  isLong: bool
//
//	  entryPrice: coins
//	  entryAt: uint32
//
//	  exitAt: uint32
//	  exitPrice: coins
//	  isLiquidated: bool
//
//	  quarantineTill: uint32
//	}
//
// Important: config is stored as a reference cell, hence the `^` tag.
type DerivativeStorage struct {
	Key    []byte           `tlb:"bits 256"`
	Config DerivativeConfig `tlb:"^"`

	Amount   tlb.Coins `tlb:"."`
	Leverage uint16    `tlb:"## 16"`
	IsLong   bool      `tlb:"bool"`

	EntryPrice tlb.Coins `tlb:"."`
	EntryAt    uint32    `tlb:"## 32"`
	CreatedAt  uint32    `tlb:"## 32"`

	ExitAt       uint32    `tlb:"## 32"`
	ExitPrice    tlb.Coins `tlb:"."`
	IsLiquidated bool      `tlb:"bool"`

	QuarantineTill uint32 `tlb:"## 32"`
}

// DerivativeResolve mirrors FunC struct DerivativeResolve
//
//	struct DerivativeResolve {
//	  key: int256
//	  amount: coins
//	  at: uint32
//	}
type DerivativeResolve struct {
	Key    []byte    `tlb:"bits 256"`
	Amount tlb.Coins `tlb:"."`
	At     uint32    `tlb:"## 32"`
}

// PriceProof mirrors FunC struct (0x11223344) PriceProof
//
//	struct (0x11223344) PriceProof {
//	  at: uint32
//	  price: coins
//	}
type PriceProof struct {
	_     tlb.Magic `tlb:"#11223344"`
	At    uint32    `tlb:"## 32"`
	Price tlb.Coins `tlb:"."`
}

// ProxySettle mirrors FunC struct (0x0f8a7ea5) ProxySettle
// It carries a boolean and a reference to serialized SettleConditionalsMessage body.
// We keep the message as raw cell to avoid coupling with higher-level message types.
//
//	struct (0x0f8a7ea5) ProxySettle {
//	  toA: bool
//	  msg: Cell<SettleConditionalsMessage>
//	}
type ProxySettle struct {
	_   tlb.Magic  `tlb:"#0f8a7ea5"`
	ToA bool       `tlb:"bool"`
	Msg *cell.Cell `tlb:"^"`
}

// Commit mirrors FunC struct (0x0f8a7ea6) Commit
// Since FunC uses `RemainingBitsAndRefs` for signedBody (the remainder of the cell),
// we store it as a raw cell that should contain the exact body that was signed.
//
//	struct (0x0f8a7ea6) Commit {
//	  signature: bits512
//	  signedBody: RemainingBitsAndRefs
//	}
//
// Note: `SignedBody` is expected to contain a cell starting with a `PriceProof` (or other agreed type),
// serialized exactly as it was signed off-chain. It is caller responsibility to prepare it correctly.
type Commit struct {
	_     tlb.Magic  `tlb:"#0f8a7ea6"`
	Entry PriceInner `tlb:"^"`
	Exit  PriceInner `tlb:"^"`
}

type PriceInner struct {
	Signature struct {
		V []byte `tlb:"bits 512"`
	} `tlb:"."`
	SignedBody *cell.Cell `tlb:"."`
}

// LoadDerivativeStorage parses storage cell of the Derivative contract and returns storage.
func LoadDerivativeStorage(data *cell.Cell) (*DerivativeStorage, error) {
	if data == nil {
		return nil, fmt.Errorf("nil storage data")
	}
	var st DerivativeStorage
	if err := tlb.Parse(&st, data); err != nil {
		return nil, fmt.Errorf("failed to parse derivative storage: %w", err)
	}
	return &st, nil
}

// BuildDerivativeStorage builds the initial on-chain storage cell for deployment.
// Required arguments mirror the storage fields. Exit* and liquidation/quarantine are initialized to zero/false.
func BuildDerivativeStorage(key []byte, cfg DerivativeConfig, amount tlb.Coins, leverage uint16, isLong bool, entryPrice tlb.Coins, createdAt uint32) (*cell.Cell, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("key must be 32 bytes (256 bits), got %d", len(key))
	}
	st := DerivativeStorage{
		Key:            key,
		Config:         cfg,
		Amount:         amount,
		Leverage:       leverage,
		IsLong:         isLong,
		EntryPrice:     entryPrice,
		EntryAt:        0,
		CreatedAt:      createdAt,
		ExitAt:         0,
		ExitPrice:      tlb.ZeroCoins,
		IsLiquidated:   false,
		QuarantineTill: 0,
	}
	c, err := tlb.ToCell(st)
	if err != nil {
		return nil, fmt.Errorf("failed to build derivative storage cell: %w", err)
	}
	return c, nil
}

// BuildDerivativeStateInit composes a minimal StateInit from given code and data cells.
// Caller is responsible for providing a verified code cell of this contract.
func BuildDerivativeStateInit(data *cell.Cell) (*tlb.StateInit, error) {
	if data == nil {
		return nil, fmt.Errorf("code and data must be non-nil")
	}
	si := tlb.StateInit{Code: Codes[0], Data: data}
	return &si, nil
}

// CalcDerivativeAddress computes the contract address for the given state init
// (workchain 0) using standard TON address derivation from the state hash.
func CalcDerivativeAddress(si *tlb.StateInit) (*address.Address, error) {
	if si == nil || si.Code == nil || si.Data == nil {
		return nil, fmt.Errorf("state init is nil or incomplete")
	}
	c, err := tlb.ToCell(*si)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize state init: %w", err)
	}
	return address.NewAddress(0, 0, c.Hash()), nil
}
