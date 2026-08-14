package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jawalab-com/payrouter/internal/adapters/stub"
	"github.com/jawalab-com/payrouter/internal/config"
	"github.com/jawalab-com/payrouter/internal/gateway"
	"github.com/jawalab-com/payrouter/internal/orchestrator"
	"github.com/jawalab-com/payrouter/internal/server"
	"github.com/jawalab-com/payrouter/internal/store"
	"github.com/jawalab-com/payrouter/internal/webhook"
	stripe "github.com/stripe/stripe-go/v81"
)

const testAPIKey = "sk_test_e2e_secret_token_123"

type testHarness struct {
	srv        *httptest.Server
	store      store.Store
	gateway    gateway.Gateway
	cfg        config.Config
	router     *orchestrator.Router
	webhookURL string
	receivedWH chan []byte
}

func setupE2EHarness(t *testing.T, customGW gateway.Gateway) *testHarness {
	t.Helper()

	cfg := config.Config{
		Addr:          ":0",
		APIKey:        testAPIKey,
		Livemode:      false,
		ActiveGateway: "stub",
		MaxBodyBytes:  1024 * 1024,
		RateLimitRPS:  0, // disabled in test for raw throughput
		CheckoutUI:    true,
		PublicURL:     "http://localhost:8787",
	}

	st := store.NewMemory()
	gw := customGW
	if gw == nil {
		gw = stub.New()
	}

	srvHandler := server.New(cfg, st, gw)
	ts := httptest.NewServer(srvHandler)

	t.Cleanup(func() {
		ts.Close()
	})

	return &testHarness{
		srv:     ts,
		store:   st,
		gateway: gw,
		cfg:     cfg,
	}
}

func (h *testHarness) request(t *testing.T, method, path string, form url.Values, authKey string) (*http.Response, map[string]any) {
	t.Helper()

	var reqBody io.Reader
	if form != nil {
		reqBody = strings.NewReader(form.Encode())
	}

	req, err := http.NewRequest(method, h.srv.URL+path, reqBody)
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}

	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	if authKey != "" {
		req.Header.Set("Authorization", "Bearer "+authKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HTTP request %s %s failed: %v", method, path, err)
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("failed to read response body: %v", err)
	}

	var jsonMap map[string]any
	if len(bodyBytes) > 0 && strings.HasPrefix(strings.TrimSpace(string(bodyBytes)), "{") {
		_ = json.Unmarshal(bodyBytes, &jsonMap)
	}

	return resp, jsonMap
}

func TestE2E_HealthCheck(t *testing.T) {
	h := setupE2EHarness(t, nil)

	// GET /healthz
	resp, body := h.request(t, "GET", "/healthz", nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected /healthz status 200, got %d", resp.StatusCode)
	}
	if body["status"] != "ok" {
		t.Fatalf("expected status=ok, got %v", body)
	}

	// GET /health/ready
	resp, body = h.request(t, "GET", "/health/ready", nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected /health/ready status 200, got %d", resp.StatusCode)
	}
	if body["status"] != "ready" {
		t.Fatalf("expected status=ready, got %v", body)
	}
}

func TestE2E_AuthAndSecurityBarrier(t *testing.T) {
	h := setupE2EHarness(t, nil)

	// 1. Missing Authorization header
	resp, body := h.request(t, "GET", "/v1/customers", nil, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated request, got %d", resp.StatusCode)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj["type"] != "authentication_error" && errObj["code"] != "authentication_required" {
		t.Fatalf("expected authentication error, got %v", errObj)
	}

	// 2. Wrong API Key
	resp, body = h.request(t, "GET", "/v1/customers", nil, "sk_test_wrong_key_xyz")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for wrong API key, got %d", resp.StatusCode)
	}

	// 3. Valid API Key passes
	resp, _ = h.request(t, "GET", "/v1/customers", nil, testAPIKey)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for valid API key, got %d", resp.StatusCode)
	}
}

func TestE2E_CustomerLifecycle(t *testing.T) {
	h := setupE2EHarness(t, nil)

	// 1. Create customer
	form := url.Values{
		"name":  {"Ariefan Chadmon"},
		"email": {"ariefan@example.com"},
		"phone": {"+628123456789"},
	}
	resp, body := h.request(t, "POST", "/v1/customers", form, testAPIKey)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 creating customer, got %d", resp.StatusCode)
	}

	custID, _ := body["id"].(string)
	if !strings.HasPrefix(custID, "cus_") {
		t.Fatalf("expected customer id prefix cus_, got %s", custID)
	}
	if body["name"] != "Ariefan Chadmon" || body["email"] != "ariefan@example.com" {
		t.Fatalf("unexpected customer body: %v", body)
	}

	// 2. Retrieve customer
	resp, retrieved := h.request(t, "GET", "/v1/customers/"+custID, nil, testAPIKey)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 retrieving customer, got %d", resp.StatusCode)
	}
	if retrieved["id"] != custID {
		t.Fatalf("expected retrieved customer id %s, got %v", custID, retrieved["id"])
	}

	// 3. Retrieve non-existent customer (404)
	resp, _ = h.request(t, "GET", "/v1/customers/cus_nonexistent_9999", nil, testAPIKey)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for missing customer, got %d", resp.StatusCode)
	}
}

func TestE2E_CatalogProductsAndPrices(t *testing.T) {
	h := setupE2EHarness(t, nil)

	// 1. Create Product
	formProduct := url.Values{
		"name":        {"Pro Plan Subscription"},
		"description": {"Ultra low latency payment routing service"},
	}
	resp, prodBody := h.request(t, "POST", "/v1/products", formProduct, testAPIKey)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 creating product, got %d", resp.StatusCode)
	}
	prodID, _ := prodBody["id"].(string)
	if !strings.HasPrefix(prodID, "prod_") {
		t.Fatalf("expected prod_ prefix, got %s", prodID)
	}

	// 2. Create One-Time Price
	formPriceOneTime := url.Values{
		"product":     {prodID},
		"unit_amount": {"150000"},
		"currency":    {"idr"},
	}
	resp, priceBody := h.request(t, "POST", "/v1/prices", formPriceOneTime, testAPIKey)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 creating one-time price, got %d", resp.StatusCode)
	}
	priceID, _ := priceBody["id"].(string)
	if !strings.HasPrefix(priceID, "price_") {
		t.Fatalf("expected price_ prefix, got %s", priceID)
	}

	// 3. Create Recurring Price
	formPriceRecurring := url.Values{
		"product":            {prodID},
		"unit_amount":        {"500000"},
		"currency":           {"idr"},
		"recurring[interval]": {"month"},
	}
	resp, recBody := h.request(t, "POST", "/v1/prices", formPriceRecurring, testAPIKey)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 creating recurring price, got %d", resp.StatusCode)
	}
	recPriceID, _ := recBody["id"].(string)
	if !strings.HasPrefix(recPriceID, "price_") {
		t.Fatalf("expected recurring price_ prefix, got %s", recPriceID)
	}

	// 4. Retrieve Price
	resp, retPrice := h.request(t, "GET", "/v1/prices/"+priceID, nil, testAPIKey)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 retrieving price, got %d", resp.StatusCode)
	}
	if retPrice["unit_amount"].(float64) != 150000 {
		t.Fatalf("expected unit_amount 150000, got %v", retPrice["unit_amount"])
	}
}

func TestE2E_PaymentIntentLifecycle(t *testing.T) {
	h := setupE2EHarness(t, nil)

	// 1. Create PaymentIntent with QRIS
	form := url.Values{
		"amount":                    {"250000"},
		"currency":                  {"idr"},
		"payment_method_types[0]":    {"id_qris"},
		"description":               {"Order #8899"},
		"metadata[customer_ref]":    {"user_42"},
	}
	resp, body := h.request(t, "POST", "/v1/payment_intents", form, testAPIKey)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 creating payment_intent, got %d", resp.StatusCode)
	}

	piID, _ := body["id"].(string)
	if !strings.HasPrefix(piID, "pi_") {
		t.Fatalf("expected pi_ prefix, got %s", piID)
	}
	clientSecret, _ := body["client_secret"].(string)
	if !strings.HasPrefix(clientSecret, piID+"_secret_") {
		t.Fatalf("expected valid client_secret format, got %s", clientSecret)
	}

	nextAction, ok := body["next_action"].(map[string]any)
	if !ok || nextAction == nil {
		t.Fatalf("expected next_action in response, got %v", body)
	}

	// 2. Retrieve PaymentIntent
	resp, retrieved := h.request(t, "GET", "/v1/payment_intents/"+piID, nil, testAPIKey)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 retrieving payment_intent, got %d", resp.StatusCode)
	}
	if retrieved["id"] != piID {
		t.Fatalf("expected id %s, got %v", piID, retrieved["id"])
	}

	// 3. Confirm PaymentIntent
	resp, confirmed := h.request(t, "POST", "/v1/payment_intents/"+piID+"/confirm", nil, testAPIKey)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 confirming payment_intent, got %d", resp.StatusCode)
	}
	if confirmed["id"] != piID {
		t.Fatalf("expected confirmed intent id %s, got %v", piID, confirmed["id"])
	}
}

func TestE2E_CheckoutSessionFlow(t *testing.T) {
	h := setupE2EHarness(t, nil)

	// Create Checkout Session
	form := url.Values{
		"mode":                                  {"payment"},
		"success_url":                           {"https://shop.example.com/success?session_id={CHECKOUT_SESSION_ID}"},
		"cancel_url":                            {"https://shop.example.com/cancel"},
		"line_items[0][price_data][currency]":    {"idr"},
		"line_items[0][price_data][unit_amount]": {"100000"},
		"line_items[0][price_data][product_data][name]": {"Mechanical Keyboard"},
		"line_items[0][quantity]":                {"1"},
	}

	resp, body := h.request(t, "POST", "/v1/checkout/sessions", form, testAPIKey)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 creating checkout session, got %d: %v", resp.StatusCode, body)
	}

	csID, _ := body["id"].(string)
	if !strings.HasPrefix(csID, "cs_") {
		t.Fatalf("expected cs_ prefix, got %s", csID)
	}
	sessionURL, _ := body["url"].(string)
	if sessionURL == "" {
		t.Fatalf("expected checkout session url to be present, got empty")
	}

	// Retrieve Checkout Session
	resp, retrieved := h.request(t, "GET", "/v1/checkout/sessions/"+csID, nil, testAPIKey)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 retrieving checkout session, got %d", resp.StatusCode)
	}
	if retrieved["id"] != csID {
		t.Fatalf("expected session id %s, got %v", csID, retrieved["id"])
	}
}

func TestE2E_RefundsFlow(t *testing.T) {
	h := setupE2EHarness(t, nil)

	// Create a payment intent first
	formPI := url.Values{
		"amount":   {"100000"},
		"currency": {"idr"},
	}
	resp, piBody := h.request(t, "POST", "/v1/payment_intents", formPI, testAPIKey)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 creating payment intent, got %d", resp.StatusCode)
	}
	piID := piBody["id"].(string)

	// Request refund
	formRefund := url.Values{
		"payment_intent": {piID},
		"amount":         {"50000"},
		"reason":         {"requested_by_customer"},
	}
	resp, refBody := h.request(t, "POST", "/v1/refunds", formRefund, testAPIKey)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 creating refund, got %d: %v", resp.StatusCode, refBody)
	}
	refID, _ := refBody["id"].(string)
	if !strings.HasPrefix(refID, "re_") {
		t.Fatalf("expected re_ prefix, got %s", refID)
	}

	// Retrieve refund
	resp, refRetrieved := h.request(t, "GET", "/v1/refunds/"+refID, nil, testAPIKey)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 retrieving refund, got %d", resp.StatusCode)
	}
	if refRetrieved["id"] != refID {
		t.Fatalf("expected refund id %s, got %v", refID, refRetrieved["id"])
	}
}

func TestE2E_OrchestratorLeastCostAndCircuitBreakerFailover(t *testing.T) {
	cfg, err := orchestrator.LoadConfig("../../config.yaml")
	if err != nil {
		t.Fatalf("failed to load config.yaml: %v", err)
	}

	// Create 2 mock adapters: Midtrans (cheapest QRIS, will fail) and Xendit (secondary, will succeed)
	mockMdt := &mockE2EAdapter{name: "midtrans", shouldFail: true, err: fmt.Errorf("500 internal server error")}
	mockXnd := &mockE2EAdapter{name: "xendit", shouldFail: false}

	og := orchestrator.NewOrchestratedGateway(cfg, map[string]gateway.Gateway{
		"midtrans": mockMdt,
		"xendit":   mockXnd,
	})

	h := setupE2EHarness(t, og)

	// Make payment request with QRIS
	form := url.Values{
		"amount":                 {"100000"},
		"currency":               {"idr"},
		"payment_method_types[0]": {"id_qris"},
	}

	// Midtrans is primary, but fails -> payrouter must automatically failover in-flight to Xendit
	resp, body := h.request(t, "POST", "/v1/payment_intents", form, testAPIKey)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 after auto-failover, got %d: %v", resp.StatusCode, body)
	}

	piID, _ := body["id"].(string)
	if !strings.HasPrefix(piID, "pi_") {
		t.Fatalf("expected pi_ prefix, got %s", piID)
	}

	// Verify metadata reflects the failover gateway selection
	metadata, _ := body["metadata"].(map[string]any)
	if metadata["payment_gateway_selected"] != "xendit" {
		t.Fatalf("expected payment_gateway_selected to be xendit, got %v", metadata["payment_gateway_selected"])
	}
}

func TestE2E_OutboundWebhookReSigning(t *testing.T) {
	// Setup webhook receiver
	receivedEvents := make(chan string, 1)
	whReceiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sig := r.Header.Get("Stripe-Signature")
		body, _ := io.ReadAll(r.Body)
		receivedEvents <- sig + "|" + string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer whReceiver.Close()

	st := store.NewMemory()
	signingSecret := "whsec_test_stripe_webhook_signature_secret_999"
	deliverer := webhook.New(st, whReceiver.URL, signingSecret, nil)

	// Enqueue an event into store.Memory
	payload := []byte(`{"id":"evt_123","type":"payment_intent.succeeded","data":{"object":{"id":"pi_123"}}}`)
	st.EnqueueWebhook(&store.WebhookDelivery{
		ID:        "evt_123",
		Type:      "payment_intent.succeeded",
		Reference: "pi_123",
		Payload:   payload,
		Status:    store.DeliveryPending,
		Created:   time.Now().Unix(),
	})

	// Deliver the queued event
	if n := deliverer.DeliverOnce(); n != 1 {
		t.Fatalf("expected 1 delivered event, got %d", n)
	}

	select {
	case received := <-receivedEvents:
		parts := strings.SplitN(received, "|", 2)
		sigHeader := parts[0]
		bodyContent := parts[1]

		if !strings.Contains(sigHeader, "t=") || !strings.Contains(sigHeader, "v1=") {
			t.Fatalf("expected standard Stripe-Signature header, got: %s", sigHeader)
		}
		if !strings.Contains(bodyContent, "payment_intent.succeeded") {
			t.Fatalf("expected payment_intent.succeeded event body, got: %s", bodyContent)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for outbound webhook delivery")
	}
}

type mockE2EAdapter struct {
	name       string
	shouldFail bool
	err        error
}

func (m *mockE2EAdapter) Name() string { return m.name }
func (m *mockE2EAdapter) CreatePayment(ctx context.Context, in *gateway.CreatePaymentInput) (*gateway.PaymentResult, error) {
	if m.shouldFail {
		return nil, m.err
	}
	return &gateway.PaymentResult{
		Reference: in.Reference,
		NextAction: &stripe.PaymentIntentNextAction{
			Type: stripe.PaymentIntentNextActionTypeRedirectToURL,
			RedirectToURL: &stripe.PaymentIntentNextActionRedirectToURL{
				URL: "https://pay.example.com/" + m.name,
			},
		},
	}, nil
}
func (m *mockE2EAdapter) GetStatus(ctx context.Context, gatewayRef string) (*gateway.PaymentResult, error) {
	return nil, nil
}
func (m *mockE2EAdapter) Refund(ctx context.Context, in *gateway.RefundInput) (*gateway.RefundResult, error) {
	return nil, nil
}
func (m *mockE2EAdapter) ParseWebhook(ctx context.Context, r *http.Request) ([]gateway.WebhookEvent, error) {
	return nil, nil
}
