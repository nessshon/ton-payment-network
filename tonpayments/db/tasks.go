package db

import (
	"crypto/ed25519"
	"encoding/json"
	"github.com/xssnick/ton-payment-network/tonpayments/transport"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"time"
)

type Task struct {
	ID             string
	Type           string
	Queue          string
	Data           json.RawMessage
	LockedTill     *time.Time
	LockedAt       *time.Time
	ExecuteAfter   time.Time
	ReExecuteAfter *time.Time
	ExecuteTill    *time.Time
	CreatedAt      time.Time
	CompletedAt    *time.Time
	LastError      string
}

type ChannelTask struct {
	Address string
}

type FinalizeSettleTask struct {
	ChannelAddress      string
	ExpectedActionsHash []byte
}

type BlockOffset struct {
	Seqno     uint32
	UpdatedAt time.Time
}

type ChannelUncooperativeCloseTask struct {
	Address              string
	CheckCondStillExists []byte
	ChannelInitiatedAt   *time.Time
}

type TopupTask struct {
	Address            string
	Amount             string
	BalanceID          string
	ChannelInitiatedAt time.Time
	FromBalanceControl bool
}

type ActionCommitTask struct {
	Address            string
	ActionId           []byte
	ChannelInitiatedAt time.Time
	ForFee             bool
}

type CommitExecuteTask struct {
	ChannelAddress string
	SignedRequest  *cell.Cell
}

type WaitPendingTxTask struct {
	ChannelAddress string
	IsOurSide      bool
	PendingID      string

	MsgHash   []byte
	StartedAt time.Time
}

type WaitDepositCompletionTask struct {
	ChannelAddress string
	BalanceID      string

	UnlockBalanceControl bool
	MsgHash              []byte
	FromAddress          string
	StartedAt            time.Time
}

type RefreshOnchainBalanceTask struct {
	ChannelAddress string
	IsOurSide      bool
	BlockAfter     int64
}

type ExecuteExternalTxTask struct {
	ChannelAddress string
	OurSide        bool
	Body           *cell.Cell
	WalletSeqno    uint32
}

type RequestExternalTxTask struct {
	ChannelAddress string
	PackedMessages *cell.Cell
	WalletSeqno    uint32
}

type SettleConditionalStepTask struct {
	Step               int
	Address            string
	Message            *cell.Cell
	ChannelInitiatedAt *time.Time
}

type SettleActionStepTask struct {
	Step               int
	Address            string
	Message            *cell.Cell
	ChannelInitiatedAt *time.Time
}

type ChannelCooperativeCloseTask struct {
	Address            string
	ChannelInitiatedAt time.Time
}

type ConfirmCloseVirtualTask struct {
	VirtualKey []byte
}

type CloseNextVirtualTask struct {
	VirtualKey []byte
}

type CommitVirtualTask struct {
	ChannelAddress string
	VirtualKey     []byte
}

type AddConditionalTask struct {
	SenderKey           ed25519.PublicKey
	FinalDestinationKey ed25519.PublicKey // known only for initiator
	PrevChannelAddress  string
	PrevConditionalID   []byte
	ChannelAddress      string
	Deadline            int64
	TransportAction     transport.AddConditionalAction
}

type SwapTask struct {
	ChannelAddress  string
	TransportAction transport.SwapAction
}

type AskRemoveVirtualTask struct {
	Key            []byte
	ChannelAddress string
}

type AskCloseVirtualTask struct {
	ID             []byte
	ChannelAddress string
}

type IncrementStatesTask struct {
	ChannelAddress string
	WantResponse   bool
}

type RemoveVirtualTask struct {
	Key []byte
}
