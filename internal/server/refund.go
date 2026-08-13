package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/jawalab-com/payrouter/internal/account"
	"github.com/jawalab-com/payrouter/internal/gateway"
	"github.com/jawalab-com/payrouter/internal/store"
)

// stripeRefund is the Stripe-shaped Refund object returned by the facade.
// PaymentIntent is emitted as the intent id string (Stripe's default form).
type stripeRefund struct {
	ID            string            `json:"id"`
	Object        string            `json:"object"`
	Amount        int64             `json:"amount"`
	Currency      string            `json:"currency"`
	Status        string            `json:"status"`
	Reason        string            `json:"reason,omitempty"`
	PaymentIntent string            `json:"payment_intent"`
	Metadata      map[string]string `json:"metadata,omitempty"`
	Created       int64             `json:"created"`
	Livemode      bool              `json:"livemode"`
}

func (s *Server) createRefund(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeStripeError(w, http.StatusBadRequest, "invalid_request_error", "Unable to parse request body.")
		return
	}
	piID := r.PostFormValue("payment_intent")
	if piID == "" {
		writeStripeError(w, http.StatusBadRequest, "parameter_missing", "Missing required param: payment_intent.")
		return
	}
	pi, err := s.readIntent(r, piID)
	if err != nil {
		writeStripeError(w, http.StatusNotFound, "resource_missing", "No such payment_intent.")
		return
	}

	// amount omitted/0 means a full refund.
	amount := int64(0)
	if v := r.PostFormValue("amount"); v != "" {
		amount, err = strconv.ParseInt(v, 10, 64)
		if err != nil || amount < 0 {
			writeStripeError(w, http.StatusBadRequest, "parameter_invalid", "amount must be a non-negative integer (minor units).")
			return
		}
	}
	reason := r.PostFormValue("reason")
	switch reason {
	case "", "duplicate", "fraudulent", "requested_by_customer":
		// Stripe restricts refund reason to this enum; empty means "unspecified".
	default:
		writeStripeError(w, http.StatusBadRequest, "parameter_invalid",
			"reason must be one of: duplicate, fraudulent, requested_by_customer.")
		return
	}

	now := time.Now().Unix()
	accID := account.From(r.Context())
	rf := &store.Refund{
		ID: commandID(r.Context(), "re_", "refunds.create"), AccountID: accID,
		PaymentIntentID: pi.ID, Amount: amount, Currency: pi.Currency,
		Status: "pending", Reason: reason, Metadata: collectMetadata(r),
		Created: now, Livemode: s.cfg.Livemode,
	}
	if s.payments != nil {
		if err := s.store.RunInTx(r.Context(), func(ctx context.Context) error {
			if err := s.payments.ReserveRefund(ctx, rf); err != nil {
				return err
			}
			if s.audit != nil {
				return s.audit.RecordAttempt(ctx, &store.ProviderAttempt{AccountID: accID, PaymentIntentID: pi.ID, Reference: rf.ID, Gateway: s.gw.Name(), Operation: "refund", Status: "processing"})
			}
			return nil
		}); err != nil {
			if errors.Is(err, store.ErrRefundExceedsPayment) {
				writeStripeError(w, http.StatusBadRequest, "amount_too_large", "Refund amount exceeds the remaining refundable amount.")
			} else {
				writeStripeError(w, http.StatusInternalServerError, "api_error", "Unable to reserve refund.")
			}
			return
		}
		if rf.Status == "succeeded" {
			writeJSON(w, http.StatusOK, toStripeRefund(rf))
			return
		}
	} else if rf.Amount == 0 {
		rf.Amount = pi.AmountMinor
	}

	res, err := s.gw.Refund(r.Context(), &gateway.RefundInput{
		Reference:        rf.ID,
		PaymentReference: pi.ID, // the gateway order/external id (= pi_), as Midtrans refunds by order_id
		AmountMinor:      rf.Amount,
		Reason:           reason,
	})
	if err != nil {
		if s.payments != nil {
			rf.Status = "failed"
			if dbErr := s.store.RunInTx(r.Context(), func(ctx context.Context) error {
				if dbErr := s.payments.WriteRefund(ctx, rf); dbErr != nil {
					return dbErr
				}
				if s.audit != nil {
					return s.audit.RecordAttempt(ctx, &store.ProviderAttempt{AccountID: accID, PaymentIntentID: pi.ID, Reference: rf.ID, Gateway: s.gw.Name(), Operation: "refund", Status: "failed", Error: "provider request failed"})
				}
				return nil
			}); dbErr != nil {
				writeStripeError(w, http.StatusInternalServerError, "api_error", "Unable to persist provider failure.")
				return
			}
		}
		writeStripeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	status := res.Status
	if status == "" {
		status = "succeeded"
	}

	rf.Status = status
	if s.payments != nil {
		att := &store.ProviderAttempt{
			AccountID: accID, PaymentIntentID: pi.ID, Reference: rf.ID,
			Gateway: s.gw.Name(), GatewayReference: res.GatewayReference,
			Operation: "refund", Status: rf.Status,
		}
		if err := s.store.RunInTx(r.Context(), func(ctx context.Context) error {
			if err := s.payments.WriteRefund(ctx, rf); err != nil {
				return err
			}
			if s.audit != nil {
				return s.audit.RecordAttempt(ctx, att)
			}
			return nil
		}); err != nil {
			writeStripeError(w, http.StatusInternalServerError, "api_error", "Unable to persist refund outcome.")
			return
		}
	} else {
		s.store.PutRefund(rf)
	}

	// Emit refund.created so webhook-based refund tracking works (Stripe fires it
	// on every refund). data.object is the Refund object.
	if s.payments == nil {
		refundJSON, _ := json.Marshal(toStripeRefund(rf))
		s.enqueueEvent("refund.created", rf.ID, refundJSON, now)
	}

	writeJSON(w, http.StatusOK, toStripeRefund(rf))
}

func (s *Server) retrieveRefund(w http.ResponseWriter, r *http.Request) {
	var rf *store.Refund
	if s.payments != nil {
		got, err := s.payments.ReadRefund(r.Context(), account.From(r.Context()), r.PathValue("id"))
		if err != nil {
			writeStripeError(w, http.StatusNotFound, "resource_missing", "No such refund.")
			return
		}
		rf = got
	} else {
		got, err := s.store.GetRefund(r.PathValue("id"))
		if err != nil {
			writeStripeError(w, http.StatusNotFound, "resource_missing", "No such refund.")
			return
		}
		rf = got
	}
	writeJSON(w, http.StatusOK, toStripeRefund(rf))
}

func toStripeRefund(r *store.Refund) *stripeRefund {
	return &stripeRefund{
		ID:            r.ID,
		Object:        "refund",
		Amount:        r.Amount,
		Currency:      r.Currency,
		Status:        r.Status,
		Reason:        r.Reason,
		PaymentIntent: r.PaymentIntentID,
		Metadata:      r.Metadata,
		Created:       r.Created,
		Livemode:      r.Livemode,
	}
}
