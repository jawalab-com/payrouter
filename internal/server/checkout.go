package server

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jawalab-com/payrouter/internal/account"
	"github.com/jawalab-com/payrouter/internal/gateway"
	"github.com/jawalab-com/payrouter/internal/store"
	stripe "github.com/stripe/stripe-go/v81"
)

// stripeCheckoutSession is the Stripe-shaped Checkout Session object returned by
// the facade. Customer and PaymentIntent are emitted as id strings (Stripe's
// default, non-expanded form), which stripe-go deserializes into its *Customer /
// *PaymentIntent expandable fields.
type stripeCheckoutSession struct {
	ID                string                        `json:"id"`
	Object            string                        `json:"object"`
	Mode              string                        `json:"mode"`
	Status            string                        `json:"status"`
	PaymentStatus     string                        `json:"payment_status"`
	AmountSubtotal    int64                         `json:"amount_subtotal"`
	AmountTotal       int64                         `json:"amount_total"`
	Currency          string                        `json:"currency"`
	Customer          string                        `json:"customer,omitempty"`
	CustomerEmail     string                        `json:"customer_email,omitempty"`
	CustomerDetails   *stripeSessionCustomerDetails `json:"customer_details,omitempty"`
	SuccessURL        string                        `json:"success_url,omitempty"`
	CancelURL         string                        `json:"cancel_url,omitempty"`
	URL               string                        `json:"url,omitempty"`
	PaymentIntent     string                        `json:"payment_intent,omitempty"`
	Subscription      string                        `json:"subscription,omitempty"`
	ClientReferenceID string                        `json:"client_reference_id,omitempty"`
	Metadata          map[string]string             `json:"metadata,omitempty"`
	Created           int64                         `json:"created"`
	ExpiresAt         int64                         `json:"expires_at,omitempty"`
	Livemode          bool                          `json:"livemode"`
}

// stripeSessionCustomerDetails mirrors the Session.customer_details object.
// Stripe's own webhook quickstart reads customer_details.email for the receipt
// address, so the facade mirrors customer_email into it.
type stripeSessionCustomerDetails struct {
	Email string `json:"email,omitempty"`
}

// createCheckoutSession handles POST /v1/checkout/sessions. It resolves line items
// (inline price_data or referenced price ids), creates the gateway hosted page via
// CreatePayment, stores the PaymentIntent + Session, and returns the redirect URL.
func (s *Server) createCheckoutSession(w http.ResponseWriter, r *http.Request) {
	if !parseForm(w, r) {
		return
	}

	mode := r.PostFormValue("mode")
	if mode == "" {
		mode = "payment"
	}
	if mode == "subscription" {
		s.createCheckoutSubscription(w, r)
		return
	}
	if mode != "payment" {
		writeStripeError(w, http.StatusBadRequest, "parameter_invalid",
			"mode '"+mode+"' is not supported; use 'payment' or 'subscription'.")
		return
	}

	successURL := r.PostFormValue("success_url")
	if successURL == "" {
		writeStripeError(w, http.StatusBadRequest, "parameter_missing", "Missing required param: success_url.")
		return
	}

	items, err := s.resolveLineItems(r)
	if err != nil {
		writeStripeError(w, http.StatusBadRequest, "parameter_invalid", err.Error())
		return
	}
	if len(items) == 0 {
		writeStripeError(w, http.StatusBadRequest, "parameter_missing", "Missing required param: line_items.")
		return
	}

	var amountTotal int64
	var names []string
	currency := strings.ToLower(r.PostFormValue("currency"))
	for _, li := range items {
		amountTotal += li.unitAmount * li.quantity
		if li.name != "" {
			names = append(names, li.name)
		}
		if currency == "" {
			currency = li.currency
		}
	}
	if currency == "" {
		currency = "idr"
	}
	description := strings.Join(names, ", ")

	// Resolve the customer: an existing cus_ id (looked up for email/name) or an
	// inline customer_email. Passed to the gateway so it can address the payer.
	var cust *gateway.Customer
	customerID := r.PostFormValue("customer")
	customerEmail := r.PostFormValue("customer_email")
	if customerID != "" {
		if c, err := s.store.GetCustomer(customerID); err == nil {
			cust = &gateway.Customer{ID: c.ID, Email: c.Email, Name: c.Name, Phone: c.Phone}
			if customerEmail == "" {
				customerEmail = c.Email
			}
		}
	}
	if cust == nil && customerEmail != "" {
		cust = &gateway.Customer{Email: customerEmail}
	}

	now := time.Now().Unix()
	piID := "pi_" + randID()
	clientSecret := piID + "_secret_" + randID()

	// With the hosted UI enabled, do NOT call a gateway yet. The customer picks a
	// method on our own page, and only then can the orchestrator resolve a real
	// fee channel and compare gateways on it. Choosing a gateway here would mean
	// choosing before the method is known, which is exactly why hosted checkout
	// cannot be least-cost routed.
	if s.cfg.CheckoutUI {
		s.createDeferredCheckoutSession(w, r, deferredSession{
			piID: piID, clientSecret: clientSecret, amountTotal: amountTotal,
			currency: currency, description: description,
			customerID: customerID, customerEmail: customerEmail, now: now,
		})
		return
	}

	// Create the gateway hosted page. IDHosted asks the gateway to show all
	// merchant-activated methods (Midtrans Snap with no enabled_payments filter;
	// Xendit Invoice / DOKU Checkout / Mayar link are hosted by nature).
	res, err := s.gw.CreatePayment(r.Context(), &gateway.CreatePaymentInput{
		Reference:         piID,
		AmountMinor:       amountTotal,
		Currency:          currency,
		Description:       description,
		Customer:          cust,
		PaymentMethodType: gateway.IDHosted,
		ReturnURL:         successURL,
	})
	if err != nil {
		writeStripeError(w, http.StatusBadGateway, "api_error", err.Error())
		return
	}

	meta := s.enrichMetadata(r, res)
	pi := &store.PaymentIntent{
		ID:               piID,
		AccountID:        account.From(r.Context()),
		AmountMinor:      amountTotal,
		Currency:         currency,
		Status:           string(res.Status),
		ClientSecret:     clientSecret,
		GatewayReference: res.GatewayReference,
		Description:      description,
		Metadata:         meta,
		Created:          now,
		Livemode:         s.cfg.Livemode,
	}
	applyNextAction(pi, res.NextAction)
	s.store.Put(pi)

	url := ""
	if res.NextAction != nil && res.NextAction.RedirectToURL != nil {
		url = res.NextAction.RedirectToURL.URL
	}
	sess := &store.Session{
		ID:                "cs_" + randID(),
		AccountID:         account.From(r.Context()),
		Mode:              "payment",
		Status:            "open",
		PaymentStatus:     "unpaid",
		AmountSubtotal:    amountTotal,
		AmountTotal:       amountTotal,
		Currency:          currency,
		CustomerID:        customerID,
		CustomerEmail:     customerEmail,
		SuccessURL:        successURL,
		CancelURL:         r.PostFormValue("cancel_url"),
		URL:               url,
		PaymentIntentID:   piID,
		ClientReferenceID: r.PostFormValue("client_reference_id"),
		Description:       description,
		Metadata:          meta,
		Created:           now,
		ExpiresAt:         now + 24*60*60,
		Livemode:          s.cfg.Livemode,
	}
	s.store.PutSession(sess)

	writeJSON(w, http.StatusOK, toStripeCheckoutSession(sess))
}

// createCheckoutSubscription handles POST /v1/checkout/sessions with
// mode=subscription. It creates a subscription via the shared buildSubscription
// path and returns a Checkout Session whose URL is the authorization redirect
// and whose Subscription is the new sub_ id (Checkout's subscription-mode shape).
func (s *Server) createCheckoutSubscription(w http.ResponseWriter, r *http.Request) {
	if !parseForm(w, r) {
		return
	}
	successURL := r.PostFormValue("success_url")
	if successURL == "" {
		writeStripeError(w, http.StatusBadRequest, "parameter_missing", "Missing required param: success_url.")
		return
	}
	// Subscriptions need a customer; resolve from 'customer', or create one from
	// 'customer_email' (mirroring Checkout's payment-mode customer handling).
	customerID := r.PostFormValue("customer")
	customerEmail := r.PostFormValue("customer_email")
	if customerID == "" && customerEmail != "" {
		c := &store.Customer{ID: "cus_" + randID(), AccountID: account.From(r.Context()), Email: customerEmail, Created: time.Now().Unix(), Livemode: s.cfg.Livemode}
		s.store.PutCustomer(c)
		customerID = c.ID
	}
	if customerID == "" {
		writeStripeError(w, http.StatusBadRequest, "parameter_missing", "Missing required param: customer (or customer_email).")
		return
	}

	sub, err := s.buildSubscription(r, customerID, successURL)
	if err != nil {
		writeSubError(w, err)
		return
	}

	url := ""
	if pi, err := s.store.Get(sub.AuthPaymentIntentID); err == nil {
		url = pi.NextActionURL
	}
	now := time.Now().Unix()
	sess := &store.Session{
		ID:             "cs_" + randID(),
		AccountID:      account.From(r.Context()),
		Mode:           "subscription",
		Status:         "open",
		PaymentStatus:  "unpaid",
		AmountSubtotal: sub.AmountMinor,
		AmountTotal:    sub.AmountMinor,
		Currency:       sub.Currency,
		CustomerID:     customerID,
		CustomerEmail:  customerEmail,
		SuccessURL:     successURL,
		CancelURL:      r.PostFormValue("cancel_url"),
		URL:            url,
		SubscriptionID: sub.ID,
		Created:        now,
		ExpiresAt:      now + 24*60*60,
		Livemode:       s.cfg.Livemode,
	}
	s.store.PutSession(sess)
	writeJSON(w, http.StatusOK, toStripeCheckoutSession(sess))
}

func (s *Server) retrieveCheckoutSession(w http.ResponseWriter, r *http.Request) {
	sess, err := s.store.GetSession(r.PathValue("id"))
	if err != nil || sess.AccountID != account.From(r.Context()) {
		writeStripeError(w, http.StatusNotFound, "resource_missing", "No such checkout session.")
		return
	}
	writeJSON(w, http.StatusOK, toStripeCheckoutSession(sess))
}

func (s *Server) listCheckoutSessions(w http.ResponseWriter, r *http.Request) {
	accID := account.From(r.Context())
	sessions := s.store.ListSessions(accID, 100)
	var data []*stripeCheckoutSession
	for _, sess := range sessions {
		data = append(data, toStripeCheckoutSession(sess))
	}
	if data == nil {
		data = []*stripeCheckoutSession{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object":   "list",
		"data":     data,
		"has_more": false,
		"url":      "/v1/checkout/sessions",
	})
}

func toStripeCheckoutSession(s *store.Session) *stripeCheckoutSession {
	var details *stripeSessionCustomerDetails
	if s.CustomerEmail != "" {
		details = &stripeSessionCustomerDetails{Email: s.CustomerEmail}
	}
	return &stripeCheckoutSession{
		ID:                s.ID,
		Object:            "checkout.session",
		Mode:              s.Mode,
		Status:            s.Status,
		PaymentStatus:     s.PaymentStatus,
		AmountSubtotal:    s.AmountSubtotal,
		AmountTotal:       s.AmountTotal,
		Currency:          s.Currency,
		Customer:          s.CustomerID,
		CustomerEmail:     s.CustomerEmail,
		CustomerDetails:   details,
		SuccessURL:        s.SuccessURL,
		CancelURL:         s.CancelURL,
		URL:               s.URL,
		PaymentIntent:     s.PaymentIntentID,
		Subscription:      s.SubscriptionID,
		ClientReferenceID: s.ClientReferenceID,
		Metadata:          s.Metadata,
		Created:           s.Created,
		ExpiresAt:         s.ExpiresAt,
		Livemode:          s.Livemode,
	}
}

// resolvedLineItem is a line item with its per-unit amount, currency, and name
// resolved from either a referenced Price or inline price_data.
type resolvedLineItem struct {
	unitAmount int64
	currency   string
	name       string
	quantity   int64
}

// resolveLineItems parses Stripe's line_items form params and resolves each item.
// Supports both encodings: a referenced price id (line_items[n][price]) and inline
// price_data (line_items[n][price_data][unit_amount|currency] and optional
// [price_data][product_data][name]). Returns items in index order.
func (s *Server) resolveLineItems(r *http.Request) ([]resolvedLineItem, error) {
	const prefix = "line_items["
	type rawLine struct {
		priceID       string
		quantity      int64
		unitAmount    int64
		currency      string
		name          string
		hasPriceData  bool
		hasUnitAmount bool
	}
	items := map[int]*rawLine{}
	for k, vs := range r.PostForm {
		if !strings.HasPrefix(k, prefix) || !strings.HasSuffix(k, "]") || len(vs) == 0 {
			continue
		}
		rest := k[len(prefix):] // e.g. "0][price_data][unit_amount]"
		br := strings.IndexByte(rest, ']')
		if br < 0 {
			continue
		}
		idx, err := strconv.Atoi(rest[:br])
		if err != nil {
			continue // skip the "[]" suffix style; the official SDK uses the indexed form
		}
		remainder := rest[br+1:] // e.g. "[price_data][unit_amount]"
		it := items[idx]
		if it == nil {
			it = &rawLine{}
			items[idx] = it
		}
		switch remainder {
		case "[price]":
			it.priceID = vs[0]
		case "[quantity]":
			if n, err := strconv.ParseInt(vs[0], 10, 64); err == nil && n > 0 {
				it.quantity = n
			}
		case "[price_data][currency]":
			it.currency = strings.ToLower(vs[0])
			it.hasPriceData = true
		case "[price_data][unit_amount]":
			if n, err := strconv.ParseInt(vs[0], 10, 64); err == nil {
				it.unitAmount = n
				it.hasUnitAmount = true
			}
			it.hasPriceData = true
		case "[price_data][product_data][name]":
			it.name = vs[0]
			it.hasPriceData = true
		}
	}
	if len(items) == 0 {
		return nil, nil
	}
	maxIdx := 0
	for i := range items {
		if i > maxIdx {
			maxIdx = i
		}
	}
	out := make([]resolvedLineItem, 0, len(items))
	for i := 0; i <= maxIdx; i++ {
		it := items[i]
		if it == nil {
			continue
		}
		quantity := it.quantity
		if quantity == 0 {
			quantity = 1
		}
		switch {
		case it.priceID != "":
			price, err := s.store.GetPrice(it.priceID)
			if err != nil {
				return nil, fmt.Errorf("line_items[%d]: %v", i, err)
			}
			name := ""
			if prod, err := s.store.GetProduct(price.ProductID); err == nil {
				name = prod.Name
			}
			out = append(out, resolvedLineItem{
				unitAmount: price.UnitAmount,
				currency:   price.Currency,
				name:       name,
				quantity:   quantity,
			})
		case it.hasPriceData && it.hasUnitAmount:
			out = append(out, resolvedLineItem{
				unitAmount: it.unitAmount,
				currency:   it.currency,
				name:       it.name,
				quantity:   quantity,
			})
		default:
			return nil, fmt.Errorf("line_items[%d]: provide either 'price' or price_data with unit_amount", i)
		}
	}
	return out, nil
}

// deferredSession carries the values createCheckoutSession already computed into
// the deferred path, so nothing is recalculated or allowed to drift.
type deferredSession struct {
	piID          string
	clientSecret  string
	amountTotal   int64
	currency      string
	description   string
	customerID    string
	customerEmail string
	now           int64
}

// createDeferredCheckoutSession creates a session whose URL points at our own
// checkout page rather than a gateway's.
//
// The PaymentIntent starts in requires_payment_method — accurate, because no
// method has been chosen and no gateway has been contacted. Both become true
// when the customer selects a method and an instrument is issued.
func (s *Server) createDeferredCheckoutSession(w http.ResponseWriter, r *http.Request, d deferredSession) {
	meta := s.enrichMetadata(r, nil)
	pi := &store.PaymentIntent{
		ID:           d.piID,
		AccountID:    account.From(r.Context()),
		AmountMinor:  d.amountTotal,
		Currency:     d.currency,
		Status:       string(stripe.PaymentIntentStatusRequiresPaymentMethod),
		ClientSecret: d.clientSecret,
		Description:  d.description,
		Metadata:     meta,
		Created:      d.now,
		Livemode:     s.cfg.Livemode,
	}
	s.store.Put(pi)

	sessID := "cs_" + randID()
	sess := &store.Session{
		ID:                sessID,
		AccountID:         account.From(r.Context()),
		Mode:              "payment",
		Status:            "open",
		PaymentStatus:     "unpaid",
		AmountSubtotal:    d.amountTotal,
		AmountTotal:       d.amountTotal,
		Currency:          d.currency,
		CustomerID:        d.customerID,
		CustomerEmail:     d.customerEmail,
		SuccessURL:        r.PostFormValue("success_url"),
		CancelURL:         r.PostFormValue("cancel_url"),
		URL:               s.checkoutBaseURL(r) + sessID,
		PaymentIntentID:   d.piID,
		ClientReferenceID: r.PostFormValue("client_reference_id"),
		Description:       d.description,
		Metadata:          meta,
		Created:           d.now,
		ExpiresAt:         d.now + 24*60*60,
		Livemode:          s.cfg.Livemode,
	}
	s.store.PutSession(sess)

	writeJSON(w, http.StatusOK, toStripeCheckoutSession(sess))
}
