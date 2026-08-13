// Package compat contains the definition-of-done test (milestone M3): it points
// the OFFICIAL stripe-go SDK at the facade and exercises the full loop an
// existing Stripe integration uses — create + retrieve a PaymentIntent, then
// receive and verify a webhook with the SDK's own webhook.ConstructEvent. If this
// passes with no facade-specific hacks, the "drop-in Stripe-compatible API" claim
// holds for the SDK path.
package compat

import (
	"bytes"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stripe-compatible-facade/internal/adapters/midtrans"
	"github.com/stripe-compatible-facade/internal/config"
	facadeserver "github.com/stripe-compatible-facade/internal/server"
	"github.com/stripe-compatible-facade/internal/store"
	facadewebhook "github.com/stripe-compatible-facade/internal/webhook"

	stripe "github.com/stripe/stripe-go/v81"
	checkoutsession "github.com/stripe/stripe-go/v81/checkout/session"
	"github.com/stripe/stripe-go/v81/customer"
	"github.com/stripe/stripe-go/v81/paymentintent"
	"github.com/stripe/stripe-go/v81/price"
	"github.com/stripe/stripe-go/v81/product"
	"github.com/stripe/stripe-go/v81/refund"
	"github.com/stripe/stripe-go/v81/subscription"
	stripewebhook "github.com/stripe/stripe-go/v81/webhook"
)

const (
	facadeAPIKey  = "sk_test_compat"
	midtransSKey  = "SB-Mid-server-COMPAT"
	webhookSecret = "whsec_compat"
	grossAmount   = "75000.00"
)

// midtransSig is an independent reimplementation of the Midtrans signature so
// this test does not call the adapter under test to sign its own input.
func midtransSig(orderID, status, gross, serverKey string) string {
	sum := sha512.Sum512([]byte(orderID + status + gross + serverKey))
	return hex.EncodeToString(sum[:])
}

func signedMidtransNotif(t *testing.T, orderID, txStatus string) []byte {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"transaction_status": txStatus,
		"transaction_id":     "tx-compat",
		"status_code":        "200",
		"gross_amount":       grossAmount,
		"order_id":           orderID,
		"fraud_status":       "accept",
		"payment_type":       "qris",
		"signature_key":      midtransSig(orderID, "200", grossAmount, midtransSKey),
	})
	return body
}

// signedMidtransSaveCardNotif builds a save-card notification carrying saved_token_id
// — the subscription activation trigger. The digest is identical to a one-time
// notification's: saved_token_id is NOT part of the signature.
func signedMidtransSaveCardNotif(t *testing.T, orderID, token string) []byte {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"transaction_status": "capture",
		"transaction_id":     "tx-savecard",
		"status_code":        "200",
		"gross_amount":       grossAmount,
		"order_id":           orderID,
		"fraud_status":       "accept",
		"payment_type":       "credit_card",
		"saved_token_id":     token,
		"signature_key":      midtransSig(orderID, "200", grossAmount, midtransSKey),
	})
	return body
}

// signedMidtransCycleNotif builds a recurring-cycle notification carrying the gateway
// subscription_id — the correlation key for a gateway-side auto-debit charge.
func signedMidtransCycleNotif(t *testing.T, orderID, gatewaySubID string) []byte {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"transaction_status": "settlement",
		"transaction_id":     "tx-cycle",
		"status_code":        "200",
		"gross_amount":       grossAmount,
		"order_id":           orderID,
		"fraud_status":       "accept",
		"payment_type":       "credit_card",
		"subscription_id":    gatewaySubID,
		"signature_key":      midtransSig(orderID, "200", grossAmount, midtransSKey),
	})
	return body
}

// TestStripeClientEndToEnd is the definition-of-done: the official stripe-go SDK
// drives the facade for create/retrieve and verifies the forwarded webhook.
func TestStripeClientEndToEnd(t *testing.T) {
	// Fake Midtrans Snap so CreatePayment returns a hosted redirect.
	snap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			TD struct {
				OrderID string `json:"order_id"`
			} `json:"transaction_details"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":        "snap-compat",
			"redirect_url": "https://snap.local/pay/" + req.TD.OrderID,
		})
	}))
	t.Cleanup(snap.Close)

	st := newTestStore(t)
	cfg := config.Config{APIKey: facadeAPIKey, Livemode: false, ActiveGateway: "midtrans"}
	gw := midtrans.New(midtransSKey, true)
	gw.SetBaseURLs(snap.URL, snap.URL)
	facade := httptest.NewServer(facadeserver.New(cfg, st, gw))
	t.Cleanup(facade.Close)

	// Point the official SDK at the facade (custom API backend + our key).
	stripe.SetBackend(stripe.APIBackend, stripe.GetBackendWithConfig(stripe.APIBackend, &stripe.BackendConfig{
		URL: stripe.String(facade.URL),
	}))
	stripe.Key = facadeAPIKey

	// 1) Create a PaymentIntent exactly as a real Stripe integration would.
	pi, err := paymentintent.New(&stripe.PaymentIntentParams{
		Amount:             stripe.Int64(75000),
		Currency:           stripe.String(string(stripe.CurrencyIDR)),
		PaymentMethodTypes: stripe.StringSlice([]string{"id_qris"}),
		ReturnURL:          stripe.String("https://app.test/done"),
	})
	if err != nil {
		t.Fatalf("paymentintent.New: %v", err)
	}
	if !strings.HasPrefix(pi.ID, "pi_") {
		t.Errorf("id = %q; want pi_ prefix", pi.ID)
	}
	if pi.Status != stripe.PaymentIntentStatusRequiresAction {
		t.Errorf("status = %s; want requires_action", pi.Status)
	}
	if pi.NextAction == nil || pi.NextAction.RedirectToURL == nil || pi.NextAction.RedirectToURL.URL == "" {
		t.Fatalf("missing next_action.redirect_to_url: %+v", pi.NextAction)
	}
	if !strings.Contains(pi.NextAction.RedirectToURL.URL, pi.ID) {
		t.Errorf("redirect url %q does not reference the intent", pi.NextAction.RedirectToURL.URL)
	}

	// 2) Retrieve it via the SDK — proves the response round-trips into *PaymentIntent.
	got, err := paymentintent.Get(pi.ID, nil)
	if err != nil {
		t.Fatalf("paymentintent.Get: %v", err)
	}
	if got.ID != pi.ID || got.Amount != 75000 || got.Currency != stripe.CurrencyIDR {
		t.Errorf("retrieved intent mismatch: id=%q amount=%d currency=%s", got.ID, got.Amount, got.Currency)
	}

	// 3) Simulate the gateway confirming the payment, then forward + verify.
	var recvPayload []byte
	var recvSig string
	merchant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recvSig = r.Header.Get("Stripe-Signature")
		b, _ := io.ReadAll(r.Body)
		recvPayload = b
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(merchant.Close)

	// Push a signed Midtrans notification into the facade.
	resp, err := http.Post(facade.URL+"/v1/webhooks/midtrans", "application/json", bytes.NewReader(signedMidtransNotif(t, pi.ID, "settlement")))
	if err != nil {
		t.Fatalf("post notification: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("webhook inbound status = %d; want 200", resp.StatusCode)
	}

	// Deliver the re-signed Stripe event to the merchant.
	facadewebhook.New(st, merchant.URL, webhookSecret, nil).DeliverOnce()
	if recvPayload == nil {
		t.Fatal("merchant received no forwarded event")
	}

	// 4) The merchant verifies with the SDK's own ConstructEvent — the real test.
	evt, err := stripewebhook.ConstructEvent(recvPayload, recvSig, webhookSecret)
	if err != nil {
		t.Fatalf("ConstructEvent rejected our event: %v", err)
	}
	if evt.Type != "payment_intent.succeeded" {
		t.Errorf("event type = %q; want payment_intent.succeeded", evt.Type)
	}
	if status, _ := evt.Data.Object["status"].(string); status != "succeeded" {
		t.Errorf("event data.object.status = %q; want succeeded", status)
	}
	if id, _ := evt.Data.Object["id"].(string); id != pi.ID {
		t.Errorf("event data.object.id = %q; want %q", id, pi.ID)
	}

	// 5) A subsequent SDK Get now reflects the succeeded status.
	final, err := paymentintent.Get(pi.ID, nil)
	if err != nil {
		t.Fatalf("final paymentintent.Get: %v", err)
	}
	if final.Status != stripe.PaymentIntentStatusSucceeded {
		t.Errorf("final status = %s; want succeeded", final.Status)
	}
}

// setupFacade spins up a facade backed by a fake Midtrans Snap (so CreatePayment
// returns a redirect) and points the official stripe-go SDK at it. Returns the
// running facade and its store.
// fakeGatewaySubID is the gateway subscription id the fake Core API returns from
// POST /v1/subscriptions (ActivateSubscription). It is the correlation key the
// recurring-cycle notification must carry back so recordSubscriptionCycle can
// attribute the charge to the subscription.
const fakeGatewaySubID = "sub-fake-1"

func setupFacade(t *testing.T) (*httptest.Server, store.Store) {
	t.Helper()
	// One fake server serves BOTH hosts (snapBase == apiBase): Snap transactions
	// and the Core API recurring endpoints, branched on path so each returns the
	// right shape. Unrecognized paths fall through to the Snap create-transaction
	// response (backward compatible with the one-time payment tests).
	snap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/subscriptions":
			// ActivateSubscription registers the recurring schedule.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":     fakeGatewaySubID,
				"status": "active",
				"schedule": map[string]any{
					"next_execution_at": "2026-09-08 10:00:00 +0700",
				},
			})
			return
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cancel"):
			// CancelSubscription cancels a recurring schedule.
			_ = json.NewEncoder(w).Encode(map[string]any{"id": fakeGatewaySubID, "status": "canceled"})
			return
		}
		var req struct {
			TD struct {
				OrderID string `json:"order_id"`
			} `json:"transaction_details"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":        "snap-cs",
			"redirect_url": "https://snap.local/pay/" + req.TD.OrderID,
		})
	}))
	t.Cleanup(snap.Close)

	st := newTestStore(t)
	cfg := config.Config{APIKey: facadeAPIKey, Livemode: false, ActiveGateway: "midtrans"}
	gw := midtrans.New(midtransSKey, true)
	gw.SetBaseURLs(snap.URL, snap.URL)
	facade := httptest.NewServer(facadeserver.New(cfg, st, gw))
	t.Cleanup(facade.Close)

	stripe.SetBackend(stripe.APIBackend, stripe.GetBackendWithConfig(stripe.APIBackend, &stripe.BackendConfig{
		URL: stripe.String(facade.URL),
	}))
	stripe.Key = facadeAPIKey
	return facade, st
}

// TestCheckoutSurface drives the new most-used Stripe surface through the official
// SDK: Products, Prices, Customers, Checkout Sessions (inline price_data and a
// referenced price id), PaymentIntent confirm, and Refunds. If these SDK calls
// work against the facade unchanged, the drop-in claim holds for the broad surface.
func TestCheckoutSurface(t *testing.T) {
	setupFacade(t)

	prod, err := product.New(&stripe.ProductParams{Name: stripe.String("T-shirt")})
	if err != nil {
		t.Fatalf("product.New: %v", err)
	}
	if !strings.HasPrefix(prod.ID, "prod_") {
		t.Errorf("product id = %q; want prod_ prefix", prod.ID)
	}

	priceObj, err := price.New(&stripe.PriceParams{
		Product:    stripe.String(prod.ID),
		UnitAmount: stripe.Int64(50000),
		Currency:   stripe.String(string(stripe.CurrencyIDR)),
	})
	if err != nil {
		t.Fatalf("price.New: %v", err)
	}
	if !strings.HasPrefix(priceObj.ID, "price_") || priceObj.UnitAmount != 50000 {
		t.Errorf("price = %+v", priceObj)
	}

	cust, err := customer.New(&stripe.CustomerParams{Email: stripe.String("buyer@example.com"), Name: stripe.String("Buyer")})
	if err != nil {
		t.Fatalf("customer.New: %v", err)
	}
	if !strings.HasPrefix(cust.ID, "cus_") {
		t.Errorf("customer id = %q", cust.ID)
	}

	// Checkout Session with inline price_data.
	sess, err := checkoutsession.New(&stripe.CheckoutSessionParams{
		Mode:       stripe.String(string(stripe.CheckoutSessionModePayment)),
		SuccessURL: stripe.String("https://app.test/done"),
		CancelURL:  stripe.String("https://app.test/cancel"),
		LineItems: []*stripe.CheckoutSessionLineItemParams{
			{
				Quantity: stripe.Int64(2),
				PriceData: &stripe.CheckoutSessionLineItemPriceDataParams{
					Currency:   stripe.String(string(stripe.CurrencyIDR)),
					UnitAmount: stripe.Int64(50000),
					ProductData: &stripe.CheckoutSessionLineItemPriceDataProductDataParams{
						Name: stripe.String("T-shirt"),
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("checkoutsession.New (price_data): %v", err)
	}
	if !strings.HasPrefix(sess.ID, "cs_") {
		t.Errorf("session id = %q; want cs_ prefix", sess.ID)
	}
	if sess.URL == "" {
		t.Errorf("expected a redirect url, got: %+v", sess)
	}
	if sess.PaymentIntent == nil || sess.PaymentIntent.ID == "" {
		t.Fatalf("expected a linked payment_intent: %+v", sess.PaymentIntent)
	}
	if sess.AmountTotal != 100000 { // 50000 * 2
		t.Errorf("amount_total = %d; want 100000", sess.AmountTotal)
	}

	// Checkout Session referencing the price id created above.
	sess2, err := checkoutsession.New(&stripe.CheckoutSessionParams{
		Mode:       stripe.String(string(stripe.CheckoutSessionModePayment)),
		SuccessURL: stripe.String("https://app.test/done"),
		LineItems: []*stripe.CheckoutSessionLineItemParams{
			{Quantity: stripe.Int64(1), Price: stripe.String(priceObj.ID)},
		},
	})
	if err != nil {
		t.Fatalf("checkoutsession.New (price id): %v", err)
	}
	if sess2.AmountTotal != 50000 {
		t.Errorf("price-id amount_total = %d; want 50000", sess2.AmountTotal)
	}

	// Confirm the session's PaymentIntent surfaces the redirect next_action.
	pi, err := paymentintent.Confirm(sess.PaymentIntent.ID, &stripe.PaymentIntentConfirmParams{
		ReturnURL: stripe.String("https://app.test/back"),
	})
	if err != nil {
		t.Fatalf("paymentintent.Confirm: %v", err)
	}
	if pi.NextAction == nil || pi.NextAction.RedirectToURL == nil || pi.NextAction.RedirectToURL.URL == "" {
		t.Fatalf("confirm missing next_action.redirect_to_url: %+v", pi.NextAction)
	}

	// Refund the session's PaymentIntent (Midtrans supports refunds).
	ref, err := refund.New(&stripe.RefundParams{PaymentIntent: stripe.String(sess.PaymentIntent.ID)})
	if err != nil {
		t.Fatalf("refund.New: %v", err)
	}
	if !strings.HasPrefix(ref.ID, "re_") || ref.Status != stripe.RefundStatusSucceeded {
		t.Errorf("refund = %+v", ref)
	}
}

// TestCheckoutSessionWebhook proves a gateway payment on a session-backed intent
// emits BOTH payment_intent.succeeded and checkout.session.completed, each
// verifiable with the SDK's own webhook.ConstructEvent.
func TestCheckoutSessionWebhook(t *testing.T) {
	facade, st := setupFacade(t)

	sess, err := checkoutsession.New(&stripe.CheckoutSessionParams{
		Mode:       stripe.String(string(stripe.CheckoutSessionModePayment)),
		SuccessURL: stripe.String("https://app.test/done"),
		LineItems: []*stripe.CheckoutSessionLineItemParams{
			{
				Quantity: stripe.Int64(1),
				PriceData: &stripe.CheckoutSessionLineItemPriceDataParams{
					Currency:   stripe.String(string(stripe.CurrencyIDR)),
					UnitAmount: stripe.Int64(75000),
					ProductData: &stripe.CheckoutSessionLineItemPriceDataProductDataParams{
						Name: stripe.String("Ticket"),
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("checkoutsession.New: %v", err)
	}
	piID := sess.PaymentIntent.ID

	type delivery struct {
		payload []byte
		sig     string
	}
	var got []delivery
	merchant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, delivery{payload: b, sig: r.Header.Get("Stripe-Signature")})
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(merchant.Close)

	// The gateway confirms the session's payment.
	resp, err := http.Post(facade.URL+"/v1/webhooks/midtrans", "application/json", bytes.NewReader(signedMidtransNotif(t, piID, "settlement")))
	if err != nil {
		t.Fatalf("post notification: %v", err)
	}
	resp.Body.Close()

	facadewebhook.New(st, merchant.URL, webhookSecret, nil).DeliverOnce()

	var types []string
	var sessionObj map[string]any
	for _, d := range got {
		evt, err := stripewebhook.ConstructEvent(d.payload, d.sig, webhookSecret)
		if err != nil {
			t.Fatalf("ConstructEvent rejected event: %v", err)
		}
		types = append(types, string(evt.Type))
		if evt.Type == "checkout.session.completed" {
			sessionObj = evt.Data.Object
		}
	}
	if !contains(types, "payment_intent.succeeded") || !contains(types, "checkout.session.completed") {
		t.Fatalf("expected both event types, got %v", types)
	}
	if id, _ := sessionObj["id"].(string); id != sess.ID {
		t.Errorf("checkout.session.completed id = %q; want %q", id, sess.ID)
	}
	if status, _ := sessionObj["status"].(string); status != "complete" {
		t.Errorf("session status = %q; want complete", status)
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// TestRecurringPriceSerialization proves a recurring Price round-trips through the
// facade into stripe-go's Price.Recurring object — the shape Laravel Cashier and
// other subscription frameworks read to know a price is recurring.
func TestRecurringPriceSerialization(t *testing.T) {
	setupFacade(t)

	p, err := price.New(&stripe.PriceParams{
		UnitAmount:  stripe.Int64(75000),
		Currency:    stripe.String(string(stripe.CurrencyIDR)),
		ProductData: &stripe.PriceProductDataParams{Name: stripe.String("Pro plan")},
		Recurring: &stripe.PriceRecurringParams{
			Interval:      stripe.String(string(stripe.PriceRecurringIntervalMonth)),
			IntervalCount: stripe.Int64(1),
			UsageType:     stripe.String(string(stripe.PriceRecurringUsageTypeLicensed)),
		},
	})
	if err != nil {
		t.Fatalf("price.New: %v", err)
	}
	if p.Type != "recurring" {
		t.Errorf("type = %q; want recurring", p.Type)
	}
	if p.Recurring == nil {
		t.Fatal("missing recurring object on the price")
	}
	if p.Recurring.Interval != stripe.PriceRecurringIntervalMonth {
		t.Errorf("interval = %q; want month", p.Recurring.Interval)
	}
	if p.Recurring.IntervalCount != 1 {
		t.Errorf("interval_count = %d; want 1", p.Recurring.IntervalCount)
	}
	if p.Recurring.UsageType != stripe.PriceRecurringUsageTypeLicensed {
		t.Errorf("usage_type = %q; want licensed", p.Recurring.UsageType)
	}
}

// TestSubscriptionRecurringFlow is the recurring definition-of-done: the official
// SDK creates a subscription (incomplete + authorization redirect), a save-card
// webhook activates it (active + customer.subscription.updated + the first
// invoice.payment_succeeded), a recurring-cycle webhook emits a second
// invoice.payment_succeeded, and cancellation emits customer.subscription.deleted —
// every outbound event verified with the SDK's own webhook.ConstructEvent.
func TestSubscriptionRecurringFlow(t *testing.T) {
	facade, st := setupFacade(t)

	// A subscription needs a recurring price and a customer.
	priceObj, err := price.New(&stripe.PriceParams{
		UnitAmount:  stripe.Int64(75000),
		Currency:    stripe.String(string(stripe.CurrencyIDR)),
		ProductData: &stripe.PriceProductDataParams{Name: stripe.String("Pro plan")},
		Recurring: &stripe.PriceRecurringParams{
			Interval:      stripe.String(string(stripe.PriceRecurringIntervalMonth)),
			IntervalCount: stripe.Int64(1),
		},
	})
	if err != nil {
		t.Fatalf("price.New: %v", err)
	}
	cust, err := customer.New(&stripe.CustomerParams{Email: stripe.String("sub@example.com")})
	if err != nil {
		t.Fatalf("customer.New: %v", err)
	}

	// 1) Create — incomplete, with the authorization redirect nested on the
	//    latest_invoice's payment_intent (Stripe's incomplete-subscription shape).
	sub, err := subscription.New(&stripe.SubscriptionParams{
		Customer: stripe.String(cust.ID),
		Items: []*stripe.SubscriptionItemsParams{
			{Price: stripe.String(priceObj.ID)},
		},
	})
	if err != nil {
		t.Fatalf("subscription.New: %v", err)
	}
	if sub.Status != stripe.SubscriptionStatusIncomplete {
		t.Fatalf("status = %q; want incomplete", sub.Status)
	}
	if sub.LatestInvoice == nil || sub.LatestInvoice.PaymentIntent == nil ||
		sub.LatestInvoice.PaymentIntent.NextAction == nil ||
		sub.LatestInvoice.PaymentIntent.NextAction.RedirectToURL == nil ||
		sub.LatestInvoice.PaymentIntent.NextAction.RedirectToURL.URL == "" {
		t.Fatalf("missing latest_invoice.payment_intent.next_action.redirect_to_url: %+v", sub.LatestInvoice)
	}
	authPI := sub.LatestInvoice.PaymentIntent.ID
	if !strings.HasPrefix(authPI, "pi_") {
		t.Fatalf("authorization payment_intent id = %q; want pi_ prefix", authPI)
	}

	// Merchant webhook receiver — collects every re-signed delivery.
	type delivery struct {
		payload []byte
		sig     string
	}
	var got []delivery
	merchant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, delivery{payload: b, sig: r.Header.Get("Stripe-Signature")})
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(merchant.Close)

	// 2) Push the save-card notification (carries saved_token_id) → the facade makes
	//    the one follow-up ActivateSubscription call and flips the sub to active.
	resp, err := http.Post(facade.URL+"/v1/webhooks/midtrans", "application/json",
		bytes.NewReader(signedMidtransSaveCardNotif(t, authPI, "tok-fake")))
	if err != nil {
		t.Fatalf("post save-card notification: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("save-card webhook inbound status = %d; want 200", resp.StatusCode)
	}
	facadewebhook.New(st, merchant.URL, webhookSecret, nil).DeliverOnce()

	active, err := subscription.Get(sub.ID, nil)
	if err != nil {
		t.Fatalf("subscription.Get after activation: %v", err)
	}
	if active.Status != stripe.SubscriptionStatusActive {
		t.Fatalf("status = %q; want active", active.Status)
	}

	// 3) A recurring-cycle notification (new order_id + the gateway subscription id
	//    the activation returned) emits a second invoice.payment_succeeded.
	cycleResp, err := http.Post(facade.URL+"/v1/webhooks/midtrans", "application/json",
		bytes.NewReader(signedMidtransCycleNotif(t, "pi_cycle_1", fakeGatewaySubID)))
	if err != nil {
		t.Fatalf("post cycle notification: %v", err)
	}
	cycleResp.Body.Close()
	facadewebhook.New(st, merchant.URL, webhookSecret, nil).DeliverOnce()

	// 4) Cancel → customer.subscription.deleted.
	if _, err := subscription.Cancel(sub.ID, nil); err != nil {
		t.Fatalf("subscription.Cancel: %v", err)
	}
	facadewebhook.New(st, merchant.URL, webhookSecret, nil).DeliverOnce()

	// Every delivery verifies with the SDK's own ConstructEvent, and the expected
	// event types are present with the right multiplicities.
	var types []string
	for _, d := range got {
		evt, err := stripewebhook.ConstructEvent(d.payload, d.sig, webhookSecret)
		if err != nil {
			t.Fatalf("ConstructEvent rejected an event: %v", err)
		}
		types = append(types, string(evt.Type))
	}
	if countType(types, "customer.subscription.updated") != 1 {
		t.Errorf("customer.subscription.updated = %d; want 1 (types=%v)", countType(types, "customer.subscription.updated"), types)
	}
	if countType(types, "invoice.payment_succeeded") != 2 {
		t.Errorf("invoice.payment_succeeded = %d; want 2 (activation + cycle)", countType(types, "invoice.payment_succeeded"))
	}
	if countType(types, "customer.subscription.deleted") != 1 {
		t.Errorf("customer.subscription.deleted = %d; want 1", countType(types, "customer.subscription.deleted"))
	}

	canceled, err := subscription.Get(sub.ID, nil)
	if err != nil {
		t.Fatalf("subscription.Get after cancel: %v", err)
	}
	if canceled.Status != stripe.SubscriptionStatusCanceled {
		t.Errorf("status = %q; want canceled", canceled.Status)
	}
}

func countType(types []string, want string) int {
	n := 0
	for _, t := range types {
		if t == want {
			n++
		}
	}
	return n
}
