package server_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stripe-compatible-facade/internal/adapters/stub"
	"github.com/stripe-compatible-facade/internal/config"
	"github.com/stripe-compatible-facade/internal/server"
	"github.com/stripe-compatible-facade/internal/store"
)

// TestContractRecurringDurableAndIsolated: subscriptions created by account A are
// 404 to account B and survive a fresh server handle; invoices and checkout
// sessions are account-scoped too.
func TestContractRecurringDurableAndIsolated(t *testing.T) {
	f := setupFacade(t)

	// Catalog scaffolding owned by acc_a: product, recurring price, customer.
	_, body := f.do(t, http.MethodPost, "/v1/products", "sk_test_aaa", "", "name=Pro")
	prodID := extractID(body)
	_, body = f.do(t, http.MethodPost, "/v1/prices", "sk_test_aaa", "", "product="+prodID+"&currency=usd&unit_amount=500&recurring[interval]=month")
	priceID := extractID(body)
	_, body = f.do(t, http.MethodPost, "/v1/customers", "sk_test_aaa", "", "email=a@x.test")
	cusID := extractID(body)

	// Create a subscription (stub gateway authorizes; returns incomplete + redirect).
	status, body := f.do(t, http.MethodPost, "/v1/subscriptions", "sk_test_aaa", "", "customer="+cusID+"&items[0][price]="+priceID)
	if status != http.StatusOK {
		t.Fatalf("create subscription: %d %s", status, body)
	}
	subID := extractID(body)
	if subID == "" {
		t.Fatalf("no sub id in %s", body)
	}

	// Owner retrieves; cross-account gets 404.
	if status, _ := f.do(t, http.MethodGet, "/v1/subscriptions/"+subID, "sk_test_aaa", "", ""); status != http.StatusOK {
		t.Fatalf("owner retrieve sub: %d", status)
	}
	if status, _ := f.do(t, http.MethodGet, "/v1/subscriptions/"+subID, "sk_test_bbb", "", ""); status != http.StatusNotFound {
		t.Fatalf("cross-account sub: %d want 404", status)
	}

	// Restart persistence: a brand-new server handle reads acc_a's subscription.
	restored := server.New(config.Config{APIKey: "sk_test_aaa", ActiveGateway: "stub"}, f.pg, stub.New())
	rs := httptest.NewServer(restored)
	t.Cleanup(rs.Close)
	req, _ := http.NewRequest(http.MethodGet, rs.URL+"/v1/subscriptions/"+subID, nil)
	req.Header.Set("Authorization", "Bearer sk_test_aaa")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("retrieve sub after restart: %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Invoice isolation: a row owned by acc_a is 404 to acc_b.
	inv := &store.Invoice{ID: "in_iso", AccountID: "acc_a", SubscriptionID: subID, CustomerID: cusID, AmountMinor: 500, Currency: "usd", Status: "open", BillingReason: "subscription_create", Created: 1}
	f.pg.PutInvoice(inv)
	if status, _ := f.do(t, http.MethodGet, "/v1/invoices/in_iso", "sk_test_aaa", "", ""); status != http.StatusOK {
		t.Fatalf("owner retrieve invoice: %d", status)
	}
	if status, _ := f.do(t, http.MethodGet, "/v1/invoices/in_iso", "sk_test_bbb", "", ""); status != http.StatusNotFound {
		t.Fatalf("cross-account invoice: %d want 404", status)
	}

	// Checkout session isolation: a row owned by acc_a is 404 to acc_b.
	sess := &store.Session{ID: "cs_iso", AccountID: "acc_a", Mode: "payment", Status: "open", PaymentStatus: "unpaid", AmountTotal: 500, Currency: "usd", Created: 1, Livemode: false}
	f.pg.PutSession(sess)
	if status, _ := f.do(t, http.MethodGet, "/v1/checkout/sessions/cs_iso", "sk_test_bbb", "", ""); status != http.StatusNotFound {
		t.Fatalf("cross-account session: %d want 404", status)
	}
	if status, _ := f.do(t, http.MethodGet, "/v1/checkout/sessions/cs_iso", "sk_test_aaa", "", ""); status != http.StatusOK {
		t.Fatalf("owner retrieve session: %d", status)
	}
}
