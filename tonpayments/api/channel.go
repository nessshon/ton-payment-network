package api

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/tonpayments/db"
	"github.com/xssnick/tonutils-go/address"
	"math/big"
	"net/http"
	"time"
)

type Side struct {
	Key                  string            `json:"key"`
	Balances             map[string]string `json:"balances"`
	LatestProcessedLT    uint64            `json:"processed_lt"`
	LatestCommittedSeqno uint64            `json:"committed_seqno"`
	LatestWalletSeqno    uint32            `json:"wallet_seqno"`
}

type OnchainChannel struct {
	ID               string `json:"id"`
	Address          string `json:"address"`
	TheirAddress     string `json:"their_address"`
	AcceptingActions bool   `json:"accepting_actions"`
	Status           string `json:"status"`
	WeLeft           bool   `json:"we_left"`
	Our              Side   `json:"our"`
	Their            Side   `json:"their"`

	InitAt    time.Time `json:"init_at"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Server) handleChannelOpen(w http.ResponseWriter, r *http.Request) {
	type request struct {
		WithNode string `json:"with_node"`
	}
	type response struct {
		Address string `json:"address"`
	}

	if r.Method != "POST" {
		writeErr(w, 400, "incorrect request method")
		return
	}

	var req request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "incorrect request body: "+err.Error())
		return
	}

	key, err := parseKey(req.WithNode)
	if err != nil {
		writeErr(w, 400, "incorrect node key format: "+err.Error())
		return
	}

	addr, err := s.svc.OpenChannelWithNode(r.Context(), key)
	if err != nil {
		writeErr(w, 500, "failed to open channel: "+err.Error())
		return
	}

	writeResp(w, response{
		Address: addr.String(),
	})
}

func (s *Server) handleTopup(w http.ResponseWriter, r *http.Request) {
	type request struct {
		Address  string `json:"address"`
		Amount   string `json:"amount_nano"`
		Currency string `json:"currency"`
	}

	if r.Method != "POST" {
		writeErr(w, 400, "incorrect request method")
		return
	}

	var req request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "incorrect request body: "+err.Error())
		return
	}

	amt, _ := new(big.Int).SetString(req.Amount, 10)
	if amt == nil || amt.Sign() <= 0 || amt.BitLen() > 256 {
		writeErr(w, 400, "incorrect amount format")
		return
	}

	addr, err := address.ParseAddr(req.Address)
	if err != nil {
		writeErr(w, 400, "incorrect channel address format: "+err.Error())
		return
	}

	ch, err := s.svc.GetActiveChannel(r.Context(), addr.String())
	if err != nil {
		writeErr(w, 500, "failed to get channel: "+err.Error())
		return
	}

	cc, err := s.svc.ResolveCoinConfigBySymbol(req.Currency)
	if err != nil {
		writeErr(w, 500, "failed to resolve coin config: "+err.Error())
		return
	}

	if err = s.svc.TopupChannel(r.Context(), ch, cc.BalanceID, cc.MustAmount(amt), false); err != nil {
		writeErr(w, 500, "failed to topup channel: "+err.Error())
		return
	}

	writeSuccess(w)
}

func (s *Server) handleWithdraw(w http.ResponseWriter, r *http.Request) {
	type request struct {
		Address  string `json:"address"`
		To       string `json:"to"`
		Amount   string `json:"amount_nano"`
		Currency string `json:"currency"`
	}

	if r.Method != "POST" {
		writeErr(w, 400, "incorrect request method")
		return
	}

	var req request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "incorrect request body: "+err.Error())
		return
	}

	amt, _ := new(big.Int).SetString(req.Amount, 10)
	if amt == nil || amt.Sign() <= 0 || amt.BitLen() > 256 {
		writeErr(w, 400, "incorrect amount format")
		return
	}

	addr, err := address.ParseAddr(req.Address)
	if err != nil {
		writeErr(w, 400, "incorrect channel address format: "+err.Error())
		return
	}

	to, err := address.ParseAddr(req.To)
	if err != nil {
		writeErr(w, 400, "incorrect to address format: "+err.Error())
		return
	}

	cc, err := s.svc.ResolveCoinConfigBySymbol(req.Currency)
	if err != nil {
		writeErr(w, 500, "failed to resolve coin config: "+err.Error())
		return
	}

	if err = s.svc.RequestWithdrawToAddr(r.Context(), addr.Bounce(true).String(), to, cc, amt); err != nil {
		writeErr(w, 500, "failed to request withdraw channel: "+err.Error())
		return
	}

	writeSuccess(w)
}

func (s *Server) handleChannelClose(w http.ResponseWriter, r *http.Request) {
	type request struct {
		Address string `json:"address"`
		Force   bool   `json:"force"`
	}

	if r.Method != "POST" {
		writeErr(w, 400, "incorrect request method")
		return
	}

	var req request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "incorrect request body: "+err.Error())
		return
	}

	addr, err := address.ParseAddr(req.Address)
	if err != nil {
		writeErr(w, 400, "incorrect amount format: "+err.Error())
		return
	}

	if req.Force {
		if err = s.svc.RequestUncooperativeClose(r.Context(), addr.String()); err != nil {
			writeErr(w, 500, "failed to uncooperative close channel: "+err.Error())
			return
		}
	} else {
		if err = s.svc.RequestCooperativeClose(r.Context(), addr.String()); err != nil {
			writeErr(w, 500, "failed to cooperative close channel: "+err.Error())
			return
		}
	}

	writeSuccess(w)
}

func (s *Server) handleChannelsList(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeErr(w, 400, "incorrect request method")
		return
	}

	var err error
	var key ed25519.PublicKey
	var status = db.ChannelStateAny

	if qKey := r.URL.Query().Get("key"); qKey != "" {
		key, err = parseKey(qKey)
		if err != nil {
			writeErr(w, 400, "incorrect node key format: "+err.Error())
			return
		}
	}

	if q := r.URL.Query().Get("status"); q != "" {
		switch q {
		case "active":
			status = db.ChannelStateActive
		case "closing":
			status = db.ChannelStateClosing
		case "inactive":
			status = db.ChannelStateInactive
		case "any":
		default:
			writeErr(w, 400, "unknown status: "+q)
			return
		}
	}

	list, err := s.svc.ListChannels(r.Context(), key, status)
	if err != nil {
		writeErr(w, 500, "failed to list channels: "+err.Error())
		return
	}

	res := make([]OnchainChannel, 0, len(list))
	for i, channel := range list {
		v, err := s.convertChannel(r.Context(), channel)
		if err != nil {
			writeErr(w, 500, "failed to convert channel "+fmt.Sprint(i)+": "+err.Error())
			return
		}
		res = append(res, v)
	}

	writeResp(w, res)
}

func (s *Server) handleChannelGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeErr(w, 400, "incorrect request method")
		return
	}

	var err error
	var addr *address.Address
	if q := r.URL.Query().Get("address"); q != "" {
		addr, err = address.ParseAddr(q)
		if err != nil {
			writeErr(w, 400, "incorrect address format: "+err.Error())
			return
		}
	} else {
		writeErr(w, 400, "channel address is not passed")
		return
	}

	ch, err := s.svc.GetChannel(r.Context(), addr.String())
	if err != nil {
		writeErr(w, 500, "failed to get channel: "+err.Error())
		return
	}

	res, err := s.convertChannel(r.Context(), ch)
	if err != nil {
		writeErr(w, 500, "failed to convert channel: "+err.Error())
		return
	}

	writeResp(w, res)
}

func (s *Server) convertChannel(ctx context.Context, c *db.Channel) (OnchainChannel, error) {
	var status string
	switch c.Status {
	case db.ChannelStateActive:
		status = "active"
	case db.ChannelStateClosing:
		status = "closing"
	default:
		status = "inactive"
	}

	convBalances := func(bls map[string]*payments.BalanceInfo) map[string]string {
		m := map[string]string{}
		for _, info := range bls {
			m[info.CoinConfig.Symbol] = info.CoinConfig.MustAmount(info.Available()).String()
		}
		return m
	}

	theirBalance, err := c.CalcBalance(ctx, true, s.svc)
	if err != nil {
		return OnchainChannel{}, fmt.Errorf("failed to calc balance: %w", err)
	}
	ourBalance, err := c.CalcBalance(ctx, false, s.svc)
	if err != nil {
		return OnchainChannel{}, fmt.Errorf("failed to calc balance: %w", err)
	}

	return OnchainChannel{
		ID:               base64.StdEncoding.EncodeToString(c.ID),
		Address:          c.Our.Address,
		TheirAddress:     c.Their.Address,
		AcceptingActions: c.AcceptingActions,
		Status:           status,
		WeLeft:           c.WeLeft,
		Our: Side{
			Key:                  base64.StdEncoding.EncodeToString(c.Our.OnchainInfo.Key),
			Balances:             convBalances(ourBalance),
			LatestProcessedLT:    c.Our.LatestProcessedLT,
			LatestCommittedSeqno: c.Our.LatestCommitedSeqno,
			LatestWalletSeqno:    c.Our.LatestWalletSeqno,
		},
		Their: Side{
			Key:                  base64.StdEncoding.EncodeToString(c.Their.OnchainInfo.Key),
			Balances:             convBalances(theirBalance),
			LatestProcessedLT:    c.Their.LatestProcessedLT,
			LatestCommittedSeqno: c.Their.LatestCommitedSeqno,
			LatestWalletSeqno:    c.Their.LatestWalletSeqno,
		},
		InitAt:    c.InitAt,
		CreatedAt: c.CreatedAt,
	}, nil
}

func (s *Server) PushChannelEvent(ctx context.Context, ch *db.Channel) error {
	res, err := s.convertChannel(ctx, ch)
	if err != nil {
		return fmt.Errorf("failed to convert channel: %w", err)
	}

	if err = s.queue.CreateTask(ctx, WebhooksTaskPool, "onchain-channel-event", "events",
		ch.Our.Address+"-"+fmt.Sprint(res.Our.LatestProcessedLT),
		res, nil, nil,
	); err != nil {
		return fmt.Errorf("failed to create webhook task: %w", err)
	}

	return nil
}
