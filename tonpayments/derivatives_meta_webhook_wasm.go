//go:build js && wasm

package tonpayments

import "context"
import "time"

func (s *Service) sendDerivativeHedgeWebhookImpl(ctx context.Context, req derivativeHedgeWebhookRequest, timeout time.Duration) error {
	// In WASM/web, outbound hedge callback webhooks are not executed from this runtime.
	_ = s
	_ = req
	_ = ctx
	_ = timeout
	return nil
}
