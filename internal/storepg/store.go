package storepg

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jawalab-com/payrouter/internal/store"
)

// Store is the durable PostgreSQL implementation of store.Store.
//
// PaymentIntent and Refund are persisted natively (account-scoped); the rest
// (sessions, customers, products, prices, subscriptions, invoices, and the
// outbound webhook delivery queue) delegate to an in-process *store.Memory and
// migrate in a later slice. It also implements AccountKeyStore, IdempotencyStore,
// AuditStore, and PaymentDurable so the auth/idempotency middleware and the
// PaymentIntent/Refund handlers can drive durable, account-safe semantics.
type Store struct {
	pool    *pgxpool.Pool
	gateway string        // active gateway name, stamped on provider attempts
	mem     *store.Memory // delegate for not-yet-migrated resources
}

// New wraps an already-migrated pool. gateway is the active adapter name, used to
// stamp provider_attempts.
func New(pool *pgxpool.Pool, gateway string) *Store {
	return &Store{pool: pool, gateway: gateway, mem: store.NewMemory()}
}

// HashAPIKey returns the SHA-256 hash of a raw Bearer secret plus a non-secret,
// deterministic prefix (first 6 bytes of the hash, hex-encoded) used as the
// searchable api_keys.key_prefix.
func HashAPIKey(secret string) (hash []byte, prefix string) {
	sum := sha256.Sum256([]byte(secret))
	return sum[:], hex.EncodeToString(sum[:6])
}

// RunInTx runs fn against a real database transaction, threading it through the
// context so capability methods (WriteIntent, RecordAttempt, ClaimIdempotency,
// …) participate in the same transaction.
func (s *Store) RunInTx(ctx context.Context, fn func(context.Context) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	tctx := context.WithValue(ctx, txKey{}, tx)
	if err := fn(tctx); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	return tx.Commit(tctx)
}

// querier is the common Exec/Query surface satisfied by both pgx.Tx and
// *pgxpool.Pool.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// q returns the transaction bound to ctx (set by RunInTx), or the pool otherwise.
func (s *Store) q(ctx context.Context) querier {
	if tx, ok := txFrom(ctx); ok {
		return tx
	}
	return s.pool
}

// ----------------------------------------------------------------------------
// store.Store: PaymentIntent (native durable)
// ----------------------------------------------------------------------------

const intentCols = "id,account_id,amount_minor,currency,status,client_secret,payment_method_type,gateway_reference,next_action_type,next_action_url,next_action_return,display_json,description,failure_message,metadata,livemode,created"

// Put satisfies store.Store; it upserts the intent with a background context.
func (s *Store) Put(pi *store.PaymentIntent) { _ = s.WriteIntent(context.Background(), pi) }

// Get satisfies store.Store; it is NOT account-scoped (legacy compatibility).
// Account-isolated reads go through PaymentDurable.ReadIntent.
func (s *Store) Get(id string) (*store.PaymentIntent, error) {
	row := s.pool.QueryRow(context.Background(), "SELECT "+intentCols+" FROM payment_intents WHERE id=$1", id)
	pi, err := scanIntent(row)
	return pi, wrapNotFound(err)
}

// GetByGatewayReference satisfies store.Store (legacy, not account-scoped).
func (s *Store) GetByGatewayReference(gwRef string) (*store.PaymentIntent, error) {
	row := s.pool.QueryRow(context.Background(), "SELECT "+intentCols+" FROM payment_intents WHERE gateway_reference=$1", gwRef)
	pi, err := scanIntent(row)
	return pi, wrapNotFound(err)
}

// ----------------------------------------------------------------------------
// store.Store: Refund (native durable)
// ----------------------------------------------------------------------------

const refundCols = "id,account_id,payment_intent_id,amount_minor,currency,status,reason,metadata,livemode,created"

func (s *Store) PutRefund(r *store.Refund) { _ = s.WriteRefund(context.Background(), r) }

func (s *Store) GetRefund(id string) (*store.Refund, error) {
	row := s.pool.QueryRow(context.Background(), "SELECT "+refundCols+" FROM refunds WHERE id=$1", id)
	rf, err := scanRefund(row)
	return rf, wrapNotFound(err)
}

// ----------------------------------------------------------------------------
// store.Store: delegated resources (in-process memory; migrate later)
// ----------------------------------------------------------------------------

// Customer/Product/Price are persisted natively in catalog.go (account-scoped).
// Session/Subscription/Invoice are persisted natively in recurring.go.

// --- outbound webhook delivery queue (in-memory for 5A) ---

func (s *Store) EnqueueWebhook(d *store.WebhookDelivery) { s.mem.EnqueueWebhook(d) }
func (s *Store) LeaseDueWebhook(now int64) *store.WebhookDelivery {
	return s.mem.LeaseDueWebhook(now)
}
func (s *Store) CompleteWebhook(id string, delivered bool, lastError string, nextAttempt, maxAttempts int64) {
	s.mem.CompleteWebhook(id, delivered, lastError, nextAttempt, maxAttempts)
}
func (s *Store) GetWebhook(id string) (*store.WebhookDelivery, bool) { return s.mem.GetWebhook(id) }
func (s *Store) ListWebhooks() []*store.WebhookDelivery              { return s.mem.ListWebhooks() }

// ----------------------------------------------------------------------------
// AccountKeyStore
// ----------------------------------------------------------------------------

// ResolveAPIKey hashes the secret and maps it to an active account id, updating
// last_used_at. Unknown/revoked keys return ErrUnknownAPIKey.
func (s *Store) ResolveAPIKey(ctx context.Context, secret string) (string, error) {
	sum := sha256.Sum256([]byte(secret))
	var accountID string
	err := s.q(ctx).QueryRow(ctx, `SELECT COALESCE(resolve_api_key($1), '')`, sum[:]).Scan(&accountID)
	if err != nil {
		return "", err
	}
	if accountID == "" {
		return "", store.ErrUnknownAPIKey
	}
	return accountID, nil
}

func (s *Store) EnsureAccount(ctx context.Context, accountID string, livemode bool) error {
	_, err := s.q(ctx).Exec(ctx,
		`INSERT INTO accounts(id,livemode) VALUES($1,$2) ON CONFLICT DO NOTHING`, accountID, livemode)
	return err
}

func (s *Store) EnsureAPIKey(ctx context.Context, accountID, keyPrefix string, keyHash []byte) error {
	q := s.q(ctx)
	if _, err := q.Exec(ctx,
		`INSERT INTO api_keys(id,account_id,key_prefix,key_hash) VALUES('key_'||$4,$1,$2,$3) ON CONFLICT (key_hash) DO NOTHING`,
		accountID, keyPrefix, keyHash, hex.EncodeToString(keyHash)); err != nil {
		return err
	}
	var owner string
	if err := q.QueryRow(ctx, `SELECT account_id FROM api_keys WHERE key_hash=$1`, keyHash).Scan(&owner); err != nil {
		return err
	}
	if owner != accountID {
		return fmt.Errorf("api key is already assigned to another account")
	}
	return nil
}

// ----------------------------------------------------------------------------
// IdempotencyStore
// ----------------------------------------------------------------------------

// ClaimIdempotency claims (account, operation, key). The caller owns the row only
// if it created it (RETURNING id); a pre-existing completed row with the same
// request hash is replayed, and any pre-existing row with a different hash is a
// payload mismatch.
func (s *Store) ClaimIdempotency(ctx context.Context, accountID, op, key string, requestHash []byte) (store.ClaimResult, error) {
	q := s.q(ctx)
	var id int64
	err := q.QueryRow(ctx,
		`INSERT INTO idempotency_records(account_id,operation,idempotency_key,request_hash,status,processing_expires_at)
		 VALUES($1,$2,$3,$4,'processing',now()+interval '30 seconds') ON CONFLICT DO NOTHING RETURNING id`,
		accountID, op, key, requestHash).Scan(&id)
	if err == nil {
		return store.ClaimResult{Status: store.IdempotencyProcessing, Owned: true}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return store.ClaimResult{}, err
	}
	// Pre-existing row: read its state.
	var hash []byte
	var respStatus *int
	var respBody []byte
	var statusStr string
	var processingExpires *time.Time
	if err := q.QueryRow(ctx,
		`SELECT status,request_hash,response_status,response_body,processing_expires_at FROM idempotency_records WHERE account_id=$1 AND operation=$2 AND idempotency_key=$3 FOR UPDATE`,
		accountID, op, key).Scan(&statusStr, &hash, &respStatus, &respBody, &processingExpires); err != nil {
		return store.ClaimResult{}, err
	}
	if !bytes.Equal(hash, requestHash) {
		return store.ClaimResult{}, store.ErrIdempotencyPayloadMismatch
	}
	if statusStr == store.IdempotencyCompleted {
		rs := 0
		if respStatus != nil {
			rs = *respStatus
		}
		return store.ClaimResult{Status: store.IdempotencyCompleted, ResponseStatus: rs, ResponseBody: respBody, Owned: false}, nil
	}
	if statusStr == store.IdempotencyFailed || processingExpires == nil || processingExpires.Before(time.Now()) {
		if _, err := q.Exec(ctx, `UPDATE idempotency_records SET status='processing',processing_expires_at=now()+interval '30 seconds',updated_at=now() WHERE account_id=$1 AND operation=$2 AND idempotency_key=$3`, accountID, op, key); err != nil {
			return store.ClaimResult{}, err
		}
		return store.ClaimResult{Status: store.IdempotencyProcessing, Owned: true}, nil
	}
	// Same body with an unexpired claim is owned by another caller.
	return store.ClaimResult{}, store.ErrIdempotencyInProgress
}

func (s *Store) CompleteIdempotency(ctx context.Context, accountID, op, key string, responseStatus int, responseBody []byte) error {
	tag, err := s.q(ctx).Exec(ctx,
		`UPDATE idempotency_records SET status='completed',response_status=$4,response_body=$5,completed_at=now(),processing_expires_at=NULL,updated_at=now()
		 WHERE account_id=$1 AND operation=$2 AND idempotency_key=$3 AND status='processing'`,
		accountID, op, key, responseStatus, responseBody)
	if err == nil && tag.RowsAffected() != 1 {
		return fmt.Errorf("idempotency claim is no longer owned")
	}
	return err
}

func (s *Store) FailIdempotency(ctx context.Context, accountID, op, key string) error {
	_, err := s.q(ctx).Exec(ctx, `UPDATE idempotency_records SET status='failed',processing_expires_at=NULL,updated_at=now() WHERE account_id=$1 AND operation=$2 AND idempotency_key=$3 AND status='processing'`, accountID, op, key)
	return err
}

// ----------------------------------------------------------------------------
// AuditStore (append-only)
// ----------------------------------------------------------------------------

func (s *Store) RecordAttempt(ctx context.Context, att *store.ProviderAttempt) error {
	req, _ := json.Marshal(att.Request)
	resp, _ := json.Marshal(att.Response)
	_, err := s.q(ctx).Exec(ctx,
		`INSERT INTO provider_attempts(account_id,payment_intent_id,reference,gateway,gateway_reference,operation,status,request,response,error)
		 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		att.AccountID, att.PaymentIntentID, att.Reference, att.Gateway, att.GatewayReference,
		att.Operation, att.Status, req, resp, att.Error)
	return err
}

func (s *Store) RecordTransition(ctx context.Context, tr *store.PaymentTransition) error {
	meta, _ := json.Marshal(tr.Metadata)
	_, err := s.q(ctx).Exec(ctx,
		`INSERT INTO payment_transitions(account_id,payment_intent_id,from_status,to_status,reason,metadata)
		 VALUES($1,$2,$3,$4,$5,$6)`,
		tr.AccountID, tr.PaymentIntentID, tr.FromStatus, tr.ToStatus, tr.Reason, meta)
	return err
}

// ----------------------------------------------------------------------------
// PaymentDurable (account-scoped, transaction-aware)
// ----------------------------------------------------------------------------

func (s *Store) WriteIntent(ctx context.Context, pi *store.PaymentIntent) error {
	meta, _ := json.Marshal(pi.Metadata)
	_, err := s.q(ctx).Exec(ctx, `
		INSERT INTO payment_intents (id,account_id,amount_minor,currency,status,client_secret,payment_method_type,gateway,gateway_reference,next_action_type,next_action_url,next_action_return,display_json,description,failure_message,metadata,livemode,created,updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,now())
		ON CONFLICT (id) DO UPDATE SET
			amount_minor=EXCLUDED.amount_minor, currency=EXCLUDED.currency, status=EXCLUDED.status,
			client_secret=EXCLUDED.client_secret, payment_method_type=EXCLUDED.payment_method_type,
			gateway=EXCLUDED.gateway, gateway_reference=EXCLUDED.gateway_reference,
			next_action_type=EXCLUDED.next_action_type, next_action_url=EXCLUDED.next_action_url,
			next_action_return=EXCLUDED.next_action_return, display_json=EXCLUDED.display_json,
			description=EXCLUDED.description,
			failure_message=EXCLUDED.failure_message, metadata=EXCLUDED.metadata, livemode=EXCLUDED.livemode,
			updated_at=now()`,
		pi.ID, pi.AccountID, pi.AmountMinor, pi.Currency, pi.Status, pi.ClientSecret, pi.PaymentMethodType,
		s.gateway, pi.GatewayReference, pi.NextActionType, pi.NextActionURL, pi.NextActionReturn,
		pi.DisplayJSON, pi.Description, pi.FailureMessage, meta, pi.Livemode, pi.Created)
	return err
}

func (s *Store) ReadIntent(ctx context.Context, accountID, id string) (*store.PaymentIntent, error) {
	row := s.q(ctx).QueryRow(ctx, "SELECT "+intentCols+" FROM payment_intents WHERE account_id=$1 AND id=$2", accountID, id)
	pi, err := scanIntent(row)
	return pi, wrapNotFound(err)
}

func (s *Store) ReadIntentByGatewayRef(ctx context.Context, accountID, gatewayRef string) (*store.PaymentIntent, error) {
	row := s.q(ctx).QueryRow(ctx, "SELECT "+intentCols+" FROM payment_intents WHERE account_id=$1 AND gateway_reference=$2", accountID, gatewayRef)
	pi, err := scanIntent(row)
	return pi, wrapNotFound(err)
}

// SetIntentStatus updates an intent's status and failure_message (cleared on
// success) and returns the updated row. The caller records the transition.
func (s *Store) SetIntentStatus(ctx context.Context, accountID, id, toStatus, failureMessage string) (*store.PaymentIntent, error) {
	if _, err := s.q(ctx).Exec(ctx,
		`UPDATE payment_intents SET status=$3, failure_message=$4, updated_at=now() WHERE account_id=$1 AND id=$2`,
		accountID, id, toStatus, failureMessage); err != nil {
		return nil, err
	}
	return s.ReadIntent(ctx, accountID, id)
}

func (s *Store) WriteRefund(ctx context.Context, rf *store.Refund) error {
	meta, _ := json.Marshal(rf.Metadata)
	_, err := s.q(ctx).Exec(ctx,
		`INSERT INTO refunds(id,account_id,payment_intent_id,amount_minor,currency,status,reason,metadata,livemode,created)
		 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		 ON CONFLICT (id) DO UPDATE SET status=EXCLUDED.status, reason=EXCLUDED.reason, metadata=EXCLUDED.metadata`,
		rf.ID, rf.AccountID, rf.PaymentIntentID, rf.Amount, rf.Currency, rf.Status, rf.Reason, meta, rf.Livemode, rf.Created)
	return err
}

// ReserveRefund serializes refunds for an intent, computes a full refund from
// the remaining balance, and persists a pending reservation before the provider
// call. The surrounding transaction releases the intent lock only after commit.
func (s *Store) ReserveRefund(ctx context.Context, rf *store.Refund) error {
	q := s.q(ctx)
	var paid int64
	if err := q.QueryRow(ctx, `SELECT amount_minor FROM payment_intents WHERE account_id=$1 AND id=$2 FOR UPDATE`, rf.AccountID, rf.PaymentIntentID).Scan(&paid); err != nil {
		return wrapNotFound(err)
	}
	// A reclaimed idempotency lease must resume the same reservation rather
	// than count it a second time. Successful reservations are returned to the
	// handler as-is; pending/failed ones may safely retry the deterministic
	// provider reference.
	var existingPI, existingCurrency, existingStatus string
	var existingAmount int64
	err := q.QueryRow(ctx,
		`SELECT payment_intent_id, amount_minor, currency, status
		 FROM refunds WHERE account_id=$1 AND id=$2 FOR UPDATE`,
		rf.AccountID, rf.ID,
	).Scan(&existingPI, &existingAmount, &existingCurrency, &existingStatus)
	if err == nil {
		if existingPI != rf.PaymentIntentID || existingCurrency != rf.Currency || (rf.Amount != 0 && rf.Amount != existingAmount) {
			return store.ErrIdempotencyPayloadMismatch
		}
		rf.Amount = existingAmount
		rf.Status = existingStatus
		if existingStatus == "failed" {
			rf.Status = "pending"
			return s.WriteRefund(ctx, rf)
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	var reserved int64
	if err := q.QueryRow(ctx, `SELECT COALESCE(SUM(amount_minor),0)::bigint FROM refunds WHERE account_id=$1 AND payment_intent_id=$2 AND status IN ('pending','succeeded')`, rf.AccountID, rf.PaymentIntentID).Scan(&reserved); err != nil {
		return err
	}
	remaining := paid - reserved
	if rf.Amount == 0 {
		rf.Amount = remaining
	}
	if rf.Amount <= 0 || rf.Amount > remaining {
		return store.ErrRefundExceedsPayment
	}
	rf.Status = "pending"
	return s.WriteRefund(ctx, rf)
}

func (s *Store) ReadRefund(ctx context.Context, accountID, id string) (*store.Refund, error) {
	row := s.q(ctx).QueryRow(ctx, "SELECT "+refundCols+" FROM refunds WHERE account_id=$1 AND id=$2", accountID, id)
	rf, err := scanRefund(row)
	return rf, wrapNotFound(err)
}

// ----------------------------------------------------------------------------
// row scanning
// ----------------------------------------------------------------------------

func scanIntent(row pgx.Row) (*store.PaymentIntent, error) {
	var pi store.PaymentIntent
	var meta []byte
	if err := row.Scan(
		&pi.ID, &pi.AccountID, &pi.AmountMinor, &pi.Currency, &pi.Status, &pi.ClientSecret, &pi.PaymentMethodType,
		&pi.GatewayReference, &pi.NextActionType, &pi.NextActionURL, &pi.NextActionReturn,
		&pi.DisplayJSON, &pi.Description, &pi.FailureMessage, &meta, &pi.Livemode, &pi.Created); err != nil {
		return nil, err
	}
	if len(meta) > 0 {
		_ = json.Unmarshal(meta, &pi.Metadata)
	}
	return &pi, nil
}

func scanRefund(row pgx.Row) (*store.Refund, error) {
	var rf store.Refund
	var meta []byte
	if err := row.Scan(
		&rf.ID, &rf.AccountID, &rf.PaymentIntentID, &rf.Amount, &rf.Currency, &rf.Status, &rf.Reason, &meta, &rf.Livemode, &rf.Created); err != nil {
		return nil, err
	}
	if len(meta) > 0 {
		_ = json.Unmarshal(meta, &rf.Metadata)
	}
	return &rf, nil
}

func wrapNotFound(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return store.ErrNotFound
	}
	return err
}

// ListIntents returns PaymentIntents for an account ordered by created DESC.
// limit<=0 defaults to 100, capped at 100.
func (s *Store) ListIntents(accountID string, limit int) []*store.PaymentIntent {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	rows, err := s.pool.Query(context.Background(),
		"SELECT "+intentCols+" FROM payment_intents WHERE account_id=$1 ORDER BY created DESC LIMIT $2",
		accountID, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []*store.PaymentIntent
	for rows.Next() {
		pi, err := scanIntent(rows)
		if err == nil {
			out = append(out, pi)
		}
	}
	return out
}

// ListCustomers returns Customers for an account ordered by created DESC.
func (s *Store) ListCustomers(accountID string, limit int) []*store.Customer {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	rows, err := s.pool.Query(context.Background(),
		`SELECT id,account_id,email,name,phone,metadata,created,livemode
		 FROM customers WHERE account_id=$1 ORDER BY created DESC LIMIT $2`,
		accountID, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []*store.Customer
	for rows.Next() {
		c, err := scanCustomer(rows)
		if err == nil {
			out = append(out, c)
		}
	}
	return out
}

// ListSessions returns Checkout Sessions for an account ordered by created DESC.
// Sessions are persisted in-memory (not yet migrated to PostgreSQL), so this
// delegates to the embedded memory store.
func (s *Store) ListSessions(accountID string, limit int) []*store.Session {
	return s.mem.ListSessions(accountID, limit)
}
