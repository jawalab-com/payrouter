package server

import (
	"net/http"
	"time"

	"github.com/jawalab-com/payrouter/internal/account"
	"github.com/jawalab-com/payrouter/internal/store"
)

// stripeProduct is the Stripe-shaped Product object returned by the facade.
type stripeProduct struct {
	ID          string            `json:"id"`
	Object      string            `json:"object"`
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Active      bool              `json:"active"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	Created     int64             `json:"created"`
	Updated     int64             `json:"updated"`
	Livemode    bool              `json:"livemode"`
}

func (s *Server) createProduct(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeStripeError(w, http.StatusBadRequest, "invalid_request_error", "Unable to parse request body.")
		return
	}
	name := r.PostFormValue("name")
	if name == "" {
		writeStripeError(w, http.StatusBadRequest, "parameter_missing", "Missing required param: name.")
		return
	}
	now := time.Now().Unix()
	p := &store.Product{
		ID:          "prod_" + randID(),
		AccountID:   account.From(r.Context()),
		Name:        name,
		Description: r.PostFormValue("description"),
		Active:      formBool(r, "active", true),
		Metadata:    collectMetadata(r),
		Created:     now,
		Updated:     now,
		Livemode:    s.cfg.Livemode,
	}
	s.store.PutProduct(p)
	writeJSON(w, http.StatusOK, toStripeProduct(p))
}

func (s *Server) retrieveProduct(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.GetProduct(r.PathValue("id"))
	if err != nil || p.AccountID != account.From(r.Context()) {
		writeStripeError(w, http.StatusNotFound, "resource_missing", "No such product.")
		return
	}
	writeJSON(w, http.StatusOK, toStripeProduct(p))
}

func toStripeProduct(p *store.Product) *stripeProduct {
	return &stripeProduct{
		ID:          p.ID,
		Object:      "product",
		Name:        p.Name,
		Description: p.Description,
		Active:      p.Active,
		Metadata:    p.Metadata,
		Created:     p.Created,
		Updated:     p.Updated,
		Livemode:    p.Livemode,
	}
}
