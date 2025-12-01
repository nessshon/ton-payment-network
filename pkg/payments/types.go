package payments

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/xssnick/ton-payment-network/pkg/log"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"math/big"
	"time"
)

const Version = 3

type VirtualConfig struct {
	MaxCapacityToRentPerTx      tlb.Coins
	CapacityDepositFee          tlb.Coins
	CapacityFeePercentPer30Days float64
	ProxyMaxCapacity            tlb.Coins
	ProxyMinFee                 tlb.Coins
	ProxyFeePercent             float64
	AllowTunneling              bool
}

type JettonClient interface {
	GetRootAddress() *address.Address
	GetWalletAddress(ctx context.Context, addr *address.Address) (*address.Address, error)
	GetBalance(ctx context.Context, addr *address.Address, blockAfter time.Time) (*big.Int, error)
}

type ActionResolver interface {
	ResolveAction(ctx context.Context, id []byte) (Action, error)
}

type BalanceTypeResolver interface {
	ResolveBalanceType(id string) (*CoinConfig, error)
	GetKnownBalanceTypes() []*CoinConfig
}

type FullResolver interface {
	ActionResolver
	BalanceTypeResolver
}

type CoinConfig struct {
	Enabled               bool
	VirtualTunnelConfig   VirtualConfig
	Symbol                string
	Decimals              uint8
	MinCapacityRequest    tlb.Coins
	JettonClient          JettonClient
	FeePerWithdrawPropose tlb.Coins

	BalanceID string
}

func (c *CoinConfig) MustAmount(nano *big.Int) tlb.Coins {
	return tlb.MustFromNano(nano, int(c.Decimals))
}

func (c *CoinConfig) MustAmountDecimal(str string) tlb.Coins {
	return tlb.MustFromDecimal(str, int(c.Decimals))
}

type WalletMessage struct {
	Mode            uint8
	InternalMessage *tlb.InternalMessage
}

type BalanceInfo struct {
	Onchain            *big.Int
	Action             *big.Int
	OnHold             *big.Int
	ConditionalLocked  *big.Int // from us to them
	ConditionalPending *big.Int // from them to us

	CoinConfig *CoinConfig
}

var PaymentChannelCodeBOCs = []string{
	"b5ee9c724102480100101e000114ff00f4a413f4bcf2c80b01020120023302014803310202cc04250201200519020120061404f34f891f240ed44d0d200d200d33fd31fd3ffd3ffd37fd4fa40f404d10ad72c250cf6d464e302d72c27011887bc8e1ad4d4f89210cd10bc10ab109a10891078106710561045f0035f0ae0d72c2335c9fa3c8e1ad4d4f89210cd10bc10ab109a10891078106710561045f0045f0ae0d72c20bb27e594e30289d7278070a0d0e01fe31d4d4f8920af2d06420f90123d0546191f9109722d027f910c300923070e2f2e065c8cf92867b6a3213cccc21cf16c90182106a78d1a824f005d70b3f27bef2e069f82a29b38b026d02c8ca0070cf0b6028cf0bff27cf0bff26cf0b7f25cf14cef400c95c5cf90001f9005ad76501d765c8cf8c0804d2cb0fcb0fcbffcbff0802f671f90400c8cf8a0040cbffc9d024d0d35f31fa00302070fb0253b1c70593145f048e3caa00c8cf89080153435cf90001f9005ad76501d765c8cf8c0804d2cb0fcb0fcbffcbff71f90400cf0bff01fa0281008dcf0b7013cccc12ccc970fb00e2f82828c7059137e30e7f54780654787654787d5612db3c6ca1ed5409460054c8cf850818ce8d0680000000000000000000000000006a993b6d800000000000000040cf16c98042fb0001fed31fd3000195d4d481009495306d6d6d70e2f8922df2e0755360c705f2e0656d6d6d6d6d7056156e9257158e4a5f060fd0d33fd3000197d3ffd3ff810099946d6d5870e201d31fd200d20031d3ff31d181009c2dd0d70b1f23a0f823bcf2e06f27c000f2d06a532bb9923b1a9132e20111140115144330e25614c000b350050b03d85612e3045614c000b3976c316d6d587001df1114c000b3500470e304707007c000951029373730e30ec8cf850813ce8d0680000000000000000000000000006a993b6d800000000000000040cf16c98042fb0007111007446413db3c54787654787654787f29db3c6ca1ed540c424600c86c2225f90108d05468f1f91007d054180df91016b0f2e06503d72c217831c724f2bfd37fd33fd4d4d1533cba3403f2e072531fbef2e06905c0009357117f96201112bcc300e2f2e0692f983302d0d3ffd3ffd1983002d0d3ffd3ffd1e20550338100990100083fb3e346032ee302d72c21fd1a4e84e302d72c212a19548c31e3025f0a0f111301fe8308d718f8922af2e0752c6ef2d06b21f901547c78e3044140f910f2e06582102fa8f89c25f005d4d4f4050dd025d001d33fd3000197d3ffd3ff810099946d6d5870e201d31fd200d200d70bff08d31fd70b1f5242a0a0f823b9f2e08027c300f2e08e23c000f2d08d5087f0076d56146e9257149830111323f0071113e2281003fef9008b085420038307f42e6fa11116138307f40e6fa13011159621c700b3c3009170e2f2e08c09d0ed1e561103011115015614011116da4006d768c8cf850818ce8d0680000000000000000000000000006a993b6d800000000000000040cf16c98042fb00471550640311100302111001db3c54787654787654787f29db3c42464701fed70bfff89229f2e0752b6ef2d06b5302c705f2e0650bd023d001d33fd3000197d3ffd3ff810099946d6d5870e201d31fd200d200d70bff08d31fd70b1f5242a020f823b9f2e06ea0f823bcf2e07007c000f2e070c8cf850801111201ce8d0680000000000000000000000000006a993b6d800000000000000040cf16c98042120244fb001047103655220211100201111001db3c54787654787654787f29db3c6ca1ed54424602fcf89228f2e0752a6ef2d06b2ad023d001d33f31d3000199d3ff31d3ff318100999170e201d70b1f02d31fd31fd70b1f03c000993031a0f823b9f2e0719b03a058a0a0f823b9f2e071e2f8005469905469905469905469905290561301f00a303770296e9a3608d0d70b3f6d509906df06a454786054787654787d5612db3c46470101581503f22cf2e07521f901c8cf93808c43de25cf1424cf1423cf16c905d05461c1f9109903d0541309f910c30093303270e2f2e06582109bc30c1227f005d33fd200f405531eba92317f8e11511ebd9822d72c0131b3c3009170e2c300e2f2e065531bbcf2e06c246ee301f800206e92303ae30e2b544b302b544b302b161718003c24d0d33fd70b00938100999170e2c0009330346d9722b9926d35de04e2040054d0f404f404d4d12f9112df226e935f033a8e16d0ed1e02d0216eb39201d092316de2103d5420f3da40e20138544b302b544b302b544b03f0083054798754798754798729db3ced54460201201a1f0201201b1e0101201c01f62cf2e07521f901c8cf919ae4fd1e25cf1424cf1423cf16c905d05461c1f9109903d0541309f910c30093303270e2f2e065821051a6fa0827f005d33fd70a00530dba92307f8e102dbd9821d72c0131b3c3009170e2c300e2f2e065520bbcf2e06cf8002b544b302b544b302b544b302b544b302b544b03f00930381d014470286e9a3707d0d70b3f6d508807df07a45479705479875479875611db3ced540708460101202102012020230379208408b5869a4976cf348034c7f4c7fd01544f6ebcb8210a6ebcb8217e08ef3cb8223e0001e9151ea6151ce6151ea60ab6cf3b553e03c9dba44df8c3a0214622002002d31fd37f5023baf2e06858baf2e07200a87028d739308e4220d74bc002f2e093c028f2e093d72c20761e436cf2e093d4d307d74c0172b0f2e089d0d2000191309fd30231fa4031fa403025c705f2d093e2d7393001a421c70012e6308407bbf2e09307ed5501012024003201d739f2e073d30701c003f2e07320d70bff58baf2e073d74c020120262f020120272a02012028280101202900a4326c443434345113c7058e1ad0d35f31fa0030c8cf858813ce58fa0271cf0b6accc970fb007fe15bc8cf8508ce8d0680000000000000000000000000006a993b6d800000000000000040cf16c98042fb00700201202b2d0101202c00a8316c63335122c7058e1ed0d35f31fa0030c8cf858812ce01fa02821025432a91cf0b8ac970fb007fe130c8cf8508ce8d0680000000000000000000000000006a993b6d800000000000000040cf16c98042fb00700101202e00ea36383838383838385157c7058e3a04d0d35f31fa0030c8cf905d93f2ca14cb1f05c0009710235f0301cf819704cf83cc13cccee2c9c8cf858813ce01fa0271cf0b6accc970fb007fe15f06c8cf8508ce8d0680000000000000000000000000006a993b6d800000000000000040cf16c98042fb00700101d43000b0326c443434345113c7058e20d0d35f31fa0030c8cf858813ce58fa0282103fa349d0cf0b8acbffc970fb007fe15bc8cf8508ce8d0680000000000000000000000000006a993b6d800000000000000040cf16c98042fb007001cfa1432bdb45dbf7da89a1a40063a401a67e63a63e63a7fe63a7fe63a6fe63a9f48063e809a20524b6e1c242dd24b6e3c003a003a003a67e63a6000333a7fe63a7fe6302013322e1c403ae163e05a63fa63fae163ea68541f0477926be0ae5c007800124be09c61ceb3200385331a021a0f823bc955f0473db31e003a058a0a0f823bc9374db31e004f8f2ed44d0d200d200d33fd31fd3ffd3ffd37fd4fa40f404d10ad72c25443dc7dc8e3e28942a6ec3009170e2f2e065d4d420f90103d0546391f9109901d0541206f910c300936c2170e2f2e087109a1089107810671056104510344130f0065f0ae0d72c218d20b464e302d72c27011887bce302d72c2335c9fa3ce30234353637005628f2d0658308d71820f901547b76e3044130f910f2e087109a1089107810671056104510344130f0065f0a0034d4d48b0210cd10bc10ab109a10891078106710561045f0035f0a0034d4d48b0210cd10bc10ab109a10891078106710561045f0045f0a042a89d727e302d72c212af085b4e302d72c25c8b5e3e438393c3f0008bd1cdbdb01fe8308d71829f2e07520f901547b76e3044130f910f2e06582109a50b6bc24f005d3000195d4d481009495306d6d6d70e26d6d6d6d6d7056136e8e3d5f062dd0d33fd3000197d3ffd3ff810099946d6d5870e201d31fd200d20031d3ff31d181009c2cd0d70b1f23a0f823bcf2e06f01f2d06a26c000f2d06adf20c000b350063a03ee5611e30425c000b3986c22326d6d5a7002df05c000b39330f823df7f707028c000e301f8008b0256150656150656150656150656150656150656150656150656150605111f05544114050311160302111502011114011113f00b3047155063444001111001db3c54787654787654787f29db3c6ca1ed543b424600ca353527f9012ad052105611f9102ad0125610f910b0f2e06527d72c217831c724f2bfd37fd33fd4d4d1235611ba3403f2e072215614bef2e06905c00092377f955208bcc300e2f2e0695613983302d0d3ffd3ffd1983002d0d3ffd3ffd1e25054810099444401fe8308d71829f2e0752b6ef2d06b20f901547b76e3044130f910f2e06582108a3056b724f005d31ff404d4d74c5139baf2e0852cd025d001d33fd3000197d3ffd3ff810099946d6d5870e201d31fd200d200d70bff08d31fd70b1f5242a020f823b9f2e06ea0f823bc9420b3c3009170e2f2e07023c000f2d08d5085f00750933d02fcf007f8000ea456110156110156115110561101561101561101561101561101111bdb3ced54f80f8e35068307f4966fa5208e258b0840058307f42e6fa19620c700b3c3009170e2f2e073ed1e0211100201111001da214e1e926c21e2b317e636d7680cd7688b02c8cf900e367a1a2ecf0bff22cf0bffc9c8cf872012ce71463e0248cf0b61ccc970fb001027106c0504103c401cdb3c547876547c7654787b29db3c6ca1ed544246032ee302d72c2195c86454e302d72c212a19548c31e302f23f40434501fc8308d71829f2e0752b6ef2d06b20f901547b76e3044130f910f2e06582101c1b99b824f005d31fd70bff5117baf2e0852ad023d001d33fd3000197d3ffd3ff810099946d6d5870e201d31fd200d200d70bff08d31fd70b1f5242a020f823b9f2e06ea0f823bc9420b3c3009170e2f2e07023c000f2d08d5184baf2e06907410292f2d064f8008b02561002561002561002561002561002561002561002561002561002111a511cf00c307f0ca4104710361025111043431cdb3c547876547c7654787b29db3c6ca1ed544246004607c8cb3f04c00095343401cf819a03cf8315cbff13cbff12e2cb1f12ca00ca00cbffc901fc8308d71829f2e0752b6ef2d06b2bd024d001d33f31d30001948308d721ded70b1f01d31fd31fd31f31fa00305aa058a0f823b9f2e08021f901547c87e3044140f910f2e06582103e6c2d6125f005d31fd74cf8276f1023aa00bcf2e0765118baf2e085d0d72c21fd9f1a34f2bf8308d718f80008a4547ba95473a9547ba94401725615db3c316c443535353501ed54f80fc8cf90fecf8d1a22d74bf2498308baf28912cecec9c8cf858823cf163359fa0271cf0b6accc970fb004602fc8b0228f2e0752a6ef2d06b2ad023d001d33f31d3000199d3ff31d3ff318100999170e201d70b1f02d31fd31fd70b1f03c000993031a0f823b9f2e0719b03a058a0a0f823b9f2e071e2f8005469905469905469905469905290561301f00a303770296e9a3608d0d70b3f6d509906df06a454786054787654787d5612db3c4647003209c8ca0018ca0016cb3f14cb1f12cbffcbffcb7fcccef400c900086ca1ed54a0ef3c3a",
}

var PaymentChannelCodes = func() []*cell.Cell {
	var codes []*cell.Cell
	for _, c := range PaymentChannelCodeBOCs {
		codeBoC, _ := hex.DecodeString(c)
		code, _ := cell.FromBOC(codeBoC)
		codes = append(codes, code)
	}
	return codes
}()

type Signature struct {
	Value []byte `tlb:"bits 512"`
}
type ChannelStorageData struct {
	IsA            bool   `tlb:"bool"`
	Initialized    bool   `tlb:"bool"`
	CommittedSeqno uint64 `tlb:"## 64"`
	WalletSeqno    uint32 `tlb:"## 32"`

	KeyA          ed25519.PublicKey `tlb:"bits 256"`
	KeyB          ed25519.PublicKey `tlb:"bits 256"`
	ChannelID     ChannelID         `tlb:"bits 128"`
	ClosingConfig ClosingConfig     `tlb:"^"`
	PartyAddress  *address.Address  `tlb:"addr"`

	Quarantine *QuarantinedState `tlb:"maybe ^"`
}

type QuarantinedState struct {
	Seqno                  uint64     `tlb:"## 64"`
	TheirState             *StateSide `tlb:"maybe ."`
	QuarantineStarts       uint32     `tlb:"## 32"`
	CommittedByOwner       bool       `tlb:"bool"`
	OurSettlementFinalized bool       `tlb:"bool"`
	ActionsToExecuteHash   []byte     `tlb:"bits 256"`
}

type ClosingConfig struct {
	QuarantineDuration             uint32    `tlb:"## 32"`
	ConditionalCloseDuration       uint32    `tlb:"## 32"`
	ActionsDuration                uint32    `tlb:"## 32"`
	ReplicationMessageAttachAmount tlb.Coins `tlb:"."`
}

type StateSide struct {
	ConditionalsHash []byte `tlb:"bits 256"`
	ActionStatesHash []byte `tlb:"bits 256"`
}

type StateBody struct {
	_         tlb.Magic `tlb:"#2f0638e4"`
	ChannelID ChannelID `tlb:"bits 128"`
	Seqno     uint64    `tlb:"## 64"`
	A         StateSide `tlb:"^"`
	B         StateSide `tlb:"^"`
}

type StateBodySigned struct {
	SignatureA Signature `tlb:"^"`
	SignatureB Signature `tlb:"^"`
	Body       StateBody `tlb:"."`
}

type OpenConfigContainer struct {
	Seqno         uint64        `tlb:"## 64"`
	KeyA          []byte        `tlb:"bits 256"`
	KeyB          []byte        `tlb:"bits 256"`
	ChannelID     ChannelID     `tlb:"bits 128"`
	ClosingConfig ClosingConfig `tlb:"^"`
	InitSignature Signature     `tlb:"^"`
}

type InitChannel struct {
	_          tlb.Magic `tlb:"#a19eda8c"`
	SignatureA Signature `tlb:"^"`
	SignatureB Signature `tlb:"^"`
	Signed     struct {
		_         tlb.Magic `tlb:"#6a78d1a8"`
		ChannelID ChannelID `tlb:"bits 128"`
		Seqno     uint64    `tlb:"## 64"`
	} `tlb:"."`
}

type ExternalMsgDoubleSigned struct {
	_          tlb.Magic `tlb:"#a887b8fb"`
	SignatureA Signature `tlb:"^"`
	SignatureB Signature `tlb:"^"`
	Signed     struct {
		_           tlb.Magic  `tlb:"#22d61a69"`
		ChannelID   ChannelID  `tlb:"bits 128"`
		SideA       bool       `tlb:"bool"`
		ValidUntil  uint32     `tlb:"## 32"`
		WalletSeqno uint32     `tlb:"## 32"`
		OutActions  *cell.Cell `tlb:"maybe ^"`
	} `tlb:"."`
}

type ExternalMsgOwnerSigned struct {
	_         tlb.Magic `tlb:"#31a4168c"`
	Signature Signature `tlb:"."`
	Signed    struct {
		_           tlb.Magic  `tlb:"#22d61a69"`
		ChannelID   ChannelID  `tlb:"bits 128"`
		SideA       bool       `tlb:"bool"`
		ValidUntil  uint32     `tlb:"## 32"`
		WalletSeqno uint32     `tlb:"## 32"`
		OutActions  *cell.Cell `tlb:"maybe ^"`
	} `tlb:"."`
}

type CooperativeCommitAction struct {
	StateA *cell.Cell `tlb:"maybe ^"`
	StateB *cell.Cell `tlb:"maybe ^"`
	Code   *cell.Cell `tlb:"^"`
}

type CooperativeCommit struct {
	_          tlb.Magic `tlb:"#e02310f7"`
	SignatureA Signature `tlb:"^"`
	SignatureB Signature `tlb:"^"`
	Signed     struct {
		_         tlb.Magic                `tlb:"#9bc30c12"`
		ChannelID ChannelID                `tlb:"bits 128"`
		Seqno     uint64                   `tlb:"## 64"`
		FromA     bool                     `tlb:"bool"`
		Action    *CooperativeCommitAction `tlb:"maybe ^"`
	} `tlb:"."`
}

type CooperativeClose struct {
	_          tlb.Magic `tlb:"#66b93f47"`
	SignatureA Signature `tlb:"^"`
	SignatureB Signature `tlb:"^"`
	Signed     struct {
		_         tlb.Magic `tlb:"#51a6fa08"`
		ChannelID ChannelID `tlb:"bits 128"`
		Seqno     uint64    `tlb:"## 64"`
		FromA     bool      `tlb:"bool"`
	} `tlb:"."`
}

type UncoopCloseMsg struct {
	_         tlb.Magic `tlb:"#bd1cdbdb"`
	Signature Signature `tlb:"."`
	Signed    struct {
		_         tlb.Magic        `tlb:"#9a50b6bc"`
		ChannelID ChannelID        `tlb:"bits 128"`
		State     *StateBodySigned `tlb:"maybe ."`
	} `tlb:"."`
}

type UncoopCloseReplicateMsg struct {
	_     tlb.Magic        `tlb:"#1764fcb2"`
	At    uint32           `tlb:"## 32"`
	State *StateBodySigned `tlb:"maybe ."`
}

type SettleMsg struct {
	_         tlb.Magic `tlb:"#255e10b6"`
	Signature Signature `tlb:"."`
	Signed    struct {
		_                 tlb.Magic        `tlb:"#8a3056b7"`
		ChannelID         ChannelID        `tlb:"bits 128"`
		WalletSeqno       uint32           `tlb:"## 32"`
		ToSettle          *cell.Dictionary `tlb:"dict 256"`
		ConditionalsProof *cell.Cell       `tlb:"^"`
		ActionsInputProof *cell.Cell       `tlb:"^"`
	} `tlb:"."`
}

type FinalizeSettleMsg struct {
	_         tlb.Magic `tlb:"#b916bc7c"`
	Signature Signature `tlb:"."`
	Signed    struct {
		_                tlb.Magic `tlb:"#1c1b99b8"`
		ChannelID        ChannelID `tlb:"bits 128"`
		WalletSeqno      uint32    `tlb:"## 32"`
		ActionsInputHash []byte    `tlb:"bits 256"`
	} `tlb:"."`
}

type ExecuteActionsMsg struct {
	_         tlb.Magic `tlb:"#3fb3e346"`
	Signature Signature `tlb:"."`
	Signed    struct {
		_                      tlb.Magic  `tlb:"#2fa8f89c"`
		ChannelID              ChannelID  `tlb:"bits 128"`
		Action                 *cell.Cell `tlb:"^"`
		OurActionsInputProof   *cell.Cell `tlb:"^"`
		TheirActionsInputProof *cell.Cell `tlb:"maybe ^"`
	} `tlb:"."`
}

type ProxyExecuteActionsMsg struct {
	_         tlb.Magic `tlb:"#32b90c8a"`
	Signature Signature `tlb:"."`
	Signed    struct {
		_           tlb.Magic         `tlb:"#3e6c2d61"`
		ChannelID   ChannelID         `tlb:"bits 128"`
		WalletSeqno uint32            `tlb:"## 32"`
		Msg         ExecuteActionsMsg `tlb:"^"`
	} `tlb:"."`
}

type FinishUncooperativeClose struct {
	_ tlb.Magic `tlb:"#25432a91"`
}

type ConditionalsSettledEvent struct {
	_                   tlb.Magic `tlb:"#038d9e86"`
	NewConditionalsHash []byte    `tlb:"bits 256"`
	NewActionsHash      []byte    `tlb:"bits 256"`
}

func (s *StateBodySigned) IsEmpty() bool {
	return bytes.Equal(s.SignatureA.Value, make([]byte, 64)) &&
		bytes.Equal(s.SignatureB.Value, make([]byte, 64)) &&
		bytes.Equal(s.Body.A.ConditionalsHash, make([]byte, 32)) &&
		bytes.Equal(s.Body.B.ConditionalsHash, make([]byte, 32)) &&
		bytes.Equal(s.Body.A.ActionStatesHash, make([]byte, 32)) &&
		bytes.Equal(s.Body.B.ActionStatesHash, make([]byte, 32))
}

// Verify - verify sides if key is not nil
func (s *StateBodySigned) Verify(keyA, keyB ed25519.PublicKey) error {
	if keyA == nil && keyB == nil {
		return fmt.Errorf("keys are nil")
	}

	c, err := tlb.ToCell(s.Body)
	if err != nil {
		return err
	}
	if keyA != nil {
		if !ed25519.Verify(keyA, c.Hash(), s.SignatureA.Value) {
			log.Warn().Str("sig", base64.StdEncoding.EncodeToString(s.SignatureA.Value)).Msg("invalid signature A")
			return fmt.Errorf("invalid signature A")
		}
	} else if !bytes.Equal(s.SignatureA.Value, make([]byte, 64)) {
		return fmt.Errorf("signature A expected to be empty")
	}

	if keyB != nil {
		if !ed25519.Verify(keyB, c.Hash(), s.SignatureB.Value) {
			log.Warn().Str("sig", base64.StdEncoding.EncodeToString(s.SignatureB.Value)).Msg("invalid signature B")
			return fmt.Errorf("invalid signature B")
		}
	} else if !bytes.Equal(s.SignatureB.Value, make([]byte, 64)) {
		return fmt.Errorf("signature B expected to be empty")
	}
	return nil
}

var ErrNotFound = fmt.Errorf("not found")

func FindConditional(ctx context.Context, conditionals *cell.Dictionary, id []byte, actions ActionResolver) (*cell.Cell, Conditional, error) {
	return FindConditionalWithProof(ctx, conditionals, id, nil, actions)
}

func FindConditionalWithProof(ctx context.Context, conditionals *cell.Dictionary, id []byte, proofRoot *cell.ProofSkeleton, actions ActionResolver) (idx *cell.Cell, _ Conditional, _ error) {
	// TODO: indexed dict o(1)

	var tempProofRoot *cell.ProofSkeleton
	if proofRoot != nil {
		tempProofRoot = cell.CreateProofSkeleton()
	}

	idx = cell.BeginCell().MustStoreSlice(id, 256).EndCell()
	sl, proofBranch, err := conditionals.LoadValueWithProof(idx, tempProofRoot)
	if err != nil {
		if errors.Is(err, cell.ErrNoSuchKeyInDict) {
			if proofRoot != nil {
				proofRoot.Merge(tempProofRoot)
			}
			return nil, nil, ErrNotFound
		}
		return nil, nil, err
	}

	cond, err := CodeToConditional(ctx, sl.MustToCell(), actions)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse of one of conditionals: %w", err)
	}

	if proofRoot != nil {
		proofBranch.SetRecursive()
		proofRoot.Merge(tempProofRoot)
	}
	return idx, cond, nil
}

/*
func (s *SemiChannelPacked) CheckSynchronized(with *SemiChannelPacked) error {
	if !bytes.Equal(s.ChannelID, with.ChannelID) {
		return fmt.Errorf("diff channel id")
	}

	ourStateOnTheirSide, err := tlb.ToCell(with.State.CounterpartyData)
	if err != nil {
		return fmt.Errorf("failed to serialize our state on their side: %w", err)
	}
	ourState, err := tlb.ToCell(s.State.Data)
	if err != nil {
		return fmt.Errorf("failed to serialize our state: %w", err)
	}

	if !bytes.Equal(ourStateOnTheirSide.Hash(), ourState.Hash()) {
		return fmt.Errorf("our state on their side is diff")
	}

	theirStateOnOurSide, err := tlb.ToCell(s.State.CounterpartyData)
	if err != nil {
		return fmt.Errorf("failed to serialize their state on our side: %w", err)
	}
	theirState, err := tlb.ToCell(with.State.Data)
	if err != nil {
		return fmt.Errorf("failed to serialize their state: %w", err)
	}

	if !bytes.Equal(theirStateOnOurSide.Hash(), theirState.Hash()) {
		return fmt.Errorf("their state on our side is diff")
	}

	return nil
}*/

/*
func (s *StateBody) Dump() string {
	c, err := tlb.ToCell(s.)
	if err != nil {
		return "failed cell"
	}

	cp, err := tlb.ToCell(s.CounterpartyData)
	if err != nil {
		return "failed cell"
	}

	cpData := fmt.Sprintf("(data_hash: %s seqno: %d; conditionals_hash: %s; action_states_hash: %s)",
		base64.StdEncoding.EncodeToString(cp.Hash()[:8]),
		s.CounterpartyData.Seqno, base64.StdEncoding.EncodeToString(s.CounterpartyData.ConditionalsHash), base64.StdEncoding.EncodeToString(s.CounterpartyData.ActionStatesHash))

	return fmt.Sprintf("data_hash: %s seqno: %d; conditionals_hash: %s; action_states_hash: %s; counterparty: %s",
		base64.StdEncoding.EncodeToString(c.Hash()[:8]),
		s.Data.Seqno, base64.StdEncoding.EncodeToString(s.Data.ConditionalsHash), base64.StdEncoding.EncodeToString(s.Data.ActionStatesHash), cpData)
}*/

func (s *StateSide) Copy() (StateSide, error) {
	return StateSide{
		ConditionalsHash: append([]byte{}, s.ConditionalsHash...),
		ActionStatesHash: append([]byte{}, s.ActionStatesHash...),
	}, nil
}

func (s *StateSide) Equals(other *StateSide) bool {
	return bytes.Equal(s.ConditionalsHash, other.ConditionalsHash) &&
		bytes.Equal(s.ActionStatesHash, other.ActionStatesHash)
}

func NewBalanceInfo(cc *CoinConfig) *BalanceInfo {
	return &BalanceInfo{
		Onchain:            new(big.Int),
		Action:             new(big.Int),
		OnHold:             new(big.Int),
		ConditionalLocked:  new(big.Int),
		ConditionalPending: new(big.Int),
		CoinConfig:         cc,
	}
}

func (b *BalanceInfo) Available() *big.Int {
	if b == nil {
		return big.NewInt(0)
	}

	amt := new(big.Int).Add(b.Onchain, b.Action)
	if b.OnHold.Sign() > 0 {
		amt.Sub(amt, b.OnHold)
	}
	if b.ConditionalLocked.Sign() > 0 {
		amt.Sub(amt, b.ConditionalLocked)
	}
	return amt
}

func GetTONBalanceID() string {
	return hex.EncodeToString(make([]byte, 32))
}

func GetJettonBalanceID(addr *address.Address) string {
	return hex.EncodeToString(addr.Data())
}

func GetJettonFromBalanceID(id string) *address.Address {
	a, err := hex.DecodeString(id)
	if err != nil || len(a) != 32 {
		panic("corrupted balance id: " + id)
	}
	return address.NewAddress(0, 0, a)
}

func GetECBalanceID(id uint32) string {
	a := make([]byte, 32)
	binary.BigEndian.PutUint32(a[24:], id)
	return hex.EncodeToString(a)
}

func GetECFromBalanceID(id string) uint32 {
	a, err := hex.DecodeString(id)
	if err != nil || len(a) != 32 {
		panic("corrupted balance id: " + id)
	}
	return binary.BigEndian.Uint32(a[24:])
}

var ErrMissingBalanceInfo = fmt.Errorf("missing balance info")

func ResolveActionBalances(balances map[string]*BalanceInfo, act Action, num int) ([]*BalanceInfo, error) {
	ccs := act.GetAffectedCoins()
	if len(ccs) != num {
		return nil, fmt.Errorf("unexpected balances num")
	}

	list := make([]*BalanceInfo, num)
	for i := 0; i < num; i++ {
		b := balances[ccs[i].BalanceID]
		if b == nil {
			return nil, ErrMissingBalanceInfo
		}
		list[i] = b
	}
	return list, nil
}

func ResolveActionBalance(balances map[string]*BalanceInfo, act Action) (*BalanceInfo, error) {
	b, err := ResolveActionBalances(balances, act, 1)
	if err != nil {
		return nil, err
	}
	return b[0], nil
}
