package store

import (
	"context"
	"errors"
	"time"
)

// Durable capability interfaces. The Postgres adapter (internal/storepg)
// implements these; the in-memory *Memory does not. Middleware and handlers
// type-assert against them and fall back to the legacy in-memory behavior when
// they are absent, so *Memory-backed deployments (compat/handler tests) keep
// working unchanged.

// Sentinel errors for auth, idempotency, and read outcomes. The HTTP layer maps
// these to Stripe-shaped error responses.
var (
	ErrUnknownAPIKey              = errors.New("unknown api key")
	ErrAPIKeyRevoked              = errors.New("api key revoked")
	ErrIdempotencyPayloadMismatch = errors.New("idempotency_payload_mismatch")
	ErrIdempotencyInProgress      = errors.New("idempotency_in_progress")
	ErrNotFound                   = errors.New("not found")
	ErrRefundExceedsPayment       = errors.New("refund exceeds remaining payment amount")
	ErrInboundClaimLost           = errors.New("inbound event claim lost")
	ErrOutboundClaimLost          = errors.New("outbound event claim lost")
)

// Idempotency lifecycle status values.
const (
	IdempotencyProcessing = "processing"
	IdempotencyCompleted  = "completed"
	IdempotencyFailed     = "failed"
)

// ClaimResult is the outcome of an idempotency claim.
type ClaimResult struct {
	Status         string // IdempotencyProcessing | IdempotencyCompleted | IdempotencyFailed
	ResponseStatus int    // valid when Status == IdempotencyCompleted
	ResponseBody   []byte // valid when Status == IdempotencyCompleted
	Owned          bool   // true if this caller created the row and must Complete it
}

// AccountKeyStore resolves a Stripe-style API key to an account (auth) and
// bootstraps accounts/keys (seeding). All methods are idempotent.
type AccountKeyStore interface {
	// ResolveAPIKey maps a raw Bearer secret to an active account id, or returns
	// ErrUnknownAPIKey/ErrAPIKeyRevoked. Updates last_used_at as a side effect.
	ResolveAPIKey(ctx context.Context, secret string) (accountID string, err error)
	// EnsureAccount creates the account if absent.
	EnsureAccount(ctx context.Context, accountID string, livemode bool) error
	// EnsureAPIKey registers a key for an account (no-op if the hash already exists).
	EnsureAPIKey(ctx context.Context, accountID, keyPrefix string, keyHash []byte) error
}

// IdempotencyStore claims and completes request idempotency rows. Methods must be
// called within Store.RunInTx so the claim/complete is atomic with the work.
type IdempotencyStore interface {
	// ClaimIdempotency claims (account, operation, key). If a completed row exists
	// with the same request_hash, it is replayed (Owned=false, response populated).
	// If a completed/in-progress row exists with a different hash, it returns
	// ErrIdempotencyPayloadMismatch / ErrIdempotencyInProgress. Otherwise it inserts
	// a 'processing' row owned by this caller (Owned=true).
	ClaimIdempotency(ctx context.Context, accountID, operation, key string, requestHash []byte) (ClaimResult, error)
	// CompleteIdempotency marks the owned row completed and stores the response.
	CompleteIdempotency(ctx context.Context, accountID, operation, key string, responseStatus int, responseBody []byte) error
	FailIdempotency(ctx context.Context, accountID, operation, key string) error
}

// ProviderAttempt is an audited gateway interaction for a PaymentIntent. The
// provider_attempts table is append-only; the intent's last_payment_error is
// derived from the latest attempt whose Error is non-empty.
type ProviderAttempt struct {
	ID               int64
	AccountID        string
	PaymentIntentID  string
	Reference        string
	Gateway          string
	GatewayReference string
	Operation        string // create | confirm | refund | status
	Status           string
	Request          map[string]any
	Response         map[string]any
	Error            string
	CreatedAt        time.Time
}

// PaymentTransition is an append-only PaymentIntent status change.
type PaymentTransition struct {
	ID              int64
	AccountID       string
	PaymentIntentID string
	FromStatus      string
	ToStatus        string
	Reason          string
	Metadata        map[string]string
	CreatedAt       time.Time
}

// AuditStore records provider attempts and status transitions (append-only).
// Methods must be called within Store.RunInTx to batch with the owning write.
type AuditStore interface {
	RecordAttempt(ctx context.Context, att *ProviderAttempt) error
	RecordTransition(ctx context.Context, tr *PaymentTransition) error
}

// PaymentDurable is the account-scoped, transaction-aware read/write surface for
// PaymentIntents and Refunds. Reads enforce account isolation (a row owned by
// another account is not found). Methods must be called within Store.RunInTx when
// atomicity with audit/idempotency writes is required.
type PaymentDurable interface {
	WriteIntent(ctx context.Context, pi *PaymentIntent) error
	ReadIntent(ctx context.Context, accountID, id string) (*PaymentIntent, error)
	ReadIntentByGatewayRef(ctx context.Context, accountID, gatewayRef string) (*PaymentIntent, error)
	SetIntentStatus(ctx context.Context, accountID, id, toStatus, failureMessage string) (*PaymentIntent, error)
	WriteRefund(ctx context.Context, rf *Refund) error
	ReserveRefund(ctx context.Context, rf *Refund) error
	ReadRefund(ctx context.Context, accountID, id string) (*Refund, error)
}

// Inbound lifecycle status values.
const (
	InboundPending    = "pending"
	InboundProcessing = "processing"
	InboundApplied    = "applied"
	InboundFailed     = "failed"
	InboundDead       = "dead"
)

// InboundEvent is a persisted provider notification. Stable provider identity
// is preferred for logical deduplication; EventHash is the fallback. Parsed
// holds the marshaled gateway.WebhookEvent so the
// reconciliation worker can re-apply without re-parsing raw bytes through the
// adapter. Raw is kept for forensic audit only and must never be logged or
// returned over the API.
type InboundEvent struct {
	ID              int64
	AccountID       string // "" when the referenced order is unknown at ingest
	Gateway         string
	ProviderEventID string // stable provider delivery/transaction identity when available
	EventHash       []byte
	EventType       string
	Reference       string
	Raw             []byte
	Parsed          []byte
	Status          string
	Attempts        int
	LastError       string
	ClaimToken      string
	ClaimedUntil    time.Time
	AvailableAt     time.Time
	AppliedAt       time.Time
	CreatedAt       time.Time
}

// InboundClaim is the outcome of an inbound dedup claim.
type InboundClaim struct {
	Event *InboundEvent
	Owned bool // true if this caller created the row and must apply + complete it
}

// InboundStore persists provider notifications with logical-identity/hash dedup
// and a claim/apply lifecycle: duplicate deliveries are no-ops, and crashes leave a
// reclaimable row. ClaimInbound and CompleteInbound must run within Store.RunInTx
// so the dedup claim and the state application are atomic. No HTTP/provider call
// may occur inside that transaction.
type InboundStore interface {
	// ClaimInbound inserts ev by stable provider identity (or payload fallback)
	// if absent, marking it
	// processing and returning Owned=true. If present, returns the existing row
	// with Owned=false (duplicate — caller skips).
	ClaimInbound(ctx context.Context, ev *InboundEvent) (InboundClaim, error)
	// CompleteInbound marks a claimed row applied on success, or failed/dead on
	// failure (dead when attempts reach maxAttempts).
	CompleteInbound(ctx context.Context, id int64, claimToken, accountID string, applied bool, lastError string, maxAttempts int) error
	// ReclaimStuckInbound leases one pending or past-lease processing row for the
	// reconciliation worker (crash recovery). Returns nil, nil when nothing is due.
	ReclaimStuckInbound(ctx context.Context, gateway string, leaseSeconds int) (*InboundEvent, error)
}

// Outbound lifecycle status values.
const (
	OutboundPending   = "pending"
	OutboundInFlight  = "in_flight"
	OutboundDelivered = "delivered"
	OutboundFailed    = "failed"
	OutboundDead      = "dead"
)

// OutboundEvent is a queued Stripe-shaped event awaiting delivery to the
// merchant. Payload (the full Stripe Event JSON) is immutable after insert; the
// deliverer re-signs it per attempt.
type OutboundEvent struct {
	ID            string // evt_...
	AccountID     string
	Type          string
	Reference     string
	Payload       []byte
	Status        string
	Attempts      int
	MaxAttempts   int
	NextAttemptAt time.Time
	LastError     string
	ClaimToken    string
	ClaimedUntil  time.Time
	Created       int64 // unix seconds (Stripe-shaped)
	CreatedAt     time.Time
	DeliveredAt   time.Time
}

// OutboundStore is the transactional outbox: events are enqueued in the same
// transaction as the state change that produced them, then drained by a
// deliverer. Lease/Complete use ownership tokens so a stale worker cannot
// finalize a row a fresh worker now owns.
type OutboundStore interface {
	// EnqueueOutbound inserts an event (idempotent on id). Must run within RunInTx
	// when the enqueue must be atomic with the owning state change.
	EnqueueOutbound(ctx context.Context, ev *OutboundEvent) error
	// LeaseDueOutbound claims one due row via FOR UPDATE SKIP LOCKED (pending, or
	// failed and past next_attempt_at), marking it in_flight with a fresh claim
	// token. Returns nil, nil when nothing is due.
	LeaseDueOutbound(ctx context.Context, leaseSeconds int) (*OutboundEvent, error)
	// CompleteOutbound resolves a leased row: delivered on success, or requeued at
	// nextAttempt / dead-lettered when attempts reach maxAttempts. A mismatched
	// claim token returns ErrOutboundClaimLost.
	CompleteOutbound(ctx context.Context, id, claimToken string, delivered bool, lastError string, nextAttempt time.Time, maxAttempts int) error
	// GetOutbound returns one event by id (tests/admin).
	GetOutbound(ctx context.Context, id string) (*OutboundEvent, error)
	// ReplayOutbound resets a delivered/failed event to pending for re-delivery.
	ReplayOutbound(ctx context.Context, id string) error
}
