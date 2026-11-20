package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Signed webhook scheme (CoDi confirmations, see docs/AUTH.md):
//
//	X-Kerno-Timestamp: <unix seconds>
//	X-Kerno-Signature: v1=<hex(HMAC-SHA256(secret, timestamp + "." + raw body))>
//
// The timestamp must be within WebhookTolerance of the server clock and each
// signature is accepted once (replay cache with TTL 2x tolerance).
const (
	WebhookTimestampHeader = "X-Kerno-Timestamp"
	WebhookSignatureHeader = "X-Kerno-Signature"
	WebhookTolerance       = 5 * time.Minute
	webhookSigPrefix       = "v1="
)

var (
	ErrWebhookSignature = errors.New("webhook: missing or invalid signature")
	ErrWebhookStale     = errors.New("webhook: timestamp outside tolerance window")
)

func webhookMAC(secret []byte, timestamp string, body []byte) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(timestamp))
	m.Write([]byte("."))
	m.Write(body)
	return m.Sum(nil)
}

// SignWebhook returns the X-Kerno-Signature value for body sent at ts.
func SignWebhook(secret []byte, ts int64, body []byte) string {
	return webhookSigPrefix + hex.EncodeToString(webhookMAC(secret, strconv.FormatInt(ts, 10), body))
}

// VerifyWebhookSignature checks the timestamp window and the HMAC over the raw
// body with a constant-time comparison. It returns the decoded signature so
// callers can key a replay cache on it.
func VerifyWebhookSignature(secret []byte, tsHeader, sigHeader string, body []byte, now time.Time, tolerance time.Duration) ([]byte, error) {
	if len(secret) == 0 || tsHeader == "" || !strings.HasPrefix(sigHeader, webhookSigPrefix) {
		return nil, ErrWebhookSignature
	}
	ts, err := strconv.ParseInt(tsHeader, 10, 64)
	if err != nil {
		return nil, ErrWebhookSignature
	}
	got, err := hex.DecodeString(strings.TrimPrefix(sigHeader, webhookSigPrefix))
	if err != nil {
		return nil, ErrWebhookSignature
	}
	if !hmac.Equal(got, webhookMAC(secret, tsHeader, body)) {
		return nil, ErrWebhookSignature
	}
	if d := now.Sub(time.Unix(ts, 0)); d > tolerance || d < -tolerance {
		return nil, ErrWebhookStale
	}
	return got, nil
}
