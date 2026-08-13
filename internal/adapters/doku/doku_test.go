package doku

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stripe-compatible-facade/internal/gateway"
	stripe "github.com/stripe/stripe-go/v81"
)

const (
	testClientID = "MCH-0001-TEST"
	testSecret   = "SK-TEST-SECRET"
)

// digest2/sign2 are INDEPENDENT reimplementations of DOKU's algorithm (separate
// code from signature.go) so the tests cross-check the adapter rather than call
// its own signing to validate itself.
func digest2(body []byte) string {
	s := sha256.Sum256(body)
	return base64.StdEncoding.EncodeToString(s[:])
}

func sign2(cid, rid, ts, target, dgst, secret string) string {
	c := fmt.Sprintf("Client-Id:%s\nRequest-Id:%s\nRequest-Timestamp:%s\nRequest-Target:%s\nDigest:%s",
		cid, rid, ts, target, dgst)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(c))
	return "HMACSHA256=" + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func TestCanonicalFormat(t *testing.T) {
	got := canonical("CID", "RID", "TS", "/t", "DIG")
	want := "Client-Id:CID\nRequest-Id:RID\nRequest-Timestamp:TS\nRequest-Target:/t\nDigest:DIG"
	if got != want {
		t.Errorf("canonical = %q; want %q", got, want)
	}
}

func TestVerifyRoundTrip(t *testing.T) {
	sig := sign(canonical(testClientID, "rid", "ts", "/x", "d"), testSecret)
	if !verify(sig, testClientID, "rid", "ts", "/x", "d", testSecret) {
		t.Error("verify rejected a valid signature")
	}
	if verify(sig, testClientID, "rid", "ts", "/x", "tampered", testSecret) {
		t.Error("verify accepted a tampered digest")
	}
	if verify(sig, testClientID, "rid", "ts", "/x", "d", "wrong-secret") {
		t.Error("verify accepted the wrong secret")
	}
}

func TestUUIDv4(t *testing.T) {
	u := uuidv4()
	if len(u) != 36 || u[8] != '-' || u[13] != '-' || u[14] != '4' {
		t.Errorf("uuidv4() = %q; not a v4 UUID", u)
	}
	if uuidv4() == uuidv4() {
		t.Error("uuidv4() collided")
	}
}

func TestStatusMapping(t *testing.T) {
	cases := []struct {
		in   string
		want stripe.PaymentIntentStatus
	}{
		{"SUCCESS", stripe.PaymentIntentStatusSucceeded},
		{"PENDING", stripe.PaymentIntentStatusRequiresAction},
		{"FAILED", stripe.PaymentIntentStatusRequiresPaymentMethod},
		{"success", stripe.PaymentIntentStatusSucceeded},
		{"WEIRD", stripe.PaymentIntentStatusRequiresAction},
	}
	for _, c := range cases {
		if got := mapStatus(c.in); got != c.want {
			t.Errorf("mapStatus(%q) = %s; want %s", c.in, got, c.want)
		}
	}
}

func TestCreatePayment(t *testing.T) {
	var gotBody map[string]any
	var gotSig, gotCID, gotDigest string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/checkout/v1/payment" {
			t.Errorf("request = %s %s; want POST /checkout/v1/payment", r.Method, r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		gotSig = r.Header.Get("Signature")
		gotCID = r.Header.Get("Client-Id")
		gotDigest = r.Header.Get("Digest")

		// Independently verify the adapter's signature against the received body.
		want := sign2(testClientID, r.Header.Get("Request-Id"), r.Header.Get("Request-Timestamp"),
			"/checkout/v1/payment", digest2(raw), testSecret)
		if gotSig != want {
			t.Errorf("outbound Signature %q != independently computed %q", gotSig, want)
		}
		if gotDigest != digest2(raw) {
			t.Errorf("Digest header %q != recomputed %q", gotDigest, digest2(raw))
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"payment": map[string]any{
				"url":      "https://sandbox.doku.com/checkout-link-v2/abc",
				"token_id": "tok_123",
			},
		})
	}))
	defer srv.Close()

	a := newForTest(testClientID, testSecret, srv.URL, nil)
	res, err := a.CreatePayment(context.Background(), &gateway.CreatePaymentInput{
		Reference:   "pi_test1",
		AmountMinor: 75000,
		Currency:    "idr",
		ReturnURL:   "https://app.test/done",
	})
	if err != nil {
		t.Fatalf("CreatePayment: %v", err)
	}
	if gotCID != testClientID {
		t.Errorf("Client-Id = %q; want %q", gotCID, testClientID)
	}
	if !strings.HasPrefix(gotSig, "HMACSHA256=") {
		t.Errorf("Signature not HMACSHA256= prefixed: %q", gotSig)
	}
	order, _ := gotBody["order"].(map[string]any)
	if order["invoice_number"] != "pi_test1" || order["amount"] != float64(75000) {
		t.Errorf("order = %v; want invoice_number=pi_test1 amount=75000", order)
	}
	if order["currency"] != "IDR" {
		t.Errorf("currency = %v; want IDR", order["currency"])
	}
	if res.Status != stripe.PaymentIntentStatusRequiresAction {
		t.Errorf("status = %s; want requires_action", res.Status)
	}
	if res.NextAction == nil || res.NextAction.RedirectToURL == nil ||
		res.NextAction.RedirectToURL.URL != "https://sandbox.doku.com/checkout-link-v2/abc" {
		t.Errorf("redirect URL mismatch: %+v", res.NextAction)
	}
}

func TestCreatePaymentValidation(t *testing.T) {
	a := newForTest(testClientID, testSecret, "http://unused", nil)
	if _, err := a.CreatePayment(context.Background(), nil); err == nil {
		t.Error("nil input should error")
	}
	if _, err := a.CreatePayment(context.Background(), &gateway.CreatePaymentInput{AmountMinor: 100}); err == nil {
		t.Error("missing reference should error")
	}
	if _, err := a.CreatePayment(context.Background(), &gateway.CreatePaymentInput{Reference: "pi_"}); err == nil {
		t.Error("non-positive amount should error")
	}
}

func notificationReq(t *testing.T, invoice, status, cid, rid, ts, secret string, tamperSig bool) *http.Request {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"order":       map[string]any{"invoice_number": invoice, "amount": 75000},
		"transaction": map[string]any{"status": status},
	})
	dgst := digest2(body)
	sig := sign2(cid, rid, ts, "/v1/webhooks/doku", dgst, secret)
	if tamperSig {
		sig += "deadbeef"
	}
	req, err := http.NewRequest(http.MethodPost, "/v1/webhooks/doku", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Client-Id", cid)
	req.Header.Set("Request-Id", rid)
	req.Header.Set("Request-Timestamp", ts)
	req.Header.Set("Signature", sig)
	return req
}

func TestParseWebhookValid(t *testing.T) {
	a := newForTest(testClientID, testSecret, "", nil)
	freshTS := time.Now().Format(time.RFC3339)
	req := notificationReq(t, "pi_test1", "SUCCESS", testClientID, "rid-1", freshTS, testSecret, false)
	events, err := a.ParseWebhook(context.Background(), req)
	if err != nil {
		t.Fatalf("ParseWebhook: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events; want 1", len(events))
	}
	ev := events[0]
	if ev.Type != "payment_intent.succeeded" {
		t.Errorf("type = %q; want payment_intent.succeeded", ev.Type)
	}
	if ev.Reference != "pi_test1" {
		t.Errorf("reference = %q; want pi_test1", ev.Reference)
	}
	if ev.Status != stripe.PaymentIntentStatusSucceeded {
		t.Errorf("status = %s; want succeeded", ev.Status)
	}
}

func TestParseWebhookFailed(t *testing.T) {
	a := newForTest(testClientID, testSecret, "", nil)
	freshTS := time.Now().Format(time.RFC3339)
	req := notificationReq(t, "pi_test1", "FAILED", testClientID, "rid-2", freshTS, testSecret, false)
	events, _ := a.ParseWebhook(context.Background(), req)
	if events[0].Type != "payment_intent.payment_failed" {
		t.Errorf("type = %q; want payment_intent.payment_failed", events[0].Type)
	}
	if events[0].Status != stripe.PaymentIntentStatusRequiresPaymentMethod {
		t.Errorf("status = %s; want requires_payment_method", events[0].Status)
	}
}

func TestParseWebhookBadSignature(t *testing.T) {
	a := newForTest(testClientID, testSecret, "", nil)
	freshTS := time.Now().Format(time.RFC3339)
	req := notificationReq(t, "pi_test1", "SUCCESS", testClientID, "rid-3", freshTS, testSecret, true)
	_, err := a.ParseWebhook(context.Background(), req)
	if !errors.Is(err, gateway.ErrInvalidSignature) {
		t.Errorf("err = %v; want ErrInvalidSignature", err)
	}
}

func TestParseWebhookWrongSecret(t *testing.T) {
	a := newForTest(testClientID, "real-secret", "", nil)
	// Signed with a different secret than the adapter expects -> must reject.
	req := notificationReq(t, "pi_test1", "SUCCESS", testClientID, "rid-4", time.Now().Format(time.RFC3339), "other-secret", false)
	_, err := a.ParseWebhook(context.Background(), req)
	if !errors.Is(err, gateway.ErrInvalidSignature) {
		t.Errorf("err = %v; want ErrInvalidSignature", err)
	}
}

func TestParseWebhookMissingSignature(t *testing.T) {
	a := newForTest(testClientID, testSecret, "", nil)
	body, _ := json.Marshal(map[string]any{
		"order":       map[string]any{"invoice_number": "pi_test1", "amount": 1},
		"transaction": map[string]any{"status": "SUCCESS"},
	})
	req, _ := http.NewRequest(http.MethodPost, "/v1/webhooks/doku", bytes.NewReader(body))
	_, err := a.ParseWebhook(context.Background(), req)
	if !errors.Is(err, gateway.ErrInvalidSignature) {
		t.Errorf("err = %v; want ErrInvalidSignature", err)
	}
}

func TestGetStatusUnsupported(t *testing.T) {
	a := newForTest(testClientID, testSecret, "", nil)
	if _, err := a.GetStatus(context.Background(), "x"); err == nil {
		t.Error("GetStatus should error (push-only)")
	}
}

func TestRefundUnsupported(t *testing.T) {
	a := newForTest(testClientID, testSecret, "", nil)
	if _, err := a.Refund(context.Background(), &gateway.RefundInput{PaymentReference: "x"}); err == nil {
		t.Error("Refund should error (not supported)")
	}
}
