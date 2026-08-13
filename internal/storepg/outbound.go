package storepg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jawalab-com/payrouter/internal/store"
)

// OutboundStore implementation: the transactional outbox. EnqueueOutbound is
// tx-aware (participates in the owning state-change transaction); the deliverer
// leases due rows with FOR UPDATE SKIP LOCKED and completes them under ownership
// tokens. See store.OutboundStore for the contract.

const outboundCols = "id,account_id,type,reference,payload,status,attempts,max_attempts,next_attempt_at,last_error,claim_token,claimed_until,created,created_at,delivered_at"

// EnqueueOutbound inserts an event, idempotent on id. Run within RunInTx to make
// the enqueue atomic with the state change that produced it.
func (s *Store) EnqueueOutbound(ctx context.Context, ev *store.OutboundEvent) error {
	maxAttempts := ev.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 5
	}
	next := ev.NextAttemptAt
	if next.IsZero() {
		next = time.Now().UTC()
	}
	_, err := s.q(ctx).Exec(ctx, `
		INSERT INTO outbound_events (id,account_id,type,reference,payload,status,attempts,max_attempts,next_attempt_at,claim_token,claimed_until,created,created_at)
		VALUES ($1,$2,$3,$4,$5,'pending',0,$6,$7,NULL,NULL,$8,now())
		ON CONFLICT (id) DO NOTHING`,
		ev.ID, nullableAccount(ev.AccountID), ev.Type, ev.Reference, ev.Payload,
		maxAttempts, next, ev.Created)
	return err
}

// LeaseDueOutbound claims one due row (pending, or failed past next_attempt_at)
// via FOR UPDATE SKIP LOCKED, marking it in_flight with a fresh claim token.
// Returns nil, nil when nothing is due. Called outside a transaction.
func (s *Store) LeaseDueOutbound(ctx context.Context, leaseSeconds int) (*store.OutboundEvent, error) {
	if leaseSeconds < 1 || leaseSeconds > 3600 {
		return nil, fmt.Errorf("leaseSeconds must be between 1 and 3600")
	}
	token, err := newClaimToken()
	if err != nil {
		return nil, err
	}
	row := s.pool.QueryRow(ctx, `
		WITH due AS (
			SELECT id AS due_id FROM outbound_events
			WHERE status = 'pending'
			   OR (status = 'failed' AND next_attempt_at <= now())
			   OR (status = 'in_flight' AND claimed_until < now())
			ORDER BY next_attempt_at, created
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		UPDATE outbound_events
		   SET status='in_flight', attempts=attempts+1, claim_token=$2,
		       claimed_until=now()+make_interval(secs=>$1)
		  FROM due
		 WHERE outbound_events.id=due.due_id
		RETURNING `+outboundCols, float64(leaseSeconds), token)
	ev, err := scanOutbound(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return ev, err
}

// CompleteOutbound resolves a leased row. A mismatched/absent claim token returns
// ErrOutboundClaimLost so a stale worker cannot finalize a row a fresh worker
// owns. On failure the row is requeued at nextAttempt, or dead-lettered (status
// 'failed' with a NULL next_attempt_at so the lease query never picks it up
// again) once attempts reach maxAttempts.
func (s *Store) CompleteOutbound(ctx context.Context, id, claimToken string, delivered bool, lastError string, nextAttempt time.Time, maxAttempts int) error {
	if maxAttempts < 1 {
		return fmt.Errorf("maxAttempts must be positive")
	}
	q := s.q(ctx)
	var attempts int
	if err := q.QueryRow(ctx,
		`SELECT attempts FROM outbound_events WHERE id=$1 AND claim_token=$2 AND status='in_flight' FOR UPDATE`,
		id, claimToken,
	).Scan(&attempts); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.ErrOutboundClaimLost
		}
		return err
	}
	if delivered {
		tag, err := q.Exec(ctx, `
			UPDATE outbound_events
			   SET status='delivered', delivered_at=now(), last_error='',
			       claim_token=NULL, claimed_until=NULL, next_attempt_at=now()
			 WHERE id=$1 AND claim_token=$2`, id, claimToken)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return store.ErrOutboundClaimLost
		}
		return nil
	}
	// Failure: dead-letter ('dead', not re-leased) when exhausted, else requeue as
	// 'failed' at nextAttempt.
	dead := attempts >= maxAttempts
	statusVal := store.OutboundFailed
	if dead {
		statusVal = store.OutboundDead
	}
	tag, err := q.Exec(ctx, `
		UPDATE outbound_events
		   SET last_error=$3, status=$4, claim_token=NULL, claimed_until=NULL, next_attempt_at=$5
		 WHERE id=$1 AND claim_token=$2`, id, claimToken, boundedError(lastError), statusVal, nextAttempt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return store.ErrOutboundClaimLost
	}
	return nil
}

func scanOutbound(row pgx.Row) (*store.OutboundEvent, error) {
	var ev store.OutboundEvent
	var accountID, claimToken *string
	var claimedUntil, nextAttempt, deliveredAt, createdAt *time.Time
	if err := row.Scan(
		&ev.ID, &accountID, &ev.Type, &ev.Reference, &ev.Payload, &ev.Status,
		&ev.Attempts, &ev.MaxAttempts, &nextAttempt, &ev.LastError, &claimToken,
		&claimedUntil, &ev.Created, &createdAt, &deliveredAt); err != nil {
		return nil, err
	}
	if accountID != nil {
		ev.AccountID = *accountID
	}
	if claimToken != nil {
		ev.ClaimToken = *claimToken
	}
	if nextAttempt != nil {
		ev.NextAttemptAt = *nextAttempt
	}
	if claimedUntil != nil {
		ev.ClaimedUntil = *claimedUntil
	}
	if createdAt != nil {
		ev.CreatedAt = *createdAt
	}
	if deliveredAt != nil {
		ev.DeliveredAt = *deliveredAt
	}
	return &ev, nil
}

// GetOutbound returns one event by id.
func (s *Store) GetOutbound(ctx context.Context, id string) (*store.OutboundEvent, error) {
	row := s.q(ctx).QueryRow(ctx, "SELECT "+outboundCols+" FROM outbound_events WHERE id=$1", id)
	ev, err := scanOutbound(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return ev, err
}

// ReplayOutbound resets a delivered/failed event to pending so the deliverer
// re-sends it (admin/manual replay). Attempts and last_error are preserved for
// audit; next_attempt_at is now so it is immediately due.
func (s *Store) ReplayOutbound(ctx context.Context, id string) error {
	tag, err := s.q(ctx).Exec(ctx, `
		UPDATE outbound_events
		   SET status='pending', next_attempt_at=now(), claim_token=NULL, claimed_until=NULL
		 WHERE id=$1 AND status IN ('delivered','failed')`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return store.ErrNotFound
	}
	return nil
}
