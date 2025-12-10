package web

import "crypto/ed25519"

type Event struct {
	Key       ed25519.PublicKey `json:"key"`
	QueryID   string            `json:"query_id,omitempty"`
	Data      []byte            `json:"data"`
	Signature []byte            `json:"signature,omitempty"`
}

type SubscribeAuth struct {
	PeerKey   ed25519.PublicKey `json:"key"`
	Timestamp int64             `json:"timestamp"`
	Signature []byte            `json:"signature"`
}

type SubscribeAuthResult struct {
	Token string `json:"token"`
}

type QueryResponseAccepted struct {
	Success bool `json:"success"`
}

func (e *Event) Sign(key ed25519.PrivateKey) {
	e.Signature = ed25519.Sign(key, append(append([]byte("signed|"), e.Data...), e.QueryID...))
}

func (e *Event) Verify(key ed25519.PublicKey) bool {
	return ed25519.Verify(key, append(append([]byte("signed|"), e.Data...), e.QueryID...), e.Signature)
}
