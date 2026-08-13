package server

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
	"github.com/stripe-compatible-facade/internal/store"
	"github.com/stripe-compatible-facade/internal/webhook"
)

const (
	whServerKey = "SB-Mid-server-TEST"
	whSecret    = "whsec_testsecret"
)

// midtransSig is an independent reimplementation of the Midtrans signature so
// the test cross-checks the adapter rather than calling its internals.
func midtransSig(orderID, statusCode, gross, serverKey string) string {
	sum := sha512.Sum512([]byte(orderID + statusCode + gross + serverKey))
	return hex.EncodeToString(sum[:])
}

func newWebhookServer(t *testing.T) (*Server, *store.Memory) {
	t.Helper()
	st := store.NewMemory()
	cfg := config.Config{APIKey: "sk_test_x", Livemode: false, ActiveGateway: "midtrans"}
	return New(cfg, st, midtrans.New(whServerKey, true)), st
}

func midtransNotification(orderID, txStatus string) []byte {
	body, _ := json.Marshal(map[string]any{
		"transaction_status": txStatus,
		"transaction_id":     "tx-abc",
		"status_code":        "200",
		"gross_amount":       "50000.00",
		"order_id":           orderID,
		"fraud_status":       "accept",
		"payment_type":       "qris",
		"signature_key":      midtransSig(orderID, "200", "50000.00", whServerKey),
	})
	return body
}

func postWebhook(t *testing.T, srv *Server, gatewayName string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/"+gatewayName, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func TestWebhookInUpdatesStoreAndEnqueues(t *testing.T) {
	srv, st := newWebhookServer(t)
	st.Put(&store.PaymentIntent{ID: "pi_1", AmountMinor: 50000, Currency: "idr", Status: "requires_action", Created: 1})

	rec := postWebhook(t, srv, "midtrans", midtransNotification("pi_1", "settlement"))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	pi, err := st.Get("pi_1")
	if err != nil {
		t.Fatal(err)
	}
	if pi.Status != "succeeded" {
		t.Errorf("PI status = %q; want succeeded", pi.Status)
	}
	if deliveries := st.ListWebhooks(); len(deliveries) != 1 {
		t.Errorf("enqueued %d events; want 1", len(deliveries))
	} else if deliveries[0].Type != "payment_intent.succeeded" {
		t.Errorf("event type = %q; want payment_intent.succeeded", deliveries[0].Type)
	}
}

func TestWebhookInIdempotent(t *testing.T) {
	srv, st := newWebhookServer(t)
	st.Put(&store.PaymentIntent{ID: "pi_1", Status: "requires_action", Created: 1})

	// First notification: status changes, one event enqueued.
	postWebhook(t, srv, "midtrans", midtransNotification("pi_1", "settlement"))
	// Re-send the same status: must NOT enqueue a duplicate event.
	postWebhook(t, srv, "midtrans", midtransNotification("pi_1", "settlement"))

	if deliveries := st.ListWebhooks(); len(deliveries) != 1 {
		t.Errorf("enqueued %d events; want 1 (idempotent)", len(deliveries))
	}
}

func TestWebhookInBadSignature(t *testing.T) {
	srv, st := newWebhookServer(t)
	st.Put(&store.PaymentIntent{ID: "pi_1", Status: "requires_action", Created: 1})

	body := midtransNotification("pi_1", "settlement")
	// Corrupt the signature.
	body = bytes.Replace(body, []byte(midtransSig("pi_1", "200", "50000.00", whServerKey)), []byte(strings.Repeat("a", 128)), 1)

	rec := postWebhook(t, srv, "midtrans", body)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
	if deliveries := st.ListWebhooks(); len(deliveries) != 0 {
		t.Errorf("bad-signature notification should not enqueue events; got %d", len(deliveries))
	}
}

func TestWebhookInUnknownGateway404(t *testing.T) {
	srv, _ := newWebhookServer(t)
	rec := postWebhook(t, srv, "xendit", midtransNotification("pi_1", "settlement"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for inactive gateway, got %d", rec.Code)
	}
}

// TestWebhookEndToEnd is the full M2 proof: Midtrans notification in -> verify ->
// store update -> enqueue -> deliverer re-signs as Stripe-Signature -> merchant
// verifies it and reads a valid Stripe Event.
func TestWebhookEndToEnd(t *testing.T) {
	srv, st := newWebhookServer(t)
	st.Put(&store.PaymentIntent{ID: "pi_e2e", AmountMinor: 50000, Currency: "idr",
		Status: "requires_action", PaymentMethodType: "id_qris", Created: 1})

	var gotSig, gotBody string
	merchant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSig = r.Header.Get("Stripe-Signature")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer merchant.Close()

	postWebhook(t, srv, "midtrans", midtransNotification("pi_e2e", "settlement"))

	// Run one delivery cycle against the merchant endpoint.
	d := webhook.New(st, merchant.URL, whSecret, nil)
	d.DeliverOnce()

	if gotSig == "" || gotBody == "" {
		t.Fatal("merchant received no delivery")
	}
	// Merchant verifies the signature with the shared secret (Stripe SDK behavior).
	if err := webhook.Verify([]byte(gotBody), gotSig, whSecret, 0, 0); err != nil {
		t.Fatalf("Stripe-Signature did not verify at merchant: %v", err)
	}

	// The delivered payload is a Stripe Event wrapping a succeeded PaymentIntent.
	var ev struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		Data struct {
			Object struct {
				ID     string `json:"id"`
				Object string `json:"object"`
				Status string `json:"status"`
			} `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(gotBody), &ev); err != nil {
		t.Fatalf("merchant body is not a valid Stripe Event: %v", err)
	}
	if ev.Type != "payment_intent.succeeded" {
		t.Errorf("event type = %q; want payment_intent.succeeded", ev.Type)
	}
	if ev.Data.Object.Status != "succeeded" || ev.Data.Object.ID != "pi_e2e" {
		t.Errorf("data.object = %+v", ev.Data.Object)
	}
}
