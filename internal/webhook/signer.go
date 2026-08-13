// Package webhook produces Stripe-shaped outbound events: it signs payloads with
// the Stripe-Signature scheme and delivers them to the merchant's endpoint with
// retry. This is the outbound half of M2 — the inbound half (receiving Midtrans
// notifications and re-signing them as Stripe events) lives in package server.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Sign builds a Stripe-Signature header value for body at the given unix time.
//
// Stripe's scheme: the signed payload is "<timestamp>.<body>", and
//
//	v1 = hex_lower(HMAC-SHA256(signed_payload, endpoint_secret))
//	header = "t=<timestamp>,v1=<v1>"
//
// (See https://stripe.com/docs/webhooks — the facade reproduces this so the
// merchant verifies with the exact same code they already use for Stripe.)
func Sign(body []byte, secret string, now int64) string {
	stamp := strconv.FormatInt(now, 10)
	signed := stamp + "." + string(body)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signed))
	return "t=" + stamp + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

// ErrInvalidSignature is returned by Verify when the header is absent, malformed,
// or does not match (or is outside the replay tolerance).
var ErrInvalidSignature = errors.New("webhook: invalid signature")

// Verify validates a Stripe-Signature header against body+secret. tolerance is
// the maximum allowed skew between the header timestamp and now; pass 0 to skip
// the freshness check. Mirrors Stripe's official verification algorithm.
func Verify(body []byte, header, secret string, now int64, tolerance time.Duration) error {
	t, sig, ok := parseSignature(header)
	if !ok {
		return ErrInvalidSignature
	}
	if tolerance > 0 && now-t > int64(tolerance.Seconds()) {
		return ErrInvalidSignature
	}
	signed := strconv.FormatInt(t, 10) + "." + string(body)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signed))
	want := mac.Sum(nil)
	got, err := hex.DecodeString(sig)
	if err != nil || len(got) != len(want) {
		return ErrInvalidSignature
	}
	if subtle.ConstantTimeCompare(want, got) != 1 {
		return ErrInvalidSignature
	}
	return nil
}

// parseSignature extracts the timestamp and v1 signature from a Stripe-Signature
// header ("t=…,v1=…").
func parseSignature(header string) (timestamp int64, v1 string, ok bool) {
	for _, part := range strings.Split(header, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "t":
			timestamp, _ = strconv.ParseInt(kv[1], 10, 64)
		case "v1":
			v1 = kv[1]
		}
	}
	if timestamp == 0 || v1 == "" {
		return 0, "", false
	}
	return timestamp, v1, true
}

// prefixForSecret returns the canonical Stripe "whsec_..." form for a generated
// secret, so it looks familiar to merchants copying it into their verifier.
func prefixForSecret(s string) string {
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "whsec_") {
		return s
	}
	return fmt.Sprintf("whsec_%s", s)
}
