package conditionals

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/pkg/payments/actions"
	"github.com/xssnick/ton-payment-network/pkg/payments/vm"
	"github.com/xssnick/tonutils-go/adnl/keys"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"math/big"
	"time"
)

func init() {
	payments.ConditionalTypes[string(virtualChannelUniversalStaticCode.Hash())] = func() payments.Conditional {
		return &ConditionalVirtualChannel{}
	}
}

type ConditionalVirtualChannel struct {
	Key      ed25519.PublicKey
	Capacity *big.Int
	Fee      *big.Int
	Prepay   *big.Int
	Deadline int64

	Action payments.Action
}

type ConditionalVirtualChannelInstructionDetails struct {
	ExpectedFee      tlb.Coins `tlb:"."`
	ExpectedCapacity tlb.Coins `tlb:"."`

	NextFee tlb.Coins `tlb:"."`
	// Should be <= ExpectedCapacity
	NextCapacity tlb.Coins `tlb:"."`
}

type VirtualChannelState struct {
	Signature []byte
	Amount    *big.Int
}

func (c *ConditionalVirtualChannel) Serialize() *cell.Cell {
	return cell.BeginCell().
		MustStoreBuilder(vm.PushIntOP(new(big.Int).SetBytes(c.Action.Serialize().Hash()))).
		MustStoreBuilder(vm.PushIntOP(c.Fee)).
		MustStoreBuilder(vm.PushIntOP(c.Capacity)).
		MustStoreBuilder(vm.PushIntOP(c.Prepay)).
		MustStoreBuilder(vm.PushIntOP(big.NewInt(c.Deadline))).
		MustStoreBuilder(vm.PushIntOP(new(big.Int).SetBytes(c.Key))).
		// we pack immutable part of code to ref for better BoC compression and cheaper transactions
		MustStoreRef(virtualChannelUniversalStaticCode). // implicit jump
		EndCell()
}

func (c *ConditionalVirtualChannel) Parse(ctx context.Context, s *cell.Slice, actions payments.ActionResolver) error {
	actHashInt, err := vm.ReadIntOP(s)
	if err != nil {
		return fmt.Errorf("failed to parse fee: %w", err)
	}

	fee, err := vm.ReadIntOP(s)
	if err != nil {
		return fmt.Errorf("failed to parse fee: %w", err)
	}
	if fee.BitLen() > 127 {
		return fmt.Errorf("failed to parse fee: incorrect bits len")
	}
	if fee.Sign() < 0 {
		return fmt.Errorf("failed to parse fee: cannot be negative")
	}

	capacity, err := vm.ReadIntOP(s)
	if err != nil {
		return fmt.Errorf("failed to parse capacity: %w", err)
	}
	if capacity.BitLen() > 127 {
		return fmt.Errorf("failed to parse capacity: incorrect bits len")
	}
	if capacity.Sign() < 0 {
		return fmt.Errorf("failed to parse capacity: cannot be negative")
	}

	prepay, err := vm.ReadIntOP(s)
	if err != nil {
		return fmt.Errorf("failed to parse prepay: %w", err)
	}
	if prepay.BitLen() > 127 {
		return fmt.Errorf("failed to parse prepay: incorrect bits len")
	}
	if prepay.Sign() < 0 {
		return fmt.Errorf("failed to parse prepay: cannot be negative")
	}

	deadline, err := vm.ReadIntOP(s)
	if err != nil {
		return fmt.Errorf("failed to parse deadline: %w", err)
	}
	if deadline.BitLen() > 32 {
		return fmt.Errorf("failed to parse deadline: incorrect bits len")
	}
	if deadline.Sign() <= 0 {
		return fmt.Errorf("failed to parse deadline: cannot be negative or zero")
	}

	keyInt, err := vm.ReadIntOP(s)
	if err != nil {
		return fmt.Errorf("failed to parse key: %w", err)
	}

	key := keyInt.Bytes()
	if len(key) > 32 {
		return fmt.Errorf("too big key size")
	}

	if len(key) < 32 {
		// prepend it with zeroes
		key = append(make([]byte, 32-len(key)), key...)
	}

	actHash := actHashInt.Bytes()
	if len(actHash) > 32 {
		return fmt.Errorf("too big act hash size")
	}

	if len(actHash) < 32 {
		// prepend it with zeroes
		actHash = append(make([]byte, 32-len(actHash)), actHash...)
	}

	code, err := s.LoadRefCell()
	if err != nil {
		return fmt.Errorf("failed to parse code: %w", err)
	}

	if !bytes.Equal(code.Hash(), virtualChannelUniversalStaticCode.Hash()) {
		return fmt.Errorf("incorrect code")
	}

	if s.BitsLeft() != 0 || s.RefsNum() != 0 {
		return fmt.Errorf("unexpected data in condition")
	}

	a, err := actions.ResolveAction(ctx, actHash)
	if err != nil {
		return fmt.Errorf("failed to resolve action: %w", err)
	}

	if len(a.GetAffectedCoins()) != 1 {
		return fmt.Errorf("unexpected number of affected coins")
	}

	c.Action = a
	c.Key = key
	c.Capacity = capacity
	c.Fee = fee
	c.Prepay = prepay
	c.Deadline = int64(deadline.Uint64())
	return nil
}

func (c *ConditionalVirtualChannel) GetAction() payments.Action {
	return c.Action
}

func (c *ConditionalVirtualChannel) EmulateBalance(balances map[string]*payments.BalanceInfo, fromUs bool) error {
	b, err := payments.ResolveActionBalance(balances, c.Action)
	if err != nil {
		return fmt.Errorf("failed to resolve balance: %w", err)
	}

	amt := new(big.Int).Add(c.Capacity, c.Fee)
	if c.Prepay.Sign() > 0 {
		amt = new(big.Int).Sub(amt, c.Prepay)
	}

	if amt.Sign() <= 0 {
		return nil
	}

	if fromUs {
		b.ConditionalLocked.Add(b.ConditionalLocked, amt)
	} else {
		b.ConditionalPending.Add(b.ConditionalPending, amt)
	}

	return nil
}

func (c *ConditionalVirtualChannel) GetKey() []byte {
	return c.Key
}

func (c *ConditionalVirtualChannel) Commit(updated payments.Conditional, actState *cell.Cell) (*cell.Cell, error) {
	newCond, ok := updated.(*ConditionalVirtualChannel)
	if !ok {
		return nil, fmt.Errorf("unexpected cond type")
	}

	pOld, pNew := c.Prepay, newCond.Prepay

	c.Prepay, newCond.Prepay = big.NewInt(0), big.NewInt(0)
	nHash := newCond.Serialize().Hash()
	oHash := c.Serialize().Hash()
	c.Prepay, newCond.Prepay = pOld, pNew

	if !bytes.Equal(nHash, oHash) {
		return nil, fmt.Errorf("something except of prepay was changed")
	}

	prepayAmt := new(big.Int).Sub(newCond.Prepay, c.Prepay)
	if prepayAmt.Sign() < 0 {
		return nil, fmt.Errorf("prepay cannot be decreased")
	}

	var state actions.StateActionSend
	if err := payments.LoadState(&state, actState); err != nil {
		return nil, fmt.Errorf("failed to load old state: %w", err)
	}

	state.Amount.Val = new(big.Int).Add(state.Amount.Nano(), prepayAmt)

	newState, err := tlb.ToCell(state)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize new state: %w", err)
	}
	c.Prepay = newCond.Prepay // only prepay can change

	return newState, nil
}

func (c *ConditionalVirtualChannel) GetDeadline() time.Time {
	return time.Unix(c.Deadline, 0)
}

func (c *ConditionalVirtualChannel) ValidateOnAdd() error {
	if c.Capacity.Sign() <= 0 {
		return fmt.Errorf("invalid capacity")
	}

	if c.Fee.Sign() < 0 {
		return fmt.Errorf("invalid fee")
	}

	if c.Prepay.Sign() != 0 {
		return fmt.Errorf("prepay is not zero")
	}

	if c.Deadline < time.Now().UTC().Unix() {
		return fmt.Errorf("expired deadline")
	}

	return nil
}

func (c *ConditionalVirtualChannel) CheckInstruction(detailsCell *cell.Cell, isFinalDest bool, balances map[string]*payments.BalanceInfo, finalState *cell.Cell) error {
	if detailsCell == nil {
		return fmt.Errorf("missing details cell")
	}

	var details ConditionalVirtualChannelInstructionDetails
	if err := payments.LoadState(&details, detailsCell); err != nil {
		return err
	}

	if details.ExpectedFee.Nano().Cmp(c.Fee) != 0 || details.ExpectedCapacity.Nano().Cmp(c.Capacity) != 0 {
		return fmt.Errorf("incorrect values, not equals to expected")
	}

	balanceInfo, err := payments.ResolveActionBalance(balances, c.Action)
	if err != nil {
		return fmt.Errorf("failed to resolve balances: %w", err)
	}

	balance := balanceInfo.Available()

	if isFinalDest {
		var state VirtualChannelState
		if finalState != nil {
			// for channels with known final state we allow credit, so we will wait for their topup and use it
			if err := payments.LoadState(&state, finalState); err != nil {
				return fmt.Errorf("failed to parse virtual channel state: %w", err)
			}

			if !state.Verify(c.Key) {
				return fmt.Errorf("final state is incorrect")
			}

			if state.Amount.Cmp(c.Capacity) != 0 {
				return fmt.Errorf("final state should use full capacity")
			}

			if state.Amount.Cmp(balanceInfo.CoinConfig.VirtualTunnelConfig.MaxCapacityToRentPerTx.Nano()) > 0 {
				return fmt.Errorf("amount to receive is too big to rent in single operation")
			}
		} else if balance.Sign() < 0 {
			return fmt.Errorf("not enough available balance, you need %s more to open virtual channel with me", balance.Abs(balance).String())
		}

		return nil
	}

	// willing to open a tunnel for a virtual channel, for this we require party to have enough balance
	if balance.Sign() < 0 {
		return fmt.Errorf("not enough available balance, you need %s more tunnel channel through me", balance.Abs(balance).String())
	}

	if details.NextCapacity.Nano().Cmp(c.Capacity) > 0 {
		return fmt.Errorf("capacity cannot increase")
	}

	if !balanceInfo.CoinConfig.VirtualTunnelConfig.AllowTunneling {
		return fmt.Errorf("tunneling of such coin is not allowed through this node")
	}

	wantFeePercent := balanceInfo.CoinConfig.VirtualTunnelConfig.ProxyFeePercent / 100.0

	wantFeeInt := new(big.Int).Add(details.NextCapacity.Nano(), details.NextFee.Nano())

	if wantFeeInt.Cmp(balanceInfo.CoinConfig.VirtualTunnelConfig.ProxyMaxCapacity.Nano()) > 0 {
		return fmt.Errorf("too big next capacity+fee")
	}

	wantFeeInt, _ = new(big.Float).Mul(new(big.Float).SetInt(wantFeeInt), big.NewFloat(wantFeePercent)).Int(wantFeeInt)
	wantFee := tlb.MustFromNano(wantFeeInt, int(balanceInfo.CoinConfig.Decimals))
	if wantFee.Compare(balanceInfo.CoinConfig.VirtualTunnelConfig.ProxyMinFee) < 0 {
		wantFee = balanceInfo.CoinConfig.VirtualTunnelConfig.ProxyMinFee
	}

	proposedFee := new(big.Int).Sub(c.Fee, details.NextFee.Nano())
	if proposedFee.Cmp(wantFee.Nano()) < 0 {
		return fmt.Errorf("min fee to open channel is %s", wantFee.String())
	}

	return nil
}

func (c *ConditionalVirtualChannel) PrepareNext(instructionDetailsCell *cell.Cell, act payments.Action, nextDeadline time.Time) (payments.Conditional, error) {
	var details ConditionalVirtualChannelInstructionDetails
	if err := tlb.LoadFromCell(&details, instructionDetailsCell.BeginParse()); err != nil {
		return nil, err
	}

	return &ConditionalVirtualChannel{
		Action:   act,
		Key:      c.Key,
		Capacity: details.NextCapacity.Nano(),
		Fee:      details.NextFee.Nano(),
		Prepay:   big.NewInt(0),
		Deadline: nextDeadline.Unix(),
	}, nil
}

func (c *ConditionalVirtualChannel) PrepareCommit(condState *cell.Cell) (payments.Conditional, error) {
	var state VirtualChannelState
	if err := payments.LoadState(&state, condState); err != nil {
		return nil, err
	}

	if !state.Verify(c.Key) {
		return nil, fmt.Errorf("new state signature is incorrect")
	}

	if state.Amount.Cmp(c.Capacity) > 0 {
		return nil, fmt.Errorf("amount is more than capacity")
	}

	newPrepay := new(big.Int).Add(state.Amount, c.Fee)
	if c.Prepay.Cmp(newPrepay) > 0 {
		return nil, fmt.Errorf("prepay is already more than required")
	}

	return &ConditionalVirtualChannel{
		Key:      c.Key,
		Capacity: c.Capacity,
		Fee:      c.Fee,
		Prepay:   newPrepay,
		Deadline: c.Deadline,
		Action:   c.Action,
	}, nil
}

func (c *ConditionalVirtualChannel) ScoreTunnelTarget(instructionDetailsCell *cell.Cell, targetBalances map[string]*payments.BalanceInfo) (*big.Int, error) {
	if instructionDetailsCell == nil {
		return nil, fmt.Errorf("missing details cell")
	}

	var details ConditionalVirtualChannelInstructionDetails
	if err := tlb.LoadFromCell(&details, instructionDetailsCell.BeginParse()); err != nil {
		return nil, err
	}

	balance, err := payments.ResolveActionBalance(targetBalances, c.Action)
	if err != nil && !errors.Is(err, payments.ErrMissingBalanceInfo) {
		return nil, err
	}

	avb := big.NewInt(0)
	if balance != nil {
		avb = balance.Available()
	}

	amt := new(big.Int).Add(details.NextCapacity.Nano(), details.NextFee.Nano())
	return new(big.Int).Sub(avb, amt), nil
}

func (c *ConditionalVirtualChannel) ValidateState(oldStateCell, newStateCell *cell.Cell) error {
	var oldState VirtualChannelState
	if oldStateCell != nil {
		if err := payments.LoadState(&oldState, oldStateCell); err != nil {
			return err
		}
	} else {
		oldState.Amount = big.NewInt(0)
	}

	var newState VirtualChannelState
	if err := payments.LoadState(&newState, newStateCell); err != nil {
		return err
	}

	if !newState.Verify(c.Key) {
		return fmt.Errorf("new state signature is incorrect")
	}

	if oldStateCell != nil && oldState.Amount.Cmp(newState.Amount) > 0 {
		return fmt.Errorf("amount is less than known state")
	}

	if newState.Amount.Cmp(c.Capacity) > 0 {
		return fmt.Errorf("amount is more than capacity")
	}

	return nil
}

func (c *ConditionalVirtualChannel) Execute(actionState, latestCondState *cell.Cell, locked map[string]*payments.LockedDepositInfo) (*cell.Cell, error) {
	var condState VirtualChannelState
	if err := payments.LoadState(&condState, latestCondState); err != nil {
		return nil, err
	}

	if !condState.Verify(c.Key) {
		return nil, fmt.Errorf("cond state signature is incorrect")
	}

	var actState actions.StateActionSend
	if err := payments.LoadState(&actState, actionState); err != nil {
		return nil, fmt.Errorf("failed to load old state: %w", err)
	}

	needAmt := new(big.Int).Add(condState.Amount, c.Fee)
	needAmt = needAmt.Sub(needAmt, c.Prepay)

	if needAmt.Sign() < 0 {
		// prepaid more than actual, we consider diff as our earning because of user's strange behave
		needAmt.SetInt64(0)
	}

	ccs := c.Action.GetAffectedCoins()

	actState.Amount.Val = new(big.Int).Add(actState.Amount.Nano(), needAmt)

	newActionState, err := tlb.ToCell(actState)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize new action state: %w", err)
	}

	if locked != nil {
		if lk := locked[ccs[0].BalanceID]; lk != nil {
			// mark part of the rented deposit as used
			lk.Used.Add(lk.Used, needAmt)
			if lk.Amount.Cmp(lk.Used) < 0 {
				// cap it
				lk.Used.Set(lk.Amount)
			}
		}
	}

	return newActionState, nil
}

func (c *ConditionalVirtualChannel) GetLogInfo() map[string]any {
	ccs := c.Action.GetAffectedCoins()

	return map[string]any{
		"cond_type": "send",
		"key":       base64.StdEncoding.EncodeToString(c.Key),
		"capacity":  ccs[0].MustAmount(c.Capacity).String(),
		"fee":       ccs[0].MustAmount(c.Fee).String(),
		"prepaid":   ccs[0].MustAmount(c.Prepay).String(),
		"deadline":  c.Deadline,
	}
}

func SignVirtualChannelState(amount tlb.Coins, signKey ed25519.PrivateKey, to ed25519.PublicKey) (res VirtualChannelState, encrypted []byte, err error) {
	st := VirtualChannelState{
		Amount: amount.Nano(),
	}
	st.Sign(signKey)

	cll, err := st.ToCell()
	if err != nil {
		return VirtualChannelState{}, nil, fmt.Errorf("failed to serialize cell: %w", err)
	}
	data := cll.ToBOC()

	sharedKey, err := keys.SharedKey(signKey, to)
	if err != nil {
		return VirtualChannelState{}, nil, fmt.Errorf("failed to generate shared key: %w", err)
	}
	pub := signKey.Public().(ed25519.PublicKey)

	stream, err := keys.BuildSharedCipher(sharedKey, pub)
	if err != nil {
		return VirtualChannelState{}, nil, fmt.Errorf("failed to init cipher: %w", err)
	}
	// we encrypt state to be sure no one can hijack it and use in the middle of the chain
	stream.XORKeyStream(data, data)

	return st, append(pub, data...), nil
}

func ParseVirtualChannelState(data []byte, to ed25519.PrivateKey) (ed25519.PublicKey, VirtualChannelState, error) {
	if len(data) <= 32+64 || len(data) > 32+64+64 {
		return nil, VirtualChannelState{}, fmt.Errorf("incorrect len of state")
	}

	sharedKey, err := keys.SharedKey(to, data[:32])
	if err != nil {
		return nil, VirtualChannelState{}, fmt.Errorf("failed to generate shared key: %w", err)
	}

	var payload = data[32:]
	stream, err := keys.BuildSharedCipher(sharedKey, data[:32])
	if err != nil {
		return nil, VirtualChannelState{}, fmt.Errorf("failed to init cipher: %w", err)
	}
	stream.XORKeyStream(payload, payload)

	cll, err := cell.FromBOC(payload)
	if err != nil {
		return nil, VirtualChannelState{}, fmt.Errorf("failed to parse cell: %w", err)
	}

	var res VirtualChannelState
	if err = tlb.LoadFromCell(&res, cll.BeginParse()); err != nil {
		return nil, VirtualChannelState{}, fmt.Errorf("failed to parse state: %w", err)
	}

	if !res.Verify(data[:32]) {
		return nil, VirtualChannelState{}, fmt.Errorf("incorrect signature")
	}

	var key [32]byte
	copy(key[:], data[:32])

	return key[:], res, nil
}

func (c VirtualChannelState) ToCell() (*cell.Cell, error) {
	if len(c.Signature) != 64 {
		return nil, fmt.Errorf("incorrect signature size")
	}

	b := cell.BeginCell().
		MustStoreSlice(c.Signature, 512).
		MustStoreBuilder(c.serializePayload())
	return b.EndCell(), nil
}

func (c *VirtualChannelState) serializePayload() *cell.Builder {
	b := cell.BeginCell().MustStoreBigCoins(c.Amount)
	notFullBits := b.BitsUsed() % 8
	if notFullBits != 0 {
		b.MustStoreUInt(0, 8-notFullBits)
	}
	return b
}

func (c *VirtualChannelState) LoadFromCell(loader *cell.Slice) error {
	sign, err := loader.LoadSlice(512)
	if err != nil {
		return err
	}

	sz := loader.BitsLeft()
	coins, err := loader.LoadBigCoins()
	if err != nil {
		return err
	}
	sz -= loader.BitsLeft()

	notFullBits := sz % 8
	if notFullBits != 0 {
		_, err = loader.LoadUInt(8 - notFullBits)
		if err != nil {
			return err
		}
	}
	c.Signature = sign
	c.Amount = coins
	return nil
}

func (c *VirtualChannelState) Sign(key ed25519.PrivateKey) {
	cl := c.serializePayload().ToSlice()
	// we need hash of data part only, because CHEKSIGNS is used in condition
	c.Signature = ed25519.Sign(key, cl.MustLoadSlice(cl.BitsLeft()))
}

func (c *VirtualChannelState) Verify(key ed25519.PublicKey) bool {
	cl := c.serializePayload().ToSlice()
	// we need hash of data part only, because CHEKSIGNS is used in condition
	return ed25519.Verify(key, cl.MustLoadSlice(cl.BitsLeft()), c.Signature)
}

var virtualChannelUniversalStaticCode = func() *cell.Cell {
	// compiled using code:
	/*
		fun conditional_coins(targetActionsInput: dict, condInput: slice, actionHash: int, fee: int, capacity: int, prepaid: int, deadline: int, key: int): dict {
		    var (actInput, ok) = targetActionsInput.uDictGet(256, actionHash);
		    if (actInput == null || !ok) {
		        // we must always have action to execute condition
		        return targetActionsInput;
		    }

		    var sign: slice = condInput.loadBits(512);
		    assert(isSliceSignatureValid(condInput, sign, key) & (deadline >= blockchain.now())) throw 24;
		    var amount: int = condInput.loadCoins();
		    assert((amount <= capacity) & (prepaid <= amount)) throw 26;

		    var v = actInput.loadAny<FeeActionInput>();
		    v.amount += (amount - prepaid) + fee;

		    targetActionsInput.uDictSet(256, actionHash, v.toCell().beginParse());

		    return targetActionsInput;
		}
	*/

	data, err := hex.DecodeString("b5ee9c724101010100560000a853578307f40e6fa1216e92307f93b3c300e2925f08e0078308d7186603f91102f823be12b0f298fa00305203bb5312bbb0f29a04fa00fa00d70b3f5036a15003a012a0c801fa0201fa0212cb3fc9d0028307f416c626d1c9")
	if err != nil {
		panic(err.Error())
	}

	code, err := cell.FromBOC(data)
	if err != nil {
		panic(err.Error())
	}
	return code
}()
