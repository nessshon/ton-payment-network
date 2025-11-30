//go:build !(js && wasm)

package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/ton"
	"github.com/xssnick/tonutils-go/ton/jetton"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"math/big"
	"time"
)

type TON struct {
	api ton.APIClientWrapped
}

func NewTON(api ton.APIClientWrapped) *TON {
	return &TON{
		api: api,
	}
}

func (t *TON) GetAccount(ctx context.Context, addr *address.Address, blockAfter time.Time) (*Account, error) {
	block, err := t.api.GetMasterchainInfo(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get current masterchain info: %w", err)
	}

	hdr, err := t.api.GetBlockHeader(ctx, block)
	if err != nil {
		return nil, fmt.Errorf("failed to get block header: %w", err)
	}

	if int64(hdr.GenUtime) < blockAfter.Unix() {
		return nil, fmt.Errorf("current block is before required timestamp")
	}

	acc, err := t.api.GetAccount(ctx, block, addr)
	if err != nil {
		return nil, fmt.Errorf("failed to get account: %w", err)
	}

	a := &Account{
		Address:    addr,
		IsActive:   true,
		Code:       acc.Code,
		Data:       acc.Data,
		LastTxLT:   acc.LastTxLT,
		LastTxHash: acc.LastTxHash,
	}

	if acc.State != nil {
		a.HasState = true
		a.Balance = acc.State.Balance
		a.ExtraCurrencies = acc.State.ExtraCurrencies
	}

	if !acc.IsActive || !acc.State.IsValid || acc.State.Status != tlb.AccountStatusActive {
		a.IsActive = false
	}

	return a, nil
}

func (t *TON) GetJettonWalletAddress(ctx context.Context, root, addr *address.Address) (*address.Address, error) {
	jw, err := jetton.NewJettonMasterClient(t.api, root).GetJettonWallet(ctx, addr)
	if err != nil {
		return nil, fmt.Errorf("failed to get jetton wallet: %w", err)
	}
	return jw.Address(), nil
}

func (t *TON) GetJettonBalance(ctx context.Context, root, addr *address.Address, blockAfter time.Time) (*big.Int, error) {
	jw, err := jetton.NewJettonMasterClient(t.api, root).GetJettonWallet(ctx, addr)
	if err != nil {
		return nil, fmt.Errorf("failed to get jetton wallet: %w", err)
	}

	block, err := t.api.GetMasterchainInfo(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get current masterchain info: %w", err)
	}

	hdr, err := t.api.GetBlockHeader(ctx, block)
	if err != nil {
		return nil, fmt.Errorf("failed to get block header: %w", err)
	}

	if int64(hdr.GenUtime) < blockAfter.Unix() {
		return nil, fmt.Errorf("current block is before required timestamp")
	}

	balance, err := jw.GetBalanceAtBlock(ctx, block)
	if err != nil {
		return nil, fmt.Errorf("failed to get jetton balance: %w", err)
	}
	return balance, nil
}

func (t *TON) GetLastTransaction(ctx context.Context, addr *address.Address, blockAfter time.Time) (*Transaction, *Account, error) {
	acc, err := t.GetAccount(ctx, addr, blockAfter)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get account: %w", err)
	}

	if acc.LastTxLT == 0 {
		return nil, acc, nil
	}

	txList, err := t.api.ListTransactions(ctx, addr, 1, acc.LastTxLT, acc.LastTxHash)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get transactions: %w", err)
	}

	if len(txList) == 0 {
		return nil, nil, fmt.Errorf("failed to get transactions: no transactions returned")
	}

	tx := txList[0]
	if !bytes.Equal(tx.Hash, acc.LastTxHash) || tx.LT != acc.LastTxLT {
		return nil, nil, fmt.Errorf("failed to get transactions: last tx mismatch")
	}

	res, err := ConvertTx(tx)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to convert tx: %w", err)
	}

	return res, acc, nil
}

func (t *TON) GetTransactionByInMsgHash(ctx context.Context, addr *address.Address, msgHash []byte, after time.Time) (*Transaction, error) {
	tx, err := t.api.FindLastTransactionByInMsgHashAfterTime(ctx, addr, msgHash, after)
	if err != nil {
		if errors.Is(err, ton.ErrTxWasNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to find last transaction: %w", err)
	}

	res, err := ConvertTx(tx)
	if err != nil {
		return nil, fmt.Errorf("failed to convert tx: %w", err)
	}

	return res, nil
}

func (t *TON) GetTransactionsList(ctx context.Context, addr *address.Address, lt uint64, hash []byte) ([]*Transaction, error) {
	txList, err := t.api.ListTransactions(ctx, addr, 5, lt, hash)
	if err != nil {
		return nil, fmt.Errorf("failed to get transactions: %w", err)
	}

	if len(txList) == 0 {
		return nil, nil
	}

	var list []*Transaction
	for _, tx := range txList {
		res, err := ConvertTx(tx)
		if err != nil {
			return nil, fmt.Errorf("failed to convert tx: %w", err)
		}

		list = append(list, res)
	}

	return list, nil
}

func (t *TON) SendWaitExternalMessage(ctx context.Context, to *address.Address, body *cell.Cell) ([]byte, error) {
	ext := &tlb.ExternalMessage{
		DstAddr: to,
		Body:    body,
	}

	_, _, _, err := t.api.SendExternalMessageWaitTransaction(ctx, &tlb.ExternalMessage{
		DstAddr: to,
		Body:    body,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to send wait external message: %w", err)
	}

	return ext.NormalizedHash(), err
}

func ConvertTx(tx *tlb.Transaction) (*Transaction, error) {
	res := &Transaction{
		Hash:       tx.Hash,
		PrevTxHash: tx.PrevTxHash,
		PrevTxLT:   tx.PrevTxLT,
		LT:         tx.LT,
		At:         int64(tx.Now),
	}

	if desc, ok := tx.Description.(tlb.TransactionDescriptionOrdinary); ok {
		if comp, ok := desc.ComputePhase.Phase.(tlb.ComputePhaseVM); ok && comp.Success {
			res.Success = true
		}
	}

	toMsgInfo := func(msg *tlb.Message) (MsgInfo, error) {
		switch msg.MsgType {
		case tlb.MsgTypeExternalIn:
			m := msg.AsExternalIn()
			return MsgInfo{
				Type:    tlb.MsgTypeExternalIn,
				From:    m.SrcAddr.String(),
				To:      m.DstAddr.String(),
				MsgHash: m.NormalizedHash(),
				Body:    m.Body,
			}, nil
		case tlb.MsgTypeInternal:
			m := msg.AsInternal()
			c, err := tlb.ToCell(m)
			if err != nil {
				return MsgInfo{}, fmt.Errorf("failed to convert cell: %w", err)
			}

			return MsgInfo{
				Type:    tlb.MsgTypeInternal,
				From:    m.SrcAddr.String(),
				To:      m.DstAddr.String(),
				MsgHash: c.Hash(),
				Body:    m.Body,
			}, nil
		case tlb.MsgTypeExternalOut:
			m := msg.AsExternalOut()
			c, err := tlb.ToCell(m)
			if err != nil {
				return MsgInfo{}, fmt.Errorf("failed to convert cell: %w", err)
			}

			return MsgInfo{
				Type:    tlb.MsgTypeExternalOut,
				From:    m.SrcAddr.String(),
				To:      m.DstAddr.String(),
				MsgHash: c.Hash(),
				Body:    m.Body,
			}, nil
		default:
			return MsgInfo{}, fmt.Errorf("unknown msg type: %s", msg.MsgType)
		}
	}

	var err error
	res.In, err = toMsgInfo(tx.IO.In)
	if err != nil {
		return nil, fmt.Errorf("failed to convert in: %w", err)
	}

	if tx.IO.Out != nil {
		outList, err := tx.IO.Out.ToSlice()
		if err != nil {
			return nil, fmt.Errorf("failed to convert out to slice: %w", err)
		}

		for _, msg := range outList {
			out, err := toMsgInfo(&msg)
			if err != nil {
				return nil, fmt.Errorf("failed to convert in: %w", err)
			}

			res.Out = append(res.Out, out)
		}
	}

	return res, nil
}
