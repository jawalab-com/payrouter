package storepg_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jawalab-com/payrouter/internal/store"
	"github.com/jawalab-com/payrouter/internal/storepg"
)

// testDB is the shared Postgres URL for facade integration tests. Tests skip when
// it is unset (same convention as the Rust platform's TEST_DATABASE_URL).
var testDB = os.Getenv("PAYMENT_TEST_DATABASE_URL")

func connect(t *testing.T) (*pgxpool.Pool, *storepg.Store) {
	t.Helper()
	if testDB == "" {
		t.Skip("PAYMENT_TEST_DATABASE_URL not set; skipping PostgreSQL integration test")
	}
	ctx := context.Background()
	pool, err := storepg.ConnectPool(ctx, testDB)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := storepg.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool, storepg.New(pool, "stub")
}

// reset clears all facade tables so each test starts from a known empty state.
func reset(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		TRUNCATE facade.inbound_events, facade.payment_transitions, facade.provider_attempts, facade.refunds,
		         facade.payment_intents, facade.idempotency_records, facade.api_keys,
		         facade.accounts
		RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	pool, _ := connect(t)
	defer pool.Close()
	// Running migrate again must be a no-op (schema_migrations guards replay).
	if err := storepg.Migrate(context.Background(), pool); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	var exists bool
	if err := pool.QueryRow(context.Background(), `SELECT to_regclass('facade.inbound_events') IS NOT NULL`).Scan(&exists); err != nil || !exists {
		t.Fatalf("phase 5B migration missing: exists=%v err=%v", exists, err)
	}
}

func TestAccountAPIKeyResolveRejectRevoke(t *testing.T) {
	pool, s := connect(t)
	defer pool.Close()
	reset(t, pool)
	ctx := context.Background()

	if err := s.EnsureAccount(ctx, "acc_a", false); err != nil {
		t.Fatal(err)
	}
	hash, prefix := storepg.HashAPIKey("sk_test_secret_a")
	if err := s.EnsureAPIKey(ctx, "acc_a", prefix, hash); err != nil {
		t.Fatal(err)
	}

	if got, err := s.ResolveAPIKey(ctx, "sk_test_secret_a"); err != nil || got != "acc_a" {
		t.Fatalf("resolve: got %q err %v", got, err)
	}
	if _, err := s.ResolveAPIKey(ctx, "sk_test_unknown"); err != store.ErrUnknownAPIKey {
		t.Fatalf("unknown key: got err %v want ErrUnknownAPIKey", err)
	}
	// Revoke: delete the key (api_keys has no revoke API yet; emulate by deletion
	// — resolve_api_key ignores revoked_at, and a deleted row is unknown).
	if _, err := pool.Exec(ctx, `DELETE FROM facade.api_keys WHERE account_id='acc_a'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveAPIKey(ctx, "sk_test_secret_a"); err != store.ErrUnknownAPIKey {
		t.Fatalf("revoked key: got err %v want ErrUnknownAPIKey", err)
	}
}

func TestAPIKeyCannotBelongToTwoAccounts(t *testing.T) {
	pool, s := connect(t)
	defer pool.Close()
	reset(t, pool)
	ctx := context.Background()
	_ = s.EnsureAccount(ctx, "acc_a", false)
	_ = s.EnsureAccount(ctx, "acc_b", false)
	hash, prefix := storepg.HashAPIKey("sk_test_shared")
	if err := s.EnsureAPIKey(ctx, "acc_a", prefix, hash); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureAPIKey(ctx, "acc_b", prefix, hash); err == nil {
		t.Fatal("expected a globally unique API key to reject a second account")
	}
}

func TestPaymentIntentIsolation(t *testing.T) {
	pool, s := connect(t)
	defer pool.Close()
	reset(t, pool)
	ctx := context.Background()
	_ = s.EnsureAccount(ctx, "acc_a", false)
	_ = s.EnsureAccount(ctx, "acc_b", false)

	pi := &store.PaymentIntent{ID: "pi_a1", AccountID: "acc_a", AmountMinor: 150000, Currency: "idr", Status: "requires_action", ClientSecret: "pi_a1_secret_x", Created: 1}
	if err := s.WriteIntent(ctx, pi); err != nil {
		t.Fatal(err)
	}
	// Owner can read.
	if got, err := s.ReadIntent(ctx, "acc_a", "pi_a1"); err != nil || got == nil {
		t.Fatalf("owner read: %v %v", got, err)
	}
	// Other account cannot see it (account isolation).
	if _, err := s.ReadIntent(ctx, "acc_b", "pi_a1"); err != store.ErrNotFound {
		t.Fatalf("cross-account read: got err %v want ErrNotFound", err)
	}
}

func TestRestartPersistence(t *testing.T) {
	pool, s1 := connect(t)
	defer pool.Close()
	reset(t, pool)
	ctx := context.Background()
	_ = s1.EnsureAccount(ctx, "acc_a", false)

	pi := &store.PaymentIntent{ID: "pi_p1", AccountID: "acc_a", AmountMinor: 2000, Currency: "usd", Status: "requires_action", ClientSecret: "cs", Created: 2}
	if err := s1.WriteIntent(ctx, pi); err != nil {
		t.Fatal(err)
	}

	// Simulate a restart: a brand-new Store handle against the same pool.
	s2 := storepg.New(pool, "stub")
	got, err := s2.ReadIntent(ctx, "acc_a", "pi_p1")
	if err != nil {
		t.Fatalf("read after restart: %v", err)
	}
	if got.AmountMinor != 2000 || got.Status != "requires_action" {
		t.Fatalf("persisted intent mismatch: %+v", got)
	}
}

func TestRunInTxRollsBackPartialFinancialWrite(t *testing.T) {
	pool, s := connect(t)
	defer pool.Close()
	reset(t, pool)
	ctx := context.Background()
	_ = s.EnsureAccount(ctx, "acc_a", false)
	sentinel := errors.New("stop transaction")
	err := s.RunInTx(ctx, func(tx context.Context) error {
		if err := s.WriteIntent(tx, &store.PaymentIntent{ID: "pi_rollback", AccountID: "acc_a", AmountMinor: 50, Currency: "idr", Status: "requires_payment_method", ClientSecret: "cs", Created: 1}); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("transaction error = %v, want sentinel", err)
	}
	if _, err := s.ReadIntent(ctx, "acc_a", "pi_rollback"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("rolled-back intent error = %v, want not found", err)
	}
}

func TestIdempotencyReplayAndMismatch(t *testing.T) {
	pool, s := connect(t)
	defer pool.Close()
	reset(t, pool)
	ctx := context.Background()
	_ = s.EnsureAccount(ctx, "acc_a", false)

	hashA := []byte("hash-body-A")
	hashB := []byte("hash-body-B")

	// First claim with body A → owned, processing.
	claim, err := claimTx(s, ctx, "acc_a", "payment_intents.create", "key-1", hashA)
	if err != nil {
		t.Fatal(err)
	}
	if !claim.Owned || claim.Status != store.IdempotencyProcessing {
		t.Fatalf("first claim: %+v", claim)
	}
	// Complete it with a stored response.
	if err := s.CompleteIdempotency(ctx, "acc_a", "payment_intents.create", "key-1", 200, []byte(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	// Replay with same body → returns the stored response, not owned.
	claim, err = claimTx(s, ctx, "acc_a", "payment_intents.create", "key-1", hashA)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if claim.Owned || claim.Status != store.IdempotencyCompleted || claim.ResponseStatus != 200 || string(claim.ResponseBody) != `{"ok":true}` {
		t.Fatalf("replay claim: %+v", claim)
	}
	// Same key, different body → payload mismatch.
	if _, err := claimTx(s, ctx, "acc_a", "payment_intents.create", "key-1", hashB); err != store.ErrIdempotencyPayloadMismatch {
		t.Fatalf("mismatch: got %v want ErrIdempotencyPayloadMismatch", err)
	}
}

func TestExpiredIdempotencyLeaseCanBeReclaimed(t *testing.T) {
	pool, s := connect(t)
	defer pool.Close()
	reset(t, pool)
	ctx := context.Background()
	_ = s.EnsureAccount(ctx, "acc_a", false)
	hash := []byte("same-payload")

	if first, err := claimTx(s, ctx, "acc_a", "refunds.create", "lease-key", hash); err != nil || !first.Owned {
		t.Fatalf("first claim = %+v, %v", first, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE facade.idempotency_records SET processing_expires_at=$1 WHERE account_id='acc_a'`, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := claimTx(s, ctx, "acc_a", "refunds.create", "lease-key", hash)
	if err != nil || !reclaimed.Owned {
		t.Fatalf("reclaimed claim = %+v, %v", reclaimed, err)
	}
}

func TestConcurrentIdempotencyClaimsHaveOneOwner(t *testing.T) {
	pool, s := connect(t)
	defer pool.Close()
	reset(t, pool)
	ctx := context.Background()
	_ = s.EnsureAccount(ctx, "acc_a", false)

	type result struct {
		claim store.ClaimResult
		err   error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for range 2 {
		go func() {
			<-start
			claim, err := claimTx(s, ctx, "acc_a", "payment_intents.create", "concurrent-key", []byte("payload"))
			results <- result{claim: claim, err: err}
		}()
	}
	close(start)
	owned, inProgress := 0, 0
	for range 2 {
		got := <-results
		switch {
		case got.err == nil && got.claim.Owned:
			owned++
		case errors.Is(got.err, store.ErrIdempotencyInProgress):
			inProgress++
		default:
			t.Fatalf("unexpected claim: %+v, %v", got.claim, got.err)
		}
	}
	if owned != 1 || inProgress != 1 {
		t.Fatalf("owned=%d in-progress=%d; want 1 each", owned, inProgress)
	}
}

func TestRefundReservationLimitAndReplay(t *testing.T) {
	pool, s := connect(t)
	defer pool.Close()
	reset(t, pool)
	ctx := context.Background()
	_ = s.EnsureAccount(ctx, "acc_a", false)
	if err := s.WriteIntent(ctx, &store.PaymentIntent{ID: "pi_refund", AccountID: "acc_a", AmountMinor: 1000, Currency: "idr", Status: "succeeded", ClientSecret: "cs", Created: 1}); err != nil {
		t.Fatal(err)
	}

	first := &store.Refund{ID: "re_first", AccountID: "acc_a", PaymentIntentID: "pi_refund", Amount: 600, Currency: "idr", Created: 2}
	if err := s.RunInTx(ctx, func(tx context.Context) error { return s.ReserveRefund(tx, first) }); err != nil {
		t.Fatal(err)
	}
	// Replaying the same deterministic refund is not a second reservation.
	replay := &store.Refund{ID: "re_first", AccountID: "acc_a", PaymentIntentID: "pi_refund", Amount: 600, Currency: "idr", Created: 2}
	if err := s.RunInTx(ctx, func(tx context.Context) error { return s.ReserveRefund(tx, replay) }); err != nil {
		t.Fatalf("replay reservation: %v", err)
	}
	tooLarge := &store.Refund{ID: "re_second", AccountID: "acc_a", PaymentIntentID: "pi_refund", Amount: 500, Currency: "idr", Created: 3}
	err := s.RunInTx(ctx, func(tx context.Context) error { return s.ReserveRefund(tx, tooLarge) })
	if !errors.Is(err, store.ErrRefundExceedsPayment) {
		t.Fatalf("excess reservation error = %v", err)
	}
	remaining := &store.Refund{ID: "re_remaining", AccountID: "acc_a", PaymentIntentID: "pi_refund", Currency: "idr", Created: 4}
	if err := s.RunInTx(ctx, func(tx context.Context) error { return s.ReserveRefund(tx, remaining) }); err != nil {
		t.Fatal(err)
	}
	if remaining.Amount != 400 {
		t.Fatalf("full remaining refund = %d, want 400", remaining.Amount)
	}
}

func TestAppendOnlyAttemptsRejectMutation(t *testing.T) {
	pool, s := connect(t)
	defer pool.Close()
	reset(t, pool)
	ctx := context.Background()
	_ = s.EnsureAccount(ctx, "acc_a", false)
	_ = s.WriteIntent(ctx, &store.PaymentIntent{ID: "pi_x", AccountID: "acc_a", AmountMinor: 1, Currency: "usd", Status: "requires_action", ClientSecret: "c", Created: 1})
	att := &store.ProviderAttempt{AccountID: "acc_a", PaymentIntentID: "pi_x", Gateway: "stub", Operation: "create", Status: "requires_action"}
	if err := s.RecordAttempt(ctx, att); err != nil {
		t.Fatal(err)
	}
	// Attempts are append-only: any UPDATE must be rejected by the trigger.
	if _, err := pool.Exec(ctx, `UPDATE facade.provider_attempts SET error='tamper' WHERE id=1`); err == nil {
		t.Fatal("expected append-only trigger to reject UPDATE on provider_attempts")
	}
}

// claim wraps ClaimIdempotency in a RunInTx (required for transactional claim).
func claimTx(s *storepg.Store, ctx context.Context, accountID, op, key string, hash []byte) (store.ClaimResult, error) {
	var c store.ClaimResult
	err := s.RunInTx(ctx, func(tctx context.Context) error {
		got, err := s.ClaimIdempotency(tctx, accountID, op, key, hash)
		c = got
		return err
	})
	return c, err
}
