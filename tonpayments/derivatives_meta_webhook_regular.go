//go:build !js || !wasm

package tonpayments

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/xssnick/ton-payment-network/tonpayments/hedgeauth"
)

func (s *Service) sendDerivativeHedgeWebhookImpl(ctx context.Context, req derivativeHedgeWebhookRequest, timeout time.Duration) error {
	if !s.derivativesHedgingEnabled() {
		return nil
	}

	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("failed to encode derivative hedge webhook: %w", err)
	}
	key, secret, err := s.derivativeHedgeAuth()
	if err != nil {
		return fmt.Errorf("invalid derivative hedge auth config: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(callCtx, http.MethodPost, s.cfg.DerivativesHedge.WebhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to build derivative hedge webhook request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	target := httpReq.URL.EscapedPath()
	if target == "" {
		target = "/"
	}
	reqMeta, err := hedgeauth.ApplySignedRequestHeaders(
		httpReq.Header,
		httpReq.Method,
		hedgeauth.CanonicalTarget(target, httpReq.URL.RawQuery),
		body,
		key,
		secret,
		time.Now(),
	)
	if err != nil {
		return fmt.Errorf("failed to sign derivative hedge webhook request: %w", err)
	}

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("failed to send derivative hedge webhook: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, derivativeHedgeWebhookMaxResponse+1))
	if err != nil {
		return fmt.Errorf("failed to read derivative hedge webhook response: %w", err)
	}
	if len(respBody) > derivativeHedgeWebhookMaxResponse {
		return fmt.Errorf("derivative hedge webhook response body is too large")
	}
	if err = hedgeauth.VerifyResponse(resp.Header, reqMeta, resp.StatusCode, respBody, key, secret, time.Now(), hedgeauth.DefaultMaxClockSkew); err != nil {
		return fmt.Errorf("failed to verify derivative hedge webhook response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("derivative hedge webhook response status is: %d %s", resp.StatusCode, resp.Status)
	}

	var res derivativeHedgeWebhookResponse
	dec := json.NewDecoder(bytes.NewReader(respBody))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&res); err != nil {
		return fmt.Errorf("bad derivative hedge webhook response: %w", err)
	}
	if err = dec.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("bad derivative hedge webhook response: invalid trailing data")
	}
	if !res.Success {
		return fmt.Errorf("derivative hedge webhook response is not success")
	}
	return nil
}
