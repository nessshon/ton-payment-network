package tonpayments

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xssnick/ton-payment-network/pkg/log"
	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/pkg/payments/actions"
	"github.com/xssnick/ton-payment-network/tonpayments/db"
	"github.com/xssnick/ton-payment-network/tonpayments/transport"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"math/big"
	"reflect"
	"time"
)

func (s *Service) updateOurStateWithAction(ctx context.Context, channel *db.Channel, action transport.Action, details any) (func(ctx context.Context) error, *payments.StateBodySigned, error) {
	var onSuccess func(ctx context.Context) error

	var idempotency bool

	state := channel.LoadSignedState()

	switch act := action.(type) {
	case transport.IncrementStatesAction:
	case transport.AddConditionalAction:
		cond := details.(payments.Conditional)

		if err := cond.ValidateOnAdd(); err != nil {
			return nil, nil, err
		}

		val := cond.Serialize()
		key := cell.BeginCell().MustStoreSlice(val.Hash(), 256).EndCell()

		_, err := channel.Our.Data.Conditionals.LoadValue(key)
		if err == nil {
			// idempotency
			idempotency = true
			break
		} else if !errors.Is(err, cell.ErrNoSuchKeyInDict) {
			return nil, nil, fmt.Errorf("failed to load our condition: %w", err)
		}

		var saveAction bool
		actId := cond.GetAction().IDCell()
		_, err = channel.Our.Data.ActionStates.LoadValue(actId)
		if err != nil {
			if !errors.Is(err, cell.ErrNoSuchKeyInDict) {
				return nil, nil, fmt.Errorf("failed to load our action state: %w", err)
			}

			if act.NewActionCode == nil {
				return nil, nil, fmt.Errorf("action code myst be set")
			}

			if err := channel.Our.Data.ActionStates.Set(actId, cond.GetAction().GetEmptyState()); err != nil {
				return nil, nil, fmt.Errorf("failed to set action state: %w", err)
			}
			saveAction = true
		}

		// TODO: virtual channels limit?

		if err := channel.Our.Data.Conditionals.Set(key, val); err != nil {
			return nil, nil, fmt.Errorf("failed to set condition: %w", err)
		}

		if saveAction {
			if err = s.SaveAction(ctx, cond.GetAction()); err != nil {
				return nil, nil, fmt.Errorf("failed to save action: %w", err)
			}
		}

		onSuccess = func(ctx context.Context) error {
			log.Info().Fields(cond.GetLogInfo()).
				Str("channel", channel.Our.Address).
				Msg("our conditional added")
			return nil
		}
	case transport.CommitVirtualAction:
		upd, err := payments.CodeToConditional(ctx, act.UpdatedConditional, s)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to parse updated conditional: %w", err)
		}

		idx, cond, err := payments.FindConditional(ctx, channel.Our.Data.Conditionals, act.ID, s)
		if err != nil {
			return nil, nil, err
		}

		condAction := cond.GetAction()
		actIdx := condAction.IDCell()

		actState, err := channel.Their.Data.ActionStates.LoadValue(actIdx)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to load action state: %w", err)
		}

		if bytes.Equal(act.UpdatedConditional.Hash(), cond.Serialize().Hash()) {
			// same
			idempotency = true
			break
		}

		updatedActState, err := cond.Commit(upd, actState.MustToCell())
		if err != nil {
			return nil, nil, fmt.Errorf("failed to commit conditional: %w", err)
		}

		if err := channel.Our.Data.Conditionals.Set(idx, cond.Serialize()); err != nil {
			return nil, nil, fmt.Errorf("failed to set condition: %w", err)
		}

		if err := channel.Our.Data.ActionStates.Set(actIdx, updatedActState); err != nil {
			return nil, nil, fmt.Errorf("failed to set condition: %w", err)
		}

		onSuccess = func(_ context.Context) error {
			log.Info().Fields(cond.GetLogInfo()).
				Str("channel", channel.Our.Address).
				Msg("conditional commit confirmed")
			return nil
		}
	case transport.RemoveConditionalAction:
		idx, vch, err := payments.FindConditional(ctx, channel.Their.Data.Conditionals, act.ID, s)
		if err != nil {
			if errors.Is(err, payments.ErrNotFound) {
				// idempotency, if not found we consider it already closed
				idempotency = true
				break
			}
			return nil, nil, err
		}

		meta, err := s.db.GetVirtualChannelMeta(ctx, vch.GetKey())
		if err != nil {
			return nil, nil, fmt.Errorf("failed to load virtual channel meta: %w", err)
		}

		if err = channel.Their.Data.Conditionals.Delete(idx); err != nil {
			return nil, nil, err
		}

		onSuccess = func(_ context.Context) error {
			if s.webhook != nil {
				if err = s.webhook.PushVirtualChannelEvent(ctx, db.VirtualChannelEventTypeRemove, meta); err != nil {
					return fmt.Errorf("failed to push virtual channel close event: %w", err)
				}
			}

			log.Info().Fields(vch.GetLogInfo()).
				Msg("their conditional successfully removed")
			return nil
		}
	case transport.ExecuteConditionalAction:
		meta := details.(*db.ConditionalMeta)

		idx, cond, err := payments.FindConditional(ctx, channel.Their.Data.Conditionals, act.ID, s)
		if err != nil {
			if errors.Is(err, payments.ErrNotFound) {
				// idempotency, if not found we consider it already closed
				idempotency = true
				break
			}
			return nil, nil, err
		}

		if err = cond.ValidateState(nil, act.State); err != nil {
			return nil, nil, fmt.Errorf("failed to validate state: %w", err)
		}

		if cond.GetDeadline().Before(time.Now()) {
			return nil, nil, fmt.Errorf("conditional has expired")
		}

		if err = channel.Their.Data.Conditionals.Delete(idx); err != nil {
			return nil, nil, err
		}

		actId := cond.GetAction().IDCell()
		actState, err := channel.Their.Data.ActionStates.LoadValue(actId)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to load action state: %w", err)
		}

		newActState, err := cond.Execute(actState.MustToCell(), act.State, channel.Their.LockedDeposits)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to execute condition: %w", err)
		}

		if err = channel.Their.Data.ActionStates.Set(actId, newActState); err != nil {
			return nil, nil, fmt.Errorf("failed to set action: %w", err)
		}

		balanceDiff, err := cond.GetAction().StatesDiff(actState.MustToCell(), newActState)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to calc balance diff: %w", err)
		}

		var senderKey []byte
		if meta.Incoming != nil {
			senderKey = meta.Incoming.SenderKey
		}

		evData := db.ChannelHistoryActionTransferInData{
			Amounts: s.formatDiff(balanceDiff),
			From:    senderKey,
		}
		jsonData, err := json.Marshal(evData)
		if err != nil {
			log.Error().Err(err).Msg("failed to marshal event data")
		}

		onSuccess = func(ctx context.Context) error {
			meta.Status = db.ConditionalStateClosed
			meta.UpdatedAt = time.Now()
			if err = s.db.UpdateVirtualChannelMeta(ctx, meta); err != nil {
				return fmt.Errorf("failed to update virtual channel meta: %w", err)
			}

			if err = s.db.CreateChannelEvent(ctx, channel, time.Now(), db.ChannelHistoryItem{
				Action: db.ChannelHistoryActionTransferIn,
				Data:   jsonData,
			}); err != nil {
				return fmt.Errorf("failed to create channel event: %w", err)
			}

			if s.webhook != nil {
				if err = s.webhook.PushVirtualChannelEvent(ctx, db.VirtualChannelEventTypeClose, meta); err != nil {
					return fmt.Errorf("failed to push virtual channel close event: %w", err)
				}
			}

			log.Info().Fields(cond.GetLogInfo()).
				Str("channel", channel.Our.Address).
				Fields(s.repackDiffForLogs(evData.Amounts)).
				Msg("their conditional executed, amounts received")
			return nil
		}
	case transport.RentCapacityAction:
		bi := hex.EncodeToString(act.BalanceID)
		cc, err := s.ResolveBalanceType(bi)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to resolve balance type %s: %w", bi, err)
		}

		amount := new(big.Int).SetBytes(act.Amount)
		till := time.Unix(int64(act.Till), 0)
		totalFee := channel.CalcDepositFee(cc, amount, till, true)

		a, err := actions.NewSendActionFromBalanceID(ctx, cc, channel.SideA().Address, channel.SideB().Address)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to create send action: %w", err)
		}

		actId := a.IDCell()
		aState, err := channel.Our.Data.ActionStates.LoadValue(actId)
		if err != nil && !errors.Is(err, cell.ErrNoSuchKeyInDict) {
			return nil, nil, fmt.Errorf("failed to load action state: %w", err)
		}

		var saveAction bool
		if aState == nil {
			saveAction = true
			aState = a.GetEmptyState().BeginParse()
		}

		var actState actions.StateActionSend
		if err = payments.LoadState(&actState, aState.MustToCell()); err != nil {
			return nil, nil, fmt.Errorf("failed to load action state: %w", err)
		}
		actState.Amount.Val = new(big.Int).Add(actState.Amount.Nano(), totalFee)

		updatedState, err := tlb.ToCell(actState)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to serialize updated action state: %w", err)
		}

		if err := channel.Our.Data.ActionStates.Set(actId, updatedState); err != nil {
			return nil, nil, fmt.Errorf("failed to set condition: %w", err)
		}

		ld := channel.Their.LockedDeposits[cc.BalanceID]
		used := big.NewInt(0)
		if ld != nil && ld.Till.After(time.Now()) {
			used = ld.Used
			if amount.Cmp(ld.Amount) <= 0 {
				return nil, nil, fmt.Errorf("amount should increase only")
			}
			if till.Before(ld.Till) {
				return nil, nil, fmt.Errorf("new till should be greater than old one")
			}
		}

		channel.Their.LockedDeposits[cc.BalanceID] = &payments.LockedDepositInfo{
			Amount: amount,
			Till:   till,
			Used:   used,
		}

		evData := db.ChannelHistoryActionRentCapData{
			BalanceID: cc.BalanceID,
			Amount:    amount.String(),
			Fee:       totalFee.String(),
			Till:      till.Unix(),
		}
		jsonData, err := json.Marshal(evData)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to marshal event data: %w", err)
		}

		if saveAction {
			if err = s.SaveAction(ctx, a); err != nil {
				return nil, nil, fmt.Errorf("failed to save action: %w", err)
			}
		}

		onSuccess = func(ctx context.Context) error {
			if err = s.db.CreateChannelEvent(ctx, channel, time.Now(), db.ChannelHistoryItem{
				Action: db.ChannelHistoryActionTheirCapacityRented,
				Data:   jsonData,
			}); err != nil {
				return fmt.Errorf("failed to create channel our cap rent event: %w", err)
			}

			log.Info().Str("balance_id", cc.BalanceID).Str("fee", cc.MustAmount(totalFee).String()).
				Str("amount", cc.MustAmount(amount).String()).
				Str("channel", channel.Our.Address).
				Time("till", till).
				Msg("capacity rent confirmed")
			return nil
		}
	case transport.CooperativeCommitAction:
		if channel.PendingCommit != nil {
			return nil, nil, fmt.Errorf("can't execute action while there is already pending commit")
		}
		if len(channel.Their.PendingOnchainTransfers) > 0 || len(channel.Our.PendingOnchainTransfers) > 0 {
			return nil, nil, fmt.Errorf("can't execute action while there are pending onchian transfers")
		}

		fees := make(map[string]*big.Int)
		var payFee *bool
		if act.WithFee {
			side := channel.WeLeft
			payFee = &side

			a, err := s.ResolveAction(ctx, act.ActionID)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to resolve action: %w", err)
			}

			fees, err = a.GetFeesPerCommitPropose()
			if err != nil {
				return nil, nil, fmt.Errorf("failed to get fees per commit propose: %w", err)
			}
		}

		jsonData, err := json.Marshal(db.ChannelHistoryActionTxRequest{
			Fees: s.formatDiff(fees),
		})
		if err != nil {
			return nil, nil, fmt.Errorf("failed to marshal event data: %w", err)
		}

		req, ourPending, theirPending, _, err := s.getCommitRequest(ctx, channel, act.ActionID, !act.WithFee, payFee)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to prepare execute action channel request: %w", err)
		}

		msg, err := tlb.ToCell(req.Signed)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to serialize pending commit message: %w", err)
		}

		if !msg.Verify(channel.Our.OnchainInfo.Key, act.MsgSignature) {
			return nil, nil, fmt.Errorf("commit state missmatch")
		}

		aid := cell.BeginCell().MustStoreSlice(act.ActionID, 256).EndCell()

		our, their := req.Signed.Action.StateA, req.Signed.Action.StateB
		if !channel.WeLeft {
			our, their = their, our
		}

		if our != nil {
			if err = channel.Our.Data.ActionStates.Set(aid, our); err != nil {
				return nil, nil, fmt.Errorf("failed to set our action state: %w", err)
			}
		}
		if their != nil {
			if err = channel.Their.Data.ActionStates.Set(aid, their); err != nil {
				return nil, nil, fmt.Errorf("failed to set their action state: %w", err)
			}
		}

		if ourPending != nil {
			channel.Our.PendingOnchainTransfers[pendingIDCommit(req.Signed.Seqno)] = ourPending
		}
		if theirPending != nil {
			channel.Their.PendingOnchainTransfers[pendingIDCommit(req.Signed.Seqno)] = theirPending
		}

		channel.PendingCommit = &db.PendingCommit{
			Seqno:   req.Signed.Seqno,
			Message: msg,
		}

		onSuccess = func(ctx context.Context) error {
			if err = s.db.CreateChannelEvent(ctx, channel, time.Now(), db.ChannelHistoryItem{
				Action: db.ChannelHistoryActionWithdrawTransactionRequest,
				Data:   jsonData,
			}); err != nil {
				return fmt.Errorf("failed to create channel our cap rent event: %w", err)
			}

			log.Info().Str("channel", channel.Our.Address).Uint64("seqno", req.Signed.Seqno).
				Str("action_id", base64.StdEncoding.EncodeToString(act.ActionID)).Msg("commit proposal accepted")
			return nil
		}
	case transport.SwapAction:
		fromCC, err := s.ResolveCoinConfig(hex.EncodeToString(act.FromBalanceID))
		if err != nil {
			return nil, nil, fmt.Errorf("failed to resolve from coin config: %w", err)
		}

		toCC, err := s.ResolveCoinConfig(hex.EncodeToString(act.ToBalanceID))
		if err != nil {
			return nil, nil, fmt.Errorf("failed to resolve to coin config: %w", err)
		}

		fromAmt := fromCC.MustAmount(new(big.Int).SetBytes(act.FromAmount))
		toAmt := toCC.MustAmount(new(big.Int).SetBytes(act.ToAmount))

		fromAct, err := actions.NewSendActionFromBalanceID(ctx, fromCC, channel.SideA().Address, channel.SideB().Address)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to create send action from 'from' balance id: %w", err)
		}

		toAct, err := actions.NewSendActionFromBalanceID(ctx, toCC, channel.SideA().Address, channel.SideB().Address)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to create send action from 'to' balance id: %w", err)
		}

		saveOurAction, saveTheirAction := false, false

		theirState, err := channel.Their.Data.ActionStates.LoadValue(toAct.IDCell())
		if err != nil {
			if !errors.Is(err, cell.ErrNoSuchKeyInDict) {
				return nil, nil, fmt.Errorf("failed to load to action state: %w", err)
			}
			saveTheirAction = true
			theirState = toAct.GetEmptyState().BeginParse()
		}
		ourState, err := channel.Our.Data.ActionStates.LoadValue(fromAct.IDCell())
		if err != nil {
			if !errors.Is(err, cell.ErrNoSuchKeyInDict) {
				return nil, nil, fmt.Errorf("failed to load from action state: %w", err)
			}
			saveOurAction = true
			ourState = fromAct.GetEmptyState().BeginParse()
		}

		newTheirState, err := toAct.AddCoins(theirState.MustToCell(), toAmt.Nano(), channel.Their.LockedDeposits)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to add coins to their action state: %w", err)
		}
		newOurState, err := fromAct.AddCoins(ourState.MustToCell(), fromAmt.Nano(), channel.Our.LockedDeposits)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to add coins to our action state: %w", err)
		}

		if err = channel.Their.Data.ActionStates.Set(toAct.IDCell(), newTheirState); err != nil {
			return nil, nil, fmt.Errorf("failed to set their action state: %w", err)
		}
		if err = channel.Our.Data.ActionStates.Set(fromAct.IDCell(), newOurState); err != nil {
			return nil, nil, fmt.Errorf("failed to set our action state: %w", err)
		}

		resolver := tmpFullResolver{[]payments.Action{fromAct, toAct}, s}

		theirBalance, err := channel.CalcBalance(ctx, true, resolver)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to calc their balance: %w", err)
		}
		if b := theirBalance[fromCC.BalanceID]; b == nil || b.Available().Sign() < 0 {
			return nil, nil, fmt.Errorf("not enough funds on their balance")
		}

		ourBalance, err := channel.CalcBalance(ctx, false, resolver)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to calc our balance: %w", err)
		}
		if b := ourBalance[toCC.BalanceID]; b == nil || b.Available().Sign() < 0 {
			return nil, nil, fmt.Errorf("not enough funds on our balance")
		}

		if saveOurAction {
			if err = s.SaveAction(ctx, fromAct); err != nil {
				return nil, nil, fmt.Errorf("failed to save action: %w", err)
			}
		}

		if saveTheirAction {
			if err = s.SaveAction(ctx, toAct); err != nil {
				return nil, nil, fmt.Errorf("failed to save action: %w", err)
			}
		}

		onSuccess = func(ctx context.Context) error {
			log.Info().Str("addr", channel.Our.Address).
				Str("from", fromAmt.String()+" "+fromCC.Symbol).Str("to", toAmt.String()+" "+toCC.Symbol).
				Msg("requested swap confirmed")

			return nil
		}
	default:
		return nil, nil, fmt.Errorf("unexpected action type: %s", reflect.TypeOf(act).String())
	}

	var ourCond, theirCond *cell.Cell
	if !channel.Our.Data.Conditionals.IsEmpty() {
		ourCond = channel.Our.Data.Conditionals.AsCell()
	}
	if !channel.Their.Data.Conditionals.IsEmpty() {
		theirCond = channel.Their.Data.Conditionals.AsCell()
	}

	var ourAct, theirAct *cell.Cell
	if !channel.Our.Data.ActionStates.IsEmpty() {
		ourAct = channel.Our.Data.ActionStates.AsCell()
	}
	if !channel.Their.Data.ActionStates.IsEmpty() {
		theirAct = channel.Their.Data.ActionStates.AsCell()
	}

	if !idempotency {
		state.Body.Seqno++

		our, their := &state.Body.A, &state.Body.B
		if !channel.WeLeft {
			our, their = their, our
		}

		if ourCond != nil {
			our.ConditionalsHash = ourCond.Hash()
		} else {
			our.ConditionalsHash = make([]byte, 32)
		}
		if theirCond != nil {
			their.ConditionalsHash = theirCond.Hash()
		} else {
			their.ConditionalsHash = make([]byte, 32)
		}

		if ourAct != nil {
			our.ActionStatesHash = ourAct.Hash()
		} else {
			our.ActionStatesHash = make([]byte, 32)
		}
		if theirAct != nil {
			their.ActionStatesHash = theirAct.Hash()
		} else {
			their.ActionStatesHash = make([]byte, 32)
		}
	}

	toSign, err := tlb.ToCell(state.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to serialize state for signing: %w", err)
	}
	if channel.WeLeft {
		state.SignatureA = payments.Signature{Value: toSign.Sign(s.key)}
		state.SignatureB = payments.Signature{Value: make([]byte, 64)}
	} else {
		state.SignatureA = payments.Signature{Value: make([]byte, 64)}
		state.SignatureB = payments.Signature{Value: toSign.Sign(s.key)}
	}

	return onSuccess, state, nil
}

func pendingIDCommit(seqno uint64) string {
	return fmt.Sprintf("commit_%d", seqno)
}

func pendingIDWallet(seqno uint32) string {
	return fmt.Sprintf("wallet_%d", seqno)
}
