package storepg_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stripe-compatible-facade/internal/store"
)

func inbound(gateway, providerID string, hash byte) *store.InboundEvent {
	return &store.InboundEvent{
		Gateway: gateway, ProviderEventID: providerID, EventHash: []byte{hash},
		EventType: "payment_intent.succeeded", Reference: "pi_inbound",
		Raw: []byte(`{"status":"paid"}`), Parsed: []byte(`{"status":"succeeded"}`),
	}
}

func TestInboundLogicalIdentityDeduplicatesReserializedDelivery(t *testing.T) {
	pool, s := connect(t)
	defer pool.Close()
	reset(t, pool)
	ctx := context.Background()

	first, err := claimInboundTx(s, ctx, inbound("midtrans", "transaction-1", 1))
	if err != nil || !first.Owned {
		t.Fatalf("first claim = %+v, %v", first, err)
	}
	duplicate, err := claimInboundTx(s, ctx, inbound("midtrans", "transaction-1", 2))
	if err != nil {
		t.Fatal(err)
	}
	if duplicate.Owned || duplicate.Event.ID != first.Event.ID {
		t.Fatalf("logical duplicate = %+v, want existing event %d", duplicate, first.Event.ID)
	}
}

func TestInboundFallbackHashDeduplicatesWithoutProviderIdentity(t *testing.T) {
	pool, s := connect(t)
	defer pool.Close()
	reset(t, pool)
	ctx := context.Background()

	first, err := claimInboundTx(s, ctx, inbound("mayar", "", 3))
	if err != nil || !first.Owned {
		t.Fatalf("first claim = %+v, %v", first, err)
	}
	duplicate, err := claimInboundTx(s, ctx, inbound("mayar", "", 3))
	if err != nil || duplicate.Owned || duplicate.Event.ID != first.Event.ID {
		t.Fatalf("hash duplicate = %+v, %v", duplicate, err)
	}
}

func TestInboundExpiredLeaseRejectsStaleCompletion(t *testing.T) {
	pool, s := connect(t)
	defer pool.Close()
	reset(t, pool)
	ctx := context.Background()
	_ = s.EnsureAccount(ctx, "acc_a", false)

	first, err := claimInboundTx(s, ctx, inbound("xendit", "invoice-1", 4))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE facade.inbound_events SET claimed_until=now()-interval '1 second' WHERE id=$1`, first.Event.ID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := s.ReclaimStuckInbound(ctx, "xendit", 30)
	if err != nil || reclaimed == nil || reclaimed.ClaimToken == first.Event.ClaimToken {
		t.Fatalf("reclaim = %+v, %v", reclaimed, err)
	}
	err = s.RunInTx(ctx, func(tx context.Context) error {
		return s.CompleteInbound(tx, first.Event.ID, first.Event.ClaimToken, "acc_a", true, "", 5)
	})
	if !errors.Is(err, store.ErrInboundClaimLost) {
		t.Fatalf("stale completion = %v, want claim lost", err)
	}
	if err := s.RunInTx(ctx, func(tx context.Context) error {
		return s.CompleteInbound(tx, reclaimed.ID, reclaimed.ClaimToken, "acc_a", true, "", 5)
	}); err != nil {
		t.Fatal(err)
	}
	var accountID, status string
	if err := pool.QueryRow(ctx, `SELECT account_id,status FROM facade.inbound_events WHERE id=$1`, reclaimed.ID).Scan(&accountID, &status); err != nil {
		t.Fatal(err)
	}
	if accountID != "acc_a" || status != store.InboundApplied {
		t.Fatalf("bound completion = account %q status %q", accountID, status)
	}
}

func TestInboundFailureWaitsForBackoffAndEventuallyDeadLetters(t *testing.T) {
	pool, s := connect(t)
	defer pool.Close()
	reset(t, pool)
	ctx := context.Background()

	claim, err := claimInboundTx(s, ctx, inbound("doku", "payment-1", 5))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RunInTx(ctx, func(tx context.Context) error {
		return s.CompleteInbound(tx, claim.Event.ID, claim.Event.ClaimToken, "", false, "temporary", 2)
	}); err != nil {
		t.Fatal(err)
	}
	if immediate, err := s.ReclaimStuckInbound(ctx, "doku", 30); err != nil || immediate != nil {
		t.Fatalf("immediate reclaim = %+v, %v; want nil during backoff", immediate, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE facade.inbound_events SET available_at=now()-interval '1 second' WHERE id=$1`, claim.Event.ID); err != nil {
		t.Fatal(err)
	}
	retry, err := s.ReclaimStuckInbound(ctx, "doku", 30)
	if err != nil || retry == nil || retry.Attempts != 2 {
		t.Fatalf("retry claim = %+v, %v", retry, err)
	}
	if err := s.RunInTx(ctx, func(tx context.Context) error {
		return s.CompleteInbound(tx, retry.ID, retry.ClaimToken, "", false, "permanent", 2)
	}); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM facade.inbound_events WHERE id=$1`, retry.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != store.InboundDead {
		t.Fatalf("status = %q, want dead", status)
	}
}

func TestInboundClaimRequiresTransaction(t *testing.T) {
	pool, s := connect(t)
	defer pool.Close()
	reset(t, pool)
	if _, err := s.ClaimInbound(context.Background(), inbound("midtrans", "tx", 6)); err == nil {
		t.Fatal("expected ClaimInbound outside RunInTx to fail")
	}
}

func TestInboundForensicIdentityIsImmutable(t *testing.T) {
	pool, s := connect(t)
	defer pool.Close()
	reset(t, pool)
	ctx := context.Background()
	claim, err := claimInboundTx(s, ctx, inbound("midtrans", "immutable", 9))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE facade.inbound_events SET raw='tampered' WHERE id=$1`, claim.Event.ID); err == nil {
		t.Fatal("expected forensic payload mutation to be rejected")
	}
}

func TestInboundConcurrentRecoveryHasOneLeaseOwner(t *testing.T) {
	pool, s := connect(t)
	defer pool.Close()
	reset(t, pool)
	ctx := context.Background()
	claim, err := claimInboundTx(s, ctx, inbound("midtrans", "transaction-race", 7))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE facade.inbound_events SET claimed_until=now()-interval '1 second' WHERE id=$1`, claim.Event.ID); err != nil {
		t.Fatal(err)
	}
	type result struct {
		event *store.InboundEvent
		err   error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for range 2 {
		go func() {
			<-start
			event, err := s.ReclaimStuckInbound(ctx, "midtrans", 30)
			results <- result{event: event, err: err}
		}()
	}
	close(start)
	owners := 0
	for range 2 {
		got := <-results
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.event != nil {
			owners++
		}
	}
	if owners != 1 {
		t.Fatalf("lease owners = %d, want 1", owners)
	}
}

func claimInboundTx(s interface {
	RunInTx(context.Context, func(context.Context) error) error
	ClaimInbound(context.Context, *store.InboundEvent) (store.InboundClaim, error)
}, ctx context.Context, event *store.InboundEvent) (store.InboundClaim, error) {
	var claim store.InboundClaim
	err := s.RunInTx(ctx, func(tx context.Context) error {
		var err error
		claim, err = s.ClaimInbound(tx, event)
		return err
	})
	return claim, err
}

func TestInboundBackoffTimestampIsInFuture(t *testing.T) {
	pool, s := connect(t)
	defer pool.Close()
	reset(t, pool)
	ctx := context.Background()
	claim, _ := claimInboundTx(s, ctx, inbound("midtrans", "future", 8))
	before := time.Now().UTC()
	if err := s.RunInTx(ctx, func(tx context.Context) error {
		return s.CompleteInbound(tx, claim.Event.ID, claim.Event.ClaimToken, "", false, "temporary", 5)
	}); err != nil {
		t.Fatal(err)
	}
	var available time.Time
	if err := pool.QueryRow(ctx, `SELECT available_at FROM facade.inbound_events WHERE id=$1`, claim.Event.ID).Scan(&available); err != nil {
		t.Fatal(err)
	}
	if !available.After(before) {
		t.Fatalf("available_at = %s, want after %s", available, before)
	}
}
