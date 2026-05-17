package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/tonpayments/db"
	"github.com/xssnick/ton-payment-network/tonpayments/hedgeauth"
	"github.com/xssnick/ton-payment-network/tonpayments/transport"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

type Queue interface {
	CreateTask(ctx context.Context, poolName, typ, queue, id string, data any, executeAfter, executeTill *time.Time) error
	AcquireTask(ctx context.Context, poolName string) (*db.Task, error)
	RetryTask(ctx context.Context, task *db.Task, reason string, retryAt time.Time) error
	CompleteTask(ctx context.Context, poolName string, task *db.Task) error
}

type Service interface {
	GetChannel(ctx context.Context, addr string) (*db.Channel, error)
	GetActiveChannel(ctx context.Context, addr string) (*db.Channel, error)
	ListChannels(ctx context.Context, key ed25519.PublicKey, status db.ChannelStatus) ([]*db.Channel, error)

	GetVirtualChannelMeta(ctx context.Context, key ed25519.PublicKey) (*db.ConditionalMeta, error)

	RequestCooperativeClose(ctx context.Context, channelAddr string) error
	RequestUncooperativeClose(ctx context.Context, addr string) error
	CloseConditional(ctx context.Context, virtualKey ed25519.PublicKey) error
	AddConditionalResolve(ctx context.Context, virtualKey ed25519.PublicKey, state *cell.Cell) error
	CreateSendConditional(ctx context.Context, instructionKey ed25519.PublicKey, private ed25519.PrivateKey, firstPart, lastPart transport.TunnelChainPart, chain []transport.AddConditionalInstruction, cc *payments.CoinConfig) error
	OpenChannelWithNode(ctx context.Context, nodeKey ed25519.PublicKey) (*address.Address, error)
	TopupChannel(ctx context.Context, channel *db.Channel, balanceId string, amount tlb.Coins, unlockBalanceControl bool) error
	RequestCommitAction(ctx context.Context, addr *address.Address, actionId []byte) error
	ResolveCoinConfig(balanceId string) (*payments.CoinConfig, error)
	ResolveCoinConfigBySymbol(sym string) (*payments.CoinConfig, error)
	RequestWithdrawToAddr(ctx context.Context, channelAddr string, addr *address.Address, cc *payments.CoinConfig, amount *big.Int) error
	GetPrivateKey() ed25519.PrivateKey

	ResolveAction(ctx context.Context, id []byte) (payments.Action, error)
	ResolveBalanceType(id string) (*payments.CoinConfig, error)
	GetKnownBalanceTypes() []*payments.CoinConfig
}

type DerivativesService interface {
	GetDerivativesPosition(ctx context.Context, channelAddr string, symbol string) (any, error)
	OpenPosition(ctx context.Context, channelAddr string, symbol string, side string, leverage int, amount string, typ string, price string) (string, error)
	ClosePosition(ctx context.Context, channelAddr string, symbol string, typ string) error
	SetPositionHedged(ctx context.Context, orderID string, hedged bool) error
}

type Success struct {
	Success bool `json:"success"`
}

type Error struct {
	Error string `json:"error"`
}

type Server struct {
	svc            Service
	deriv          DerivativesService
	queue          Queue
	webhook        string
	webhookKey     string
	webhookSignal  chan bool
	hedgeAuth      *hedgeAuthState
	hedgeNonces    map[string]time.Time
	hedgeNoncesMx  sync.Mutex
	srv            http.Server
	sender         http.Client
	apiCredentials *Credentials
}

type Credentials struct {
	Login    string
	Password string
}

type HedgeAuthConfig struct {
	Key                          string
	SignatureHMACSHA256KeyBase64 string
}

type hedgeAuthState struct {
	key    string
	secret []byte
}

type captureResponseWriter struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (w *captureResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *captureResponseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(data)
}

func (w *captureResponseWriter) WriteHeader(code int) {
	if w.status != 0 {
		return
	}
	w.status = code
}

func NewServer(addr, webhook, webhookKey string, svc Service, deriv DerivativesService, queue Queue, credentials *Credentials, hedgeAuthCfg *HedgeAuthConfig) (*Server, error) {
	hedgeAuth, err := newHedgeAuthState(hedgeAuthCfg)
	if err != nil {
		return nil, err
	}

	s := &Server{
		svc:         svc,
		deriv:       deriv,
		queue:       queue,
		webhook:     webhook,
		webhookKey:  webhookKey,
		hedgeAuth:   hedgeAuth,
		hedgeNonces: map[string]time.Time{},
		sender: http.Client{
			Timeout: 10 * time.Second,
		},
		apiCredentials: credentials,
	}

	mx := http.NewServeMux()
	mx.HandleFunc("/api/v1/channel/onchain/open", s.checkCredentials(s.handleChannelOpen))
	mx.HandleFunc("/api/v1/channel/onchain/topup", s.checkCredentials(s.handleTopup))
	mx.HandleFunc("/api/v1/channel/onchain/withdraw", s.checkCredentials(s.handleWithdraw))
	mx.HandleFunc("/api/v1/channel/onchain/close", s.checkCredentials(s.handleChannelClose))
	mx.HandleFunc("/api/v1/channel/onchain/list", s.checkCredentials(s.handleChannelsList))
	mx.HandleFunc("/api/v1/channel/onchain", s.checkCredentials(s.handleChannelGet))

	mx.HandleFunc("/api/v1/channel/conditional/open", s.checkCredentials(s.handleVirtualOpen))
	mx.HandleFunc("/api/v1/channel/conditional/close", s.checkCredentials(s.handleVirtualClose))
	mx.HandleFunc("/api/v1/channel/conditional/transfer", s.checkCredentials(s.handleVirtualTransfer))
	mx.HandleFunc("/api/v1/channel/conditional/state", s.checkCredentials(s.handleVirtualState))
	mx.HandleFunc("/api/v1/channel/conditional/list", s.checkCredentials(s.handleVirtualList))
	mx.HandleFunc("/api/v1/channel/conditional", s.checkCredentials(s.handleVirtualGet))

	// Derivatives endpoints
	mx.HandleFunc("/api/v1/derivatives/position", s.checkCredentials(s.handleDerivativesPosition))
	mx.HandleFunc("/api/v1/derivatives/open", s.checkCredentials(s.handleDerivativesOpen))
	mx.HandleFunc("/api/v1/derivatives/close", s.checkCredentials(s.handleDerivativesClose))
	mx.HandleFunc("/api/v1/derivatives/hedged", s.checkHedgeCredentials(s.handleDerivativesHedged))

	s.srv = http.Server{
		Addr:    addr,
		Handler: mx,
	}

	return s, nil
}

func (s *Server) Start() error {
	if s.webhook != "" {
		go s.startWebhooksSender()
	}
	return s.srv.ListenAndServe()
}

func (s *Server) checkCredentials(handler func(w http.ResponseWriter, r *http.Request)) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authorizeRequest(w, r) {
			return
		}

		handler(w, r)
	}
}

func newHedgeAuthState(cfg *HedgeAuthConfig) (*hedgeAuthState, error) {
	if cfg == nil {
		return nil, nil
	}

	key := strings.TrimSpace(cfg.Key)
	secretKey := strings.TrimSpace(cfg.SignatureHMACSHA256KeyBase64)
	if key == "" || secretKey == "" {
		return nil, fmt.Errorf("hedge auth key and signature key must both be configured")
	}

	secret, err := hedgeauth.DecodeBase64Key(secretKey)
	if err != nil {
		return nil, err
	}

	return &hedgeAuthState{
		key:    key,
		secret: secret,
	}, nil
}

func (s *Server) hedgeAuthEnabled() bool {
	return s != nil && s.hedgeAuth != nil
}

func (s *Server) checkHedgeCredentials(handler func(w http.ResponseWriter, r *http.Request)) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.hedgeAuthEnabled() {
			if !s.authorizeRequest(w, r) {
				return
			}
			handler(w, r)
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, maxDerivativesRequestBodyBytes+1))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "failed to read request body")
			return
		}
		if len(body) > maxDerivativesRequestBodyBytes {
			writeErr(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}

		now := time.Now()
		reqMeta, _, err := hedgeauth.VerifyRequest(
			r.Header,
			r.Method,
			hedgeauth.CanonicalTarget(r.URL.EscapedPath(), r.URL.RawQuery),
			body,
			s.hedgeAuth.key,
			s.hedgeAuth.secret,
			now,
			hedgeauth.DefaultMaxClockSkew,
		)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if !s.registerHedgeNonce(reqMeta.Key, reqMeta.Nonce, now.Add(hedgeauth.DefaultMaxClockSkew)) {
			writeErr(w, http.StatusUnauthorized, "replayed request")
			return
		}

		r.Body = io.NopCloser(bytes.NewReader(body))
		capture := &captureResponseWriter{}
		handler(capture, r)

		status := capture.status
		if status == 0 {
			status = http.StatusOK
		}

		if err = hedgeauth.ApplySignedResponseHeaders(w.Header(), reqMeta, status, capture.body.Bytes(), s.hedgeAuth.key, s.hedgeAuth.secret, time.Now()); err != nil {
			writeErr(w, http.StatusInternalServerError, "failed to sign hedge response")
			return
		}
		for key, vals := range capture.Header() {
			for _, val := range vals {
				w.Header().Add(key, val)
			}
		}
		w.WriteHeader(status)
		_, _ = w.Write(capture.body.Bytes())
	}
}

func (s *Server) registerHedgeNonce(key, nonce string, expiresAt time.Time) bool {
	s.hedgeNoncesMx.Lock()
	defer s.hedgeNoncesMx.Unlock()

	now := time.Now()
	for cacheKey, till := range s.hedgeNonces {
		if !till.After(now) {
			delete(s.hedgeNonces, cacheKey)
		}
	}

	cacheKey := key + ":" + nonce
	if till, ok := s.hedgeNonces[cacheKey]; ok && till.After(now) {
		return false
	}
	s.hedgeNonces[cacheKey] = expiresAt
	return true
}

func (s *Server) authorizeRequest(w http.ResponseWriter, r *http.Request) bool {
	if s.apiCredentials == nil {
		return true
	}

	login, password, ok := r.BasicAuth()
	if !ok {
		writeErr(w, 401, "unauthorized")
		return false
	}
	if s.apiCredentials.Password != password || s.apiCredentials.Login != login {
		writeErr(w, 401, "unauthorized")
		return false
	}
	return true
}

func writeErr(w http.ResponseWriter, code int, text string) {
	data, _ := json.Marshal(Error{text})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(data)
}

func writeResp(w http.ResponseWriter, obj any) {
	data, _ := json.Marshal(obj)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	_, _ = w.Write(data)
}

func writeSuccess(w http.ResponseWriter) {
	writeResp(w, Success{true})
}

func parseKey(key string) (ed25519.PublicKey, error) {
	k, err := base64.StdEncoding.DecodeString(key)
	if err != nil {
		return nil, fmt.Errorf("incorrect key format, should be in base64: %w", err)
	}
	if len(k) != 32 {
		return nil, fmt.Errorf("incorrect key length, should be 32")
	}
	return ed25519.PublicKey(k), nil
}
