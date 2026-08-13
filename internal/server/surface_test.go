package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jawalab-com/payrouter/internal/adapters/doku"
	"github.com/jawalab-com/payrouter/internal/config"
	"github.com/jawalab-com/payrouter/internal/gateway"
	"github.com/jawalab-com/payrouter/internal/store"
	stripe "github.com/stripe/stripe-go/v81"
)

// jsonGet parses a JSON response body and returns a top-level string field.
func jsonGet(body, key string) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func postForm(t *testing.T, srv *Server, path, form string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form))
	req.Header.Set("Authorization", "Bearer sk_test_x")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func get(t *testing.T, srv *Server, path string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer sk_test_x")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// --- Customers --------------------------------------------------------------

func TestCustomerCreateRetrieve(t *testing.T) {
	srv := newTestServer(t)
	code, body := postForm(t, srv, "/v1/customers", "email=a@b.test&name=Ari&phone=6281")
	if code != http.StatusOK {
		t.Fatalf("create: expected 200, got %d: %s", code, body)
	}
	if id := jsonGet(body, "id"); !strings.HasPrefix(id, "cus_") {
		t.Fatalf("expected cus_ id, got: %s", body)
	}
	if jsonGet(body, "object") != "customer" {
		t.Fatalf("expected object=customer: %s", body)
	}
	id := jsonGet(body, "id")

	code, body = get(t, srv, "/v1/customers/"+id)
	if code != http.StatusOK || jsonGet(body, "id") != id {
		t.Fatalf("retrieve mismatch: %d %s", code, body)
	}

	code, _ = get(t, srv, "/v1/customers/cus_nope")
	if code != http.StatusNotFound {
		t.Fatalf("missing customer: expected 404, got %d", code)
	}
}

// --- Products & Prices ------------------------------------------------------

func TestProductCreateRequiresName(t *testing.T) {
	srv := newTestServer(t)
	code, body := postForm(t, srv, "/v1/products", "description=no name")
	if code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing name, got %d: %s", code, body)
	}
}

func TestPriceCreateInlineProduct(t *testing.T) {
	srv := newTestServer(t)
	code, body := postForm(t, srv, "/v1/prices",
		"currency=idr&unit_amount=15000&product_data[name]=Mug")
	if code != http.StatusOK {
		t.Fatalf("create price: expected 200, got %d: %s", code, body)
	}
	id := jsonGet(body, "id")
	if !strings.HasPrefix(id, "price_") || jsonGet(body, "object") != "price" {
		t.Fatalf("unexpected price: %s", body)
	}
	if jsonGet(body, "type") != "one_time" {
		t.Fatalf("expected one_time type: %s", body)
	}
	if prod := jsonGet(body, "product"); !strings.HasPrefix(prod, "prod_") {
		t.Fatalf("expected inline product id, got: %s", body)
	}
}

func TestPriceCreateWithProductID(t *testing.T) {
	srv := newTestServer(t)
	_, body := postForm(t, srv, "/v1/products", "name=Sticker")
	prodID := jsonGet(body, "id")

	code, body := postForm(t, srv, "/v1/prices",
		"currency=idr&unit_amount=5000&product="+prodID)
	if code != http.StatusOK || jsonGet(body, "product") != prodID {
		t.Fatalf("price with product id: %d %s", code, body)
	}
}

// --- PaymentIntents confirm -------------------------------------------------

func TestConfirmPaymentIntent(t *testing.T) {
	srv := newTestServer(t)
	_, body := postForm(t, srv, "/v1/payment_intents",
		"amount=10000&currency=idr&payment_method_types[]=id_virtual_account")
	piID := jsonGet(body, "id")

	code, body := postForm(t, srv, "/v1/payment_intents/"+piID+"/confirm", "return_url=https://app.test/back")
	if code != http.StatusOK {
		t.Fatalf("confirm: expected 200, got %d: %s", code, body)
	}
	if jsonGet(body, "id") != piID {
		t.Fatalf("confirm should return the same intent: %s", body)
	}
	if !strings.Contains(body, `"redirect_to_url":`) {
		t.Fatalf("confirm should surface the redirect next_action: %s", body)
	}
	if !strings.Contains(body, "https://app.test/back") {
		t.Fatalf("confirm should honor return_url: %s", body)
	}
}

// --- Refunds ----------------------------------------------------------------

func TestRefundCreateRetrieve(t *testing.T) {
	srv := newTestServer(t)
	_, body := postForm(t, srv, "/v1/payment_intents",
		"amount=20000&currency=idr&payment_method_types[]=id_virtual_account")
	piID := jsonGet(body, "id")

	code, body := postForm(t, srv, "/v1/refunds", "payment_intent="+piID)
	if code != http.StatusOK {
		t.Fatalf("refund: expected 200, got %d: %s", code, body)
	}
	id := jsonGet(body, "id")
	if !strings.HasPrefix(id, "re_") || jsonGet(body, "status") != "succeeded" {
		t.Fatalf("unexpected refund: %s", body)
	}
	if jsonGet(body, "payment_intent") != piID {
		t.Fatalf("refund should reference the intent: %s", body)
	}
	if !strings.Contains(body, `"amount":20000`) {
		t.Fatalf("full refund should equal intent amount: %s", body)
	}

	code, body = get(t, srv, "/v1/refunds/"+id)
	if code != http.StatusOK || jsonGet(body, "id") != id {
		t.Fatalf("retrieve refund mismatch: %d %s", code, body)
	}
}

// --- Checkout Sessions ------------------------------------------------------

func TestCheckoutSessionPriceData(t *testing.T) {
	srv := newTestServer(t)
	form := "mode=payment" +
		"&success_url=https://app.test/done" +
		"&cancel_url=https://app.test/cancel" +
		"&customer_email=buyer@example.com" +
		"&line_items[0][price_data][currency]=idr" +
		"&line_items[0][price_data][unit_amount]=50000" +
		"&line_items[0][price_data][product_data][name]=T-shirt" +
		"&line_items[0][quantity]=2"
	code, body := postForm(t, srv, "/v1/checkout/sessions", form)
	if code != http.StatusOK {
		t.Fatalf("checkout: expected 200, got %d: %s", code, body)
	}
	csID := jsonGet(body, "id")
	piID := jsonGet(body, "payment_intent")
	if !strings.HasPrefix(csID, "cs_") {
		t.Fatalf("expected cs_ id: %s", body)
	}
	if !strings.HasPrefix(piID, "pi_") {
		t.Fatalf("expected linked payment_intent: %s", body)
	}
	if jsonGet(body, "url") == "" {
		t.Fatalf("expected a redirect url: %s", body)
	}
	if jsonGet(body, "status") != "open" || jsonGet(body, "payment_status") != "unpaid" {
		t.Fatalf("expected open/unpaid: %s", body)
	}
	if !strings.Contains(body, `"amount_total":100000`) { // 50000 * 2
		t.Fatalf("expected amount_total=100000: %s", body)
	}

	// Retrieve.
	code, body = get(t, srv, "/v1/checkout/sessions/"+csID)
	if code != http.StatusOK || jsonGet(body, "id") != csID {
		t.Fatalf("retrieve session mismatch: %d %s", code, body)
	}
}

func TestCheckoutSessionPriceID(t *testing.T) {
	srv := newTestServer(t)
	_, body := postForm(t, srv, "/v1/products", "name=Sticker")
	prodID := jsonGet(body, "id")
	_, body = postForm(t, srv, "/v1/prices", "currency=idr&unit_amount=30000&product="+prodID)
	priceID := jsonGet(body, "id")

	form := "success_url=https://app.test/done" +
		"&line_items[0][price]=" + priceID +
		"&line_items[0][quantity]=1"
	code, body := postForm(t, srv, "/v1/checkout/sessions", form)
	if code != http.StatusOK {
		t.Fatalf("checkout price-id: expected 200, got %d: %s", code, body)
	}
	if !strings.Contains(body, `"amount_total":30000`) {
		t.Fatalf("expected amount_total=30000 from price: %s", body)
	}
}

func TestCheckoutSessionErrors(t *testing.T) {
	srv := newTestServer(t)
	cases := []struct {
		name string
		form string
	}{
		{"missing success_url", "line_items[0][price_data][unit_amount]=1000"},
		{"bad mode", "mode=setup&success_url=https://x&line_items[0][price_data][unit_amount]=1000"},
		{"missing line_items", "success_url=https://x"},
	}
	for _, c := range cases {
		code, body := postForm(t, srv, "/v1/checkout/sessions", c.form)
		if code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d: %s", c.name, code, body)
		}
	}
}

// --- Dual-event webhook path ------------------------------------------------

// TestCheckoutSessionPaidEmitsBothEvents drives processEvent directly (the stub
// adapter doesn't parse webhooks) to prove a paid, session-backed intent emits
// both payment_intent.succeeded and checkout.session.completed, and completes the
// session.
func TestCheckoutSessionPaidEmitsBothEvents(t *testing.T) {
	srv := newTestServer(t)
	form := "success_url=https://app.test/done" +
		"&line_items[0][price_data][currency]=idr" +
		"&line_items[0][price_data][unit_amount]=50000" +
		"&line_items[0][price_data][product_data][name]=T-shirt"
	_, body := postForm(t, srv, "/v1/checkout/sessions", form)
	csID := jsonGet(body, "id")
	piID := jsonGet(body, "payment_intent")

	srv.processEvent(gateway.WebhookEvent{
		Type:      "payment_intent.succeeded",
		Reference: piID,
		Status:    stripe.PaymentIntentStatusSucceeded,
	}, 1700000000)

	var sawPI, sawCS bool
	for _, d := range srv.store.ListWebhooks() {
		switch d.Type {
		case "payment_intent.succeeded":
			sawPI = true
		case "checkout.session.completed":
			sawCS = true
		}
	}
	if !sawPI || !sawCS {
		t.Fatalf("expected both events, got %+v", srv.store.ListWebhooks())
	}

	sess, err := srv.store.GetSession(csID)
	if err != nil {
		t.Fatalf("session gone: %v", err)
	}
	if sess.Status != "complete" || sess.PaymentStatus != "paid" {
		t.Fatalf("expected complete/paid session, got %s/%s", sess.Status, sess.PaymentStatus)
	}
}

// --- Doc-conformance (always-present fields) --------------------------------

// jsonMap parses a JSON response body into a generic map.
func jsonMap(body string) map[string]any {
	var m map[string]any
	_ = json.Unmarshal([]byte(body), &m)
	return m
}

func hasWebhookType(srv *Server, typ string) bool {
	for _, d := range srv.store.ListWebhooks() {
		if d.Type == typ {
			return true
		}
	}
	return false
}

// TestStripeFieldConformance asserts the response objects carry the fields a real
// Stripe client/framework reads (verified against the Stripe API reference):
// price.billing_scheme, payment_intent capture_method/confirmation_method/
// amount_received/last_payment_error, checkout.session customer_details, and the
// refund.reason enum + refund.created event.
func TestStripeFieldConformance(t *testing.T) {
	srv := newTestServer(t)

	// Price always carries billing_scheme (Stripe defaults it to "per_unit").
	_, body := postForm(t, srv, "/v1/prices", "currency=idr&unit_amount=15000&product_data[name]=Mug")
	if jsonGet(body, "billing_scheme") != "per_unit" {
		t.Fatalf("price.billing_scheme want per_unit: %s", body)
	}

	// PaymentIntent always carries capture_method/confirmation_method; amount_received
	// is 0 until the intent succeeds.
	_, body = postForm(t, srv, "/v1/payment_intents", "amount=20000&currency=idr&payment_method_types[]=id_virtual_account")
	piID := jsonGet(body, "id")
	if jsonGet(body, "capture_method") != "automatic" || jsonGet(body, "confirmation_method") != "manual" {
		t.Fatalf("PI capture/confirmation_method: %s", body)
	}
	if v, _ := jsonMap(body)["amount_received"].(float64); v != 0 {
		t.Fatalf("amount_received should be 0 pre-success: %s", body)
	}

	// A failed payment surfaces last_payment_error (generic; the gateway gives no detail).
	srv.processEvent(gateway.WebhookEvent{
		Type:      "payment_intent.payment_failed",
		Reference: piID,
		Status:    stripe.PaymentIntentStatusRequiresPaymentMethod,
	}, 1700000000)
	_, body = get(t, srv, "/v1/payment_intents/"+piID)
	if _, ok := jsonMap(body)["last_payment_error"]; !ok {
		t.Fatalf("expected last_payment_error on failure: %s", body)
	}

	// A subsequent success sets amount_received = amount and clears the error.
	srv.processEvent(gateway.WebhookEvent{
		Type:      "payment_intent.succeeded",
		Reference: piID,
		Status:    stripe.PaymentIntentStatusSucceeded,
	}, 1700000001)
	_, body = get(t, srv, "/v1/payment_intents/"+piID)
	if v, _ := jsonMap(body)["amount_received"].(float64); v != 20000 {
		t.Fatalf("amount_received should equal amount on success: %s", body)
	}
	if _, ok := jsonMap(body)["last_payment_error"]; ok {
		t.Fatalf("last_payment_error should clear on success: %s", body)
	}

	// Checkout Session carries customer_details.email (Stripe's quickstart reads it).
	form := "mode=payment&success_url=https://app.test/done&customer_email=buyer@example.com" +
		"&line_items[0][price_data][currency]=idr&line_items[0][price_data][unit_amount]=5000" +
		"&line_items[0][price_data][product_data][name]=X"
	_, body = postForm(t, srv, "/v1/checkout/sessions", form)
	if !strings.Contains(body, `"customer_details":{"email":"buyer@example.com"}`) {
		t.Fatalf("expected customer_details.email: %s", body)
	}

	// Refund reason is validated against Stripe's enum; a bad value is rejected.
	code, _ := postForm(t, srv, "/v1/refunds", "payment_intent="+piID+"&reason=bogus")
	if code != http.StatusBadRequest {
		t.Fatalf("bad refund reason: expected 400, got %d", code)
	}

	// A valid refund emits refund.created (Stripe fires it on every refund).
	code, body = postForm(t, srv, "/v1/refunds", "payment_intent="+piID+"&reason=duplicate")
	if code != http.StatusOK {
		t.Fatalf("valid refund: expected 200, got %d: %s", code, body)
	}
	if !hasWebhookType(srv, "refund.created") {
		t.Fatalf("expected a refund.created event, got %+v", srv.store.ListWebhooks())
	}
}

// --- Subscriptions (v2 recurring) -------------------------------------------

// del issues an authenticated DELETE and returns the status + body.
func del(t *testing.T, srv *Server, path string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, path, nil)
	req.Header.Set("Authorization", "Bearer sk_test_x")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// nestedString walks a chain of object keys into a JSON body and returns the
// terminal string ("" if any step is missing or the wrong type).
func nestedString(body string, keys ...string) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		return ""
	}
	for i, k := range keys {
		if i == len(keys)-1 {
			if s, ok := m[k].(string); ok {
				return s
			}
			return ""
		}
		next, ok := m[k].(map[string]any)
		if !ok {
			return ""
		}
		m = next
	}
	return ""
}

func countWebhookType(srv *Server, typ string) int {
	n := 0
	for _, d := range srv.store.ListWebhooks() {
		if d.Type == typ {
			n++
		}
	}
	return n
}

// TestSubscriptionUnsupportedGateway proves a gateway that does not implement
// SubscriptionGateway (DOKU today) declines subscription creation with the same
// Stripe error shape DOKU/Mayar use for refunds — no recurring capability leak.
func TestSubscriptionUnsupportedGateway(t *testing.T) {
	srv := New(config.Config{APIKey: "sk_test_x", Livemode: false}, store.NewMemory(), doku.New("cid", "key", true))

	code, body := postForm(t, srv, "/v1/subscriptions",
		"customer=cus_x"+
			"&items[0][price_data][currency]=idr"+
			"&items[0][price_data][unit_amount]=50000"+
			"&items[0][price_data][product_data][name]=Pro"+
			"&items[0][price_data][recurring][interval]=month")
	if code != http.StatusBadRequest {
		t.Fatalf("expected 400 on an unsupported gateway, got %d: %s", code, body)
	}
	if !strings.Contains(body, "not supported") {
		t.Fatalf("expected a not-supported message: %s", body)
	}
}

// TestSubscriptionRecurringLoopStub drives the full recurring loop at the server
// level against the stub gateway: create (incomplete) → activation webhook →
// active + events → recurring-cycle webhook → second invoice → cancel → deleted.
// The stub doesn't parse webhooks, so the recurring events are fed directly to
// processSubscriptionEvent (mirroring TestCheckoutSessionPaidEmitsBothEvents).
func TestSubscriptionRecurringLoopStub(t *testing.T) {
	srv := newTestServer(t)

	// Recurring price + customer.
	_, body := postForm(t, srv, "/v1/products", "name=Pro")
	prodID := jsonGet(body, "id")
	_, body = postForm(t, srv, "/v1/prices",
		"currency=idr&unit_amount=50000&product="+prodID+"&recurring[interval]=month")
	if jsonGet(body, "type") != "recurring" {
		t.Fatalf("expected recurring price type: %s", body)
	}
	priceID := jsonGet(body, "id")
	_, body = postForm(t, srv, "/v1/customers", "email=sub@x.test&name=Sub")
	custID := jsonGet(body, "id")

	// Create → incomplete, with the auth payment_intent nested on latest_invoice.
	code, body := postForm(t, srv, "/v1/subscriptions",
		"customer="+custID+"&items[0][price]="+priceID)
	if code != http.StatusOK {
		t.Fatalf("create subscription: expected 200, got %d: %s", code, body)
	}
	subID := jsonGet(body, "id")
	if jsonGet(body, "status") != "incomplete" {
		t.Fatalf("expected incomplete status: %s", body)
	}
	authPI := nestedString(body, "latest_invoice", "payment_intent", "id")
	if authPI == "" {
		t.Fatalf("expected an authorization payment_intent id on latest_invoice: %s", body)
	}

	// Activation webhook → active + customer.subscription.updated + first invoice.
	srv.processSubscriptionEvent(gateway.WebhookEvent{
		Type:       "customer.subscription.activating",
		Kind:       "subscription",
		Reference:  authPI,
		SavedToken: "tok-stub",
	}, 1700000000)
	code, body = get(t, srv, "/v1/subscriptions/"+subID)
	if code != http.StatusOK || jsonGet(body, "status") != "active" {
		t.Fatalf("expected active after activation: %d %s", code, body)
	}
	if !hasWebhookType(srv, "customer.subscription.updated") {
		t.Fatalf("expected customer.subscription.updated: %+v", srv.store.ListWebhooks())
	}
	if countWebhookType(srv, "invoice.payment_succeeded") != 1 {
		t.Fatalf("expected one invoice.payment_succeeded (activation): %+v", srv.store.ListWebhooks())
	}

	// Recurring-cycle webhook (gateway sub id = "stub-sub-"+subID) → second invoice.
	srv.processSubscriptionEvent(gateway.WebhookEvent{
		Type:         "invoice.payment_succeeded",
		Kind:         "subscription",
		Reference:    "pi_stub_cycle",
		GatewaySubID: "stub-sub-" + subID,
		AmountMinor:  50000,
	}, 1700000001)
	if countWebhookType(srv, "invoice.payment_succeeded") != 2 {
		t.Fatalf("expected two invoice.payment_succeeded (activation + cycle): %+v", srv.store.ListWebhooks())
	}

	// Cancel → deleted + canceled status.
	code, body = del(t, srv, "/v1/subscriptions/"+subID)
	if code != http.StatusOK {
		t.Fatalf("cancel: expected 200, got %d: %s", code, body)
	}
	if jsonGet(body, "status") != "canceled" {
		t.Fatalf("expected canceled status: %s", body)
	}
	if !hasWebhookType(srv, "customer.subscription.deleted") {
		t.Fatalf("expected customer.subscription.deleted: %+v", srv.store.ListWebhooks())
	}
}
