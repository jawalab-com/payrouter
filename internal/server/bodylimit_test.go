package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jawalab-com/payrouter/internal/adapters/stub"
	"github.com/jawalab-com/payrouter/internal/config"
	"github.com/jawalab-com/payrouter/internal/store"
)

// newLimitedServer builds a server with a deliberately tiny body ceiling so the
// limit is exercised without allocating a large test fixture.
func newLimitedServer(t *testing.T, maxBody int64) *Server {
	t.Helper()
	return New(config.Config{
		APIKey: "sk_test_x", ActiveGateway: "stub", MaxBodyBytes: maxBody,
	}, store.NewMemory(), stub.New())
}

// TestOversizedBodyIsRejected covers the idempotency middleware's io.ReadAll,
// which previously read an attacker-controlled body into memory with no ceiling.
// An authenticated caller could OOM the process with a single request.
func TestOversizedBodyIsRejected(t *testing.T) {
	srv := newLimitedServer(t, 512)

	body := "amount=50000&currency=idr&description=" + strings.Repeat("A", 4096)
	req := httptest.NewRequest(http.MethodPost, "/v1/payment_intents", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk_test_x")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// An Idempotency-Key routes the request through the io.ReadAll path.
	req.Header.Set("Idempotency-Key", "key_oversize")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("oversized body was accepted (status 200); the cap is not applied")
	}
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want %d: %s", rec.Code, http.StatusRequestEntityTooLarge, rec.Body.String())
	}
}

// TestOversizedBodyRejectedOnParseFormPath covers handlers that call ParseForm
// directly rather than going through the idempotency middleware.
func TestOversizedBodyRejectedOnParseFormPath(t *testing.T) {
	srv := newLimitedServer(t, 512)

	body := "mode=payment&success_url=https://a.test/ok&customer_email=" + strings.Repeat("b", 4096)
	req := httptest.NewRequest(http.MethodPost, "/v1/checkout/sessions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk_test_x")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("oversized body was accepted on the ParseForm path")
	}
}

// TestWebhookBodyIsCapped matters most: gateway callbacks are unauthenticated by
// design, so an uncapped body there is reachable without any credential.
func TestWebhookBodyIsCapped(t *testing.T) {
	srv := newLimitedServer(t, 512)

	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/stub",
		strings.NewReader(strings.Repeat("x", 8192)))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Errorf("oversized unauthenticated webhook body was accepted: %s", rec.Body.String())
	}
}

// TestNormalBodyStillPasses guards against the cap being set so low it breaks
// ordinary traffic.
func TestNormalBodyStillPasses(t *testing.T) {
	srv := newTestServer(t) // default ceiling

	req := httptest.NewRequest(http.MethodPost, "/v1/payment_intents",
		strings.NewReader("amount=50000&currency=idr&payment_method_types[]=id_qris"))
	req.Header.Set("Authorization", "Bearer sk_test_x")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Idempotency-Key", "key_normal")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("normal request rejected: %d %s", rec.Code, rec.Body.String())
	}
}

// TestDefaultCeilingAppliesWhenUnset verifies a zero-value config still bounds
// the body rather than falling back to unlimited.
func TestDefaultCeilingAppliesWhenUnset(t *testing.T) {
	srv := New(config.Config{APIKey: "sk_test_x", ActiveGateway: "stub"},
		store.NewMemory(), stub.New())

	if got := srv.maxBodyBytes(); got != config.DefaultMaxBodyBytes {
		t.Errorf("maxBodyBytes() = %d, want %d", got, config.DefaultMaxBodyBytes)
	}
}
