package store

import "context"

// Store is the persistence boundary for facade-owned resources. *Memory is the
// in-process implementation (test/dev only); a PostgreSQL adapter
// (internal/storepg) is the durable implementation. Getters return defensive
// copies; callers must not mutate shared state.
//
// The 20 resource methods are unchanged from the original *Memory surface —
// they are the translation state the HTTP layer needs. Account isolation is an
// application concern: handlers set AccountID on resources before Put and
// compare it on Get (the durable store also scopes by account_id at the DB
// layer). Durable-only concerns (api-key resolution, idempotency, attempts,
// transitions) live on separate capability interfaces implemented by the
// Postgres adapter.
type Store interface {
	// Payment intents
	Put(pi *PaymentIntent)
	Get(id string) (*PaymentIntent, error)
	GetByGatewayReference(gwRef string) (*PaymentIntent, error)

	// Checkout sessions
	PutSession(s *Session)
	GetSession(id string) (*Session, error)
	GetSessionByPaymentIntent(piID string) (*Session, error)

	// Customers
	PutCustomer(c *Customer)
	GetCustomer(id string) (*Customer, error)

	// Refunds
	PutRefund(r *Refund)
	GetRefund(id string) (*Refund, error)

	// Products / Prices
	PutProduct(p *Product)
	GetProduct(id string) (*Product, error)
	PutPrice(p *Price)
	GetPrice(id string) (*Price, error)

	// Subscriptions / Invoices
	PutSubscription(s *Subscription)
	GetSubscription(id string) (*Subscription, error)
	GetSubscriptionByGatewayID(gwID string) (*Subscription, error)
	GetSubscriptionByAuthPI(piID string) (*Subscription, error)
	PutInvoice(i *Invoice)
	GetInvoice(id string) (*Invoice, error)
	GetInvoiceByPI(piID string) (*Invoice, error)

	// Outbound webhook delivery queue
	EnqueueWebhook(d *WebhookDelivery)
	LeaseDueWebhook(now int64) *WebhookDelivery
	CompleteWebhook(id string, delivered bool, lastError string, nextAttempt, maxAttempts int64)
	GetWebhook(id string) (*WebhookDelivery, bool)
	ListWebhooks() []*WebhookDelivery

	// RunInTx runs fn within a transaction. The durable store uses a real DB
	// transaction; the memory store simply runs fn (its individual operations are
	// already mutex-guarded, and test doubles do not require cross-operation
	// atomicity). The context passed to fn is returned to fn unchanged so callers
	// can thread their own scope.
	RunInTx(ctx context.Context, fn func(ctx context.Context) error) error
}
