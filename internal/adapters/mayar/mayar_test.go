package mayar

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jawalab-com/payrouter/internal/gateway"
	stripe "github.com/stripe/stripe-go/v81"
)

const (
	testAPIKey = "mayar_api_key_test"
	testToken  = "mayar_webhook_secret_xyz"
)

func TestVerifyToken(t *testing.T) {
	if verifyToken("anything", "") {
		t.Error("empty expected token should never verify")
	}
	if !verifyToken(testToken, testToken) {
		t.Error("matching token should verify")
	}
	if verifyToken("wrong", testToken) {
		t.Error("mismatched token should not verify")
	}
	if verifyToken("", "") {
		t.Error("both-empty should never verify")
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
			"statusCode": 200,
			"messages":   "success",
			"data": map[string]any{
				"id":            "e890d24a-cfc0-4915-83d2-3166b9ffba9e",
				"transactionId": "040d5adb-1496-45de-8435-5cab16526a8c",
				"link":          "https://andiak.myr.id/invoices/ohsjrd3wko",
			},
		})
	}))
	defer srv.Close()

	a := newForTest(testAPIKey, testToken, srv.URL, nil)
	res, err := a.CreatePayment(context.Background(), &gateway.CreatePaymentInput{
		Reference:   "pi_test1",
		AmountMinor: 75000,
		Currency:    "idr",
		Description: "Test order",
		ReturnURL:   "https://app.test/done",
		Customer:    &gateway.Customer{Name: "Arief", Email: "a@b.test", Phone: "0812000000"},
	})
	if err != nil {
		t.Fatalf("CreatePayment: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/hl/v1/payment/create" {
		t.Errorf("request = %s %s; want POST /hl/v1/payment/create", gotMethod, gotPath)
	}
	if gotAuth != "Bearer "+testAPIKey {
		t.Errorf("auth = %q; want Bearer %s", gotAuth, testAPIKey)
	}
	if gotBody["amount"] != float64(75000) {
		t.Errorf("amount = %v; want 75000", gotBody["amount"])
	}
	if gotBody["redirectUrl"] != "https://app.test/done" {
		t.Errorf("redirectUrl = %v; want app.test/done", gotBody["redirectUrl"])
	}
	if gotBody["name"] != "Arief" || gotBody["email"] != "a@b.test" || gotBody["mobile"] != "0812000000" {
		t.Errorf("customer fields missing: %v", gotBody)
	}
	if res.Status != stripe.PaymentIntentStatusRequiresAction {
		t.Errorf("status = %s; want requires_action", res.Status)
	}
	if res.GatewayReference != "e890d24a-cfc0-4915-83d2-3166b9ffba9e" {
		t.Errorf("gateway ref = %q; want Mayar data.id", res.GatewayReference)
	}
	if res.NextAction == nil || res.NextAction.RedirectToURL == nil ||
		res.NextAction.RedirectToURL.URL != "https://andiak.myr.id/invoices/ohsjrd3wko" {
		t.Errorf("redirect URL mismatch: %+v", res.NextAction)
	}
}

func TestCreatePaymentValidation(t *testing.T) {
	a := newForTest(testAPIKey, testToken, "http://unused", nil)
	if _, err := a.CreatePayment(context.Background(), nil); err == nil {
		t.Error("nil input should error")
	}
	if _, err := a.CreatePayment(context.Background(), &gateway.CreatePaymentInput{Reference: "pi_"}); err == nil {
		t.Error("non-positive amount should error")
	}
}

func callbackReq(t *testing.T, query string, body []byte) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "/v1/webhooks/mayar?"+query, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestParseWebhookPaid(t *testing.T) {
	a := newForTest(testAPIKey, testToken, "", nil)
	body, _ := json.Marshal(map[string]any{
		"event": "payment.received",
		"data":  map[string]any{"id": "e890d24a", "status": true, "amount": 75000},
	})
	req := callbackReq(t, "token="+testToken, body)
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
	if ev.Reference != "e890d24a" {
		t.Errorf("reference = %q; want Mayar data.id", ev.Reference)
	}
	if ev.Status != stripe.PaymentIntentStatusSucceeded {
		t.Errorf("status = %s; want succeeded", ev.Status)
	}
}

func TestParseWebhookBadToken(t *testing.T) {
	a := newForTest(testAPIKey, testToken, "", nil)
	body, _ := json.Marshal(map[string]any{"event": "payment.received", "data": map[string]any{"id": "x", "status": true}})
	req := callbackReq(t, "token=WRONG", body)
	_, err := a.ParseWebhook(context.Background(), req)
	if !errors.Is(err, gateway.ErrInvalidSignature) {
		t.Errorf("err = %v; want ErrInvalidSignature", err)
	}
}

func TestParseWebhookMissingToken(t *testing.T) {
	a := newForTest(testAPIKey, testToken, "", nil)
	body, _ := json.Marshal(map[string]any{"event": "payment.received", "data": map[string]any{"id": "x", "status": true}})
	req := callbackReq(t, "", body) // no token query
	_, err := a.ParseWebhook(context.Background(), req)
	if !errors.Is(err, gateway.ErrInvalidSignature) {
		t.Errorf("err = %v; want ErrInvalidSignature", err)
	}
}

func TestParseWebhookNonPaymentEvent(t *testing.T) {
	a := newForTest(testAPIKey, testToken, "", nil)
	body, _ := json.Marshal(map[string]any{"event": "payment.reminder", "data": map[string]any{"id": "x", "status": true}})
	req := callbackReq(t, "token="+testToken, body)
	events, err := a.ParseWebhook(context.Background(), req)
	if err != nil {
		t.Fatalf("ParseWebhook: %v", err)
	}
	if events != nil {
		t.Errorf("non-payment event should yield no events; got %v", events)
	}
}

func TestParseWebhookAmbiguousStatus(t *testing.T) {
	// payment.received but status != true: ambiguous, so we do not flip state.
	a := newForTest(testAPIKey, testToken, "", nil)
	body, _ := json.Marshal(map[string]any{"event": "payment.received", "data": map[string]any{"id": "x", "status": false}})
	req := callbackReq(t, "token="+testToken, body)
	events, _ := a.ParseWebhook(context.Background(), req)
	if events != nil {
		t.Errorf("ambiguous status should yield no events; got %v", events)
	}
}

func TestGetStatusUnsupported(t *testing.T) {
	a := newForTest(testAPIKey, testToken, "", nil)
	if _, err := a.GetStatus(context.Background(), "x"); err == nil {
		t.Error("GetStatus should error (webhook-only)")
	}
}

func TestRefundUnsupported(t *testing.T) {
	a := newForTest(testAPIKey, testToken, "", nil)
	if _, err := a.Refund(context.Background(), &gateway.RefundInput{PaymentReference: "x"}); err == nil {
		t.Error("Refund should error (not supported)")
	}
}
