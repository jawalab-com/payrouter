package server_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jawalab-com/payrouter/internal/config"
	"github.com/jawalab-com/payrouter/internal/gateway"
	"github.com/jawalab-com/payrouter/internal/server"
	"github.com/jawalab-com/payrouter/internal/store"
	"github.com/jawalab-com/payrouter/internal/storepg"
	stripe "github.com/stripe/stripe-go/v81"
)

var inboundDB = os.Getenv("PAYMENT_TEST_DATABASE_URL")

// fakeWebhookGW is a test-only gateway.Gateway whose ParseWebhook returns a
// settable list of events (ignoring the body), so the durable inbound path can be
// driven deterministically without a real PSP notification harness.
type fakeWebhookGW struct {
	name string
	mu   sync.Mutex
	evs  []gateway.WebhookEvent
}

func (g *fakeWebhookGW) Name() string { return g.name }
func (g *fakeWebhookGW) setEvents(evs []gateway.WebhookEvent) {
	g.mu.Lock()
	g.evs = evs
	g.mu.Unlock()
}
func (g *fakeWebhookGW) CreatePayment(_ context.Context, in *gateway.CreatePaymentInput) (*gateway.PaymentResult, error) {
	return &gateway.PaymentResult{Reference: in.Reference, GatewayReference: "fake_" + in.Reference, Status: stripe.PaymentIntentStatusRequiresAction}, nil
}
func (g *fakeWebhookGW) GetStatus(_ context.Context, gwRef string) (*gateway.PaymentResult, error) {
	return &gateway.PaymentResult{Reference: gwRef, GatewayReference: gwRef, Status: stripe.PaymentIntentStatusRequiresAction}, nil
}
func (g *fakeWebhookGW) Refund(_ context.Context, in *gateway.RefundInput) (*gateway.RefundResult, error) {
	return &gateway.RefundResult{Reference: in.Reference, GatewayReference: "fake_" + in.PaymentReference, Status: "succeeded", AmountMinor: in.AmountMinor}, nil
}
func (g *fakeWebhookGW) ParseWebhook(_ context.Context, _ *http.Request) ([]gateway.WebhookEvent, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]gateway.WebhookEvent, len(g.evs))
	copy(out, g.evs)
	return out, nil
}

func inboundSetup(t *testing.T) (*httptest.Server, *pgxpool.Pool, *storepg.Store, *fakeWebhookGW, context.Context) {
	t.Helper()
	if inboundDB == "" {
		t.Skip("PAYMENT_TEST_DATABASE_URL not set; skipping durable inbound test")
	}
	ctx := context.Background()
	pool, err := storepg.ConnectPool(ctx, inboundDB)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := storepg.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		TRUNCATE facade.payment_transitions, facade.provider_attempts, facade.refunds,
		         facade.payment_intents, facade.idempotency_records, facade.inbound_events,
		         facade.api_keys, facade.accounts
		RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("reset: %v", err)
	}
	pg := storepg.New(pool, "fakegw")
	if err := pg.EnsureAccount(ctx, "acc_a", false); err != nil {
		t.Fatal(err)
	}
	fake := &fakeWebhookGW{name: "fakegw"}
	srv := server.New(config.Config{APIKey: "sk_test_aaa", ActiveGateway: "fakegw"}, pg, fake)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts, pool, pg, fake, ctx
}

func postWebhook(t *testing.T, ts *httptest.Server, body []byte) int {
	t.Helper()
	resp, err := http.Post(ts.URL+"/v1/webhooks/fakegw", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

func countRows(t *testing.T, pool *pgxpool.Pool, table string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM `+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// TestInboundDurableApplyAndDedup: one notification applies the intent; an
// identical re-delivery is a no-op (one transition, one inbound row).
func TestInboundDurableApplyAndDedup(t *testing.T) {
	ts, pool, pg, fake, ctx := inboundSetup(t)
	pi := &store.PaymentIntent{ID: "pi_inb", AccountID: "acc_a", AmountMinor: 5000, Currency: "usd", Status: string(stripe.PaymentIntentStatusRequiresAction), ClientSecret: "pi_inb_secret", Created: 1}
	if err := pg.WriteIntent(ctx, pi); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"notification":"succeeded-pi_inb"}`)
	fake.setEvents([]gateway.WebhookEvent{{
		ProviderEventID: "tx-1", Type: "payment_intent.succeeded",
		Reference: "pi_inb", Status: stripe.PaymentIntentStatusSucceeded,
	}})

	if code := postWebhook(t, ts, body); code != http.StatusOK {
		t.Fatalf("first post: %d", code)
	}
	got, err := pg.ReadIntent(ctx, "acc_a", "pi_inb")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != string(stripe.PaymentIntentStatusSucceeded) {
		t.Fatalf("intent status: %q want succeeded", got.Status)
	}
	if n := countRows(t, pool, "facade.inbound_events"); n != 1 {
		t.Fatalf("inbound rows after first: %d want 1", n)
	}
	if n := countRows(t, pool, "facade.payment_transitions"); n != 1 {
		t.Fatalf("transitions after first: %d want 1", n)
	}

	// Re-deliver the identical body: deduped (no new rows, status unchanged).
	if code := postWebhook(t, ts, body); code != http.StatusOK {
		t.Fatalf("dup post: %d", code)
	}
	if n := countRows(t, pool, "facade.inbound_events"); n != 1 {
		t.Fatalf("dedup failed: inbound rows %d want 1", n)
	}
	if n := countRows(t, pool, "facade.payment_transitions"); n != 1 {
		t.Fatalf("dedup failed: transitions %d want 1", n)
	}
}

// TestInboundReconcilerCrashRecovery: a row stuck in 'processing' (simulated
// crash with an expired lease) is reclaimed, re-applied idempotently, and
// resolved by the reconciler.
func TestInboundReconcilerCrashRecovery(t *testing.T) {
	ts, pool, pg, fake, ctx := inboundSetup(t)
	pi := &store.PaymentIntent{ID: "pi_rec", AccountID: "acc_a", AmountMinor: 3000, Currency: "usd", Status: string(stripe.PaymentIntentStatusRequiresAction), ClientSecret: "pi_rec_secret", Created: 2}
	if err := pg.WriteIntent(ctx, pi); err != nil {
		t.Fatal(err)
	}
	fake.setEvents([]gateway.WebhookEvent{{
		ProviderEventID: "tx-2", Type: "payment_intent.succeeded",
		Reference: "pi_rec", Status: stripe.PaymentIntentStatusSucceeded,
	}})
	if code := postWebhook(t, ts, []byte(`{"n":"rec"}`)); code != http.StatusOK {
		t.Fatalf("post: %d", code)
	}

	// Simulate a crash: leave the row 'processing' with an expired lease.
	if _, err := pool.Exec(ctx, `
		UPDATE facade.inbound_events SET status='processing', claimed_until=now()-interval '1 minute', claim_token='stale'
		WHERE gateway='fakegw'`); err != nil {
		t.Fatal(err)
	}

	// A reconciler built against a server with the same fake+events drains the row.
	reFake := &fakeWebhookGW{name: "fakegw"}
	reFake.setEvents([]gateway.WebhookEvent{{
		ProviderEventID: "tx-2", Type: "payment_intent.succeeded",
		Reference: "pi_rec", Status: stripe.PaymentIntentStatusSucceeded,
	}})
	srv := server.New(config.Config{APIKey: "sk_test_aaa", ActiveGateway: "fakegw"}, pg, reFake)
	rec := server.NewReconciler(srv)
	if n := rec.ReclaimOnce(ctx); n != 1 {
		t.Fatalf("reclaim claimed %d rows, want 1", n)
	}

	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM facade.inbound_events WHERE gateway='fakegw'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != store.InboundApplied {
		t.Fatalf("reconciler left row %q, want applied", status)
	}
	got, _ := pg.ReadIntent(ctx, "acc_a", "pi_rec")
	if got.Status != string(stripe.PaymentIntentStatusSucceeded) {
		t.Fatalf("intent after recovery: %q", got.Status)
	}
	// Idempotent re-apply did not add a second transition.
	if n := countRows(t, pool, "facade.payment_transitions"); n != 1 {
		t.Fatalf("transitions after recovery: %d want 1", n)
	}
}

// TestInboundUnknownReferenceIsHarmless: a notification for an unknown order is
// recorded and acknowledged (not an error), with no state change.
func TestInboundUnknownReferenceIsHarmless(t *testing.T) {
	ts, pool, pg, fake, ctx := inboundSetup(t)
	_ = pg
	fake.setEvents([]gateway.WebhookEvent{{
		ProviderEventID: "tx-x", Type: "payment_intent.succeeded",
		Reference: "pi_missing", Status: stripe.PaymentIntentStatusSucceeded,
	}})
	if code := postWebhook(t, ts, []byte(`{"n":"unknown"}`)); code != http.StatusOK {
		t.Fatalf("post: %d", code)
	}
	// Recorded and resolved (applied — nothing to apply is a quiet success).
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM facade.inbound_events WHERE gateway='fakegw'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != store.InboundApplied {
		t.Fatalf("unknown-ref row %q, want applied", status)
	}
	_ = ctx
}
