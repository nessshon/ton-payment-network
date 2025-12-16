package transport

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/pkg/payments/conditionals"
	"github.com/xssnick/tonutils-go/adnl/keys"
	"github.com/xssnick/tonutils-go/tl"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"math"
	"math/big"
	mRand "math/rand"
	"reflect"
	"time"
)

var ErrNotConnected = fmt.Errorf("not connected with peer")

func init() {
	tl.RegisterAllowedGroup("payments.proposable",
		"payments.addConditionalAction",
		"payments.executeConditionalAction",
		"payments.removeConditionalAction",
		"payments.incrementStatesAction",
		"payments.commitVirtualAction",
		"payments.rentCapacityAction",
		"payments.cooperativeCommitAction",
		"payments.removeActionAction",
		"payments.swapAction")

	tl.RegisterAllowedGroup("payments.requestable",
		"payments.cooperativeCloseAction",
		"payments.executeTransactionAction")

	tl.Register(Ping{}, "payments.ping value:long = payments.Ping")
	tl.Register(Pong{}, "payments.pong value:long = payments.Pong")

	tl.Register(Decision{}, "payments.decision agreed:Bool reason:string signature:bytes = payments.Decision")
	tl.Register(ProposalDecision{}, "payments.proposalDecision agreed:Bool reason:string signedState:bytes = payments.ProposalDecision")
	tl.Register(ChannelConfigDecision{}, "payments.channelConfig ok:Bool reason:string = payments.ChannelConfigDecision")
	tl.Register(AuthenticateToSign{}, "payments.authenticateToSign a:int256 b:int256 timestamp:long = payments.AuthenticateToSign")
	tl.Register(NodeAddress{}, "payments.nodeAddress adnl_addr:int256 = payments.NodeAddress")

	tl.Register(ExecuteConditionalAction{}, "payments.executeConditionalAction id:int256 state:bytes = payments.Action")
	tl.Register(RemoveConditionalAction{}, "payments.removeConditionalAction id:int256 = payments.Action")
	tl.Register(AddConditionalAction{}, "payments.addConditionalAction newActionCode:bytes conditional:bytes instruction_key:int256 instructions:payments.instructionsToSign signature:bytes = payments.Action")
	tl.Register(RemoveActionAction{}, "payments.removeActionAction id:int256 = payments.Action")
	tl.Register(CommitVirtualAction{}, "payments.commitVirtualAction id:int256 prepayAmount:bytes = payments.Action")
	tl.Register(CooperativeCloseAction{}, "payments.cooperativeCloseAction signedCloseRequest:bytes = payments.Action")
	tl.Register(CooperativeCommitAction{}, "payments.cooperativeCommitAction action_id:int256 msg_signature:bytes withFee:bool = payments.Action")
	tl.Register(IncrementStatesAction{}, "payments.incrementStatesAction wantResponse:Bool = payments.Action")
	tl.Register(RentCapacityAction{}, "payments.rentCapacityAction till:long amount:bytes balance_id:int256 = payments.Action")
	tl.Register(ExecuteTransactionAction{}, "payments.executeTransactionAction body:bytes = payments.Action")
	tl.Register(SwapAction{}, "payments.swapAction from_balance_id:int256 to_balance_id:int256 from_amount:bytes to_amount:bytes = payments.Action")

	tl.Register(ProposeChannelConfig{}, "payments.proposeChannelConfig replicateAttachAmount:bytes quarantineDuration:int actionsExecuteDuration:int conditionalCloseDuration:int nodeVersion:int codeHash:int256 = payments.Request")
	tl.Register(RequestAction{}, "payments.requestAction channelAddr:int256 action:payments.Action = payments.Request")
	tl.Register(ProposeAction{}, "payments.proposeAction lockId:long channelAddr:int256 action:payments.Action state:bytes = payments.Request")
	tl.Register(Authenticate{}, "payments.authenticate key:int256 timestamp:long signature:bytes = payments.Authenticate")
	tl.Register(OpenChannelOffchain{}, "payments.openChannelOffchain codeHash:int256 openConfig:bytes nodeVersion:int = payments.OpenChannelOffchain")
	tl.Register(OpenChannelOffchainResponse{}, "payments.openChannelOffchainResponse addr:int256 initBodySignature:bytes reason:string = payments.OpenChannelOffchainResponse")

	tl.Register(InstructionContainer{}, "payments.instructionContainer hash:int256 data:bytes = payments.InstructionContainer")
	tl.RegisterWithFabric(InstructionsToSign{}, "payments.instructionsToSign list:(vector payments.instructionContainer) = payments.InstructionsToSign", func() reflect.Value {
		return reflect.ValueOf(&InstructionsToSign{})
	})
	tl.Register(AddConditionalInstruction{}, "payments.openVirtualInstruction target:int256 nextInstructionKey:int256 expectedDeadline:long nextTarget:int256 nextDeadline:long details:bytes finalState:bytes = payments.AddConditionalInstruction")

	tl.Register(RequestChannelLock{}, "payments.requestChannelLock lockId:long channel:int256 lock:Bool = payments.RequestChannelLock")
	tl.Register(IsChannelUnlocked{}, "payments.isChannelUnlocked lockId:long channel:int256 = payments.IsChannelUnlocked")
}

type Action any

// NodeAddress - DHT record value which stores adnl addr related to node's public key used for channels
type NodeAddress struct {
	ADNLAddr []byte `tl:"int256"`
}

// OpenChannelOffchain - open and create chanel before deployment
type OpenChannelOffchain struct {
	CodeHash    []byte     `tl:"int256"`
	OpenConfig  *cell.Cell `tl:"cell"`
	NodeVersion uint32     `tl:"int"`
}

// OpenChannelOffchainResponse - result for OpenChannelOffchain
type OpenChannelOffchainResponse struct {
	Addr              []byte `tl:"int256"`
	InitBodySignature []byte `tl:"bytes"`
	Reason            string `tl:"string"`
}

// Ping - check connection is alive and delay
type Ping struct {
	Value int64 `tl:"long"`
}

// RequestChannelLock - lock/unlock channel to propose actions
type RequestChannelLock struct {
	LockID      int64  `tl:"long"`
	ChannelAddr []byte `tl:"int256"`
	Lock        bool   `tl:"bool"`
}

// IsChannelUnlocked - check is channel still locked with specific id
type IsChannelUnlocked struct {
	LockID      int64  `tl:"long"`
	ChannelAddr []byte `tl:"int256"`
}

// Pong - response on check connection is alive
type Pong struct {
	Value int64 `tl:"long"`
}

// Authenticate - auth with both sides adnl ids signature, to establish connection
type Authenticate struct {
	Key       []byte `tl:"int256"`
	Timestamp int64  `tl:"long"`
	// It should be the signature of AuthenticateToSign, signed by node channel key
	Signature []byte `tl:"bytes"`
}

// AuthenticateToSign - payload to sign for auth, A and B are adnl addresses of parties
type AuthenticateToSign struct {
	A         []byte `tl:"int256"`
	B         []byte `tl:"int256"`
	Timestamp int64  `tl:"long"`
}

// ProposeAction - request party to update state with action,
// for example open virtual channel and add conditional payment
type ProposeAction struct {
	LockID      int64      `tl:"long"`
	ChannelAddr []byte     `tl:"int256"`
	Action      any        `tl:"struct boxed [payments.proposable]"`
	SignedState *cell.Cell `tl:"cell"`
}

// RequestAction - request party to propose some action
type RequestAction struct {
	ChannelAddr []byte `tl:"int256"`
	Action      any    `tl:"struct boxed [payments.requestable]"`
}

// Decision - response for actions request, Reason is filled when not agreed
type Decision struct {
	Agreed    bool   `tl:"bool"`
	Reason    string `tl:"string"`
	Signature []byte `tl:"bytes"`
}

// ProposalDecision - response for actions proposals, Reason is filled when not agreed
type ProposalDecision struct {
	Agreed      bool       `tl:"bool"`
	Reason      string     `tl:"string"`
	SignedState *cell.Cell `tl:"cell optional"`
}

// CommitVirtualAction - prepay virtual channel for amount, can be used for graceful shutdown,
// to not trigger uncooperative close when you offline, by other party virtual closure attempt
type CommitVirtualAction struct {
	ID                 []byte     `tl:"int256"`
	UpdatedConditional *cell.Cell `tl:"cell"`
}

// RemoveActionAction - remove an empty or used action
type RemoveActionAction struct {
	ID []byte `tl:"int256"`
}

// AddConditionalAction - request party to open virtual channel (tunnel) with specified target
type AddConditionalAction struct {
	NewActionCode *cell.Cell `tl:"cell optional"`
	Conditional   *cell.Cell `tl:"cell"`
	// We use instruction keys to guarantee instructions execution order
	// next node can know instruction key only from previous node, then shared key can be calculated
	InstructionKey []byte             `tl:"int256"`
	Instructions   InstructionsToSign `tl:"struct"`
	Signature      []byte             `tl:"bytes"`
}

// SwapAction - swap one coin to another if a party agrees
type SwapAction struct {
	FromBalanceID []byte `tl:"int256"`
	ToBalanceID   []byte `tl:"int256"`
	FromAmount    []byte `tl:"bytes"`
	ToAmount      []byte `tl:"bytes"`
}

// RentCapacityAction - request party to deposit inbound capacity for us, for coins optionally
type RentCapacityAction struct {
	Till      uint64 `tl:"long"`
	Amount    []byte `tl:"bytes"`
	BalanceID []byte `tl:"int256"`
}

type InstructionsToSign struct {
	// ED25519 Montgomery encrypted slice of InstructionContainer.Data + stub,
	// private of Key is used + public of next target from prev instruction.
	// Garlic-like virtual channels
	List []InstructionContainer `tl:"vector struct"`
}

type InstructionContainer struct {
	Hash []byte `tl:"int256"`
	Data []byte `tl:"bytes"`
}

type AddConditionalInstruction struct {
	Target             []byte `tl:"int256"`
	NextInstructionKey []byte `tl:"int256"`

	ExpectedDeadline int64 `tl:"long"`

	NextTarget   []byte `tl:"int256"`
	NextDeadline int64  `tl:"long"`

	Details *cell.Cell `tl:"cell optional"`

	// can be set for the final receiver, so virtual channel will be closed immediately,
	// can be used for simple transfers with immediate delivery
	FinalState *cell.Cell `tl:"cell optional"`

	instructionPrivateKey ed25519.PrivateKey `tl:"-"`
}

// CooperativeCloseAction - request party to close onchain channel
type CooperativeCloseAction struct {
	SignedCloseRequest *cell.Cell `tl:"cell"`
}

// CooperativeCommitAction - request party to commit onchain channel state
type CooperativeCommitAction struct {
	ActionID     []byte `tl:"int256"`
	MsgSignature []byte `tl:"bytes"`
	WithFee      bool   `tl:"bool"`
}

type ExecuteTransactionAction struct {
	ExternalBody *cell.Cell `tl:"cell"`
}

// RemoveConditionalAction - request party to remove expired condition to unlock funds
type RemoveConditionalAction struct {
	ID []byte `tl:"int256"`
}

// ExecuteConditionalAction - execute conditional and increase the unconditional amount
type ExecuteConditionalAction struct {
	ID    []byte     `tl:"int256"`
	State *cell.Cell `tl:"cell"`
}

// IncrementStatesAction - send our state with incremented seqno to party,
// and expect same from them too when WantResponse = true. Can be used to confirm rollback or for the first state exchange
type IncrementStatesAction struct {
	WantResponse bool `tl:"bool"`
}

// ProposeChannelConfig - request channel params supported by party,
// to deploy contract and initialize communication
type ProposeChannelConfig struct {
	ReplicateAttachAmount    []byte `tl:"bytes"`
	QuarantineDuration       uint32 `tl:"int"`
	ActionsExecuteDuration   uint32 `tl:"int"`
	ConditionalCloseDuration uint32 `tl:"int"`
	NodeVersion              uint32 `tl:"int"`
	CodeHash                 []byte `tl:"int256"`
}

// ChannelConfigDecision - response for ProposeChannelConfig
type ChannelConfigDecision struct {
	Ok     bool   `tl:"bool"`
	Reason string `tl:"string"`
}

func (a *AddConditionalAction) SetInstructions(actions []AddConditionalInstruction, key ed25519.PrivateKey) error {
	a.Instructions = InstructionsToSign{}

	maxLen := 0
	serializedActions := make([][]byte, len(actions))
	for i := 0; i < len(actions); i++ {
		data, err := tl.Serialize(actions[i], true)
		if err != nil {
			return fmt.Errorf("failed to serialize action data: %w", err)
		}
		serializedActions[i] = data
		if len(data) > maxLen {
			maxLen = len(data)
		}
	}

	// randomly increase len to complicate external analysis for instructions count
	fuzz, err := rand.Int(rand.Reader, big.NewInt(512))
	if err != nil {
		return err
	}
	maxLen += int(fuzz.Int64())

	for i := 0; i < len(actions); i++ {
		lenDiff := maxLen - len(serializedActions[i])
		// add random stub data to hide real size
		data := append(serializedActions[i], make([]byte, lenDiff)...)
		// fill padding with random bytes to avoid potential zero padding attacks
		_, _ = rand.Read(data[len(serializedActions[i]):])

		sharedKey, err := keys.SharedKey(actions[i].instructionPrivateKey, actions[i].Target)
		if err != nil {
			return fmt.Errorf("failed to calc shared key: %w", err)
		}

		hash := sha256.Sum256(data)
		stream, err := keys.BuildSharedCipher(sharedKey, hash[:])
		if err != nil {
			return fmt.Errorf("failed to init cipher: %w", err)
		}
		stream.XORKeyStream(data, data)

		a.Instructions.List = append(a.Instructions.List, InstructionContainer{
			Hash: hash[:],
			Data: data,
		})
	}

	data, err := tl.Serialize(a.Instructions, true)
	if err != nil {
		return fmt.Errorf("failed to serialize instructions data: %w", err)
	}
	a.Signature = ed25519.Sign(key, data)

	return nil
}

func (a *AddConditionalAction) DecryptOurInstruction(ctx context.Context, key ed25519.PrivateKey, instructionKey ed25519.PublicKey, resolver payments.ActionResolver) (*AddConditionalInstruction, error) {
	verifyData, err := tl.Serialize(a.Instructions, true)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize verify data: %w", err)
	}

	cond, err := payments.CodeToConditional(ctx, a.Conditional, resolver)
	if err != nil {
		return nil, fmt.Errorf("failed to convert conditional data: %w", err)
	}

	if !ed25519.Verify(cond.GetKey(), verifyData, a.Signature) {
		return nil, fmt.Errorf("incorrect signature")
	}

	sharedKey, err := keys.SharedKey(key, instructionKey)
	if err != nil {
		return nil, fmt.Errorf("failed to calc shared key: %w", err)
	}

	for _, instruction := range a.Instructions.List {
		stream, err := keys.BuildSharedCipher(sharedKey, instruction.Hash)
		if err != nil {
			return nil, fmt.Errorf("failed to init cipher: %w", err)
		}

		payload := make([]byte, len(instruction.Data))
		stream.XORKeyStream(payload, instruction.Data)

		hash := sha256.Sum256(payload)
		if !bytes.Equal(hash[:], instruction.Hash) {
			// not our
			continue
		}

		var value AddConditionalInstruction
		if _, err = tl.Parse(&value, payload, true); err != nil {
			return nil, fmt.Errorf("incorrect AddConditionalInstruction: %w", err)
		}
		return &value, nil
	}
	return nil, fmt.Errorf("not found")
}

type TunnelChainPart struct {
	Target   ed25519.PublicKey
	Capacity *big.Int
	Fee      *big.Int
	Deadline time.Time
}

func GenerateTunnel(key ed25519.PrivateKey, chain []TunnelChainPart, stubSize uint8, withFinalState bool, senderKey ed25519.PrivateKey, cc *payments.CoinConfig) (ed25519.PublicKey, []AddConditionalInstruction, error) {
	if len(chain) == 0 {
		return nil, nil, fmt.Errorf("chain is empty")
	}

	var firstInstructionKey ed25519.PublicKey
	var list []AddConditionalInstruction
	for i := 0; i < len(chain); i++ {
		instDetails := conditionals.ConditionalVirtualChannelInstructionDetails{
			ExpectedFee:      cc.MustAmount(chain[i].Fee),
			ExpectedCapacity: cc.MustAmount(chain[i].Capacity),
		}

		inst := AddConditionalInstruction{
			ExpectedDeadline: chain[i].Deadline.UTC().Unix(),
			Target:           chain[i].Target,
		}

		var err error
		var pub ed25519.PublicKey
		var private ed25519.PrivateKey
		if i != len(chain)-1 || senderKey == nil {
			pub, private, err = ed25519.GenerateKey(nil)
			if err != nil {
				return nil, nil, err
			}
		} else {
			// use sender key, so receiver will know from whom the transfer is received
			pub = senderKey.Public().(ed25519.PublicKey)
			private = senderKey
		}

		inst.instructionPrivateKey = private
		if i > 0 {
			list[i-1].NextInstructionKey = pub
		} else {
			firstInstructionKey = pub
		}

		if i < len(chain)-1 {
			inst.NextTarget = chain[i+1].Target
			inst.NextDeadline = chain[i+1].Deadline.UTC().Unix()

			instDetails.NextFee = cc.MustAmount(chain[i+1].Fee)
			instDetails.NextCapacity = cc.MustAmount(chain[i+1].Capacity)
		} else {
			inst.NextTarget = chain[i].Target
			inst.NextDeadline = chain[i].Deadline.UTC().Unix()

			instDetails.NextFee = cc.MustAmount(chain[i].Fee)
			instDetails.NextCapacity = cc.MustAmount(chain[i].Capacity)
			if withFinalState {
				state := conditionals.VirtualChannelState{Amount: chain[i].Capacity}
				state.Sign(key)

				fs, err := tlb.ToCell(state)
				if err != nil {
					return nil, nil, fmt.Errorf("failed to serialize final state: %w", err)
				}
				inst.FinalState = fs
			}
		}

		inst.Details, err = tlb.ToCell(instDetails)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to serialize details: %w", err)
		}

		list = append(list, inst)
	}

	if stubSize > 0 {
		// generate seed with cryptographic random
		seed, err := rand.Int(rand.Reader, big.NewInt(math.MaxInt64))
		if err != nil {
			return nil, nil, err
		}
		mRnd := mRand.New(mRand.NewSource(seed.Int64()))

		for i := 0; i < int(stubSize); i++ {
			// all those actions will be encrypted, we use similar amounts
			// just to keep bytes length similar to original for stronger security

			nextFee := randAmount(chain[len(chain)-1].Fee, chain[0].Fee)
			nextCap := randAmount(chain[len(chain)-1].Capacity, chain[0].Capacity)

			randKey := make([]byte, 32)
			_, _ = rand.Read(randKey)
			randKey2, _, _ := ed25519.GenerateKey(nil)
			randKey3, randKey3prv, _ := ed25519.GenerateKey(nil)

			// spread deadlines to look similar to original
			dlDiff := (chain[0].Deadline.UTC().Unix() - chain[len(chain)-1].Deadline.UTC().Unix()) * 2
			if dlDiff < 3600 {
				dlDiff = 3600
			}

			nextDl := (chain[len(chain)-1].Deadline.UTC().Unix() - dlDiff/2) + mRnd.Int63n(dlDiff)
			expDl := nextDl + mRnd.Int63n(dlDiff)

			instDetails := conditionals.ConditionalVirtualChannelInstructionDetails{
				ExpectedFee:      cc.MustAmount(randAmount(chain[len(chain)-1].Fee, nextFee)),
				ExpectedCapacity: cc.MustAmount(randAmount(chain[len(chain)-1].Capacity, nextCap)),
				NextFee:          cc.MustAmount(nextFee),
				NextCapacity:     cc.MustAmount(nextCap),
			}

			details, err := tlb.ToCell(instDetails)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to serialize details: %w", err)
			}

			list = append(list, AddConditionalInstruction{
				Target:                randKey2,
				ExpectedDeadline:      expDl,
				NextTarget:            randKey,
				NextInstructionKey:    randKey3,
				NextDeadline:          nextDl,
				instructionPrivateKey: randKey3prv,
				Details:               details,
			})
		}

		mRnd.Shuffle(len(list), func(i, j int) {
			list[i], list[j] = list[j], list[i]
		})
	}

	return firstInstructionKey, list, nil
}

func randAmount(from, to *big.Int) *big.Int {
	diff := new(big.Int).Sub(to, from)
	if diff.Sign() != 1 {
		return from
	}

	n, err := rand.Int(rand.Reader, diff)
	if err != nil {
		// practically impossible
		return from
	}
	return n.Add(n, from)
}
