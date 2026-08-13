package server

import (
	"net/http"

	"github.com/jawalab-com/payrouter/internal/account"
	"github.com/jawalab-com/payrouter/internal/store"
)

// stripeInvoice is the Stripe-shaped Invoice object. PaymentIntent is emitted as
// the intent id string normally; when embedded as an incomplete subscription's
// latest_invoice it is instead a small object carrying the authorization redirect
// (see stripePaymentIntentShim in subscription.go).
type stripeInvoice struct {
	ID            string `json:"id"`
	Object        string `json:"object"`
	Status        string `json:"status"`
	Customer      string `json:"customer"`
	Subscription  string `json:"subscription"`
	PaymentIntent any    `json:"payment_intent,omitempty"`
	Total         int64  `json:"total"`
	Currency      string `json:"currency"`
	BillingReason string `json:"billing_reason,omitempty"`
	Created       int64  `json:"created"`
	Livemode      bool   `json:"livemode"`
}

// retrieveInvoice handles GET /v1/invoices/{id}.
func (s *Server) retrieveInvoice(w http.ResponseWriter, r *http.Request) {
	in, err := s.store.GetInvoice(r.PathValue("id"))
	if err != nil || in.AccountID != account.From(r.Context()) {
		writeStripeError(w, http.StatusNotFound, "resource_missing", "No such invoice.")
		return
	}
	writeJSON(w, http.StatusOK, toStripeInvoice(in))
}

// toStripeInvoice serializes a stored Invoice. PaymentIntent is the intent id
// (the embedded-redirect form is assembled separately for incomplete subs).
func toStripeInvoice(in *store.Invoice) *stripeInvoice {
	return &stripeInvoice{
		ID:            in.ID,
		Object:        "invoice",
		Status:        in.Status,
		Customer:      in.CustomerID,
		Subscription:  in.SubscriptionID,
		PaymentIntent: in.PaymentIntentID,
		Total:         in.AmountMinor,
		Currency:      in.Currency,
		BillingReason: in.BillingReason,
		Created:       in.Created,
		Livemode:      in.Livemode,
	}
}
