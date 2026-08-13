package webhook

import (
	"testing"
	"time"
)

const testSecret = "whsec_testsecret"

func TestSignVerifyRoundTrip(t *testing.T) {
	body := []byte(`{"id":"evt_1","type":"payment_intent.succeeded"}`)
	sig := Sign(body, testSecret, 1700000000)
	if err := Verify(body, sig, testSecret, 1700000000, 0); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
}

func TestVerifyTamper(t *testing.T) {
	body := []byte(`{"id":"evt_1"}`)
	sig := Sign(body, testSecret, 1700000000)
	if err := Verify([]byte(`{"id":"evt_2"}`), sig, testSecret, 1700000000, 0); err == nil {
		t.Error("tampered body accepted")
	}
}

func TestVerifyWrongSecret(t *testing.T) {
	body := []byte(`{"id":"evt_1"}`)
	sig := Sign(body, testSecret, 1700000000)
	if err := Verify(body, sig, "whsec_other", 1700000000, 0); err == nil {
		t.Error("wrong secret accepted")
	}
}

func TestVerifyTolerance(t *testing.T) {
	body := []byte(`{"id":"evt_1"}`)
	sig := Sign(body, testSecret, 1700000000)
	// 100s old, 60s tolerance -> reject.
	if err := Verify(body, sig, testSecret, 1700000100, 60*time.Second); err == nil {
		t.Error("stale signature accepted within tolerance")
	}
	// 10s old, 60s tolerance -> accept.
	if err := Verify(body, sig, testSecret, 1700000010, 60*time.Second); err != nil {
		t.Errorf("fresh signature rejected: %v", err)
	}
}

func TestVerifyMalformedHeader(t *testing.T) {
	body := []byte(`x`)
	for _, bad := range []string{"", "t=1700000000", "v1=abc", "garbage"} {
		if err := Verify(body, bad, testSecret, 1700000000, 0); err == nil {
			t.Errorf("malformed header accepted: %q", bad)
		}
	}
}

func TestPrefixForSecret(t *testing.T) {
	if got := prefixForSecret("abc"); got != "whsec_abc" {
		t.Errorf("got %q", got)
	}
	if got := prefixForSecret("whsec_abc"); got != "whsec_abc" {
		t.Errorf("got %q", got)
	}
	if got := prefixForSecret(""); got != "" {
		t.Errorf("got %q", got)
	}
}
