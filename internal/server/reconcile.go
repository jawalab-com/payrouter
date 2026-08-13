package server

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/jawalab-com/payrouter/internal/gateway"
	"github.com/jawalab-com/payrouter/internal/store"
)

// Reconciler reclaims inbound provider notifications that were left 'processing'
// by a crash (or a past-lease claim) and re-applies them idempotently, then
// resolves their lifecycle. One Reconciler per facade process is sufficient at
// the target scale; the SKIP LOCKED claim makes multiple replicas safe.
type Reconciler struct {
	server       *Server
	pollInterval time.Duration
	leaseSeconds int
	maxAttempts  int
	now          func() int64 // injectable clock (tests)
}

// NewReconciler builds a Reconciler with production defaults.
func NewReconciler(s *Server) *Reconciler {
	return &Reconciler{
		server:       s,
		pollInterval: 5 * time.Second,
		leaseSeconds: 30,
		maxAttempts:  inboundMaxAttempts,
		now:          func() int64 { return time.Now().Unix() },
	}
}

// Start runs the reconciliation loop until ctx is canceled.
func (r *Reconciler) Start(ctx context.Context) {
	if r.server.inbound == nil {
		return // nothing to reconcile on the in-memory store
	}
	ticker := time.NewTicker(r.pollInterval)
	defer ticker.Stop()
	log.Printf("inbound reconciler started (gateway=%s, poll=%s)", r.server.gw.Name(), r.pollInterval)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.runOnce(ctx)
		}
	}
}

// ReclaimOnce performs a single drain pass. Tests call this to avoid real timers.
func (r *Reconciler) ReclaimOnce(ctx context.Context) int {
	return r.runOnce(ctx)
}

func (r *Reconciler) runOnce(ctx context.Context) int {
	if r.server.inbound == nil {
		return 0
	}
	var n int
	for {
		ev, err := r.server.inbound.ReclaimStuckInbound(ctx, r.server.gw.Name(), r.leaseSeconds)
		if err != nil {
			log.Printf("inbound reclaim failed (gateway=%s): %v", r.server.gw.Name(), err)
			return n
		}
		if ev == nil {
			return n
		}
		n++
		r.reapply(ctx, ev)
	}
}

// reapply re-runs the stored events for a reclaimed notification and resolves the
// row. The re-apply is idempotent (applyInboundEvent no-ops once the status
// matches); the complete is guarded by the claim token so a stale worker cannot
// finalize a row a fresh worker now owns.
func (r *Reconciler) reapply(ctx context.Context, ev *store.InboundEvent) {
	var events []gateway.WebhookEvent
	if err := json.Unmarshal(ev.Parsed, &events); err != nil || len(events) == 0 {
		// Unparseable payload: record a permanent failure so it stops retrying.
		r.complete(ctx, ev, false, "unparseable stored payload")
		return
	}
	now := r.now()
	var applyErr error
	for _, e := range events {
		// Re-apply outside a transaction: idempotent and safe to repeat on a
		// later crash; the row is resolved in its own transaction below.
		if err := r.server.applyInboundEvent(ctx, e, now); err != nil {
			applyErr = err
		}
	}
	r.complete(ctx, ev, applyErr == nil, errMsg(applyErr))
}

func (r *Reconciler) complete(ctx context.Context, ev *store.InboundEvent, applied bool, lastError string) {
	if txErr := r.server.store.RunInTx(ctx, func(tctx context.Context) error {
		return r.server.inbound.CompleteInbound(tctx, ev.ID, ev.ClaimToken, ev.AccountID, applied, lastError, r.maxAttempts)
	}); txErr != nil {
		// ErrInboundClaimLost is benign: a fresh worker reclaimed the row after
		// the lease expired and owns it now. Anything else is logged.
		if txErr != store.ErrInboundClaimLost {
			log.Printf("inbound complete failed (id=%d): %v", ev.ID, txErr)
		}
	}
}

func errMsg(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
