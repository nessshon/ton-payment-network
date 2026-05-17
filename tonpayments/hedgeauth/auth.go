package hedgeauth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	HeaderKey       = "X-Payments-Hedge-Key"
	HeaderTimestamp = "X-Payments-Hedge-Timestamp"
	HeaderNonce     = "X-Payments-Hedge-Nonce"
	HeaderSignature = "X-Payments-Hedge-Signature"

	DefaultMaxClockSkew = 30 * time.Second
)

type Header interface {
	Get(key string) string
	Set(key, value string)
}

type RequestMeta struct {
	Key       string
	Method    string
	Target    string
	Timestamp string
	Nonce     string
	BodyHash  string
}

func CanonicalTarget(path, rawQuery string) string {
	if rawQuery == "" {
		return path
	}
	return path + "?" + rawQuery
}

func DecodeBase64Key(secret string) ([]byte, error) {
	if strings.TrimSpace(secret) == "" {
		return nil, fmt.Errorf("empty hedge signature key")
	}
	key, err := base64.StdEncoding.DecodeString(secret)
	if err != nil {
		return nil, fmt.Errorf("invalid hedge signature key: %w", err)
	}
	if len(key) < 32 {
		return nil, fmt.Errorf("hedge signature key must be at least 32 bytes")
	}
	return key, nil
}

func NewNonce() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(buf), nil
}

func ApplySignedRequestHeaders(headers Header, method, target string, body []byte, key string, secret []byte, now time.Time) (RequestMeta, error) {
	nonce, err := NewNonce()
	if err != nil {
		return RequestMeta{}, err
	}

	meta := RequestMeta{
		Key:       key,
		Method:    strings.ToUpper(method),
		Target:    target,
		Timestamp: strconv.FormatInt(now.UTC().Unix(), 10),
		Nonce:     nonce,
		BodyHash:  bodyHash(body),
	}

	sig, err := SignRequest(meta, secret)
	if err != nil {
		return RequestMeta{}, err
	}

	headers.Set(HeaderKey, key)
	headers.Set(HeaderTimestamp, meta.Timestamp)
	headers.Set(HeaderNonce, meta.Nonce)
	headers.Set(HeaderSignature, sig)
	return meta, nil
}

func SignRequest(meta RequestMeta, secret []byte) (string, error) {
	if strings.TrimSpace(meta.Key) == "" {
		return "", fmt.Errorf("empty hedge key")
	}
	if strings.TrimSpace(meta.Timestamp) == "" || strings.TrimSpace(meta.Nonce) == "" {
		return "", fmt.Errorf("missing hedge timestamp or nonce")
	}

	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(requestCanonical(meta)))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil)), nil
}

func VerifyRequest(headers Header, method, target string, body []byte, expectedKey string, secret []byte, now time.Time, maxSkew time.Duration) (RequestMeta, time.Time, error) {
	meta := RequestMeta{
		Key:       headers.Get(HeaderKey),
		Method:    strings.ToUpper(method),
		Target:    target,
		Timestamp: headers.Get(HeaderTimestamp),
		Nonce:     headers.Get(HeaderNonce),
		BodyHash:  bodyHash(body),
	}
	if meta.Key == "" || meta.Timestamp == "" || meta.Nonce == "" {
		return RequestMeta{}, time.Time{}, fmt.Errorf("missing hedge auth headers")
	}
	if expectedKey != "" && meta.Key != expectedKey {
		return RequestMeta{}, time.Time{}, fmt.Errorf("unknown hedge key")
	}

	ts, err := parseTimestamp(meta.Timestamp)
	if err != nil {
		return RequestMeta{}, time.Time{}, err
	}
	if skewExceeded(now.UTC(), ts, maxSkew) {
		return RequestMeta{}, time.Time{}, fmt.Errorf("hedge timestamp is outside allowed skew")
	}

	gotSig := headers.Get(HeaderSignature)
	if err = verifySignature(gotSig, requestCanonical(meta), secret); err != nil {
		return RequestMeta{}, time.Time{}, err
	}

	return meta, ts, nil
}

func ApplySignedResponseHeaders(headers Header, req RequestMeta, statusCode int, body []byte, key string, secret []byte, now time.Time) error {
	respTimestamp := strconv.FormatInt(now.UTC().Unix(), 10)
	sig, err := SignResponse(req, statusCode, body, key, secret, respTimestamp)
	if err != nil {
		return err
	}

	headers.Set(HeaderKey, key)
	headers.Set(HeaderTimestamp, respTimestamp)
	headers.Set(HeaderSignature, sig)
	return nil
}

func SignResponse(req RequestMeta, statusCode int, body []byte, key string, secret []byte, respTimestamp string) (string, error) {
	if strings.TrimSpace(key) == "" {
		return "", fmt.Errorf("empty hedge key")
	}
	if strings.TrimSpace(respTimestamp) == "" {
		return "", fmt.Errorf("missing response timestamp")
	}

	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(responseCanonical(req, statusCode, bodyHash(body), key, respTimestamp)))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil)), nil
}

func VerifyResponse(headers Header, req RequestMeta, statusCode int, body []byte, expectedKey string, secret []byte, now time.Time, maxSkew time.Duration) error {
	key := headers.Get(HeaderKey)
	respTimestamp := headers.Get(HeaderTimestamp)
	if key == "" || respTimestamp == "" {
		return fmt.Errorf("missing hedge response auth headers")
	}
	if expectedKey != "" && key != expectedKey {
		return fmt.Errorf("unexpected hedge response key")
	}

	ts, err := parseTimestamp(respTimestamp)
	if err != nil {
		return err
	}
	if skewExceeded(now.UTC(), ts, maxSkew) {
		return fmt.Errorf("hedge response timestamp is outside allowed skew")
	}

	return verifySignature(headers.Get(HeaderSignature), responseCanonical(req, statusCode, bodyHash(body), key, respTimestamp), secret)
}

func requestCanonical(meta RequestMeta) string {
	return strings.Join([]string{
		"request",
		meta.Key,
		meta.Method,
		meta.Target,
		meta.Timestamp,
		meta.Nonce,
		meta.BodyHash,
	}, "\n")
}

func responseCanonical(req RequestMeta, statusCode int, responseBodyHash, key, responseTimestamp string) string {
	return strings.Join([]string{
		"response",
		key,
		req.Method,
		req.Target,
		req.Timestamp,
		req.Nonce,
		req.BodyHash,
		responseTimestamp,
		strconv.Itoa(statusCode),
		responseBodyHash,
	}, "\n")
}

func bodyHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func parseTimestamp(value string) (time.Time, error) {
	sec, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid hedge timestamp: %w", err)
	}
	return time.Unix(sec, 0).UTC(), nil
}

func skewExceeded(now, ts time.Time, maxSkew time.Duration) bool {
	diff := now.Sub(ts)
	if diff < 0 {
		diff = -diff
	}
	return diff > maxSkew
}

func verifySignature(encoded, payload string, secret []byte) error {
	if encoded == "" {
		return fmt.Errorf("missing hedge signature")
	}

	got, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return fmt.Errorf("invalid hedge signature encoding: %w", err)
	}

	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(payload))
	expected := mac.Sum(nil)
	if !hmac.Equal(got, expected) {
		return fmt.Errorf("invalid hedge signature")
	}
	return nil
}
