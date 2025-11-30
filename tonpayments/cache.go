package tonpayments

import (
	"context"
	"fmt"
	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/tonutils-go/address"
	"math/big"
	"time"
)

type JettonClientCacher struct {
	root *address.Address
	s    *Service
}

type ActionInfoCached struct {
	act payments.Action
	svc *Service
}

func (s *Service) SaveAction(ctx context.Context, act payments.Action) error {
	cl := act.Serialize()

	s.cacheMx.RLock()
	a := s.actionsCache[string(cl.Hash())]
	s.cacheMx.RUnlock()
	if a != nil {
		return nil
	}

	if err := s.db.CreateActionCode(ctx, cl); err != nil {
		return err
	}

	s.cacheMx.Lock()
	s.actionsCache[string(cl.Hash())] = act
	s.cacheMx.Unlock()

	return nil
}

func (s *Service) ResolveAction(ctx context.Context, id []byte) (payments.Action, error) {
	s.cacheMx.RLock()
	b := s.actionsCache[string(id)]
	s.cacheMx.RUnlock()

	if b == nil {
		cl, err := s.db.GetActionCode(ctx, id)
		if err != nil {
			return nil, err
		}

		b, err = payments.CodeToAction(ctx, cl, s)
		if err != nil {
			return nil, err
		}

		s.cacheMx.Lock()
		s.actionsCache[string(cl.Hash())] = b
		s.cacheMx.Unlock()
	}
	return b, nil
}

func (s *Service) ResolveBalanceType(id string) (*payments.CoinConfig, error) {
	b := s.knownBalanceTypes[id]
	if b == nil {
		return nil, fmt.Errorf("unknown balance type: %s", id)
	}
	return b, nil
}

func (s *Service) NewJettonCacher(root *address.Address) *JettonClientCacher {
	return &JettonClientCacher{
		root: root,
		s:    s,
	}
}

func (j *JettonClientCacher) GetWalletAddress(ctx context.Context, addr *address.Address) (*address.Address, error) {
	// TODO: cache?
	return j.s.ton.GetJettonWalletAddress(ctx, j.root, addr)
}

func (j *JettonClientCacher) GetBalance(ctx context.Context, addr *address.Address, blockAfter time.Time) (*big.Int, error) {
	// TODO: cache?
	return j.s.ton.GetJettonBalance(ctx, j.root, addr, blockAfter)
}

func (j *JettonClientCacher) GetRootAddress() *address.Address {
	return j.root
}
