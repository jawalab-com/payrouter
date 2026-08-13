package webhook_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jawalab-com/payrouter/internal/store"
	"github.com/jawalab-com/payrouter/internal/storepg"
	"github.com/jawalab-com/payrouter/internal/webhook"
	stripe "github.com/stripe/stripe-go/v81"
	stripewebhook "github.com/stripe/stripe-go/v81/webhook"
)

func TestDurableOutboundDeliverySignature(t *testing.T) {
	db := os.Getenv("PAYMENT_TEST_DATABASE_URL")
	if db == "" {
		t.Skip("PAYMENT_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := storepg.ConnectPool(ctx, db)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := storepg.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE facade.outbound_events RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("reset: %v", err)
	}
	pg := storepg.New(pool, "stub")
	if err := pg.EnsureAccount(ctx, "acc_e", false); err != nil {
		t.Fatal(err)
	}

	secret := "whsec_test_outbound_e2e"

	// Merchant endpoint captures the body + Stripe-Signature.
	var gotBody []byte
	var gotSig string
	merchant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotSig = r.Header.Get("Stripe-Signature")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(merchant.Close)

	// Enqueue a durable Stripe-shaped event.
	payload := []byte(`{"id":"evt_e2e","object":"event","api_version":"` + stripe.APIVersion + `","created":1,"type":"payment_intent.succeeded","data":{"object":{"id":"pi_e2e","object":"payment_intent","amount":5000,"currency":"usd","status":"succeeded"}}}`)
	if err := pg.EnqueueOutbound(ctx, &store.OutboundEvent{
		ID: "evt_e2e", AccountID: "acc_e", Type: "payment_intent.succeeded",
		Reference: "pi_e2e", Payload: payload, Created: time.Now().Unix(),
	}); err != nil {
		t.Fatal(err)
	}

	// Deliverer pointed at the merchant, backed by the durable store.
	d := webhook.New(pg, merchant.URL, secret, &http.Client{Timeout: 5 * time.Second})
	if n := d.DeliverOnce(); n != 1 {
		t.Fatalf("DeliverOnce delivered %d, want 1", n)
	}

	if len(gotBody) == 0 {
		t.Fatal("merchant received no body")
	}
	// The official SDK must accept the signature we produced.
	ev, err := stripewebhook.ConstructEvent(gotBody, gotSig, secret)
	if err != nil {
		t.Fatalf("ConstructEvent: %v (sig=%q)", err, gotSig)
	}
	if ev.Type != "payment_intent.succeeded" || ev.ID != "evt_e2e" {
		t.Fatalf("verified event mismatch: %+v", ev)
	}
	// The durable row is now delivered.
	g, _ := pg.GetOutbound(ctx, "evt_e2e")
	if g.Status != store.OutboundDelivered {
		t.Fatalf("outbound status %q want delivered", g.Status)
	}

	// A second drain has nothing to do (delivered rows are not re-leased).
	if n := d.DeliverOnce(); n != 0 {
		t.Fatalf("second DeliverOnce delivered %d, want 0", n)
	}

	// Replay re-delivers the same event with a fresh, valid signature.
	if err := pg.ReplayOutbound(ctx, "evt_e2e"); err != nil {
		t.Fatal(err)
	}
	gotBody = nil
	gotSig = ""
	if n := d.DeliverOnce(); n != 1 {
		t.Fatalf("post-replay DeliverOnce delivered %d, want 1", n)
	}
	if _, err := stripewebhook.ConstructEvent(gotBody, gotSig, secret); err != nil {
		t.Fatalf("replay ConstructEvent: %v", err)
	}
	// Sanity: the replayed body is the same immutable payload.
	if !strings.Contains(string(gotBody), `"evt_e2e"`) {
		t.Fatalf("replayed body lost event id: %s", gotBody)
	}
}
