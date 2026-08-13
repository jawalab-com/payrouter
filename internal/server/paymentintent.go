package server

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/stripe-compatible-facade/internal/account"
	"github.com/stripe-compatible-facade/internal/gateway"
	"github.com/stripe-compatible-facade/internal/store"
	stripe "github.com/stripe/stripe-go/v81"
)

// --- Stripe-shaped response types ------------------------------------------

type stripePaymentIntent struct {
	ID                 string                  `json:"id"`
	Object             string                  `json:"object"`
	Amount             int64                   `json:"amount"`
	AmountCapturable   int64                   `json:"amount_capturable"`
	AmountReceived     int64                   `json:"amount_received"`
	Currency           string                  `json:"currency"`
	Status             string                  `json:"status"`
	CaptureMethod      string                  `json:"capture_method"`
	ConfirmationMethod string                  `json:"confirmation_method"`
	ClientSecret       string                  `json:"client_secret,omitempty"`
	PaymentMethodTypes []string                `json:"payment_method_types"`
	NextAction         *stripeNextAction       `json:"next_action,omitempty"`
	LastPaymentError   *stripeLastPaymentError `json:"last_payment_error,omitempty"`
	Description        string                  `json:"description,omitempty"`
	Metadata           map[string]string       `json:"metadata,omitempty"`
	Created            int64                   `json:"created"`
	Livemode           bool                    `json:"livemode"`
}

// stripeLastPaymentError mirrors Stripe's PaymentIntent last_payment_error. The
// facade receives no structured decline reason from the gateway, so the message
// is generic and the type is the most honest non-specific value ("api_error").
type stripeLastPaymentError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type stripeNextAction struct {
	Type          string                    `json:"type"`
	RedirectToURL *stripeNextActionRedirect `json:"redirect_to_url,omitempty"`
}

type stripeNextActionRedirect struct {
	URL       string `json:"url"`
	ReturnURL string `json:"return_url,omitempty"`
}

// --- Handlers ---------------------------------------------------------------

func (s *Server) createPaymentIntent(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeStripeError(w, http.StatusBadRequest, "invalid_request_error", "Unable to parse request body.")
		return
	}
	amount, err := strconv.ParseInt(r.PostFormValue("amount"), 10, 64)
	if err != nil || amount < 0 {
		writeStripeError(w, http.StatusBadRequest, "parameter_invalid", "amount must be a non-negative integer (minor units).")
		return
	}
	currency := strings.ToLower(r.PostFormValue("currency"))
	if currency == "" {
		currency = "idr"
	}

	pmTypes := collectPaymentMethodTypes(r)
	pmType := gateway.IDVirtualAccount // default for the stub flow
	if len(pmTypes) > 0 {
		pmType = gateway.IDPaymentMethodType(pmTypes[0])
	}

	metadata := collectMetadata(r)
	id := commandID(r.Context(), "pi_", "payment_intents.create")
	clientSecret := id + "_secret_" + commandID(r.Context(), "", "payment_intents.client_secret")
	pi := &store.PaymentIntent{
		ID: id, AccountID: account.From(r.Context()), AmountMinor: amount,
		Currency: currency, Status: string(stripe.PaymentIntentStatusRequiresPaymentMethod),
		ClientSecret: clientSecret, PaymentMethodType: string(pmType),
		Description: r.PostFormValue("description"), Metadata: metadata,
		Created: time.Now().Unix(), Livemode: s.cfg.Livemode,
	}

	// Persist the command and an append-only started attempt before crossing the
	// provider boundary. A recovered idempotent request reuses the same pi_ id.
	if s.payments != nil {
		if err := s.store.RunInTx(r.Context(), func(ctx context.Context) error {
			if err := s.payments.WriteIntent(ctx, pi); err != nil {
				return err
			}
			if s.audit != nil {
				return s.audit.RecordAttempt(ctx, &store.ProviderAttempt{AccountID: pi.AccountID, PaymentIntentID: id, Reference: id, Gateway: s.gw.Name(), Operation: "create", Status: "processing"})
			}
			return nil
		}); err != nil {
			writeStripeError(w, http.StatusInternalServerError, "api_error", "Unable to prepare payment intent.")
			return
		}
	}

	res, err := s.gw.CreatePayment(r.Context(), &gateway.CreatePaymentInput{
		Reference:         id,
		AmountMinor:       amount,
		Currency:          currency,
		Description:       r.PostFormValue("description"),
		PaymentMethodType: pmType,
		MethodParams:      metadataToAny(metadata),
		ReturnURL:         r.PostFormValue("return_url"),
	})
	if err != nil {
		if s.payments != nil {
			if dbErr := s.store.RunInTx(r.Context(), func(ctx context.Context) error {
				if _, dbErr := s.payments.SetIntentStatus(ctx, pi.AccountID, id, string(stripe.PaymentIntentStatusCanceled), "provider request failed"); dbErr != nil {
					return dbErr
				}
				if s.audit != nil {
					if dbErr := s.audit.RecordAttempt(ctx, &store.ProviderAttempt{AccountID: pi.AccountID, PaymentIntentID: id, Reference: id, Gateway: s.gw.Name(), Operation: "create", Status: "failed", Error: "provider request failed"}); dbErr != nil {
						return dbErr
					}
					return s.audit.RecordTransition(ctx, &store.PaymentTransition{AccountID: pi.AccountID, PaymentIntentID: id, FromStatus: pi.Status, ToStatus: string(stripe.PaymentIntentStatusCanceled), Reason: "provider request failed"})
				}
				return nil
			}); dbErr != nil {
				writeStripeError(w, http.StatusInternalServerError, "api_error", "Unable to persist provider failure.")
				return
			}
		}
		writeStripeError(w, http.StatusBadGateway, "api_error", err.Error())
		return
	}

	fromStatus := pi.Status
	pi.Status = string(res.Status)
	pi.GatewayReference = res.GatewayReference
	pi.Metadata = s.enrichMetadata(r, res.Reference)
	applyNextAction(pi, res.NextAction)

	if s.payments != nil {
		// Persist the provider outcome without holding a transaction over HTTP.
		accID := pi.AccountID
		att := &store.ProviderAttempt{
			AccountID: accID, PaymentIntentID: id, Reference: id,
			Gateway: s.gw.Name(), GatewayReference: res.GatewayReference,
			Operation: "create", Status: pi.Status,
		}
		tr := &store.PaymentTransition{
			AccountID: accID, PaymentIntentID: id, FromStatus: fromStatus, ToStatus: pi.Status, Reason: "create",
		}
		if err := s.store.RunInTx(r.Context(), func(ctx context.Context) error {
			if err := s.payments.WriteIntent(ctx, pi); err != nil {
				return err
			}
			if s.audit != nil {
				if err := s.audit.RecordAttempt(ctx, att); err != nil {
					return err
				}
				if err := s.audit.RecordTransition(ctx, tr); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			writeStripeError(w, http.StatusInternalServerError, "api_error", "Unable to persist payment intent.")
			return
		}
	} else {
		s.store.Put(pi)
	}

	writeJSON(w, http.StatusOK, toStripeResponse(pi))
}

func (s *Server) retrievePaymentIntent(w http.ResponseWriter, r *http.Request) {
	pi, err := s.readIntent(r, r.PathValue("id"))
	if err != nil {
		writeStripeError(w, http.StatusNotFound, "resource_missing", "No such payment_intent.")
		return
	}
	writeJSON(w, http.StatusOK, toStripeResponse(pi))
}

// confirmPaymentIntent handles POST /v1/payment_intents/{id}/confirm. For these
// redirect/hosted gateways the gateway payment and redirect URL were produced at
// PaymentIntent creation time, so confirm surfaces the existing next_action
// (matching Stripe, which returns the intent with its redirect). An explicit
// return_url updates where the customer lands after the redirect.
func (s *Server) confirmPaymentIntent(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeStripeError(w, http.StatusBadRequest, "invalid_request_error", "Unable to parse request body.")
		return
	}
	id := r.PathValue("id")
	pi, err := s.readIntent(r, id)
	if err != nil {
		writeStripeError(w, http.StatusNotFound, "resource_missing", "No such payment_intent.")
		return
	}
	if ret := r.PostFormValue("return_url"); ret != "" && pi.NextActionType != "" {
		pi.NextActionReturn = ret
		if err := s.writeIntent(r, pi); err != nil {
			writeStripeError(w, http.StatusInternalServerError, "api_error", "Unable to persist payment intent.")
			return
		}
	}
	writeJSON(w, http.StatusOK, toStripeResponse(pi))
}

// readIntent reads a PaymentIntent with account isolation when durable, or the
// legacy unscoped Get otherwise.
func (s *Server) readIntent(r *http.Request, id string) (*store.PaymentIntent, error) {
	if s.payments != nil {
		return s.payments.ReadIntent(r.Context(), account.From(r.Context()), id)
	}
	return s.store.Get(id)
}

// writeIntent persists a PaymentIntent through the durable path (transactional)
// or the legacy in-memory Put.
func (s *Server) writeIntent(r *http.Request, pi *store.PaymentIntent) error {
	if s.payments != nil {
		return s.store.RunInTx(r.Context(), func(ctx context.Context) error {
			return s.payments.WriteIntent(ctx, pi)
		})
	}
	s.store.Put(pi)
	return nil
}

// --- Helpers ----------------------------------------------------------------

func toStripeResponse(pi *store.PaymentIntent) *stripePaymentIntent {
	var amountReceived int64
	if pi.Status == string(stripe.PaymentIntentStatusSucceeded) {
		// Stripe sets amount_received = amount on success; 0 otherwise.
		amountReceived = pi.AmountMinor
	}
	resp := &stripePaymentIntent{
		ID:                 pi.ID,
		Object:             "payment_intent",
		Amount:             pi.AmountMinor,
		AmountReceived:     amountReceived,
		Currency:           pi.Currency,
		Status:             pi.Status,
		CaptureMethod:      "automatic",
		ConfirmationMethod: "manual",
		ClientSecret:       pi.ClientSecret,
		PaymentMethodTypes: paymentMethodTypes(pi.PaymentMethodType),
		Description:        pi.Description,
		Metadata:           pi.Metadata,
		Created:            pi.Created,
		Livemode:           pi.Livemode,
	}
	if pi.FailureMessage != "" {
		resp.LastPaymentError = &stripeLastPaymentError{Type: "api_error", Message: pi.FailureMessage}
	}
	if pi.NextActionType != "" {
		resp.NextAction = &stripeNextAction{
			Type: pi.NextActionType,
			RedirectToURL: &stripeNextActionRedirect{
				URL:       pi.NextActionURL,
				ReturnURL: pi.NextActionReturn,
			},
		}
	}
	return resp
}

// applyNextAction flattens a stripe.PaymentIntentNextAction into the store record.
func applyNextAction(pi *store.PaymentIntent, na *stripe.PaymentIntentNextAction) {
	if na == nil {
		return
	}
	pi.NextActionType = string(na.Type)
	if na.RedirectToURL != nil {
		pi.NextActionURL = na.RedirectToURL.URL
		pi.NextActionReturn = na.RedirectToURL.ReturnURL
	}
}

// collectMetadata extracts Stripe-style metadata[key]=value form fields.
func collectMetadata(r *http.Request) map[string]string {
	m := map[string]string{}
	for k, vs := range r.PostForm {
		if strings.HasPrefix(k, "metadata[") && strings.HasSuffix(k, "]") {
			key := k[len("metadata[") : len(k)-1]
			if len(vs) > 0 {
				m[key] = vs[0]
			}
		}
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

func metadataToAny(m map[string]string) map[string]any {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// paymentMethodTypes returns the PaymentIntent's payment_method_types slice, or
// nil to omit it when the intent has no single method (e.g. one created from a
// Checkout Session, where the gateway hosted page shows all methods).
func paymentMethodTypes(t string) []string {
	if t == "" {
		return nil
	}
	return []string{t}
}

// collectPaymentMethodTypes reads payment_method_types from the form, accepting
// both encodings a Stripe client may send: the "[]" suffix style used by Stripe.js
// and curl (`payment_method_types[]=id_qris`), and the indexed style used by the
// official stripe-go SDK (`payment_method_types[0]=id_qris`). Results are returned
// in index order for the indexed style.
func collectPaymentMethodTypes(r *http.Request) []string {
	if vs := r.PostForm["payment_method_types[]"]; len(vs) > 0 {
		return vs
	}
	const prefix = "payment_method_types["
	indexed := map[int]string{}
	maxIdx := -1
	for k, vs := range r.PostForm {
		if !strings.HasPrefix(k, prefix) || !strings.HasSuffix(k, "]") || len(vs) == 0 {
			continue
		}
		inner := k[len(prefix) : len(k)-1]
		if inner == "" { // the "[]" case, handled above
			continue
		}
		n, err := strconv.Atoi(inner)
		if err != nil {
			continue
		}
		indexed[n] = vs[0]
		if n > maxIdx {
			maxIdx = n
		}
	}
	if len(indexed) == 0 {
		return nil
	}
	out := make([]string, 0, len(indexed))
	for i := 0; i <= maxIdx; i++ {
		if v, ok := indexed[i]; ok {
			out = append(out, v)
		}
	}
	return out
}

func (s *Server) listPaymentIntents(w http.ResponseWriter, r *http.Request) {
	accID := account.From(r.Context())
	intents := s.store.ListIntents(accID, 100)
	var data []*stripePaymentIntent
	for _, pi := range intents {
		data = append(data, toStripeResponse(pi))
	}
	if data == nil {
		data = []*stripePaymentIntent{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object":   "list",
		"data":     data,
		"has_more": false,
		"url":      "/v1/payment_intents",
	})
}
