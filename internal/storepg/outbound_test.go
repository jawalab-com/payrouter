package storepg_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jawalab-com/payrouter/internal/store"
	"github.com/jawalab-com/payrouter/internal/storepg"
)

func setupOutbound(t *testing.T) (*pgxpool.Pool, *storepg.Store, context.Context) {
	t.Helper()
	if testDB == "" {
		t.Skip("PAYMENT_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := storepg.ConnectPool(ctx, testDB)
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
	s := storepg.New(pool, "stub")
	if err := s.EnsureAccount(ctx, "acc_o", false); err != nil {
		t.Fatal(err)
	}
	return pool, s, ctx
}

func outboundEv(id string) *store.OutboundEvent {
	return &store.OutboundEvent{
		ID: id, AccountID: "acc_o", Type: "payment_intent.succeeded",
		Reference: "pi_x", Payload: []byte(`{"id":"` + id + `"}`), MaxAttempts: 3, Created: 1,
	}
}

func TestOutboundEnqueueLeaseDeliver(t *testing.T) {
	_, s, ctx := setupOutbound(t)
	if err := s.EnqueueOutbound(ctx, outboundEv("evt_1")); err != nil {
		t.Fatal(err)
	}
	ev, err := s.LeaseDueOutbound(ctx, 30)
	if err != nil || ev == nil {
		t.Fatalf("lease: %v %v", ev, err)
	}
	if ev.ID != "evt_1" || ev.Status != store.OutboundInFlight || ev.Attempts != 1 {
		t.Fatalf("leased ev: %+v", ev)
	}
	if err := s.CompleteOutbound(ctx, ev.ID, ev.ClaimToken, true, "", time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetOutbound(ctx, "evt_1")
	if got.Status != store.OutboundDelivered {
		t.Fatalf("status %q want delivered", got.Status)
	}
	if ev, err := s.LeaseDueOutbound(ctx, 30); err != nil || ev != nil {
		t.Fatalf("post-deliver lease: %v %v", ev, err)
	}
}

func TestOutboundEnqueueIdempotent(t *testing.T) {
	pool, s, ctx := setupOutbound(t)
	_ = s.EnqueueOutbound(ctx, outboundEv("evt_idem"))
	_ = s.EnqueueOutbound(ctx, outboundEv("evt_idem"))
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM facade.outbound_events WHERE id=$1`, "evt_idem").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("idempotent enqueue rows=%d want 1", n)
	}
}

func TestOutboundRetryAndDeadLetter(t *testing.T) {
	_, s, ctx := setupOutbound(t)
	ev := outboundEv("evt_retry")
	ev.MaxAttempts = 2
	_ = s.EnqueueOutbound(ctx, ev)

	e1, _ := s.LeaseDueOutbound(ctx, 30)
	if e1 == nil {
		t.Fatal("1st lease nil")
	}
	past := time.Now().Add(-time.Minute)
	if err := s.CompleteOutbound(ctx, e1.ID, e1.ClaimToken, false, "boom", past, 2); err != nil {
		t.Fatal(err)
	}
	g, _ := s.GetOutbound(ctx, "evt_retry")
	if g.Status != store.OutboundFailed || g.Attempts != 1 {
		t.Fatalf("after 1st fail: %+v", g)
	}

	e2, _ := s.LeaseDueOutbound(ctx, 30)
	if e2 == nil {
		t.Fatal("2nd lease nil")
	}
	if e2.Attempts != 2 {
		t.Fatalf("2nd lease attempts=%d want 2", e2.Attempts)
	}
	if err := s.CompleteOutbound(ctx, e2.ID, e2.ClaimToken, false, "boom2", past, 2); err != nil {
		t.Fatal(err)
	}
	g2, _ := s.GetOutbound(ctx, "evt_retry")
	if g2.Status != store.OutboundDead || g2.Attempts != 2 {
		t.Fatalf("after 2nd fail: %+v", g2)
	}
	// 'dead' is terminal: the lease query never picks it up again.
	if ev, _ := s.LeaseDueOutbound(ctx, 30); ev != nil {
		t.Fatalf("dead-letter was re-leased: %+v", ev)
	}
}

func TestOutboundReplay(t *testing.T) {
	_, s, ctx := setupOutbound(t)
	_ = s.EnqueueOutbound(ctx, outboundEv("evt_rep"))
	e, _ := s.LeaseDueOutbound(ctx, 30)
	_ = s.CompleteOutbound(ctx, e.ID, e.ClaimToken, true, "", time.Now(), 3)
	if err := s.ReplayOutbound(ctx, "evt_rep"); err != nil {
		t.Fatal(err)
	}
	g, _ := s.GetOutbound(ctx, "evt_rep")
	if g.Status != store.OutboundPending {
		t.Fatalf("replay status %q want pending", g.Status)
	}
	if ev, _ := s.LeaseDueOutbound(ctx, 30); ev == nil {
		t.Fatal("replayed event was not re-leased")
	}
}

func TestOutboundStaleCompleteRejected(t *testing.T) {
	pool, s, ctx := setupOutbound(t)
	_ = s.EnqueueOutbound(ctx, outboundEv("evt_stale"))
	e1, _ := s.LeaseDueOutbound(ctx, 30)
	if _, err := pool.Exec(ctx, `UPDATE facade.outbound_events SET claimed_until=now()-interval '1 minute' WHERE id=$1`, "evt_stale"); err != nil {
		t.Fatal(err)
	}
	e2, _ := s.LeaseDueOutbound(ctx, 30)
	if e2 == nil || e2.ClaimToken == e1.ClaimToken {
		t.Fatalf("expected a fresh reclaim with a new token")
	}
	if err := s.CompleteOutbound(ctx, e1.ID, e1.ClaimToken, true, "", time.Now(), 3); err != store.ErrOutboundClaimLost {
		t.Fatalf("stale complete: got %v want ErrOutboundClaimLost", err)
	}
}

func TestOutboundPayloadImmutable(t *testing.T) {
	pool, s, ctx := setupOutbound(t)
	_ = s.EnqueueOutbound(ctx, outboundEv("evt_imm"))
	if _, err := pool.Exec(ctx, `UPDATE facade.outbound_events SET payload='tampered' WHERE id=$1`, "evt_imm"); err == nil {
		t.Fatal("expected immutability trigger to reject payload UPDATE")
	}
}
