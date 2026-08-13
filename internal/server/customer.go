package server

import (
	"net/http"
	"time"

	"github.com/stripe-compatible-facade/internal/account"
	"github.com/stripe-compatible-facade/internal/store"
)

// stripeCustomer is the Stripe-shaped Customer object returned by the facade.
type stripeCustomer struct {
	ID       string            `json:"id"`
	Object   string            `json:"object"`
	Email    string            `json:"email,omitempty"`
	Name     string            `json:"name,omitempty"`
	Phone    string            `json:"phone,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
	Created  int64             `json:"created"`
	Livemode bool              `json:"livemode"`
}

func (s *Server) createCustomer(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeStripeError(w, http.StatusBadRequest, "invalid_request_error", "Unable to parse request body.")
		return
	}
	c := &store.Customer{
		ID:        "cus_" + randID(),
		AccountID: account.From(r.Context()),
		Email:     r.PostFormValue("email"),
		Name:      r.PostFormValue("name"),
		Phone:     r.PostFormValue("phone"),
		Metadata:  collectMetadata(r),
		Created:   time.Now().Unix(),
		Livemode:  s.cfg.Livemode,
	}
	s.store.PutCustomer(c)
	writeJSON(w, http.StatusOK, toStripeCustomer(c))
}

func (s *Server) retrieveCustomer(w http.ResponseWriter, r *http.Request) {
	c, err := s.store.GetCustomer(r.PathValue("id"))
	if err != nil || c.AccountID != account.From(r.Context()) {
		writeStripeError(w, http.StatusNotFound, "resource_missing", "No such customer.")
		return
	}
	writeJSON(w, http.StatusOK, toStripeCustomer(c))
}

func toStripeCustomer(c *store.Customer) *stripeCustomer {
	return &stripeCustomer{
		ID:       c.ID,
		Object:   "customer",
		Email:    c.Email,
		Name:     c.Name,
		Phone:    c.Phone,
		Metadata: c.Metadata,
		Created:  c.Created,
		Livemode: c.Livemode,
	}
}
