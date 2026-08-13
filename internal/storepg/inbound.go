package storepg

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jawalab-com/payrouter/internal/store"
)

// InboundStore implementation: durable provider-notification log with content-hash
// dedup and a claim/apply lifecycle. See store.InboundStore for the contract.

const inboundCols = "id,account_id,gateway,provider_event_id,event_hash,event_type,reference,raw,parsed,status,attempts,last_error,claim_token,claimed_until,available_at,applied_at,created_at"
const inboundReturningCols = "inbound_events.id,inbound_events.account_id,inbound_events.gateway,inbound_events.provider_event_id,inbound_events.event_hash,inbound_events.event_type,inbound_events.reference,inbound_events.raw,inbound_events.parsed,inbound_events.status,inbound_events.attempts,inbound_events.last_error,inbound_events.claim_token,inbound_events.claimed_until,inbound_events.available_at,inbound_events.applied_at,inbound_events.created_at"

// ClaimInbound inserts the event (by gateway+event_hash) if absent, marking it
// processing. A duplicate delivery returns the existing row with Owned=false.
// Must run within RunInTx; no HTTP/provider call may occur in that transaction.
func (s *Store) ClaimInbound(ctx context.Context, ev *store.InboundEvent) (store.InboundClaim, error) {
	if _, ok := txFrom(ctx); !ok {
		return store.InboundClaim{}, fmt.Errorf("ClaimInbound requires RunInTx")
	}
	q := s.q(ctx)
	token, err := newClaimToken()
	if err != nil {
		return store.InboundClaim{}, err
	}
	row := q.QueryRow(ctx, `
		INSERT INTO inbound_events (account_id,gateway,provider_event_id,event_hash,event_type,reference,raw,parsed,status,attempts,claim_token,claimed_until)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'processing',1,$9,now()+interval '30 seconds')
		ON CONFLICT DO NOTHING
		RETURNING `+inboundCols,
		nullableAccount(ev.AccountID), ev.Gateway, ev.ProviderEventID, ev.EventHash, ev.EventType, ev.Reference, ev.Raw, ev.Parsed, token)
	inserted, err := scanInbound(row)
	if err == nil {
		return store.InboundClaim{Event: inserted, Owned: true}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return store.InboundClaim{}, err
	}
	// Duplicate delivery: return the existing row, not owned.
	existing, err := s.readInboundDuplicate(ctx, ev)
	if err != nil {
		return store.InboundClaim{}, err
	}
	return store.InboundClaim{Event: existing, Owned: false}, nil
}

// CompleteInbound resolves a claimed row: applied on success, or failed/dead on
// failure (dead when attempts have reached maxAttempts).
func (s *Store) CompleteInbound(ctx context.Context, id int64, claimToken, accountID string, applied bool, lastError string, maxAttempts int) error {
	if _, ok := txFrom(ctx); !ok {
		return fmt.Errorf("CompleteInbound requires RunInTx")
	}
	if maxAttempts < 1 {
		return fmt.Errorf("maxAttempts must be positive")
	}
	q := s.q(ctx)
	var attempts int
	if err := q.QueryRow(ctx,
		`SELECT attempts FROM inbound_events WHERE id=$1 AND status='processing' AND claim_token=$2 FOR UPDATE`,
		id, claimToken,
	).Scan(&attempts); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.ErrInboundClaimLost
		}
		return err
	}
	if applied {
		tag, err := q.Exec(ctx, `
			UPDATE inbound_events
			SET account_id=COALESCE(NULLIF($3,''),account_id), status='applied', applied_at=now(),
			    claim_token=NULL, claimed_until=NULL, last_error=''
			WHERE id=$1 AND claim_token=$2`, id, claimToken, accountID)
		if err == nil && tag.RowsAffected() != 1 {
			return store.ErrInboundClaimLost
		}
		return err
	}
	dead := attempts >= maxAttempts
	retryAt := time.Now().UTC()
	if !dead {
		retryAt = retryAt.Add(retryDelay(attempts))
	}
	status := store.InboundFailed
	if dead {
		status = store.InboundDead
	}
	tag, err := q.Exec(ctx, `
		UPDATE inbound_events
		SET account_id=COALESCE(NULLIF($3,''),account_id), last_error=$4, status=$5,
		    claim_token=NULL, claimed_until=NULL, available_at=$6
		WHERE id=$1 AND claim_token=$2`, id, claimToken, accountID, boundedError(lastError), status, retryAt)
	if err == nil && tag.RowsAffected() != 1 {
		return store.ErrInboundClaimLost
	}
	return err
}

// ReclaimStuckInbound leases one due row (pending, failed, or processing past its
// lease) for the reconciliation worker via FOR UPDATE SKIP LOCKED, incrementing
// attempts. Returns nil, nil when nothing is due. Intended to be called outside a
// transaction (it claims atomically in its own statement).
func (s *Store) ReclaimStuckInbound(ctx context.Context, gateway string, leaseSeconds int) (*store.InboundEvent, error) {
	if leaseSeconds < 1 || leaseSeconds > 3600 {
		return nil, fmt.Errorf("leaseSeconds must be between 1 and 3600")
	}
	token, err := newClaimToken()
	if err != nil {
		return nil, err
	}
	row := s.pool.QueryRow(ctx, `
		WITH due AS (
			SELECT id FROM inbound_events
			WHERE gateway=$1
			  AND ((status IN ('pending','failed') AND available_at <= now())
			       OR (status='processing' AND claimed_until < now()))
			ORDER BY created_at, id
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		UPDATE inbound_events
		   SET status='processing', attempts=attempts+1, claim_token=$3,
		       claimed_until=now()+make_interval(secs=>$2)
		  FROM due
		 WHERE inbound_events.id=due.id
		RETURNING `+inboundReturningCols, gateway, float64(leaseSeconds), token)
	ev, err := scanInbound(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return ev, err
}

func (s *Store) readInboundDuplicate(ctx context.Context, ev *store.InboundEvent) (*store.InboundEvent, error) {
	query := "SELECT " + inboundCols + " FROM inbound_events WHERE gateway=$1 AND event_hash=$2 AND event_type=$3 AND reference=$4 AND provider_event_id=''"
	args := []any{ev.Gateway, ev.EventHash, ev.EventType, ev.Reference}
	if ev.ProviderEventID != "" {
		query = "SELECT " + inboundCols + " FROM inbound_events WHERE gateway=$1 AND provider_event_id=$2 AND event_type=$3"
		args = []any{ev.Gateway, ev.ProviderEventID, ev.EventType}
	}
	row := s.q(ctx).QueryRow(ctx, query, args...)
	return scanInbound(row)
}

func scanInbound(row pgx.Row) (*store.InboundEvent, error) {
	var ev store.InboundEvent
	var accountID *string
	var claimToken *string
	var claimedUntil, appliedAt *time.Time
	if err := row.Scan(
		&ev.ID, &accountID, &ev.Gateway, &ev.ProviderEventID, &ev.EventHash, &ev.EventType, &ev.Reference,
		&ev.Raw, &ev.Parsed, &ev.Status, &ev.Attempts, &ev.LastError, &claimToken,
		&claimedUntil, &ev.AvailableAt, &appliedAt, &ev.CreatedAt); err != nil {
		return nil, err
	}
	if accountID != nil {
		ev.AccountID = *accountID
	}
	if claimToken != nil {
		ev.ClaimToken = *claimToken
	}
	if claimedUntil != nil {
		ev.ClaimedUntil = *claimedUntil
	}
	if appliedAt != nil {
		ev.AppliedAt = *appliedAt
	}
	return &ev, nil
}

func newClaimToken() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate inbound claim token: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

func retryDelay(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	if attempts > 8 {
		attempts = 8
	}
	base := time.Duration(1<<uint(attempts-1)) * time.Second
	if base > 5*time.Minute {
		base = 5 * time.Minute
	}
	// Full bounds are 80%-120% of the exponential base.
	jitter, err := rand.Int(rand.Reader, big.NewInt(401))
	if err != nil {
		return base
	}
	return base * time.Duration(800+jitter.Int64()) / 1000
}

func boundedError(message string) string {
	message = strings.TrimSpace(message)
	if len(message) > 2048 {
		return message[:2048]
	}
	return message
}

// nullableAccount returns a value suitable for an insert into the nullable
// account_id column: nil for the empty/unknown account.
func nullableAccount(accountID string) any {
	if accountID == "" {
		return nil
	}
	return accountID
}
