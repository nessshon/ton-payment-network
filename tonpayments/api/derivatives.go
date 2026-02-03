package api

import (
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
	Symbol  string `json:"symbol"`
	Type    string `json:"type"`
}

func (s *Server) handleDerivativesPosition(w http.ResponseWriter, r *http.Request) {
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
	body, _ := io.ReadAll(r.Body)
	var req openReq
	_ = json.Unmarshal(body, &req)
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
	body, _ := io.ReadAll(r.Body)
	var req closeReq
	_ = json.Unmarshal(body, &req)
	if req.Channel == "" || req.Symbol == "" || req.Type == "" {
		writeErr(w, 400, "channel, symbol, type required")
		return
	}
	if err := s.deriv.ClosePosition(r.Context(), req.Channel, req.Symbol, req.Type); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeSuccess(w)
}
