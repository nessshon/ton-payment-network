package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/xssnick/ton-payment-network/tonpayments/hedgeauth"
)

type stubDerivativesService struct {
	orderID string
	hedged  bool
}

func (s *stubDerivativesService) GetDerivativesPosition(context.Context, string, string) (any, error) {
	return nil, nil
}

func (s *stubDerivativesService) OpenPosition(context.Context, string, string, string, int, string, string, string) (string, error) {
	return "", nil
}

func (s *stubDerivativesService) ClosePosition(context.Context, string, string, string) error {
	return nil
}

func (s *stubDerivativesService) SetPositionHedged(_ context.Context, orderID string, hedged bool) error {
	s.orderID = orderID
	s.hedged = hedged
	return nil
}

func TestHandleDerivativesHedged_Signed(t *testing.T) {
	deriv := &stubDerivativesService{}
	key, secretBase64, secret := testHedgeAuth(t)
	srv, err := NewServer("", "", "", nil, deriv, nil, nil, &HedgeAuthConfig{
		Key:                          key,
		SignatureHMACSHA256KeyBase64: secretBase64,
	})
	if err != nil {
		t.Fatalf("new server failed: %v", err)
	}

	body := []byte(`{"order_id":"abc","hedged":true}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/derivatives/hedged", bytes.NewReader(body))
	meta := signHedgeRequest(t, req, body, key, secret)

	rec := httptest.NewRecorder()
	srv.srv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	if deriv.orderID != "abc" || !deriv.hedged {
		t.Fatalf("unexpected hedged call: orderID=%q hedged=%v", deriv.orderID, deriv.hedged)
	}
	if err = hedgeauth.VerifyResponse(rec.Result().Header, meta, rec.Code, rec.Body.Bytes(), key, secret, time.Now(), hedgeauth.DefaultMaxClockSkew); err != nil {
		t.Fatalf("response signature verification failed: %v", err)
	}
}

func TestHandleDerivativesHedged_RejectsUnsigned(t *testing.T) {
	deriv := &stubDerivativesService{}
	key, secretBase64, _ := testHedgeAuth(t)
	srv, err := NewServer("", "", "", nil, deriv, nil, nil, &HedgeAuthConfig{
		Key:                          key,
		SignatureHMACSHA256KeyBase64: secretBase64,
	})
	if err != nil {
		t.Fatalf("new server failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/derivatives/hedged", bytes.NewReader([]byte(`{"order_id":"abc","hedged":true}`)))
	rec := httptest.NewRecorder()

	srv.srv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	if deriv.orderID != "" {
		t.Fatalf("hedged callback must not reach service on unsigned request")
	}
}

func TestHandleDerivativesHedged_RejectsReplay(t *testing.T) {
	deriv := &stubDerivativesService{}
	key, secretBase64, secret := testHedgeAuth(t)
	srv, err := NewServer("", "", "", nil, deriv, nil, nil, &HedgeAuthConfig{
		Key:                          key,
		SignatureHMACSHA256KeyBase64: secretBase64,
	})
	if err != nil {
		t.Fatalf("new server failed: %v", err)
	}

	body := []byte(`{"order_id":"abc","hedged":true}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/derivatives/hedged", bytes.NewReader(body))
	signHedgeRequest(t, req, body, key, secret)

	first := httptest.NewRecorder()
	srv.srv.Handler.ServeHTTP(first, req)
	if first.Code != http.StatusOK {
		t.Fatalf("first request status=%d body=%s", first.Code, first.Body.String())
	}

	replay := httptest.NewRequest(http.MethodPost, "/api/v1/derivatives/hedged", bytes.NewReader(body))
	replay.Header = req.Header.Clone()
	second := httptest.NewRecorder()
	srv.srv.Handler.ServeHTTP(second, replay)

	if second.Code != http.StatusUnauthorized {
		t.Fatalf("unexpected replay status: %d body=%s", second.Code, second.Body.String())
	}
}

func TestNewServer_RejectsPartialHedgeAuthConfig(t *testing.T) {
	_, err := NewServer("", "", "", nil, &stubDerivativesService{}, nil, nil, &HedgeAuthConfig{
		Key: "hedge-key-only",
	})
	if err == nil {
		t.Fatalf("expected partial hedge auth config to fail")
	}
}

func signHedgeRequest(t *testing.T, req *http.Request, body []byte, key string, secret []byte) hedgeauth.RequestMeta {
	t.Helper()

	target := req.URL.EscapedPath()
	if target == "" {
		target = "/"
	}
	meta, err := hedgeauth.ApplySignedRequestHeaders(
		req.Header,
		req.Method,
		hedgeauth.CanonicalTarget(target, req.URL.RawQuery),
		body,
		key,
		secret,
		time.Now(),
	)
	if err != nil {
		t.Fatalf("sign hedge request failed: %v", err)
	}
	return meta
}

func testHedgeAuth(t *testing.T) (string, string, []byte) {
	t.Helper()

	secret := []byte("0123456789abcdef0123456789abcdef")
	return "test-hedge-key", base64.StdEncoding.EncodeToString(secret), secret
}
