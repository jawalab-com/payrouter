package midtrans

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stripe-compatible-facade/internal/gateway"
	stripe "github.com/stripe/stripe-go/v81"
)

const testServerKey = "SB-Mid-server-TEST"

// --- Pure logic -------------------------------------------------------------

func TestStatusMapping(t *testing.T) {
	cases := []struct {
		tx, fraud string
		want      stripe.PaymentIntentStatus
	}{
		{"settlement", "accept", stripe.PaymentIntentStatusSucceeded},
		{"settlement", "", stripe.PaymentIntentStatusSucceeded},
		{"capture", "accept", stripe.PaymentIntentStatusSucceeded},
		{"capture", "", stripe.PaymentIntentStatusSucceeded},
		{"capture", "deny", stripe.PaymentIntentStatusRequiresPaymentMethod},
		{"pending", "", stripe.PaymentIntentStatusRequiresAction},
		{"authorize", "", stripe.PaymentIntentStatusRequiresCapture},
		{"deny", "", stripe.PaymentIntentStatusRequiresPaymentMethod},
		{"failure", "", stripe.PaymentIntentStatusRequiresPaymentMethod},
		{"cancel", "", stripe.PaymentIntentStatusCanceled},
		{"expire", "", stripe.PaymentIntentStatusCanceled},
		{"refund", "", stripe.PaymentIntentStatusSucceeded},
		{"partial_refund", "", stripe.PaymentIntentStatusSucceeded},
		{"something_unknown", "", stripe.PaymentIntentStatusRequiresAction},
	}
	for _, c := range cases {
		got := mapStatus(c.tx, c.fraud)
		if got != c.want {
			t.Errorf("mapStatus(%q,%q) = %s; want %s", c.tx, c.fraud, got, c.want)
		}
	}
}

func TestStripeEventType(t *testing.T) {
	cases := map[stripe.PaymentIntentStatus]string{
		stripe.PaymentIntentStatusSucceeded:             "payment_intent.succeeded",
		stripe.PaymentIntentStatusRequiresPaymentMethod: "payment_intent.payment_failed",
		stripe.PaymentIntentStatusCanceled:              "payment_intent.canceled",
		stripe.PaymentIntentStatusRequiresAction:        "payment_intent.requires_action",
	}
	for status, want := range cases {
		if got := stripeEventType(status); got != want {
			t.Errorf("stripeEventType(%s) = %q; want %q", status, got, want)
		}
	}
}

// TestSignatureVector validates the digest against an independently computed
// SHA-512 (see commit message), so the test is not merely self-referential.
//
//	echo -n 'order-120010000.00SB-Mid-server-TEST' | sha512sum
func TestSignatureVector(t *testing.T) {
	got := computeSignature("order-1", "200", "10000.00", testServerKey)
	const want = "31cc2fddabb6230d5d3aaf25b9f6a7a4822c7cacfd115748f0604e71c8f24aa3898e3b0764c0f0072fb2df33263fbc64bb0eb072be6eabee4739ac681aea27f3"
	if got != want {
		t.Errorf("computeSignature digest mismatch:\n got %s\nwant %s", got, want)
	}
}

func TestVerifySignature(t *testing.T) {
	sig := computeSignature("order-1", "200", "10000.00", testServerKey)
	if !verifySignature(sig, "order-1", "200", "10000.00", testServerKey) {
		t.Error("valid signature rejected")
	}
	if verifySignature(sig, "order-1", "200", "10000.00", "wrong-server-key") {
		t.Error("wrong server key accepted")
	}
	if verifySignature("tampered", "order-1", "200", "10000.00", testServerKey) {
		t.Error("tampered signature accepted")
	}
	if verifySignature("", "order-1", "200", "10000.00", testServerKey) {
		t.Error("empty signature accepted")
	}
	// gross_amount must be signed exactly as received (incl. decimals).
	if verifySignature(computeSignature("order-1", "200", "10000", testServerKey), "order-1", "200", "10000.00", testServerKey) {
		t.Error("signature accepted despite gross_amount decimal mismatch")
	}
}

func TestEnabledPayments(t *testing.T) {
	cases := []struct {
		name   string
		pm     gateway.IDPaymentMethodType
		params map[string]any
		want   []string
	}{
		{"va bca", gateway.IDVirtualAccount, map[string]any{"bank": "bca"}, []string{"bca_va"}},
		{"va mandiri", gateway.IDVirtualAccount, map[string]any{"bank": "mandiri"}, []string{"echannel"}},
		{"va any", gateway.IDVirtualAccount, nil, []string{"bca_va", "bni_va", "bri_va", "permata_va", "echannel"}},
		{"qris", gateway.IDQRIS, nil, []string{"qris"}},
		{"ewallet gopay", gateway.IDEWallet, map[string]any{"wallet": "gopay"}, []string{"gopay"}},
		{"ewallet default", gateway.IDEWallet, nil, []string{"gopay"}},
		{"retail alfamart", gateway.IDRetail, map[string]any{"store": "alfamart"}, []string{"alfamart"}},
		{"card", gateway.IDCard, nil, nil},
	}
	for _, c := range cases {
		got := enabledPayments(c.pm, c.params)
		if !equalStrings(got, c.want) {
			t.Errorf("%s: got %v; want %v", c.name, got, c.want)
		}
	}
}

func TestGrossAmountMinor(t *testing.T) {
	if got := grossAmountMinor("10000.00"); got != 10000 {
		t.Errorf("grossAmountMinor(10000.00) = %d; want 10000", got)
	}
	if got := grossAmountMinor("5539"); got != 5539 {
		t.Errorf("grossAmountMinor(5539) = %d; want 5539", got)
	}
}

func TestCustomerDetails(t *testing.T) {
	if got := customerDetails(nil); got != nil {
		t.Errorf("nil customer should yield nil, got %v", got)
	}
	cd := customerDetails(&gateway.Customer{Name: "Budi Santoso", Email: "budi@example.com", Phone: "+6281234567890"})
	if cd["first_name"] != "Budi" || cd["last_name"] != "Santoso" {
		t.Errorf("name split wrong: %v", cd)
	}
	if cd["email"] != "budi@example.com" {
		t.Errorf("email wrong: %v", cd)
	}
}

// --- HTTP paths against httptest (no real network) --------------------------

// newTestServer stands in for both Snap and Core API; it captures the last
// request and routes by path. The returned urlSnap/apiBase point at the server.
func newTestServer(t *testing.T, h http.HandlerFunc) (*Adapter, *httptest.Server) {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return newForTest(testServerKey, ts.URL, ts.URL, ts.Client()), ts
}

func TestCreatePaymentSnap(t *testing.T) {
	var gotBody map[string]any
	a, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != snapPath {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if ah := r.Header.Get("Authorization"); !strings.HasPrefix(ah, "Basic ") {
			t.Errorf("missing Basic auth: %q", ah)
		}
		buf, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(buf, &gotBody)
		writeJSON(w, snapResponse{Token: "snap-token-123", RedirectURL: "https://snap.local/vtweb/abc"})
	})

	res, err := a.CreatePayment(context.Background(), &gateway.CreatePaymentInput{
		Reference:         "pi_order1",
		AmountMinor:       50000,
		Currency:          "idr",
		PaymentMethodType: gateway.IDVirtualAccount,
		MethodParams:      map[string]any{"bank": "bca"},
		ReturnURL:         "https://app.test/done",
	})
	if err != nil {
		t.Fatalf("CreatePayment: %v", err)
	}

	td, _ := gotBody["transaction_details"].(map[string]any)
	if td["order_id"] != "pi_order1" || td["gross_amount"] != float64(50000) {
		t.Errorf("transaction_details wrong: %v", td)
	}
	if eps, _ := gotBody["enabled_payments"].([]any); len(eps) != 1 || eps[0] != "bca_va" {
		t.Errorf("enabled_payments wrong: %v", gotBody["enabled_payments"])
	}
	if cb, _ := gotBody["callbacks"].(map[string]any); cb["finish_url"] != "https://app.test/done" {
		t.Errorf("callbacks wrong: %v", gotBody["callbacks"])
	}

	if res.Status != stripe.PaymentIntentStatusRequiresAction {
		t.Errorf("status = %s; want requires_action", res.Status)
	}
	if res.GatewayReference != "snap-token-123" {
		t.Errorf("gateway ref = %q; want snap-token-123", res.GatewayReference)
	}
	if res.NextAction == nil || res.NextAction.RedirectToURL == nil ||
		res.NextAction.RedirectToURL.URL != "https://snap.local/vtweb/abc" {
		t.Errorf("next_action wrong: %+v", res.NextAction)
	}
}

func TestGetStatus(t *testing.T) {
	a, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v2/pi_order1/status" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		writeJSON(w, statusResponse{
			StatusCode: "200", TransactionID: "tx-abc", OrderID: "pi_order1",
			GrossAmount: "50000.00", TransactionStatus: "settlement", FraudStatus: "accept",
		})
	})

	res, err := a.GetStatus(context.Background(), "pi_order1")
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if res.Status != stripe.PaymentIntentStatusSucceeded {
		t.Errorf("status = %s; want succeeded", res.Status)
	}
	if res.GatewayReference != "tx-abc" {
		t.Errorf("gateway ref = %q; want tx-abc", res.GatewayReference)
	}
}

func TestRefundPartial(t *testing.T) {
	var gotBody map[string]any
	a, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v2/pi_order1/refund" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		buf, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(buf, &gotBody)
		writeJSON(w, refundResponse{
			StatusCode: "200", TransactionID: "tx-abc", GrossAmount: "50000.00",
			TransactionStatus: "partial_refund",
		})
	})

	res, err := a.Refund(context.Background(), &gateway.RefundInput{
		Reference:        "re_1",
		PaymentReference: "pi_order1",
		AmountMinor:      20000,
		Reason:           "requested_by_customer",
	})
	if err != nil {
		t.Fatalf("Refund: %v", err)
	}
	if gotBody["amount"] != float64(20000) {
		t.Errorf("refund amount body wrong: %v", gotBody["amount"])
	}
	if gotBody["reason"] != "requested_by_customer" {
		t.Errorf("refund reason body wrong: %v", gotBody["reason"])
	}
	if res.Status != "succeeded" || res.AmountMinor != 20000 {
		t.Errorf("refund result = %+v", res)
	}
}

func TestRefundFullUsesGrossAmount(t *testing.T) {
	a, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, refundResponse{
			StatusCode: "200", TransactionID: "tx-abc", GrossAmount: "50000.00",
		})
	})
	res, err := a.Refund(context.Background(), &gateway.RefundInput{
		Reference: "re_1", PaymentReference: "pi_order1", AmountMinor: 0,
	})
	if err != nil {
		t.Fatalf("Refund: %v", err)
	}
	if res.AmountMinor != 50000 {
		t.Errorf("full refund amount = %d; want 50000", res.AmountMinor)
	}
}

func TestHTTPErrorSurfaces(t *testing.T) {
	a, _ := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error_messages":["boom"]}`, http.StatusUnauthorized)
	})
	_, err := a.GetStatus(context.Background(), "pi_order1")
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("expected HTTP 401 error, got %v", err)
	}
}

// --- ParseWebhook -----------------------------------------------------------

func notificationBody(t *testing.T, tx, fraud string) []byte {
	t.Helper()
	sig := computeSignature("pi_order1", "200", "50000.00", testServerKey)
	b, err := json.Marshal(notification{
		StatusCode: "200", TransactionID: "tx-abc", OrderID: "pi_order1",
		GrossAmount: "50000.00", TransactionStatus: tx, FraudStatus: fraud,
		PaymentType: "qris", SignatureKey: sig,
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseWebhookValid(t *testing.T) {
	a := newForTest(testServerKey, "", "", nil)
	events, err := a.ParseWebhook(context.Background(), webhookReq(t, notificationBody(t, "settlement", "accept")))
	if err != nil {
		t.Fatalf("ParseWebhook: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events; want 1", len(events))
	}
	ev := events[0]
	if ev.Status != stripe.PaymentIntentStatusSucceeded {
		t.Errorf("status = %s; want succeeded", ev.Status)
	}
	if ev.Type != "payment_intent.succeeded" {
		t.Errorf("type = %q; want payment_intent.succeeded", ev.Type)
	}
	if ev.Reference != "pi_order1" {
		t.Errorf("reference = %q; want pi_order1", ev.Reference)
	}
	if !strings.Contains(string(ev.ObjectJSON), `"status":"succeeded"`) {
		t.Errorf("object payload missing succeeded status: %s", ev.ObjectJSON)
	}
	if !strings.Contains(string(ev.ObjectJSON), `"amount":50000`) {
		t.Errorf("object payload missing amount: %s", ev.ObjectJSON)
	}
}

func TestParseWebhookBadSignature(t *testing.T) {
	a := newForTest(testServerKey, "", "", nil)
	// Same valid notification, but the signature replaced with garbage.
	sig := computeSignature("pi_order1", "200", "50000.00", testServerKey)
	bad := strings.Replace(string(notificationBody(t, "settlement", "accept")), sig, sig+"deadbeef", 1)
	if bad == string(notificationBody(t, "settlement", "accept")) {
		t.Fatal("test setup failed: signature not embedded in body")
	}
	_, err := a.ParseWebhook(context.Background(), webhookReq(t, []byte(bad)))
	if err == nil || !strings.Contains(err.Error(), "invalid webhook signature") {
		t.Fatalf("expected signature error, got %v", err)
	}
}

func TestParseWebhookMalformedJSON(t *testing.T) {
	a := newForTest(testServerKey, "", "", nil)
	if _, err := a.ParseWebhook(context.Background(), webhookReq(t, []byte("{not json"))); err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

// --- helpers ----------------------------------------------------------------

// webhookReq builds a POST request carrying body for ParseWebhook.
func webhookReq(t *testing.T, body []byte) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "/v1/webhooks/midtrans", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
