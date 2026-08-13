// Package store holds facade-owned state (PaymentIntents, Refunds, and the outbound
// webhook delivery queue). Memory is the in-process implementation used for tests
// and dev; it is not durable and is lost on restart. The durable PostgreSQL
// adapter lives in internal/storepg. The Store interface in store.go is the
// persistence boundary both implementations satisfy.
package store

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// Delivery status values for WebhookDelivery.
const (
	DeliveryPending   = "pending"   // queued, waiting for next attempt
	DeliveryDelivered = "delivered" // merchant endpoint returned 2xx
	DeliveryFailed    = "failed"    // exhausted retries (dead-lettered)
)

// PaymentIntent is the facade's canonical record. The HTTP layer maps it to/from
// Stripe-shaped JSON. Status holds a stripe.PaymentIntentStatus string value.
type PaymentIntent struct {
	ID                string
	AccountID         string // owning facade account (account isolation); "" under the memory test store
	AmountMinor       int64
	Currency          string
	Status            string
	ClientSecret      string
	PaymentMethodType string
	GatewayReference  string
	NextActionType    string
	NextActionURL     string
	NextActionReturn  string
	Description       string
	FailureMessage    string // set when a payment attempt failed (surfaces as last_payment_error)
	Metadata          map[string]string
	Created           int64
	Livemode          bool
}

// Memory is a goroutine-safe in-memory store of facade-owned resources. It holds
// only what translation requires — intents and the linked Checkout/Stripe surface
// objects — so an existing Stripe integration can create/retrieve them. It is not
// durable and is lost on restart (no database, by design).
type Memory struct {
	mu            sync.RWMutex
	intents       map[string]*PaymentIntent
	gwRefIndex    map[string]string // GatewayReference -> intent ID (for gateways that emit their own id)
	sessions      map[string]*Session
	sessionByPI   map[string]string // PaymentIntentID -> session ID (to emit checkout.session.completed)
	customers     map[string]*Customer
	refunds       map[string]*Refund
	products      map[string]*Product
	prices        map[string]*Price
	subscriptions map[string]*Subscription
	subByGwID     map[string]string // gateway subscription id -> sub_ id (recurring-cycle correlation)
	subByAuthPI   map[string]string // authorization PaymentIntentID -> sub_ id (activation correlation)
	invoices      map[string]*Invoice
	invoiceByPI   map[string]string // cycle PaymentIntentID -> in_ id (cycle idempotency)
	webhooks      map[string]*WebhookDelivery
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{
		intents:       make(map[string]*PaymentIntent),
		gwRefIndex:    make(map[string]string),
		sessions:      make(map[string]*Session),
		sessionByPI:   make(map[string]string),
		customers:     make(map[string]*Customer),
		refunds:       make(map[string]*Refund),
		products:      make(map[string]*Product),
		prices:        make(map[string]*Price),
		subscriptions: make(map[string]*Subscription),
		subByGwID:     make(map[string]string),
		subByAuthPI:   make(map[string]string),
		invoices:      make(map[string]*Invoice),
		invoiceByPI:   make(map[string]string),
	}
}

// RunInTx satisfies Store.RunInTx. The memory store has no real transactions;
// it simply runs fn (each operation is individually mutex-guarded). Holding the
// mutex here would deadlock against fn's own Lock calls, so it is intentionally
// unheld — test doubles do not require cross-operation atomicity.
func (m *Memory) RunInTx(ctx context.Context, fn func(ctx context.Context) error) error {
	return fn(ctx)
}

// Put stores or replaces a PaymentIntent by id and indexes its GatewayReference
// so callbacks carrying the gateway's own reference (e.g. Mayar) can resolve back
// to the intent.
func (m *Memory) Put(pi *PaymentIntent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.intents[pi.ID] = pi
	if pi.GatewayReference != "" {
		m.gwRefIndex[pi.GatewayReference] = pi.ID
	}
}

// Get returns a PaymentIntent by id, or an error if not found.
func (m *Memory) Get(id string) (*PaymentIntent, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	pi, ok := m.intents[id]
	if !ok {
		return nil, fmt.Errorf("no such payment_intent: %s", id)
	}
	// return a defensive copy
	cp := *pi
	return &cp, nil
}

// GetByGatewayReference resolves a PaymentIntent by the gateway's own transaction
// id (stored as GatewayReference at creation). Used for gateways like Mayar whose
// callbacks carry the gateway id rather than our pi_ reference.
func (m *Memory) GetByGatewayReference(gwRef string) (*PaymentIntent, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	id, ok := m.gwRefIndex[gwRef]
	if !ok {
		return nil, fmt.Errorf("no such payment_intent for gateway reference: %s", gwRef)
	}
	pi, ok := m.intents[id]
	if !ok {
		return nil, fmt.Errorf("no such payment_intent: %s", id)
	}
	cp := *pi
	return &cp, nil
}

// --- Checkout Sessions ------------------------------------------------------

// Session is the facade's record for a Stripe Checkout Session. The HTTP layer
// creates a PaymentIntent + gateway hosted page at session-create time, then a
// gateway payment notification flips Status to "complete"/"paid" and emits
// checkout.session.completed.
type Session struct {
	ID                string
	AccountID         string // owning facade account (account isolation); "" under the memory test store
	Mode              string // "payment"
	Status            string // open | complete | expired
	PaymentStatus     string // paid | unpaid | no_payment_required
	AmountSubtotal    int64
	AmountTotal       int64
	Currency          string
	CustomerID        string // cus_... if created from a customer
	CustomerEmail     string
	SuccessURL        string
	CancelURL         string
	URL               string // gateway hosted page (present while open)
	PaymentIntentID   string // pi_... created for this session (payment mode)
	SubscriptionID    string // sub_... created for this session (subscription mode)
	ClientReferenceID string
	Description       string
	Metadata          map[string]string
	Created           int64
	ExpiresAt         int64
	Livemode          bool
}

// PutSession stores/replaces a Session and indexes it by its PaymentIntentID so a
// gateway payment notification on that intent can resolve to the session and emit
// checkout.session.completed.
func (m *Memory) PutSession(s *Session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions == nil {
		m.sessions = map[string]*Session{}
	}
	m.sessions[s.ID] = s
	if s.PaymentIntentID != "" {
		m.sessionByPI[s.PaymentIntentID] = s.ID
	}
}

// GetSession returns a Session by id, or an error if not found.
func (m *Memory) GetSession(id string) (*Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[id]
	if !ok {
		return nil, fmt.Errorf("no such checkout session: %s", id)
	}
	cp := *s
	return &cp, nil
}

// GetSessionByPaymentIntent resolves a Session from its PaymentIntent id, used by
// the webhook path to emit checkout.session.completed when the intent is paid.
func (m *Memory) GetSessionByPaymentIntent(piID string) (*Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	id, ok := m.sessionByPI[piID]
	if !ok {
		return nil, fmt.Errorf("no checkout session for payment intent: %s", piID)
	}
	s, ok := m.sessions[id]
	if !ok {
		return nil, fmt.Errorf("no such checkout session: %s", id)
	}
	cp := *s
	return &cp, nil
}

// --- Customers --------------------------------------------------------------

// Customer is a minimal Stripe Customer record (create/retrieve passthrough).
type Customer struct {
	ID        string
	AccountID string // owning facade account (account isolation); "" under the memory test store
	Email     string
	Name      string
	Phone     string
	Metadata  map[string]string
	Created   int64
	Livemode  bool
}

// PutCustomer stores/replaces a Customer by id.
func (m *Memory) PutCustomer(c *Customer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.customers == nil {
		m.customers = map[string]*Customer{}
	}
	m.customers[c.ID] = c
}

// GetCustomer returns a Customer by id, or an error if not found.
func (m *Memory) GetCustomer(id string) (*Customer, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.customers[id]
	if !ok {
		return nil, fmt.Errorf("no such customer: %s", id)
	}
	cp := *c
	return &cp, nil
}

// --- Refunds ----------------------------------------------------------------

// Refund is the facade's record for a Stripe Refund. Status is succeeded/failed.
type Refund struct {
	ID              string // re_...
	AccountID       string // owning facade account (account isolation); "" under the memory test store
	PaymentIntentID string // pi_... being refunded
	Amount          int64  // refunded amount; == PaymentIntent.AmountMinor for a full refund
	Currency        string
	Status          string // succeeded | failed | canceled | pending
	Reason          string
	Metadata        map[string]string
	Created         int64
	Livemode        bool
}

// PutRefund stores/replaces a Refund by id.
func (m *Memory) PutRefund(r *Refund) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.refunds == nil {
		m.refunds = map[string]*Refund{}
	}
	m.refunds[r.ID] = r
}

// GetRefund returns a Refund by id, or an error if not found.
func (m *Memory) GetRefund(id string) (*Refund, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.refunds[id]
	if !ok {
		return nil, fmt.Errorf("no such refund: %s", id)
	}
	cp := *r
	return &cp, nil
}

// --- Products & Prices ------------------------------------------------------

// Product is a minimal Stripe Product record.
type Product struct {
	ID          string // prod_...
	AccountID   string // owning facade account (account isolation); "" under the memory test store
	Name        string
	Description string
	Active      bool
	Metadata    map[string]string
	Created     int64
	Updated     int64
	Livemode    bool
}

// PutProduct stores/replaces a Product by id.
func (m *Memory) PutProduct(p *Product) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.products == nil {
		m.products = map[string]*Product{}
	}
	m.products[p.ID] = p
}

// GetProduct returns a Product by id, or an error if not found.
func (m *Memory) GetProduct(id string) (*Product, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.products[id]
	if !ok {
		return nil, fmt.Errorf("no such product: %s", id)
	}
	cp := *p
	return &cp, nil
}

// Price is a Stripe Price record. UnitAmount is in minor units (IDR is
// zero-decimal, so it is whole rupiah). Type is "one_time" or "recurring"; the
// recurring fields carry the schedule when Type == "recurring".
type Price struct {
	ID            string // price_...
	AccountID     string // owning facade account (account isolation); "" under the memory test store
	ProductID     string // prod_...
	UnitAmount    int64
	Currency      string
	Type          string // one_time | recurring
	Interval      string // recurring: day | week | month | year
	IntervalCount int64  // recurring; default 1
	UsageType     string // recurring: licensed (the only Stripe value we emit)
	Active        bool
	Metadata      map[string]string
	Created       int64
	Livemode      bool
}

// PutPrice stores/replaces a Price by id.
func (m *Memory) PutPrice(p *Price) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.prices == nil {
		m.prices = map[string]*Price{}
	}
	m.prices[p.ID] = p
}

// GetPrice returns a Price by id, or an error if not found.
func (m *Memory) GetPrice(id string) (*Price, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.prices[id]
	if !ok {
		return nil, fmt.Errorf("no such price: %s", id)
	}
	cp := *p
	return &cp, nil
}

// --- Subscriptions & Invoices (v2 recurring) --------------------------------

// Subscription is the facade's record for a Stripe Subscription. It is created
// incomplete (authorization pending), flipped to active when the gateway
// registers the recurring schedule, and canceled on delete. THE GATEWAY owns the
// recurring clock; this record only holds translation state. Status holds a
// stripe.SubscriptionStatus value.
type Subscription struct {
	ID                  string // sub_...
	AccountID           string // owning facade account (account isolation); "" under the memory test store
	CustomerID          string // cus_...
	PriceID             string // price_... (recurring)
	Status              string // incomplete | active | canceled | past_due
	GatewayID           string // gateway subscription id (set on activation)
	AuthPaymentIntentID string // pi_... of the authorization save-card charge
	Interval            string
	IntervalCount       int64
	AmountMinor         int64
	Currency            string
	CurrentPeriodEnd    int64
	LatestInvoiceID     string // in_...
	CanceledAt          int64
	Metadata            map[string]string
	Created             int64
	Livemode            bool
}

// PutSubscription stores/replaces a Subscription and indexes it by its gateway
// subscription id (recurring-cycle correlation) and its authorization
// PaymentIntent (activation correlation).
func (m *Memory) PutSubscription(s *Subscription) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.subscriptions == nil {
		m.subscriptions = map[string]*Subscription{}
	}
	if m.subByGwID == nil {
		m.subByGwID = map[string]string{}
	}
	if m.subByAuthPI == nil {
		m.subByAuthPI = map[string]string{}
	}
	m.subscriptions[s.ID] = s
	if s.GatewayID != "" {
		m.subByGwID[s.GatewayID] = s.ID
	}
	if s.AuthPaymentIntentID != "" {
		m.subByAuthPI[s.AuthPaymentIntentID] = s.ID
	}
}

// GetSubscription returns a Subscription by id, or an error if not found.
func (m *Memory) GetSubscription(id string) (*Subscription, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.subscriptions[id]
	if !ok {
		return nil, fmt.Errorf("no such subscription: %s", id)
	}
	cp := *s
	return &cp, nil
}

// GetSubscriptionByGatewayID resolves a Subscription from the gateway's
// subscription id, used to correlate a recurring-cycle notification.
func (m *Memory) GetSubscriptionByGatewayID(gwID string) (*Subscription, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	id, ok := m.subByGwID[gwID]
	if !ok {
		return nil, fmt.Errorf("no subscription for gateway id: %s", gwID)
	}
	s, ok := m.subscriptions[id]
	if !ok {
		return nil, fmt.Errorf("no such subscription: %s", id)
	}
	cp := *s
	return &cp, nil
}

// GetSubscriptionByAuthPI resolves a Subscription from its authorization
// PaymentIntent id, used when the save-card webhook arrives to activate it.
func (m *Memory) GetSubscriptionByAuthPI(piID string) (*Subscription, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	id, ok := m.subByAuthPI[piID]
	if !ok {
		return nil, fmt.Errorf("no subscription for auth intent: %s", piID)
	}
	s, ok := m.subscriptions[id]
	if !ok {
		return nil, fmt.Errorf("no such subscription: %s", id)
	}
	cp := *s
	return &cp, nil
}

// Invoice is the facade's record for a Stripe Invoice. One is created per
// subscription (subscription_create) and one per recurring cycle
// (subscription_cycle). Status holds a stripe.InvoiceStatus value.
type Invoice struct {
	ID              string // in_...
	AccountID       string // owning facade account (account isolation); "" under the memory test store
	SubscriptionID  string // sub_...
	PaymentIntentID string // pi_... of the charge backing this invoice
	CustomerID      string
	AmountMinor     int64
	Currency        string
	Status          string // draft | open | paid | void
	BillingReason   string // subscription_create | subscription_cycle
	Created         int64
	Livemode        bool
	Metadata        map[string]string
}

// PutInvoice stores/replaces an Invoice and indexes it by its PaymentIntent id
// (used for cycle idempotency: a re-sent cycle notification finds the invoice
// already recorded and does not double-emit).
func (m *Memory) PutInvoice(i *Invoice) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.invoices == nil {
		m.invoices = map[string]*Invoice{}
	}
	if m.invoiceByPI == nil {
		m.invoiceByPI = map[string]string{}
	}
	m.invoices[i.ID] = i
	if i.PaymentIntentID != "" {
		m.invoiceByPI[i.PaymentIntentID] = i.ID
	}
}

// GetInvoice returns an Invoice by id, or an error if not found.
func (m *Memory) GetInvoice(id string) (*Invoice, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	i, ok := m.invoices[id]
	if !ok {
		return nil, fmt.Errorf("no such invoice: %s", id)
	}
	cp := *i
	return &cp, nil
}

// GetInvoiceByPI resolves an Invoice from its PaymentIntent id (cycle idempotency).
func (m *Memory) GetInvoiceByPI(piID string) (*Invoice, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	id, ok := m.invoiceByPI[piID]
	if !ok {
		return nil, fmt.Errorf("no invoice for payment intent: %s", piID)
	}
	i, ok := m.invoices[id]
	if !ok {
		return nil, fmt.Errorf("no such invoice: %s", id)
	}
	cp := *i
	return &cp, nil
}

// --- Outbound webhook delivery queue ----------------------------------------

// WebhookDelivery is a queued outbound Stripe-shaped event awaiting delivery to
// the merchant's endpoint. Payload is the full Stripe Event JSON (already
// built at enqueue time); the deliverer re-signs it per attempt.
type WebhookDelivery struct {
	ID            string // evt_...
	Type          string // Stripe event type
	Reference     string // pi_...
	Payload       []byte // full Stripe Event JSON
	Status        string // DeliveryPending / DeliveryDelivered / DeliveryFailed
	Attempts      int
	NextAttemptAt int64  // unix seconds; earliest the deliverer may retry
	LastError     string // last delivery error (empty on success)
	Created       int64
}

// EnqueueWebhook adds an outbound event to the delivery queue.
func (m *Memory) EnqueueWebhook(d *WebhookDelivery) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.webhooks == nil {
		m.webhooks = map[string]*WebhookDelivery{}
	}
	if d.Status == "" {
		d.Status = DeliveryPending
	}
	m.webhooks[d.ID] = d
}

// LeaseDueWebhook returns a copy of one due, still-pending delivery (NextAttemptAt
// <= now), marking it in-flight so a single deliverer doesn't double-send. Returns
// nil if nothing is due. Pass the current unix time as now (tests inject it).
func (m *Memory) LeaseDueWebhook(now int64) *WebhookDelivery {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Deterministic order (oldest first) so retries are fair and tests stable.
	var ids []string
	for id, d := range m.webhooks {
		if d.Status == DeliveryPending && d.NextAttemptAt <= now {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		d := m.webhooks[id]
		if d.Status == DeliveryPending && d.NextAttemptAt <= now {
			d.Status = "in_flight"
			d.Attempts++
			cp := *d
			return &cp
		}
	}
	return nil
}

// CompleteWebhook resolves an in-flight delivery. delivered=true marks it done;
// otherwise it is requeued for retry at nextAttempt, or dead-lettered (DeliveryFailed)
// when maxAttempts is exceeded. lastError describes the failure (empty on success).
func (m *Memory) CompleteWebhook(id string, delivered bool, lastError string, nextAttempt, maxAttempts int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.webhooks[id]
	if !ok {
		return
	}
	d.LastError = lastError
	switch {
	case delivered:
		d.Status = DeliveryDelivered
		d.NextAttemptAt = 0
	case int64(d.Attempts) >= maxAttempts:
		d.Status = DeliveryFailed
	default:
		d.Status = DeliveryPending
		d.NextAttemptAt = nextAttempt
	}
}

// GetWebhook returns a copy of a delivery by id (used in tests).
func (m *Memory) GetWebhook(id string) (*WebhookDelivery, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.webhooks[id]
	if !ok {
		return nil, false
	}
	cp := *d
	return &cp, true
}

// ListWebhooks returns copies of all deliveries (created-ascending), used in tests.
func (m *Memory) ListWebhooks() []*WebhookDelivery {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*WebhookDelivery, 0, len(m.webhooks))
	for _, d := range m.webhooks {
		cp := *d
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created < out[j].Created })
	return out
}

// ListIntents returns all PaymentIntents sorted by creation time descending.
func (m *Memory) ListIntents(accountID string, limit int) []*PaymentIntent {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var list []*PaymentIntent
	for _, pi := range m.intents {
		if accountID == "" || pi.AccountID == accountID {
			cp := *pi
			list = append(list, &cp)
		}
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].Created > list[j].Created
	})
	if limit > 0 && len(list) > limit {
		list = list[:limit]
	}
	return list
}

// ListCustomers returns all Customers sorted by creation time descending.
func (m *Memory) ListCustomers(accountID string, limit int) []*Customer {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var list []*Customer
	for _, c := range m.customers {
		if accountID == "" || c.AccountID == accountID {
			cp := *c
			list = append(list, &cp)
		}
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].Created > list[j].Created
	})
	if limit > 0 && len(list) > limit {
		list = list[:limit]
	}
	return list
}

// ListSessions returns all Sessions sorted by creation time descending.
func (m *Memory) ListSessions(accountID string, limit int) []*Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var list []*Session
	for _, s := range m.sessions {
		if accountID == "" || s.AccountID == accountID {
			cp := *s
			list = append(list, &cp)
		}
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].Created > list[j].Created
	})
	if limit > 0 && len(list) > limit {
		list = list[:limit]
	}
	return list
}
