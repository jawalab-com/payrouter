package server_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stripe-compatible-facade/internal/adapters/stub"
	"github.com/stripe-compatible-facade/internal/config"
	"github.com/stripe-compatible-facade/internal/server"
	"github.com/stripe-compatible-facade/internal/storepg"
	stripe "github.com/stripe/stripe-go/v81"
	"github.com/stripe/stripe-go/v81/paymentintent"
	"github.com/stripe/stripe-go/v81/refund"
)

var contractDB = os.Getenv("FACADE_TEST_DATABASE_URL")

type facadeFixture struct {
	ts   *httptest.Server
	pg   *storepg.Store
	pool *pgxpool.Pool
	ctx  context.Context
}

func setupFacade(t *testing.T) *facadeFixture {
	t.Helper()
	if contractDB == "" {
		t.Skip("FACADE_TEST_DATABASE_URL not set; skipping durable contract test")
	}
	ctx := context.Background()
	pool, err := storepg.ConnectPool(ctx, contractDB)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := storepg.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		TRUNCATE facade.inbound_events, facade.payment_transitions, facade.provider_attempts, facade.refunds,
		         facade.payment_intents, facade.idempotency_records, facade.api_keys,
		         facade.accounts
		RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("reset: %v", err)
	}
	pg := storepg.New(pool, "stub")
	// Two accounts + keys for isolation/replay coverage.
	seedKey(t, pg, ctx, "acc_a", "sk_test_aaa")
	seedKey(t, pg, ctx, "acc_b", "sk_test_bbb")

	cfg := config.Config{APIKey: "sk_test_aaa", ActiveGateway: "stub"}
	srv := server.New(cfg, pg, stub.New())
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return &facadeFixture{ts: ts, pg: pg, pool: pool, ctx: ctx}
}

func seedKey(t *testing.T, pg *storepg.Store, ctx context.Context, accountID, secret string) {
	t.Helper()
	if err := pg.EnsureAccount(ctx, accountID, false); err != nil {
		t.Fatal(err)
	}
	hash, prefix := storepg.HashAPIKey(secret)
	if err := pg.EnsureAPIKey(ctx, accountID, prefix, hash); err != nil {
		t.Fatal(err)
	}
}

// do performs a request against the facade with the given Bearer key and optional
// Idempotency-Key. body is sent as application/x-www-form-urlencoded.
func (f *facadeFixture) do(t *testing.T, method, path, key, idemKey, body string) (int, string) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, f.ts.URL+path, r)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	if body != "" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestContractAccountAPIKey(t *testing.T) {
	f := setupFacade(t)
	// Valid key → 200.
	status, _ := f.do(t, http.MethodPost, "/v1/payment_intents", "sk_test_aaa", "", "amount=1000&currency=usd")
	if status != http.StatusOK {
		t.Fatalf("valid key: got %d want 200", status)
	}
	// Unknown key → 401.
	status, _ = f.do(t, http.MethodPost, "/v1/payment_intents", "sk_test_unknown", "", "amount=1000&currency=usd")
	if status != http.StatusUnauthorized {
		t.Fatalf("unknown key: got %d want 401", status)
	}
}

func TestContractAccountIsolation(t *testing.T) {
	f := setupFacade(t)
	// Account A creates an intent.
	status, body := f.do(t, http.MethodPost, "/v1/payment_intents", "sk_test_aaa", "", "amount=5000&currency=usd")
	if status != http.StatusOK {
		t.Fatalf("create: %d %s", status, body)
	}
	id := extractID(body)
	if id == "" {
		t.Fatalf("no id in %s", body)
	}
	// Account B cannot retrieve A's intent (404).
	status, _ = f.do(t, http.MethodGet, "/v1/payment_intents/"+id, "sk_test_bbb", "", "")
	if status != http.StatusNotFound {
		t.Fatalf("cross-account retrieve: got %d want 404", status)
	}
	// Account A can.
	status, _ = f.do(t, http.MethodGet, "/v1/payment_intents/"+id, "sk_test_aaa", "", "")
	if status != http.StatusOK {
		t.Fatalf("owner retrieve: got %d want 200", status)
	}
}

func TestContractIdempotencyReplayAndMismatch(t *testing.T) {
	f := setupFacade(t)
	body := "amount=3000&currency=usd"
	// First create with Idempotency-Key.
	s1, b1 := f.do(t, http.MethodPost, "/v1/payment_intents", "sk_test_aaa", "idem-1", body)
	if s1 != http.StatusOK {
		t.Fatalf("first: %d %s", s1, b1)
	}
	// Replay same key+body → same response (same pi_ id).
	s2, b2 := f.do(t, http.MethodPost, "/v1/payment_intents", "sk_test_aaa", "idem-1", body)
	if s2 != http.StatusOK {
		t.Fatalf("replay: %d %s", s2, b2)
	}
	if extractID(b1) != extractID(b2) {
		t.Fatalf("replay did not return same intent: %s vs %s", b1, b2)
	}
	// Same key, different body → 409 payload mismatch.
	s3, _ := f.do(t, http.MethodPost, "/v1/payment_intents", "sk_test_aaa", "idem-1", "amount=9999&currency=usd")
	if s3 != http.StatusConflict {
		t.Fatalf("mismatch: got %d want 409", s3)
	}
}

func TestDurableStripeGoCreateRefundAndReplay(t *testing.T) {
	f := setupFacade(t)
	stripe.SetBackend(stripe.APIBackend, stripe.GetBackendWithConfig(stripe.APIBackend, &stripe.BackendConfig{
		URL: stripe.String(f.ts.URL),
	}))
	stripe.Key = "sk_test_aaa"

	create := &stripe.PaymentIntentParams{Amount: stripe.Int64(1000), Currency: stripe.String(string(stripe.CurrencyIDR))}
	create.SetIdempotencyKey("sdk-create")
	pi, err := paymentintent.New(create)
	if err != nil {
		t.Fatalf("stripe-go create: %v", err)
	}
	createAgain := &stripe.PaymentIntentParams{Amount: stripe.Int64(1000), Currency: stripe.String(string(stripe.CurrencyIDR))}
	createAgain.SetIdempotencyKey("sdk-create")
	replayed, err := paymentintent.New(createAgain)
	if err != nil || replayed.ID != pi.ID {
		t.Fatalf("stripe-go replay = %v, %v; want %s", replayed, err, pi.ID)
	}

	rp := &stripe.RefundParams{PaymentIntent: stripe.String(pi.ID), Amount: stripe.Int64(400)}
	rp.SetIdempotencyKey("sdk-refund")
	rf, err := refund.New(rp)
	if err != nil {
		t.Fatalf("stripe-go refund: %v", err)
	}
	if rf.PaymentIntent == nil || rf.PaymentIntent.ID != pi.ID || rf.Amount != 400 {
		t.Fatalf("stripe-go refund mismatch: %+v", rf)
	}

	var creates int
	if err := f.pool.QueryRow(f.ctx,
		`SELECT count(*) FROM facade.provider_attempts WHERE account_id='acc_a' AND payment_intent_id=$1 AND operation='create' AND status='processing'`, pi.ID,
	).Scan(&creates); err != nil {
		t.Fatal(err)
	}
	if creates != 1 {
		t.Fatalf("provider create attempts = %d, want 1", creates)
	}
}

func TestContractPaymentIntentCreateRetrieveConfirmDurable(t *testing.T) {
	f := setupFacade(t)
	// Create.
	status, body := f.do(t, http.MethodPost, "/v1/payment_intents", "sk_test_aaa", "", "amount=12000&currency=idr")
	if status != http.StatusOK {
		t.Fatalf("create: %d %s", status, body)
	}
	id := extractID(body)
	// Retrieve through a FRESH server handle (simulates restart): a new server
	// backed by the same pool reads the persisted intent.
	restored := server.New(config.Config{APIKey: "sk_test_aaa", ActiveGateway: "stub"}, f.pg, stub.New())
	rs := httptest.NewServer(restored)
	t.Cleanup(rs.Close)
	req, _ := http.NewRequest(http.MethodGet, rs.URL+"/v1/payment_intents/"+id, nil)
	req.Header.Set("Authorization", "Bearer sk_test_aaa")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	rb, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("retrieve after restart: %d %s", resp.StatusCode, rb)
	}
	if extractID(string(rb)) != id {
		t.Fatalf("retrieved id mismatch")
	}
	// The stub gateway returns requires_action; the persisted status survives restart.
	if !strings.Contains(string(rb), `"status":"requires_action"`) {
		t.Fatalf("expected requires_action status in %s", rb)
	}
}

// extractID pulls the "id":"pi_..." value out of a JSON response body.
func extractID(body string) string {
	idx := strings.Index(body, `"id":"`)
	if idx < 0 {
		return ""
	}
	rest := body[idx+len(`"id":"`):]
	end := strings.Index(rest, `"`)
	if end < 0 {
		return ""
	}
	return rest[:end]
}
