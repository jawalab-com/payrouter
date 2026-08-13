package server

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jawalab-com/payrouter/internal/account"
	"github.com/jawalab-com/payrouter/internal/store"
)

// stripePrice is the Stripe-shaped Price object returned by the facade. Product is
// emitted as the product id string (Stripe's default, non-expanded form), which
// stripe-go deserializes into its *Product expandable field.
type stripePrice struct {
	ID            string                `json:"id"`
	Object        string                `json:"object"`
	Product       string                `json:"product"`
	UnitAmount    int64                 `json:"unit_amount"`
	Currency      string                `json:"currency"`
	Type          string                `json:"type"`
	BillingScheme string                `json:"billing_scheme"`
	Recurring     *stripePriceRecurring `json:"recurring,omitempty"`
	Active        bool                  `json:"active"`
	Metadata      map[string]string     `json:"metadata,omitempty"`
	Created       int64                 `json:"created"`
	Livemode      bool                  `json:"livemode"`
}

// stripePriceRecurring mirrors Stripe's price.recurring object.
type stripePriceRecurring struct {
	Interval      string `json:"interval"`
	IntervalCount int64  `json:"interval_count"`
	UsageType     string `json:"usage_type"`
}

func (s *Server) createPrice(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeStripeError(w, http.StatusBadRequest, "invalid_request_error", "Unable to parse request body.")
		return
	}
	currency := strings.ToLower(r.PostFormValue("currency"))
	if currency == "" {
		writeStripeError(w, http.StatusBadRequest, "parameter_missing", "Missing required param: currency.")
		return
	}
	unitAmount, err := strconv.ParseInt(r.PostFormValue("unit_amount"), 10, 64)
	if err != nil || unitAmount < 0 {
		writeStripeError(w, http.StatusBadRequest, "parameter_invalid", "unit_amount must be a non-negative integer (minor units).")
		return
	}

	// Resolve the product: either an existing prod_ id or an inline product_data[name]
	// (Stripe creates the product for you in that case).
	productID := r.PostFormValue("product")
	if productID == "" {
		if name := r.PostFormValue("product_data[name]"); name != "" {
			now := time.Now().Unix()
			prod := &store.Product{
				ID:        "prod_" + randID(),
				AccountID: account.From(r.Context()),
				Name:      name,
				Active:    true,
				Created:   now,
				Updated:   now,
				Livemode:  s.cfg.Livemode,
			}
			s.store.PutProduct(prod)
			productID = prod.ID
		} else {
			writeStripeError(w, http.StatusBadRequest, "parameter_missing", "Must provide product (id) or product_data[name].")
			return
		}
	}

	// Recurring prices carry a schedule (recurring[interval]/[interval_count]).
	// Absent → one_time (the default). Validate the Stripe interval enum.
	priceType := "one_time"
	var interval, usageType string
	var intervalCount int64
	if iv := strings.ToLower(r.PostFormValue("recurring[interval]")); iv != "" {
		switch iv {
		case "day", "week", "month", "year":
			interval = iv
		default:
			writeStripeError(w, http.StatusBadRequest, "parameter_invalid",
				"recurring[interval] must be one of: day, week, month, year.")
			return
		}
		ic := r.PostFormValue("recurring[interval_count]")
		if ic == "" {
			intervalCount = 1
		} else {
			n, err := strconv.ParseInt(ic, 10, 64)
			if err != nil || n < 1 {
				writeStripeError(w, http.StatusBadRequest, "parameter_invalid",
					"recurring[interval_count] must be a positive integer.")
				return
			}
			intervalCount = n
		}
		priceType = "recurring"
		usageType = "licensed"
	}

	p := &store.Price{
		ID:            "price_" + randID(),
		AccountID:     account.From(r.Context()),
		ProductID:     productID,
		UnitAmount:    unitAmount,
		Currency:      currency,
		Type:          priceType,
		Interval:      interval,
		IntervalCount: intervalCount,
		UsageType:     usageType,
		Active:        formBool(r, "active", true),
		Metadata:      collectMetadata(r),
		Created:       time.Now().Unix(),
		Livemode:      s.cfg.Livemode,
	}
	s.store.PutPrice(p)
	writeJSON(w, http.StatusOK, toStripePrice(p))
}

func (s *Server) retrievePrice(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.GetPrice(r.PathValue("id"))
	if err != nil || p.AccountID != account.From(r.Context()) {
		writeStripeError(w, http.StatusNotFound, "resource_missing", "No such price.")
		return
	}
	writeJSON(w, http.StatusOK, toStripePrice(p))
}

func toStripePrice(p *store.Price) *stripePrice {
	sp := &stripePrice{
		ID:            p.ID,
		Object:        "price",
		Product:       p.ProductID,
		UnitAmount:    p.UnitAmount,
		Currency:      p.Currency,
		Type:          p.Type,
		BillingScheme: "per_unit",
		Active:        p.Active,
		Metadata:      p.Metadata,
		Created:       p.Created,
		Livemode:      p.Livemode,
	}
	if p.Type == "recurring" {
		sp.Recurring = &stripePriceRecurring{
			Interval:      p.Interval,
			IntervalCount: p.IntervalCount,
			UsageType:     p.UsageType,
		}
	}
	return sp
}
