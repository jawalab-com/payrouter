package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stripe-compatible-facade/internal/adapters/stub"
	"github.com/stripe-compatible-facade/internal/config"
	"github.com/stripe-compatible-facade/internal/store"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	return New(config.Config{APIKey: "sk_test_x", ActiveGateway: "stub", Livemode: false}, store.NewMemory(), stub.New())
}

func TestCreateAndRetrievePaymentIntent(t *testing.T) {
	srv := newTestServer(t)

	// Create (form-encoded, like the Stripe SDK sends).
	body := strings.NewReader(
		"amount=50000&currency=idr" +
			"&payment_method_types[]=id_virtual_account" +
			"&metadata[x_bank]=bca" +
			"&return_url=https://app.test/done",
	)
	req := httptest.NewRequest(http.MethodPost, "/v1/payment_intents", body)
	req.Header.Set("Authorization", "Bearer sk_test_x")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("create: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	resp := rec.Body.String()
	if !strings.Contains(resp, `"id":"pi_`) {
		t.Fatalf("create: expected pi_ id: %s", resp)
	}
	if !strings.Contains(resp, `"object":"payment_intent"`) {
		t.Fatalf("create: expected object=payment_intent: %s", resp)
	}
	if !strings.Contains(resp, `"status":"requires_action"`) {
		t.Fatalf("create: expected requires_action status: %s", resp)
	}
	if !strings.Contains(resp, `"redirect_to_url":`) {
		t.Fatalf("create: expected next_action.redirect_to_url: %s", resp)
	}
	if !strings.Contains(resp, `"x_bank":"bca"`) {
		t.Fatalf("create: expected metadata to round-trip: %s", resp)
	}
	if !strings.Contains(resp, `"payment_gateway_selected":"stub"`) {
		t.Fatalf("create: expected payment_gateway_selected metadata: %s", resp)
	}
	if !strings.Contains(resp, `"payment_routing_mode":"static"`) {
		t.Fatalf("create: expected payment_routing_mode metadata: %s", resp)
	}

	id := extractID(resp)

	// Retrieve.
	req2 := httptest.NewRequest(http.MethodGet, "/v1/payment_intents/"+id, nil)
	req2.Header.Set("Authorization", "Bearer sk_test_x")
	rec2 := httptest.NewRecorder()
	srv.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("retrieve: expected 200, got %d: %s", rec2.Code, rec2.Body.String())
	}
	if !strings.Contains(rec2.Body.String(), `"id":"`+id+`"`) {
		t.Fatalf("retrieve: expected same id: %s", rec2.Body.String())
	}
}

func TestRetrieveMissingReturns404(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/payment_intents/pi_nope", nil)
	req.Header.Set("Authorization", "Bearer sk_test_x")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAuthRejectsMissingKey(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/payment_intents", strings.NewReader("amount=1000"))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestAuthRejectsBadKey(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/payment_intents", strings.NewReader("amount=1000"))
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestInvalidAmount(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/payment_intents", strings.NewReader("amount=notanumber"))
	req.Header.Set("Authorization", "Bearer sk_test_x")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func extractID(body string) string {
	i := strings.Index(body, `"id":"pi_`)
	if i < 0 {
		return ""
	}
	rest := body[i+len(`"id":"`):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		return ""
	}
	return rest[:j]
}
