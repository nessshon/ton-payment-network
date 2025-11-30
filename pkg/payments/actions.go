package payments

import (
	"context"
	"fmt"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"math/big"
	"time"
)

type Action interface {
	Serialize() *cell.Cell
	IDCell() *cell.Cell
	Parse(ctx context.Context, balanceTypes BalanceTypeResolver, s *cell.Slice) error
	GetAffectedCoins() []*CoinConfig
	GetEmptyState() *cell.Cell
	EmulateBalance(state *cell.Cell, balances map[string]*BalanceInfo, fromUs bool) error
	CheckCanRemove(commitedSeqno uint64, state *cell.Cell) (bool, error)
	PrepareNext(ctx context.Context, addrA, addrB *address.Address) (Action, error)
	PrepareExecuteState(state *cell.Cell, party *address.Address, seqno uint64, withFee bool, finalBalances map[string]*BalanceInfo) (*cell.Cell, *PendingMessageInfo, error)
	StatesDiff(before, after *cell.Cell) (map[string]*big.Int, error)
	GetFeesPerCommitPropose() (map[string]*big.Int, error)
}

type ActionSend interface {
	Action
	AddCoins(actionState *cell.Cell, amount *big.Int, locked map[string]*LockedDepositInfo) (*cell.Cell, error)
}

type Conditional interface {
	Serialize() *cell.Cell
	Parse(ctx context.Context, s *cell.Slice, actions ActionResolver) error
	GetAction() Action
	GetKey() []byte
	GetDeadline() time.Time
	GetLogInfo() map[string]any
	ValidateOnAdd() error
	ValidateState(old, new *cell.Cell) error
	EmulateBalance(balances map[string]*BalanceInfo, fromUs bool) error
	Commit(updated Conditional, actState *cell.Cell) (*cell.Cell, error)
	PrepareCommit(condState *cell.Cell) (Conditional, error)
	CheckInstruction(detailsCell *cell.Cell, isFinalDest bool, balances map[string]*BalanceInfo, finalState *cell.Cell) error
	PrepareNext(instructionDetailsCell *cell.Cell, nextAction Action, nextDeadline time.Time) (Conditional, error)
	ScoreTunnelTarget(instructionDetailsCell *cell.Cell, targetBalances map[string]*BalanceInfo) (*big.Int, error)
	Execute(actionState, latestCondState *cell.Cell, locked map[string]*LockedDepositInfo) (*cell.Cell, error)
}

var ErrAlreadyCommitted = fmt.Errorf("already committed")

type ActionInfo interface {
	GetAction() Action
}

type LockedDepositInfo struct {
	Amount *big.Int
	Till   time.Time
	Used   *big.Int
}

type PendingMessageInfo struct {
	Amounts map[string]*big.Int

	CompletionBodyPrefix []byte
	CompletionAddress    string
	LimitDepth           int
}

var ConditionalTypes = map[string]func() Conditional{}
var ActionTypes = map[string]func() Action{}

func (ld *LockedDepositInfo) Available() *big.Int {
	if ld == nil || ld.Till.Before(time.Now()) || ld.Amount.Cmp(ld.Used) <= 0 {
		return big.NewInt(0)
	}
	return new(big.Int).Sub(ld.Amount, ld.Used)
}

func DetectActionType(root *cell.Cell) (Action, error) {
	// TODO: magic?
	next := make([]*cell.Cell, 0, 4)
	for i := range int(root.RefsNum()) {
		c, err := root.PeekRef(i)
		if err != nil {
			return nil, err
		}

		if t := ActionTypes[string(c.Hash())]; t != nil {
			return t(), nil
		}

		next = append(next, c)
	}

	for _, c := range next {
		a, err := DetectActionType(c)
		if err != nil {
			return nil, err
		}

		if a != nil {
			return a, nil
		}
	}

	return nil, nil
}

func CodeToAction(ctx context.Context, code *cell.Cell, balanceTypes BalanceTypeResolver) (Action, error) {
	a, err := DetectActionType(code)
	if err != nil {
		return nil, fmt.Errorf("failed to detect action type: %w", err)
	}
	if a == nil {
		return nil, fmt.Errorf("unknown action type")
	}

	if err = a.Parse(ctx, balanceTypes, code.BeginParse()); err != nil {
		return nil, fmt.Errorf("failed to parse action code: %w", err)
	}

	return a, nil
}

func CodeToConditional(ctx context.Context, code *cell.Cell, actions ActionResolver) (Conditional, error) {
	a, err := DetectConditionalType(code)
	if err != nil {
		return nil, fmt.Errorf("failed to detect cond type: %w", err)
	}

	if err = a.Parse(ctx, code.BeginParse(), actions); err != nil {
		return nil, fmt.Errorf("failed to parse cond code: %w", err)
	}

	return a, nil
}

func DetectConditionalType(root *cell.Cell) (Conditional, error) {
	next := make([]*cell.Cell, 0, 4)
	for i := range int(root.RefsNum()) {
		c, err := root.PeekRef(i)
		if err != nil {
			return nil, err
		}

		if t := ConditionalTypes[string(c.Hash())]; t != nil {
			return t(), nil
		}

		next = append(next, c)
	}

	for _, c := range next {
		a, err := DetectConditionalType(c)
		if err != nil {
			return nil, err
		}

		if a != nil {
			return a, nil
		}
	}

	return nil, nil
}

func LoadState(v any, state *cell.Cell) error {
	ld := state.BeginParse()
	if err := tlb.LoadFromCell(v, ld); err != nil {
		return fmt.Errorf("failed to load old state: %w", err)
	}

	if ld.BitsLeft() > 0 || ld.RefsNum() > 0 {
		return fmt.Errorf("data left after parse")
	}
	return nil
}
