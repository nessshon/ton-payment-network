package api

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/tonpayments/db"
	"github.com/xssnick/ton-payment-network/tonpayments/transport"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"net/http"
	"time"
)

type NodeChain struct {
	Key                string `json:"key"`
	Fee                string `json:"fee"`
	DeadlineGapSeconds int64  `json:"deadline_gap_seconds"`
}

type ConditionalSide struct {
	ChannelAddress          string     `json:"channel_address"`
	Code                    *cell.Cell `json:"code"`
	UncooperativeDeadlineAt time.Time  `json:"uncooperative_deadline_at"`
	SafeDeadlineAt          time.Time  `json:"safe_deadline_at"`
}

type Conditional struct {
	Key       string           `json:"key"`
	Status    string           `json:"status"`
	Resolve   *cell.Cell       `json:"resolve"`
	Outgoing  *ConditionalSide `json:"outgoing"`
	Incoming  *ConditionalSide `json:"incoming"`
	CreatedAt time.Time        `json:"created_at"`
	UpdatedAt time.Time        `json:"updated_at"`
}

func (s *Server) handleVirtualGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeErr(w, 400, "incorrect request method")
		return
	}

	var err error
	var key ed25519.PublicKey
	if q := r.URL.Query().Get("key"); q != "" {
		key, err = parseKey(q)
		if err != nil {
			writeErr(w, 400, "incorrect key format: "+err.Error())
			return
		}
	} else {
		writeErr(w, 400, "channel address is not passed")
	}

	meta, err := s.svc.GetVirtualChannelMeta(r.Context(), key)
	if err != nil {
		writeErr(w, 500, "failed to get virtual channel meta: "+err.Error())
		return
	}

	res, err := s.getVirtual(r.Context(), meta)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}

	writeResp(w, res)
}

func (s *Server) handleVirtualList(w http.ResponseWriter, r *http.Request) {
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
	var our, their = make([]*Conditional, 0), make([]*Conditional, 0)

	allTheir, err := ch.Their.Data.Conditionals.LoadAll()
	if err != nil {
		writeErr(w, 500, "failed to load their conditionals: "+err.Error())
		return
	}

	for _, kv := range allTheir {
		vch, err := payments.CodeToConditional(r.Context(), kv.Value.MustToCell(), s.svc)
		if err != nil {
			continue
		}

		meta, err := s.svc.GetVirtualChannelMeta(r.Context(), vch.GetKey())
		if err != nil {
			writeErr(w, 500, "failed to get virtual channel meta: "+err.Error())
			return
		}

		res, err := s.getVirtual(r.Context(), meta)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		their = append(their, res)
	}

	allOur, err := ch.Our.Data.Conditionals.LoadAll()
	if err != nil {
		writeErr(w, 500, "failed to load our conditionals: "+err.Error())
		return
	}

	for _, kv := range allOur {
		vch, err := payments.CodeToConditional(r.Context(), kv.Value.MustToCell(), s.svc)
		if err != nil {
			continue
		}

		meta, err := s.svc.GetVirtualChannelMeta(r.Context(), vch.GetKey())
		if err != nil {
			writeErr(w, 500, "failed to get virtual channel meta: "+err.Error())
			return
		}

		res, err := s.getVirtual(r.Context(), meta)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		our = append(our, res)
	}

	writeResp(w, struct {
		Their []*Conditional `json:"their"`
		Our   []*Conditional `json:"our"`
	}{their, our})
}

func (s *Server) getVirtual(ctx context.Context, meta *db.ConditionalMeta) (*Conditional, error) {
	var status string
	switch meta.Status {
	case db.ConditionalStateActive:
		status = "active"
	case db.ConditionalStateClosed:
		status = "closed"
	case db.ConditionalStateRemoved:
		status = "removed"
	case db.ConditionalStateWantRemove:
		status = "want_remove"
	case db.ConditionalStateWantClose:
		status = "want_close"
	default:
		return nil, fmt.Errorf("unknown virtual channel %s state: %d", base64.StdEncoding.EncodeToString(meta.Key), meta.Status)
	}

	res := &Conditional{
		Key:       base64.StdEncoding.EncodeToString(meta.Key),
		Status:    status,
		Resolve:   meta.LastKnownResolve,
		CreatedAt: meta.CreatedAt,
		UpdatedAt: meta.UpdatedAt,
	}

	if meta.Status != db.ConditionalStateClosed && meta.Status != db.ConditionalStateRemoved {
		if meta.Incoming != nil {
			res.Incoming = &ConditionalSide{
				ChannelAddress:          meta.Incoming.ChannelAddress,
				Code:                    meta.Incoming.Conditional,
				UncooperativeDeadlineAt: meta.Incoming.UncooperativeDeadline,
				SafeDeadlineAt:          meta.Incoming.SafeDeadline,
			}
		}

		if meta.Outgoing != nil {
			res.Outgoing = &ConditionalSide{
				ChannelAddress:          meta.Outgoing.ChannelAddress,
				Code:                    meta.Outgoing.Conditional,
				UncooperativeDeadlineAt: meta.Outgoing.UncooperativeDeadline,
				SafeDeadlineAt:          meta.Outgoing.SafeDeadline,
			}
		}
	}

	return res, nil
}

func (s *Server) handleVirtualState(w http.ResponseWriter, r *http.Request) {
	type request struct {
		Key   string     `json:"key"`
		State *cell.Cell `json:"state"`
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

	key, err := parseKey(req.Key)
	if err != nil {
		writeErr(w, 400, "failed to parse key: "+err.Error())
		return
	}

	if err = s.svc.AddConditionalResolve(r.Context(), key, req.State); err != nil && !errors.Is(err, payments.ErrNewerConditionalStateIsKnown) {
		writeErr(w, 500, "failed to add virtual channel state: "+err.Error())
		return
	}

	writeSuccess(w)
}

func (s *Server) handleVirtualClose(w http.ResponseWriter, r *http.Request) {
	type request struct {
		Key   string     `json:"key"`
		State *cell.Cell `json:"state"`
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

	key, err := parseKey(req.Key)
	if err != nil {
		writeErr(w, 400, "failed to parse key: "+err.Error())
		return
	}

	if err = s.svc.AddConditionalResolve(r.Context(), key, req.State); err != nil && !errors.Is(err, payments.ErrNewerConditionalStateIsKnown) {
		writeErr(w, 500, "failed to add virtual channel state: "+err.Error())
		return
	}

	if err = s.svc.CloseConditional(r.Context(), key); err != nil {
		writeErr(w, 500, "failed to close virtual channel: "+err.Error())
		return
	}

	writeSuccess(w)
}

func (s *Server) handleVirtualOpen(w http.ResponseWriter, r *http.Request) {
	type request struct {
		TTLSeconds int64       `json:"ttl_seconds"`
		Capacity   string      `json:"capacity"`
		Currency   string      `json:"currency"`
		NodesChain []NodeChain `json:"nodes_chain"`
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

	if len(req.NodesChain) == 0 {
		writeErr(w, 400, "no nodes passed")
		return
	}

	deadline := time.Now().Add(time.Duration(req.TTLSeconds) * time.Second)

	deadlines := make([]time.Time, len(req.NodesChain))
	for i := range req.NodesChain {
		deadlines[i] = deadline
		deadline = deadline.Add(time.Duration(req.NodesChain[i].DeadlineGapSeconds) * time.Second)
	}

	cc, err := s.svc.ResolveCoinConfigBySymbol(req.Currency)
	if err != nil {
		writeErr(w, 400, "failed to resolve coin config"+err.Error())
		return
	}

	capacity, err := tlb.FromDecimal(req.Capacity, int(cc.Decimals))
	if err != nil {
		writeErr(w, 400, "failed to parse capacity: "+err.Error())
		return
	}

	var with []byte
	var tunChain []transport.TunnelChainPart
	for i, node := range req.NodesChain {
		key, err := parseKey(node.Key)
		if err != nil {
			writeErr(w, 400, "failed to parse node "+fmt.Sprint(i)+" key: "+err.Error())
			return
		}

		fee, err := tlb.FromDecimal(node.Fee, int(cc.Decimals))
		if err != nil {
			writeErr(w, 400, "failed to parse node "+fmt.Sprint(i)+" fee: "+err.Error())
			return
		}

		if with == nil {
			with = key
		}

		tunChain = append(tunChain, transport.TunnelChainPart{
			Target:   key,
			Capacity: capacity.Nano(),
			Fee:      fee.Nano(),
			Deadline: deadlines[i],
		})
	}

	_, vPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		writeErr(w, 500, "failed to generate key: "+err.Error())
		return
	}

	firstInstructionKey, tun, err := transport.GenerateTunnel(vPriv, tunChain, 5, true, s.svc.GetPrivateKey(), cc)
	if err != nil {
		writeErr(w, 500, "failed to generate tunnel: "+err.Error())
		return
	}

	err = s.svc.CreateSendConditional(r.Context(), firstInstructionKey, vPriv, tunChain[0], tunChain[len(tunChain)-1], tun, cc)
	if err != nil {
		writeErr(w, 403, "failed to request virtual channel open: "+err.Error())
		return
	}

	writeResp(w, struct {
		PublicKey      string    `json:"public_key"`
		PrivateKeySeed string    `json:"private_key_seed"`
		Status         string    `json:"status"`
		Deadline       time.Time `json:"deadline"`
	}{
		PublicKey:      base64.StdEncoding.EncodeToString(vPriv.Public().(ed25519.PublicKey)),
		PrivateKeySeed: base64.StdEncoding.EncodeToString(vPriv.Seed()),
		Status:         "pending",
		Deadline:       deadlines[len(req.NodesChain)-1],
	})
}

func (s *Server) handleVirtualTransfer(w http.ResponseWriter, r *http.Request) {
	type request struct {
		TTLSeconds int64       `json:"ttl_seconds"`
		Amount     string      `json:"amount"`
		Currency   string      `json:"currency"`
		NodesChain []NodeChain `json:"nodes_chain"`
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

	if len(req.NodesChain) == 0 {
		writeErr(w, 400, "no nodes passed")
		return
	}

	cc, err := s.svc.ResolveCoinConfigBySymbol(req.Currency)
	if err != nil {
		writeErr(w, 400, "failed to resolve coin config"+err.Error())
		return
	}

	deadline := time.Now().Add(time.Duration(req.TTLSeconds) * time.Second)

	deadlines := make([]time.Time, len(req.NodesChain))
	for i := range req.NodesChain {
		deadlines[i] = deadline
		deadline = deadline.Add(time.Duration(req.NodesChain[i].DeadlineGapSeconds) * time.Second)
	}

	capacity, err := tlb.FromDecimal(req.Amount, int(cc.Decimals))
	if err != nil {
		writeErr(w, 400, "failed to parse capacity: "+err.Error())
		return
	}

	var with []byte
	var tunChain []transport.TunnelChainPart
	for i, node := range req.NodesChain {
		key, err := parseKey(node.Key)
		if err != nil {
			writeErr(w, 400, "failed to parse node "+fmt.Sprint(i)+" key: "+err.Error())
			return
		}

		fee, err := tlb.FromDecimal(node.Fee, int(cc.Decimals))
		if err != nil {
			writeErr(w, 400, "failed to parse node "+fmt.Sprint(i)+" fee: "+err.Error())
			return
		}

		if with == nil {
			with = key
		}

		tunChain = append(tunChain, transport.TunnelChainPart{
			Target:   key,
			Capacity: capacity.Nano(),
			Fee:      fee.Nano(),
			Deadline: deadlines[i],
		})
	}

	_, vPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		writeErr(w, 500, "failed to generate key: "+err.Error())
		return
	}

	firstInstructionKey, tun, err := transport.GenerateTunnel(vPriv, tunChain, 5, true, s.svc.GetPrivateKey(), cc)
	if err != nil {
		writeErr(w, 500, "failed to generate tunnel: "+err.Error())
		return
	}

	err = s.svc.CreateSendConditional(r.Context(), firstInstructionKey, vPriv, tunChain[0], tunChain[len(tunChain)-1], tun, cc)
	if err != nil {
		writeErr(w, 403, "failed to request virtual channel open: "+err.Error())
		return
	}

	writeResp(w, struct {
		Status   string    `json:"status"`
		Deadline time.Time `json:"deadline"`
	}{
		Status:   "pending",
		Deadline: deadlines[len(req.NodesChain)-1],
	})
}

func (s *Server) PushVirtualChannelEvent(ctx context.Context, event db.VirtualChannelEventType, meta *db.ConditionalMeta) error {
	vc, err := s.getVirtual(ctx, meta)
	if err != nil {
		return fmt.Errorf("failed to get virtual channel: %w", err)
	}

	if err := s.queue.CreateTask(ctx, WebhooksTaskPool, "virtual-channel-event", "events",
		vc.Key+"-"+string(event)+"-"+fmt.Sprint(meta.UpdatedAt),
		db.VirtualChannelEvent{
			EventType:      event,
			VirtualChannel: vc,
		}, nil, nil,
	); err != nil {
		return fmt.Errorf("failed to create virtual-channel-event task: %w", err)
	}
	return nil
}
