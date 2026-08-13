package xendit

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stripe-compatible-facade/internal/gateway"
	stripe "github.com/stripe/stripe-go/v81"
)

const (
	testSecret = "xnd_development_test_secret"
	testToken  = "xnd_webhook_token_abc123"
)

func TestVerifyCallbackToken(t *testing.T) {
	if verifyCallbackToken("anything", "") {
		t.Error("empty expected token should never verify")
	}
	if verifyCallbackToken("", "") {
		t.Error("both-empty should never verify")
	}
	if !verifyCallbackToken(testToken, testToken) {
		t.Error("matching token should verify")
	}
	if verifyCallbackToken("wrong", testToken) {
		t.Error("mismatched token should not verify")
	}
}

func TestStatusMapping(t *testing.T) {
	cases := []struct {
		in   string
		want stripe.PaymentIntentStatus
	}{
		{"PENDING", stripe.PaymentIntentStatusRequiresAction},
		{"PAID", stripe.PaymentIntentStatusSucceeded},
		{"SETTLED", stripe.PaymentIntentStatusSucceeded},
		{"EXPIRED", stripe.PaymentIntentStatusCanceled},
		{"pending", stripe.PaymentIntentStatusRequiresAction}, // case-insensitive
		{"paid", stripe.PaymentIntentStatusSucceeded},
		{"WEIRD", stripe.PaymentIntentStatusRequiresAction}, // unknown fallback
	}
	for _, c := range cases {
		if got := mapStatus(c.in); got != c.want {
			t.Errorf("mapStatus(%q) = %s; want %s", c.in, got, c.want)
		}
	}
}

func TestStripeEventType(t *testing.T) {
	cases := map[stripe.PaymentIntentStatus]string{
		stripe.PaymentIntentStatusSucceeded:             "payment_intent.succeeded",
		stripe.PaymentIntentStatusCanceled:              "payment_intent.canceled",
		stripe.PaymentIntentStatusRequiresPaymentMethod: "payment_intent.payment_failed",
		stripe.PaymentIntentStatusRequiresAction:        "payment_intent.requires_action",
	}
	for status, want := range cases {
		if got := stripeEventType(status); got != want {
			t.Errorf("stripeEventType(%s) = %q; want %q", status, got, want)
		}
	}
}

func TestCreatePayment(t *testing.T) {
	var gotPath, gotMethod, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":          "inv_123",
			"external_id": gotBody["external_id"],
			"invoice_url": "https://checkout.xendit.co/inv_123",
			"status":      "PENDING",
			"amount":      gotBody["amount"],
		})
	}))
	defer srv.Close()

	a := newForTest(testSecret, testToken, srv.URL, nil)
	res, err := a.CreatePayment(context.Background(), &gateway.CreatePaymentInput{
		Reference:   "pi_test1",
		AmountMinor: 75000,
		Currency:    "idr",
		Description: "Test order",
		ReturnURL:   "https://app.test/done",
	})
	if err != nil {
		t.Fatalf("CreatePayment: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/v2/invoices" {
		t.Errorf("request = %s %s; want POST /v2/invoices", gotMethod, gotPath)
	}
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte(testSecret+":"))
	if gotAuth != wantAuth {
		t.Errorf("auth = %q; want %q", gotAuth, wantAuth)
	}
	if gotBody["external_id"] != "pi_test1" || gotBody["amount"] != float64(75000) {
		t.Errorf("request body = %v; want external_id=pi_test1 amount=75000", gotBody)
	}
	if gotBody["currency"] != "IDR" {
		t.Errorf("currency = %v; want IDR", gotBody["currency"])
	}
	if gotBody["success_redirect_url"] != "https://app.test/done" {
		t.Errorf("missing success_redirect_url: %v", gotBody)
	}
	if res.Status != stripe.PaymentIntentStatusRequiresAction {
		t.Errorf("status = %s; want requires_action", res.Status)
	}
	if res.GatewayReference != "inv_123" {
		t.Errorf("gateway ref = %q; want inv_123", res.GatewayReference)
	}
	if res.NextAction == nil || res.NextAction.RedirectToURL == nil ||
		res.NextAction.RedirectToURL.URL != "https://checkout.xendit.co/inv_123" {
		t.Errorf("next_action redirect URL mismatch: %+v", res.NextAction)
	}
}

func TestCreatePaymentValidation(t *testing.T) {
	a := newForTest(testSecret, testToken, "http://unused", nil)
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

func TestGetStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v2/invoices/inv_123" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":          "inv_123",
			"external_id": "pi_test1",
			"status":      "SETTLED",
			"amount":      75000,
		})
	}))
	defer srv.Close()

	a := newForTest(testSecret, testToken, srv.URL, nil)
	res, err := a.GetStatus(context.Background(), "inv_123")
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if res.Status != stripe.PaymentIntentStatusSucceeded {
		t.Errorf("status = %s; want succeeded", res.Status)
	}
	if res.Reference != "pi_test1" {
		t.Errorf("reference = %q; want pi_test1", res.Reference)
	}
}

func TestRefund(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/refunds" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     "rfd_456",
			"status": "SUCCEEDED",
			"amount": gotBody["amount"],
		})
	}))
	defer srv.Close()

	a := newForTest(testSecret, testToken, srv.URL, nil)
	res, err := a.Refund(context.Background(), &gateway.RefundInput{
		Reference:        "re_test1",
		PaymentReference: "inv_123",
		AmountMinor:      75000,
		Reason:           "customer request",
	})
	if err != nil {
		t.Fatalf("Refund: %v", err)
	}
	if gotBody["payment_id"] != "inv_123" {
		t.Errorf("payment_id = %v; want inv_123", gotBody["payment_id"])
	}
	if gotBody["external_id"] != "re_test1" {
		t.Errorf("external_id = %v; want re_test1", gotBody["external_id"])
	}
	if res.Status != "succeeded" {
		t.Errorf("status = %q; want succeeded", res.Status)
	}
	if res.GatewayReference != "rfd_456" {
		t.Errorf("gateway ref = %q; want rfd_456", res.GatewayReference)
	}
}

func TestParseWebhookValid(t *testing.T) {
	a := newForTest(testSecret, testToken, "", nil)
	body, _ := json.Marshal(map[string]any{
		"id":          "inv_123",
		"external_id": "pi_test1",
		"status":      "PAID",
		"amount":      75000,
		"paid_amount": 75000,
	})
	events, err := a.ParseWebhook(context.Background(), webhookReq(t, body, testToken))
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

func TestParseWebhookExpired(t *testing.T) {
	a := newForTest(testSecret, testToken, "", nil)
	body, _ := json.Marshal(map[string]any{"external_id": "pi_test1", "status": "EXPIRED"})
	events, err := a.ParseWebhook(context.Background(), webhookReq(t, body, testToken))
	if err != nil {
		t.Fatalf("ParseWebhook: %v", err)
	}
	if events[0].Type != "payment_intent.canceled" {
		t.Errorf("type = %q; want payment_intent.canceled", events[0].Type)
	}
	if events[0].Status != stripe.PaymentIntentStatusCanceled {
		t.Errorf("status = %s; want canceled", events[0].Status)
	}
}

func TestParseWebhookBadToken(t *testing.T) {
	a := newForTest(testSecret, testToken, "", nil)
	body, _ := json.Marshal(map[string]any{"external_id": "pi_test1", "status": "PAID"})
	_, err := a.ParseWebhook(context.Background(), webhookReq(t, body, "wrong-token"))
	if !errors.Is(err, gateway.ErrInvalidSignature) {
		t.Errorf("err = %v; want ErrInvalidSignature", err)
	}
}

func TestParseWebhookMissingToken(t *testing.T) {
	a := newForTest(testSecret, testToken, "", nil)
	body, _ := json.Marshal(map[string]any{"external_id": "pi_test1", "status": "PAID"})
	_, err := a.ParseWebhook(context.Background(), webhookReq(t, body, ""))
	if !errors.Is(err, gateway.ErrInvalidSignature) {
		t.Errorf("err = %v; want ErrInvalidSignature", err)
	}
}

func TestParseWebhookUnconfiguredToken(t *testing.T) {
	// A merchant who forgot to set XENDIT_WEBHOOK_TOKEN must never accept callbacks.
	a := newForTest(testSecret, "", "", nil)
	body, _ := json.Marshal(map[string]any{"external_id": "pi_test1", "status": "PAID"})
	_, err := a.ParseWebhook(context.Background(), webhookReq(t, body, ""))
	if !errors.Is(err, gateway.ErrInvalidSignature) {
		t.Errorf("err = %v; want ErrInvalidSignature", err)
	}
}

// webhookReq builds a POST request carrying body and an X-Callback-Token for
// ParseWebhook. An empty token leaves the header value empty (mirrors missing).
func webhookReq(t *testing.T, body []byte, token string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "/v1/webhooks/xendit", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Callback-Token", token)
	return req
}
