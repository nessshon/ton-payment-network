# Derivatives

TON Payment Network supports offchain derivative positions built on top of linked conditional payments.

This document describes:

- how derivative positions are represented
- how hedging hooks work
- how to enable derivative acceptance safely
- which API calls are used to synchronize hedge state

## Overview

Each derivative position is stored as a linked pair of conditional payments inside a channel:

- an incoming conditional which is monitored against the price feed
- an outgoing conditional linked to the same position

The pair shares stable identifiers:

- `order_id`: base64 condition key of the order on our side
- `linked_order_id`: base64 key of the linked condition in the pair

`order_id` is the same key that can be used with the node internals through `GetVirtualChannelMeta`, and it is the identifier sent in hedge webhooks and accepted by the hedge API.

## Position Lifecycle

High level flow:

1. A node opens a derivative position through `/api/v1/derivatives/open`.
2. The counterparty receives the open request inside `ProcessAction`.
3. If derivative hedging is enabled, the accepting side sends a synchronous hedge webhook before accepting the condition.
4. The hedge request is signed with the configured hedge key and HMAC secret, and the node requires a signed response that is cryptographically bound to the original request.
5. If the hedge webhook returns anything except HTTP `200` with a valid signed `{"success":true}` response, the derivative open is rejected.
6. After the derivative is later closed or cancelled, the accepting side sends an asynchronous close webhook with the same `order_id`.

The open webhook timeout is `2s`.

Close webhooks are queued and retried by the node worker.

## Hedging Semantics

Hedging is enabled only when this config is set:

```json
{
  "ChannelConfig": {
    "DerivativesHedge": {
      "WebhookURL": "http://127.0.0.1:9080/derivatives/hedge",
      "WebhookKey": "base64-key-id",
      "WebhookSignatureHMACSHA256Key": "base64-encoded-32-byte-secret"
    }
  }
}
```

If `WebhookURL` is empty:

- no hedge webhooks are sent
- no synchronous hedge approval is required on accept
- the node behaves as before

When hedge integration is enabled, the node also stores a `hedged` flag in derivative metadata.

That flag is used when price history is unavailable:

- if historical prices for the required period are available, cancellation uses the normal price-based checks
- if history is unavailable and the order is already old enough, cancellation before open is allowed only while `hedged == false`
- once `hedged == true`, the node keeps the pending order active until either price history becomes available again or the hedge flag is cleared

## Accepting Derivatives

Incoming derivative opens are controlled by `ChannelConfig.AcceptingDerivatives`.

```json
{
  "ChannelConfig": {
    "AcceptingDerivatives": true
  }
}
```

Important:

- default value is `false`
- this flag is checked only for accepting new derivative opens in `ProcessAction`
- it does not block closing already opened positions
- it does not block opening positions from this node's own side

Recommended production setup:

- leave `AcceptingDerivatives` disabled until the hedge webhook is live
- enable `AcceptingDerivatives` and `DerivativesHedge.WebhookURL` together on the nodes that should accept external derivative flow

## Webhook Contract

The accepting side sends JSON to `ChannelConfig.DerivativesHedge.WebhookURL`.

Every hedge webhook request includes:

- `X-Payments-Hedge-Key`
- `X-Payments-Hedge-Timestamp`
- `X-Payments-Hedge-Nonce`
- `X-Payments-Hedge-Signature`

The signature is HMAC-SHA256 over the canonical request. The hedge service response must also include the same `X-Payments-Hedge-Key`, a fresh `X-Payments-Hedge-Timestamp`, and a valid `X-Payments-Hedge-Signature` that is bound to the original request and response body.

Open event example:

```json
{
  "event": "open",
  "order_id": "sSKOSuuU0K9IMn16Xn7GA1FvpluHYQIKQ4hsp4VHiNM=",
  "linked_order_id": "9dkIbIjslM2B9LQyNH68ODTygrVPRtKwfoYCWqaBnHc=",
  "channel_address": "EQClgZqAyVBCgVov80ZU_wvFwCqNM63fgKV3lNwc2QOPZ74v",
  "symbol": "BTCUSDT",
  "is_long": true,
  "leverage": 10,
  "collateral": "0.01",
  "fee": "0.0005",
  "entry_price": "80",
  "hedged": false,
  "created_at": 1773317532
}
```

Close event example:

```json
{
  "event": "close",
  "order_id": "sSKOSuuU0K9IMn16Xn7GA1FvpluHYQIKQ4hsp4VHiNM=",
  "linked_order_id": "9dkIbIjslM2B9LQyNH68ODTygrVPRtKwfoYCWqaBnHc=",
  "status": "closed",
  "channel_address": "EQClgZqAyVBCgVov80ZU_wvFwCqNM63fgKV3lNwc2QOPZ74v",
  "symbol": "BTCUSDT",
  "is_long": true,
  "leverage": 10,
  "collateral": "0.01",
  "fee": "0.0005",
  "entry_price": "80",
  "hedged": true,
  "created_at": 1773317532,
  "closed_at": 1773317599
}
```

Expected response body:

```json
{
  "success": true
}
```

Anything else rejects the open on the accepting side.

Security requirements:

- the accepting node does not trust unsigned `200` responses
- redirect responses are not followed
- stale timestamps are rejected
- `/api/v1/derivatives/hedged` also rejects replayed nonces
- use HTTPS in production so hedge payloads are not exposed on the network
- the external hedge service should also cache recent nonces and reject replayed open and close webhooks

## Hedge State API

After your hedge service confirms that an order is hedged externally, call the node API:

```http
POST /api/v1/derivatives/hedged
```

Request body:

```json
{
  "order_id": "sSKOSuuU0K9IMn16Xn7GA1FvpluHYQIKQ4hsp4VHiNM=",
  "hedged": true
}
```

You can also clear the flag later with `hedged: false`.

When hedge auth is configured on the node, this API request must be signed with the same hedge auth headers and secret, and the node returns a signed response so the hedge service can verify that the callback was accepted by the expected node instance.

## Derivative Metadata

Derivative metadata exposed by the node now includes the hedge flag:

- position list responses include `hedged`
- virtual conditional responses include `derivative.hedged`

This allows operators and UIs to see whether a pending or active order is already protected externally.

## Minimal Hedge Server Example

The example below accepts open and close webhooks, logs counters, and marks open orders as hedged through the node API.

```go
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/xssnick/ton-payment-network/tonpayments/hedgeauth"
)

type hedgeEvent struct {
	Event   string `json:"event"`
	OrderID string `json:"order_id"`
	Symbol  string `json:"symbol"`
}

type hedgedRequest struct {
	OrderID string `json:"order_id"`
	Hedged  bool   `json:"hedged"`
}

var hedgedCount atomic.Int64
var closedCount atomic.Int64

const (
	nodeURL           = "http://127.0.0.1:8096"
	hedgeKey          = "base64-key-id"
	hedgeSecretBase64 = "base64-encoded-32-byte-secret"
)

var hedgeSecret = mustDecodeSecret()

func main() {
	http.HandleFunc("/derivatives/hedge", func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()

		body, err := io.ReadAll(io.LimitReader(r.Body, 16*1024+1))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		target := r.URL.EscapedPath()
		if target == "" {
			target = "/"
		}
		meta, _, err := hedgeauth.VerifyRequest(
			r.Header,
			r.Method,
			hedgeauth.CanonicalTarget(target, r.URL.RawQuery),
			body,
			hedgeKey,
			hedgeSecret,
			time.Now(),
			hedgeauth.DefaultMaxClockSkew,
		)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}

		var ev hedgeEvent
		if err := json.NewDecoder(bytes.NewReader(body)).Decode(&ev); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		status := http.StatusOK
		switch ev.Event {
		case "open":
			if err := markHedged(r.Context(), ev.OrderID, true); err != nil {
				status = http.StatusInternalServerError
			} else {
				n := hedgedCount.Add(1)
				log.Printf("hedged=%d closed=%d order=%s symbol=%s", n, closedCount.Load(), ev.OrderID, ev.Symbol)
			}
		case "close":
			n := closedCount.Add(1)
			log.Printf("hedged=%d closed=%d order=%s symbol=%s", hedgedCount.Load(), n, ev.OrderID, ev.Symbol)
		default:
			status = http.StatusBadRequest
		}

		respBody, _ := json.Marshal(map[string]bool{"success": status == http.StatusOK})
		w.Header().Set("Content-Type", "application/json")
		if err := hedgeauth.ApplySignedResponseHeaders(w.Header(), meta, status, respBody, hedgeKey, hedgeSecret, time.Now()); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write(respBody)
	})

	log.Fatal(http.ListenAndServe(":9080", nil))
}

func markHedged(parent context.Context, orderID string, hedged bool) error {
	body, err := json.Marshal(hedgedRequest{
		OrderID: orderID,
		Hedged:  hedged,
	})
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, nodeURL+"/api/v1/derivatives/hedged", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	target := req.URL.EscapedPath()
	if target == "" {
		target = "/"
	}
	meta, err := hedgeauth.ApplySignedRequestHeaders(
		req.Header,
		req.Method,
		hedgeauth.CanonicalTarget(target, req.URL.RawQuery),
		body,
		hedgeKey,
		hedgeSecret,
		time.Now(),
	)
	if err != nil {
		return err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if err := hedgeauth.VerifyResponse(resp.Header, meta, resp.StatusCode, respBody, hedgeKey, hedgeSecret, time.Now(), hedgeauth.DefaultMaxClockSkew); err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return errors.New(resp.Status)
	}
	return nil
}

func mustDecodeSecret() []byte {
	secret, err := base64.StdEncoding.DecodeString(hedgeSecretBase64)
	if err != nil {
		panic(err)
	}
	return secret
}
```

## Minimal Config Example

```json
{
  "ChannelConfig": {
    "AcceptingDerivatives": true,
    "DerivativesHedge": {
      "WebhookURL": "http://127.0.0.1:9080/derivatives/hedge",
      "WebhookKey": "base64-key-id",
      "WebhookSignatureHMACSHA256Key": "base64-encoded-32-byte-secret"
    }
  }
}
```

With this configuration:

- the node accepts incoming derivative opens
- every accepted open is checked synchronously by the hedge server
- every close or cancel sends a follow-up close webhook
- the hedge server can mark orders as hedged through `/api/v1/derivatives/hedged`
