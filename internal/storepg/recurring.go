package storepg

import (
	"context"
	"encoding/json"

	"github.com/jawalab-com/payrouter/internal/store"
)

// Native account-scoped persistence for checkout sessions, subscriptions, and
// invoices, migrated off the in-memory store in Phase 5D part 2. Put* use a
// background context (the Store interface carries none) and upsert by id; HTTP
// isolation is enforced by the handlers comparing the caller's account to the
// row's owner. Secondary lookups (by payment intent / gateway id / auth intent)
// are plain indexed queries.

// --- Sessions ---------------------------------------------------------------

const sessionCols = "id,account_id,mode,status,payment_status,amount_subtotal,amount_total,currency,customer_id,customer_email,success_url,cancel_url,url,payment_intent_id,subscription_id,client_reference_id,description,metadata,created,expires_at,livemode"

func (s *Store) PutSession(ss *store.Session) {
	meta, _ := json.Marshal(ss.Metadata)
	_, _ = s.pool.Exec(context.Background(), `
		INSERT INTO checkout_sessions (id,account_id,mode,status,payment_status,amount_subtotal,amount_total,currency,customer_id,customer_email,success_url,cancel_url,url,payment_intent_id,subscription_id,client_reference_id,description,metadata,created,expires_at,livemode)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)
		ON CONFLICT (id) DO UPDATE SET
			mode=EXCLUDED.mode, status=EXCLUDED.status, payment_status=EXCLUDED.payment_status,
			amount_subtotal=EXCLUDED.amount_subtotal, amount_total=EXCLUDED.amount_total, currency=EXCLUDED.currency,
			customer_id=EXCLUDED.customer_id, customer_email=EXCLUDED.customer_email, url=EXCLUDED.url,
			payment_intent_id=EXCLUDED.payment_intent_id, subscription_id=EXCLUDED.subscription_id,
			client_reference_id=EXCLUDED.client_reference_id, description=EXCLUDED.description,
			metadata=EXCLUDED.metadata, expires_at=EXCLUDED.expires_at, livemode=EXCLUDED.livemode`,
		ss.ID, ss.AccountID, ss.Mode, ss.Status, ss.PaymentStatus, ss.AmountSubtotal, ss.AmountTotal, ss.Currency,
		ss.CustomerID, ss.CustomerEmail, ss.SuccessURL, ss.CancelURL, ss.URL, ss.PaymentIntentID, ss.SubscriptionID,
		ss.ClientReferenceID, ss.Description, meta, ss.Created, ss.ExpiresAt, ss.Livemode)
}

func (s *Store) GetSession(id string) (*store.Session, error) {
	row := s.pool.QueryRow(context.Background(), "SELECT "+sessionCols+" FROM checkout_sessions WHERE id=$1", id)
	ss, err := scanSession(row)
	return ss, wrapNotFound(err)
}

func (s *Store) GetSessionByPaymentIntent(pi string) (*store.Session, error) {
	row := s.pool.QueryRow(context.Background(), "SELECT "+sessionCols+" FROM checkout_sessions WHERE payment_intent_id=$1", pi)
	ss, err := scanSession(row)
	return ss, wrapNotFound(err)
}

func scanSession(row interface {
	Scan(dest ...any) error
}) (*store.Session, error) {
	var ss store.Session
	var meta []byte
	if err := row.Scan(&ss.ID, &ss.AccountID, &ss.Mode, &ss.Status, &ss.PaymentStatus, &ss.AmountSubtotal,
		&ss.AmountTotal, &ss.Currency, &ss.CustomerID, &ss.CustomerEmail, &ss.SuccessURL, &ss.CancelURL,
		&ss.URL, &ss.PaymentIntentID, &ss.SubscriptionID, &ss.ClientReferenceID, &ss.Description,
		&meta, &ss.Created, &ss.ExpiresAt, &ss.Livemode); err != nil {
		return nil, err
	}
	if len(meta) > 0 {
		_ = json.Unmarshal(meta, &ss.Metadata)
	}
	return &ss, nil
}

// --- Subscriptions ----------------------------------------------------------

const subCols = "id,account_id,customer_id,price_id,status,gateway_id,auth_payment_intent_id,interval,interval_count,amount_minor,currency,current_period_end,latest_invoice_id,canceled_at,metadata,created,livemode"

func (s *Store) PutSubscription(ss *store.Subscription) {
	meta, _ := json.Marshal(ss.Metadata)
	_, _ = s.pool.Exec(context.Background(), `
		INSERT INTO subscriptions (id,account_id,customer_id,price_id,status,gateway_id,auth_payment_intent_id,interval,interval_count,amount_minor,currency,current_period_end,latest_invoice_id,canceled_at,metadata,created,livemode)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
		ON CONFLICT (id) DO UPDATE SET
			customer_id=EXCLUDED.customer_id, price_id=EXCLUDED.price_id, status=EXCLUDED.status,
			gateway_id=EXCLUDED.gateway_id, auth_payment_intent_id=EXCLUDED.auth_payment_intent_id,
			interval=EXCLUDED.interval, interval_count=EXCLUDED.interval_count, amount_minor=EXCLUDED.amount_minor,
			currency=EXCLUDED.currency, current_period_end=EXCLUDED.current_period_end,
			latest_invoice_id=EXCLUDED.latest_invoice_id, canceled_at=EXCLUDED.canceled_at,
			metadata=EXCLUDED.metadata, livemode=EXCLUDED.livemode`,
		ss.ID, ss.AccountID, ss.CustomerID, ss.PriceID, ss.Status, ss.GatewayID, ss.AuthPaymentIntentID,
		ss.Interval, ss.IntervalCount, ss.AmountMinor, ss.Currency, ss.CurrentPeriodEnd, ss.LatestInvoiceID,
		ss.CanceledAt, meta, ss.Created, ss.Livemode)
}

func (s *Store) GetSubscription(id string) (*store.Subscription, error) {
	row := s.pool.QueryRow(context.Background(), "SELECT "+subCols+" FROM subscriptions WHERE id=$1", id)
	ss, err := scanSubscription(row)
	return ss, wrapNotFound(err)
}

func (s *Store) GetSubscriptionByGatewayID(gw string) (*store.Subscription, error) {
	row := s.pool.QueryRow(context.Background(), "SELECT "+subCols+" FROM subscriptions WHERE gateway_id=$1", gw)
	ss, err := scanSubscription(row)
	return ss, wrapNotFound(err)
}

func (s *Store) GetSubscriptionByAuthPI(pi string) (*store.Subscription, error) {
	row := s.pool.QueryRow(context.Background(), "SELECT "+subCols+" FROM subscriptions WHERE auth_payment_intent_id=$1", pi)
	ss, err := scanSubscription(row)
	return ss, wrapNotFound(err)
}

func scanSubscription(row interface {
	Scan(dest ...any) error
}) (*store.Subscription, error) {
	var ss store.Subscription
	var meta []byte
	if err := row.Scan(&ss.ID, &ss.AccountID, &ss.CustomerID, &ss.PriceID, &ss.Status, &ss.GatewayID,
		&ss.AuthPaymentIntentID, &ss.Interval, &ss.IntervalCount, &ss.AmountMinor, &ss.Currency,
		&ss.CurrentPeriodEnd, &ss.LatestInvoiceID, &ss.CanceledAt, &meta, &ss.Created, &ss.Livemode); err != nil {
		return nil, err
	}
	if len(meta) > 0 {
		_ = json.Unmarshal(meta, &ss.Metadata)
	}
	return &ss, nil
}

// --- Invoices ---------------------------------------------------------------

const invoiceCols = "id,account_id,subscription_id,payment_intent_id,customer_id,amount_minor,currency,status,billing_reason,metadata,created,livemode"

func (s *Store) PutInvoice(i *store.Invoice) {
	meta, _ := json.Marshal(i.Metadata)
	_, _ = s.pool.Exec(context.Background(), `
		INSERT INTO invoices (id,account_id,subscription_id,payment_intent_id,customer_id,amount_minor,currency,status,billing_reason,metadata,created,livemode)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		ON CONFLICT (id) DO UPDATE SET
			subscription_id=EXCLUDED.subscription_id, payment_intent_id=EXCLUDED.payment_intent_id,
			customer_id=EXCLUDED.customer_id, amount_minor=EXCLUDED.amount_minor, currency=EXCLUDED.currency,
			status=EXCLUDED.status, billing_reason=EXCLUDED.billing_reason, metadata=EXCLUDED.metadata, livemode=EXCLUDED.livemode`,
		i.ID, i.AccountID, i.SubscriptionID, i.PaymentIntentID, i.CustomerID, i.AmountMinor, i.Currency,
		i.Status, i.BillingReason, meta, i.Created, i.Livemode)
}

func (s *Store) GetInvoice(id string) (*store.Invoice, error) {
	row := s.pool.QueryRow(context.Background(), "SELECT "+invoiceCols+" FROM invoices WHERE id=$1", id)
	i, err := scanInvoice(row)
	return i, wrapNotFound(err)
}

func (s *Store) GetInvoiceByPI(pi string) (*store.Invoice, error) {
	row := s.pool.QueryRow(context.Background(), "SELECT "+invoiceCols+" FROM invoices WHERE payment_intent_id=$1", pi)
	i, err := scanInvoice(row)
	return i, wrapNotFound(err)
}

func scanInvoice(row interface {
	Scan(dest ...any) error
}) (*store.Invoice, error) {
	var in store.Invoice
	var meta []byte
	if err := row.Scan(&in.ID, &in.AccountID, &in.SubscriptionID, &in.PaymentIntentID, &in.CustomerID,
		&in.AmountMinor, &in.Currency, &in.Status, &in.BillingReason, &meta, &in.Created, &in.Livemode); err != nil {
		return nil, err
	}
	if len(meta) > 0 {
		_ = json.Unmarshal(meta, &in.Metadata)
	}
	return &in, nil
}
