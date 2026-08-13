package compat

import (
	"context"
	"os"
	"testing"

	"github.com/jawalab-com/payrouter/internal/store"
	"github.com/jawalab-com/payrouter/internal/storepg"
)

var compatDB = os.Getenv("PAYMENT_TEST_DATABASE_URL")

// newTestStore returns a durable storepg store (migrated, reset, with the default
// account + facade API key seeded) when PAYMENT_TEST_DATABASE_URL is set, so the
// official stripe-go end-to-end suite runs through PostgreSQL; otherwise the
// in-memory store (the historical baseline). The storepg store implements the
// OutboundStore capability, so the deliverer drains the durable outbox.
func newTestStore(t *testing.T) store.Store {
	t.Helper()
	if compatDB == "" {
		return store.NewMemory()
	}
	ctx := context.Background()
	pool, err := storepg.ConnectPool(ctx, compatDB)
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
		         facade.outbound_events, facade.checkout_sessions, facade.subscriptions,
		         facade.invoices, facade.customers, facade.products, facade.prices,
		         facade.api_keys, facade.accounts
		RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("reset: %v", err)
	}
	pg := storepg.New(pool, "midtrans")
	if err := storepg.SeedDefaultAccount(ctx, pool, facadeAPIKey); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	return pg
}
