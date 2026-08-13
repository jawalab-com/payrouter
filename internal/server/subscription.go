package server

import (
	"encoding/json"
	"errors"
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

// errSubscriptionsNotSupported is returned when the active gateway does not
// implement SubscriptionGateway (Xendit/DOKU/Mayar today). Surfaced as a 400
// parameter_invalid error — the same shape DOKU/Mayar return for refunds.
var errSubscriptionsNotSupported = errors.New("subscriptions are not supported by the active gateway")

// stripeSubscription is the Stripe-shaped Subscription object. Customer and the
// default latest_invoice are emitted as id strings (Stripe's non-expanded form);
// latest_invoice is expanded to an object only while the subscription is
// incomplete, so the authorization redirect surfaces as
// latest_invoice.payment_intent.next_action.redirect_to_url (the shape Stripe
// uses for a subscription awaiting payment).
type stripeSubscription struct {
	ID               string            `json:"id"`
	Object           string            `json:"object"`
	Status           string            `json:"status"`
	Customer         string            `json:"customer"`
	Items            stripeSubItemList `json:"items"`
	LatestInvoice    any               `json:"latest_invoice,omitempty"`
	CancelAt         int64             `json:"cancel_at"`
	CanceledAt       int64             `json:"canceled_at,omitempty"`
	CurrentPeriodEnd int64             `json:"current_period_end"`
	Created          int64             `json:"created"`
	Livemode         bool              `json:"livemode"`
	Metadata         map[string]string `json:"metadata,omitempty"`
}

// stripeSubItemList is the List envelope Stripe wraps collection fields in
// ({object:"list", data:[...]}), which stripe-go deserializes into
// *SubscriptionItemList. A bare array would fail to deserialize.
type stripeSubItemList struct {
	Object string          `json:"object"`
	Data   []stripeSubItem `json:"data"`
}

type stripeSubItem struct {
	ID       string       `json:"id"`
	Object   string       `json:"object"`
	Price    *stripePrice `json:"price"`
	Quantity int64        `json:"quantity"`
}

// stripePaymentIntentShim is the minimal PaymentIntent embedded in an incomplete
// subscription's latest_invoice, so stripe-go deserializes the authorization
// redirect from next_action.redirect_to_url.
type stripePaymentIntentShim struct {
	ID         string            `json:"id"`
	Object     string            `json:"object"`
	Status     string            `json:"status"`
	NextAction *stripeNextAction `json:"next_action,omitempty"`
}

// subscriptionGW returns the active gateway as a SubscriptionGateway, or ok=false
// if the gateway does not support recurring. The single assertion point keeps the
// "not supported" error shape uniform across handlers.
func (s *Server) subscriptionGW() (gateway.SubscriptionGateway, bool) {
	sg, ok := s.gw.(gateway.SubscriptionGateway)
	return sg, ok
}

// createSubscription handles POST /v1/subscriptions. It resolves the recurring
// price, starts gateway authorization (a hosted save-card page), and stores the
// incomplete subscription + its first invoice + the authorization PaymentIntent.
// The returned subscription carries the redirect at
// latest_invoice.payment_intent.next_action.redirect_to_url.
func (s *Server) createSubscription(w http.ResponseWriter, r *http.Request) {
	if !parseForm(w, r) {
		return
	}
	customerID := r.PostFormValue("customer")
	if customerID == "" {
		writeStripeError(w, http.StatusBadRequest, "parameter_missing", "Missing required param: customer.")
		return
	}
	sub, err := s.buildSubscription(r, customerID, "")
	if err != nil {
		writeSubError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.toStripeSubscription(sub, true))
}

// retrieveSubscription handles GET /v1/subscriptions/{id}.
func (s *Server) retrieveSubscription(w http.ResponseWriter, r *http.Request) {
	sub, err := s.store.GetSubscription(r.PathValue("id"))
	if err != nil || sub.AccountID != account.From(r.Context()) {
		writeStripeError(w, http.StatusNotFound, "resource_missing", "No such subscription.")
		return
	}
	writeJSON(w, http.StatusOK, s.toStripeSubscription(sub, sub.Status == string(stripe.SubscriptionStatusIncomplete)))
}

// cancelSubscription handles DELETE /v1/subscriptions/{id}. If the subscription
// was activated, it cancels the gateway schedule; otherwise it just flips local
// state. Emits customer.subscription.deleted.
func (s *Server) cancelSubscription(w http.ResponseWriter, r *http.Request) {
	sub, err := s.store.GetSubscription(r.PathValue("id"))
	if err != nil || sub.AccountID != account.From(r.Context()) {
		writeStripeError(w, http.StatusNotFound, "resource_missing", "No such subscription.")
		return
	}
	if sub.GatewayID != "" {
		if sg, ok := s.subscriptionGW(); ok {
			if _, gerr := sg.CancelSubscription(r.Context(), sub.GatewayID); gerr != nil {
				writeStripeError(w, http.StatusBadGateway, "api_error", gerr.Error())
				return
			}
		}
	}
	now := time.Now().Unix()
	sub.Status = string(stripe.SubscriptionStatusCanceled)
	sub.CanceledAt = now
	s.store.PutSubscription(sub)

	subJSON, _ := json.Marshal(s.toStripeSubscription(sub, false))
	s.enqueueEvent("customer.subscription.deleted", sub.ID, subJSON, now)

	writeJSON(w, http.StatusOK, s.toStripeSubscription(sub, false))
}

// buildSubscription is the shared create path for POST /v1/subscriptions and
// mode=subscription Checkout Sessions. It resolves the recurring price, starts
// gateway authorization, stores the incomplete subscription + first (open)
// invoice + authorization PaymentIntent, and emits customer.subscription.created.
func (s *Server) buildSubscription(r *http.Request, customerID, successURL string) (*store.Subscription, error) {
	price, quantity, err := s.resolveSubItem(r)
	if err != nil {
		return nil, err
	}
	sg, ok := s.subscriptionGW()
	if !ok {
		return nil, errSubscriptionsNotSupported
	}

	amount := price.UnitAmount * quantity
	var cust *gateway.Customer
	if customerID != "" {
		if c, gerr := s.store.GetCustomer(customerID); gerr == nil {
			cust = &gateway.Customer{ID: c.ID, Email: c.Email, Name: c.Name, Phone: c.Phone}
		}
	}

	now := time.Now().Unix()
	accID := account.From(r.Context())
	authPIID := "pi_" + randID()
	name := productName(s, price)
	res, err := sg.CreateSubscription(r.Context(), &gateway.CreateSubscriptionInput{
		Reference:     authPIID,
		AmountMinor:   amount,
		Currency:      price.Currency,
		Name:          name,
		Customer:      cust,
		Interval:      price.Interval,
		IntervalCount: price.IntervalCount,
		ReturnURL:     successURL,
	})
	if err != nil {
		return nil, err
	}

	// Authorization PaymentIntent (the save-card charge). It sits in
	// requires_action until the customer completes the hosted page; the gateway
	// activation webhook then marks it succeeded.
	pi := &store.PaymentIntent{
		ID:                authPIID,
		AccountID:         accID,
		AmountMinor:       amount,
		Currency:          price.Currency,
		Status:            string(stripe.PaymentIntentStatusRequiresAction),
		ClientSecret:      authPIID + "_secret_" + randID(),
		PaymentMethodType: string(gateway.IDCard),
		GatewayReference:  res.AuthReference,
		Description:       name,
		Metadata:          collectMetadata(r),
		Created:           now,
		Livemode:          s.cfg.Livemode,
	}
	applyNextAction(pi, res.NextAction)
	s.store.Put(pi)

	sub := &store.Subscription{
		ID:                  "sub_" + randID(),
		AccountID:           accID,
		CustomerID:          customerID,
		PriceID:             price.ID,
		Status:              string(stripe.SubscriptionStatusIncomplete),
		AuthPaymentIntentID: authPIID,
		Interval:            price.Interval,
		IntervalCount:       price.IntervalCount,
		AmountMinor:         amount,
		Currency:            price.Currency,
		Created:             now,
		Livemode:            s.cfg.Livemode,
		Metadata:            collectMetadata(r),
	}
	invoice := &store.Invoice{
		ID:              "in_" + randID(),
		AccountID:       accID,
		SubscriptionID:  sub.ID,
		PaymentIntentID: authPIID,
		CustomerID:      customerID,
		AmountMinor:     amount,
		Currency:        price.Currency,
		Status:          string(stripe.InvoiceStatusOpen),
		BillingReason:   "subscription_create",
		Created:         now,
		Livemode:        s.cfg.Livemode,
	}
	sub.LatestInvoiceID = invoice.ID

	s.store.PutSubscription(sub)
	s.store.PutInvoice(invoice)

	// Stripe fires customer.subscription.created on creation.
	subJSON, _ := json.Marshal(s.toStripeSubscription(sub, false))
	s.enqueueEvent("customer.subscription.created", sub.ID, subJSON, now)

	return sub, nil
}

// resolveSubItem resolves items[0] into a recurring Price + quantity. The price
// may be a referenced recurring price_ id, or inline price_data with a recurring
// schedule (a recurring Price is created and stored, mirroring createPrice).
func (s *Server) resolveSubItem(r *http.Request) (*store.Price, int64, error) {
	quantity := int64(1)
	if q := r.PostFormValue("items[0][quantity]"); q != "" {
		if n, err := strconv.ParseInt(q, 10, 64); err == nil && n > 0 {
			quantity = n
		}
	}
	if priceID := r.PostFormValue("items[0][price]"); priceID != "" {
		p, err := s.store.GetPrice(priceID)
		if err != nil {
			return nil, 0, fmt.Errorf("items[0][price]: %v", err)
		}
		if p.Type != "recurring" {
			return nil, 0, fmt.Errorf("items[0][price]: price %s is not recurring", priceID)
		}
		return p, quantity, nil
	}

	// Inline price_data with a recurring schedule.
	interval := strings.ToLower(r.PostFormValue("items[0][price_data][recurring][interval]"))
	if interval == "" {
		return nil, 0, fmt.Errorf("items[0]: provide a recurring 'price' id or price_data[recurring][interval].")
	}
	switch interval {
	case "day", "week", "month", "year":
	default:
		return nil, 0, fmt.Errorf("items[0][price_data][recurring][interval] must be one of: day, week, month, year.")
	}
	unitAmount, err := strconv.ParseInt(r.PostFormValue("items[0][price_data][unit_amount]"), 10, 64)
	if err != nil || unitAmount < 0 {
		return nil, 0, fmt.Errorf("items[0][price_data][unit_amount] must be a non-negative integer.")
	}
	currency := strings.ToLower(r.PostFormValue("items[0][price_data][currency]"))
	if currency == "" {
		currency = "idr"
	}
	intervalCount := int64(1)
	if v := r.PostFormValue("items[0][price_data][recurring][interval_count]"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			intervalCount = n
		}
	}
	now := time.Now().Unix()
	prod := &store.Product{
		ID:        "prod_" + randID(),
		AccountID: account.From(r.Context()),
		Name:      r.PostFormValue("items[0][price_data][product_data][name]"),
		Active:    true,
		Created:   now,
		Updated:   now,
		Livemode:  s.cfg.Livemode,
	}
	s.store.PutProduct(prod)
	p := &store.Price{
		ID:            "price_" + randID(),
		AccountID:     account.From(r.Context()),
		ProductID:     prod.ID,
		UnitAmount:    unitAmount,
		Currency:      currency,
		Type:          "recurring",
		Interval:      interval,
		IntervalCount: intervalCount,
		UsageType:     "licensed",
		Active:        true,
		Created:       now,
		Livemode:      s.cfg.Livemode,
	}
	s.store.PutPrice(p)
	return p, quantity, nil
}

// writeSubError maps subscription build errors onto Stripe error shapes.
func writeSubError(w http.ResponseWriter, err error) {
	if err == errSubscriptionsNotSupported {
		writeStripeError(w, http.StatusBadRequest, "parameter_invalid", err.Error())
		return
	}
	if errors.Is(err, errSubscriptionsNotSupported) {
		writeStripeError(w, http.StatusBadRequest, "parameter_invalid", err.Error())
		return
	}
	// A gateway call failure (e.g. authorization create rejected).
	writeStripeError(w, http.StatusBadGateway, "api_error", err.Error())
}

// productName returns the linked Product's name, or a generic default.
func productName(s *Server, p *store.Price) string {
	if prod, err := s.store.GetProduct(p.ProductID); err == nil && prod.Name != "" {
		return prod.Name
	}
	return "Subscription"
}

// toStripeSubscription serializes a stored Subscription. When withLatest and the
// subscription is still incomplete, latest_invoice is expanded with the
// authorization redirect; otherwise it is the invoice id string.
func (s *Server) toStripeSubscription(sub *store.Subscription, withLatest bool) *stripeSubscription {
	out := &stripeSubscription{
		ID:               sub.ID,
		Object:           "subscription",
		Status:           sub.Status,
		Customer:         sub.CustomerID,
		Items:            stripeSubItemList{Object: "list", Data: s.subItems(sub)},
		CancelAt:         sub.CanceledAt,
		CanceledAt:       sub.CanceledAt,
		CurrentPeriodEnd: sub.CurrentPeriodEnd,
		Created:          sub.Created,
		Livemode:         sub.Livemode,
		Metadata:         sub.Metadata,
	}
	if sub.LatestInvoiceID == "" {
		return out
	}
	if !withLatest || sub.Status != string(stripe.SubscriptionStatusIncomplete) {
		out.LatestInvoice = sub.LatestInvoiceID
		return out
	}
	inv := &stripeInvoice{
		ID:            sub.LatestInvoiceID,
		Object:        "invoice",
		Status:        string(stripe.InvoiceStatusOpen),
		Customer:      sub.CustomerID,
		Subscription:  sub.ID,
		Total:         sub.AmountMinor,
		Currency:      sub.Currency,
		BillingReason: "subscription_create",
		Created:       sub.Created,
		Livemode:      sub.Livemode,
	}
	inv.PaymentIntent = s.redirectPI(sub)
	out.LatestInvoice = inv
	return out
}

// subItems builds the subscription's items list from its linked recurring Price.
func (s *Server) subItems(sub *store.Subscription) []stripeSubItem {
	if sub.PriceID == "" {
		return nil
	}
	item := stripeSubItem{ID: "si_" + randID(), Object: "subscription_item", Quantity: 1}
	if price, err := s.store.GetPrice(sub.PriceID); err == nil {
		item.Price = toStripePrice(price)
	}
	return []stripeSubItem{item}
}

// redirectPI builds the embedded PaymentIntent carrying the authorization
// redirect, or falls back to the intent id if there is no next action.
func (s *Server) redirectPI(sub *store.Subscription) any {
	pi, err := s.store.Get(sub.AuthPaymentIntentID)
	if err != nil || pi.NextActionType == "" {
		return sub.AuthPaymentIntentID
	}
	return &stripePaymentIntentShim{
		ID:     pi.ID,
		Object: "payment_intent",
		Status: pi.Status,
		NextAction: &stripeNextAction{
			Type: pi.NextActionType,
			RedirectToURL: &stripeNextActionRedirect{
				URL:       pi.NextActionURL,
				ReturnURL: pi.NextActionReturn,
			},
		},
	}
}
