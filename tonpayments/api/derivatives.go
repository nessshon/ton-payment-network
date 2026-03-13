package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"time"
)

type openReq struct {
	Channel  string `json:"channel"`
	Symbol   string `json:"symbol"`
	Side     string `json:"side"` // long/short
	Leverage int    `json:"leverage"`
	Amount   string `json:"amount"` // collateral
	Type     string `json:"type"`   // limit/market
	Price    string `json:"price"`  // optional for market
}

type closeReq struct {
	Channel string `json:"channel"`
	ID      string `json:"id"`
	Symbol  string `json:"symbol"`
	Type    string `json:"type"` // market/cancel
}

type hedgedReq struct {
	OrderID string `json:"order_id"`
	Hedged  bool   `json:"hedged"`
}

const maxDerivativesRequestBodyBytes = 16 * 1024

func (s *Server) handleDerivativesPosition(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, 405, "method not allowed")
		return
	}

	q := r.URL.Query()
	channel := q.Get("channel")
	symbol := q.Get("symbol")
	if channel == "" || symbol == "" {
		writeErr(w, 400, "channel and symbol required")
		return
	}
	pos, err := s.deriv.GetDerivativesPosition(r.Context(), channel, symbol)
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	writeResp(w, pos)
}

func (s *Server) handleDerivativesOpen(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "method not allowed")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxDerivativesRequestBodyBytes+1))
	if err != nil {
		writeErr(w, 400, "failed to read request body")
		return
	}
	if len(body) > maxDerivativesRequestBodyBytes {
		writeErr(w, 413, "request body too large")
		return
	}

	var req openReq
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&req); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	if err = dec.Decode(&struct{}{}); err != io.EOF {
		writeErr(w, 400, "invalid json")
		return
	}

	if req.Channel == "" || req.Symbol == "" || req.Side == "" || req.Leverage <= 0 || req.Type == "" || req.Amount == "" {
		writeErr(w, 400, "channel, symbol, side, leverage, amount, type required")
		return
	}
	// small sanity on params
	if req.Side != "long" && req.Side != "short" {
		writeErr(w, 400, "side must be long or short")
		return
	}
	if req.Type != "limit" && req.Type != "market" {
		writeErr(w, 400, "type must be limit or market")
		return
	}
	id, err := s.deriv.OpenPosition(r.Context(), req.Channel, req.Symbol, req.Side, req.Leverage, req.Amount, req.Type, req.Price)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeResp(w, map[string]any{"id": id, "accepted_at": time.Now().UTC()})
}

func (s *Server) handleDerivativesClose(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "method not allowed")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxDerivativesRequestBodyBytes+1))
	if err != nil {
		writeErr(w, 400, "failed to read request body")
		return
	}
	if len(body) > maxDerivativesRequestBodyBytes {
		writeErr(w, 413, "request body too large")
		return
	}

	var req closeReq
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&req); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	if err = dec.Decode(&struct{}{}); err != io.EOF {
		writeErr(w, 400, "invalid json")
		return
	}

	identifier := req.ID
	if identifier == "" {
		identifier = req.Symbol
	}

	if req.Channel == "" || identifier == "" || req.Type == "" {
		writeErr(w, 400, "channel, id|symbol, type required")
		return
	}
	if err = s.deriv.ClosePosition(r.Context(), req.Channel, identifier, req.Type); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeSuccess(w)
}

func (s *Server) handleDerivativesHedged(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "method not allowed")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxDerivativesRequestBodyBytes+1))
	if err != nil {
		writeErr(w, 400, "failed to read request body")
		return
	}
	if len(body) > maxDerivativesRequestBodyBytes {
		writeErr(w, 413, "request body too large")
		return
	}

	var req hedgedReq
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&req); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	if err = dec.Decode(&struct{}{}); err != io.EOF {
		writeErr(w, 400, "invalid json")
		return
	}
	if req.OrderID == "" {
		writeErr(w, 400, "order_id required")
		return
	}
	if err = s.deriv.SetPositionHedged(r.Context(), req.OrderID, req.Hedged); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeSuccess(w)
}
