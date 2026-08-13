package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/stripe-compatible-facade/internal/gateway"
	"github.com/stripe-compatible-facade/internal/store"
	stripe "github.com/stripe/stripe-go/v81"
)

// inboundMaxAttempts is the ceiling after which a stuck provider notification is
// dead-lettered rather than retried by the reconciler.
const inboundMaxAttempts = 8

// stripeAPIVersion is the version advertised on outbound Stripe Events. It tracks
// the stripe-go release we depend on so the official SDK's webhook.ConstructEvent
// (which enforces a matching release train) accepts our events.
const stripeAPIVersion = stripe.APIVersion

// stripeEvent is the Stripe "Event" envelope wrapped around a PaymentIntent.
type stripeEvent struct {
	ID         string          `json:"id"` // evt_...
	Object     string          `json:"object"`
	APIVersion string          `json:"api_version"`
	Created    int64           `json:"created"`
	Type       string          `json:"type"`
	Data       stripeEventData `json:"data"`
}

type stripeEventData struct {
	Object json.RawMessage `json:"object"` // full Stripe-shaped PaymentIntent
}

// handleWebhookIn receives a gateway callback (e.g. a Midtrans notification),
// verifies it via the active adapter's ParseWebhook, applies the resulting status
// to the stored PaymentIntent (idempotently), and enqueues a Stripe-shaped event
// for outbound delivery. No Bearer auth — the gateway signature IS the auth.
//
// When the durable InboundStore is present (Postgres), the raw notification is
// persisted with a content-hash dedup key BEFORE the gateway is acknowledged: a
// duplicate delivery is a no-op, and the dedup claim + state application run in
// one transaction (no HTTP/provider call inside it).
func (s *Server) handleWebhookIn(w http.ResponseWriter, r *http.Request) {
	// Only the active gateway's callback path is valid.
	if r.PathValue("gateway") != s.gw.Name() {
		writeStripeError(w, http.StatusNotFound, "resource_missing", "no webhook route for that gateway.")
		return
	}

	// Buffer the raw body once: it is the durable dedup key and the forensic
	// payload. The adapter re-reads it to verify the signature.
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeStripeError(w, http.StatusBadRequest, "invalid_request_error", "Unable to read webhook body.")
		return
	}
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(raw))

	events, err := s.gw.ParseWebhook(r.Context(), r)
	if err != nil {
		if errors.Is(err, gateway.ErrInvalidSignature) || errors.Is(err, gateway.ErrStaleTimestamp) {
			writeStripeError(w, http.StatusUnauthorized, "authentication_required", "webhook signature verification failed.")
			return
		}
		writeStripeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	now := time.Now().Unix()
	if s.inbound != nil {
		s.handleWebhookInDurable(raw, events, now)
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}

	// Legacy in-memory path (tests/dev with *store.Memory).
	for _, ev := range events {
		s.dispatchEvent(ev, now)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// dispatchEvent is the legacy in-memory event router (subscription vs one-time).
func (s *Server) dispatchEvent(ev gateway.WebhookEvent, now int64) {
	switch {
	case ev.Kind == "subscription",
		strings.HasPrefix(ev.Type, "customer.subscription."),
		strings.HasPrefix(ev.Type, "invoice."):
		s.processSubscriptionEvent(ev, now)
	default:
		s.processEvent(ev, now)
	}
}

// handleWebhookInDurable persists the notification, dedups by content hash, and
// applies all events in the notification within one transaction. It always
// acknowledges (the event is durable); a row left 'processing' by a crash is
// finished by the reconciler.
func (s *Server) handleWebhookInDurable(raw []byte, events []gateway.WebhookEvent, now int64) {
	sum := sha256.Sum256(raw)
	accountID := s.inferAccount(events)
	parsed, _ := json.Marshal(events)
	inb := &store.InboundEvent{
		AccountID:       accountID,
		Gateway:         s.gw.Name(),
		ProviderEventID: firstEventField(events, func(e gateway.WebhookEvent) string { return e.ProviderEventID }),
		EventHash:       sum[:],
		EventType:       firstEventField(events, func(e gateway.WebhookEvent) string { return e.Type }),
		Reference:       firstEventField(events, func(e gateway.WebhookEvent) string { return e.Reference }),
		Raw:             raw,
		Parsed:          parsed,
	}
	var applyErr error
	// context.Background(): the event must persist even if the gateway disconnects
	// mid-request. No HTTP/provider call happens inside this transaction.
	if txErr := s.store.RunInTx(context.Background(), func(ctx context.Context) error {
		claim, err := s.inbound.ClaimInbound(ctx, inb)
		if err != nil {
			return err
		}
		if !claim.Owned {
			return nil // duplicate delivery — already applied
		}
		for _, ev := range events {
			if e := s.applyInboundEvent(ctx, ev, now); e != nil {
				applyErr = e
			}
		}
		errMsg := ""
		if applyErr != nil {
			errMsg = applyErr.Error()
		}
		return s.inbound.CompleteInbound(ctx, claim.Event.ID, claim.Event.ClaimToken, accountID, applyErr == nil, errMsg, inboundMaxAttempts)
	}); txErr != nil {
		log.Printf("inbound webhook tx failed (gateway=%s): %v", s.gw.Name(), txErr)
	}
}

// applyInboundEvent applies one parsed gateway event. It is transaction-aware
// (uses the ctx-bound tx when called from the request path) and idempotent (a
// repeat re-apply is a no-op once the status matches), so the reconciler reuses
// it safely. Only the PaymentIntent one-time path is durable in 5B; subscription/
// session/invoice application still runs through the in-memory path and graduates
// in 5D. The error is non-nil only for a genuine persistence failure; an unknown
// reference or an unchanged status is a quiet success.
func (s *Server) applyInboundEvent(ctx context.Context, ev gateway.WebhookEvent, now int64) error {
	if ev.Kind == "subscription" || strings.HasPrefix(ev.Type, "customer.subscription.") || strings.HasPrefix(ev.Type, "invoice.") {
		s.processSubscriptionEvent(ev, now)
		return nil
	}
	if s.payments == nil {
		s.processEvent(ev, now)
		return nil
	}
	pi, err := s.findIntentByRef(ev.Reference)
	if err != nil {
		return nil // unknown order — nothing to apply
	}
	cur, err := s.payments.ReadIntent(ctx, pi.AccountID, pi.ID)
	if err != nil {
		return nil
	}
	if cur.Status == string(ev.Status) {
		return nil // unchanged — idempotent skip
	}
	toStatus := string(ev.Status)
	failure := cur.FailureMessage
	switch {
	case ev.Type == "payment_intent.payment_failed":
		failure = "Payment failed; the charge was rejected by the gateway."
	case toStatus == string(stripe.PaymentIntentStatusSucceeded):
		failure = "" // a later success clears the prior error
	}
	updated, err := s.payments.SetIntentStatus(ctx, pi.AccountID, pi.ID, toStatus, failure)
	if err != nil {
		return err
	}
	if s.audit != nil {
		_ = s.audit.RecordTransition(ctx, &store.PaymentTransition{
			AccountID: pi.AccountID, PaymentIntentID: pi.ID,
			FromStatus: cur.Status, ToStatus: toStatus, Reason: "inbound:" + ev.Type,
		})
	}
	piJSON, _ := json.Marshal(toStripeResponse(updated))
	s.queueEvent(ctx, ev.Type, pi.ID, piJSON, now)

	// Checkout Session completion (session lives in memory until 5D).
	if toStatus == string(stripe.PaymentIntentStatusSucceeded) {
		if sess, err := s.store.GetSessionByPaymentIntent(pi.ID); err == nil {
			sess.Status = "complete"
			sess.PaymentStatus = "paid"
			s.store.PutSession(sess)
			sessJSON, _ := json.Marshal(toStripeCheckoutSession(sess))
			s.queueEvent(ctx, "checkout.session.completed", sess.ID, sessJSON, now)
		}
	}
	return nil
}

// inferAccount best-effort resolves the owning account from the first resolvable
// event reference. Returns "" when the referenced order is unknown at ingest.
func (s *Server) inferAccount(events []gateway.WebhookEvent) string {
	for _, ev := range events {
		if ev.Reference == "" {
			continue
		}
		if pi, err := s.findIntentByRef(ev.Reference); err == nil && pi.AccountID != "" {
			return pi.AccountID
		}
	}
	return ""
}

// findIntentByRef resolves an intent by its pi_ reference, falling back to the
// gateway's own transaction id. Unscoped: inbound callbacks carry no account.
func (s *Server) findIntentByRef(ref string) (*store.PaymentIntent, error) {
	if pi, err := s.store.Get(ref); err == nil {
		return pi, nil
	}
	return s.store.GetByGatewayReference(ref)
}

func firstEventField(events []gateway.WebhookEvent, get func(gateway.WebhookEvent) string) string {
	for _, ev := range events {
		if v := get(ev); v != "" {
			return v
		}
	}
	return ""
}

// processEvent applies a gateway event to the store and enqueues outbound
// delivery. It is a no-op for unknown orders and for status repetitions (which
// makes us robust to Midtrans re-sending the same notification).
func (s *Server) processEvent(ev gateway.WebhookEvent, now int64) {
	// Most gateways echo our pi_ reference; some (e.g. Mayar) emit their own id,
	// so fall back to a gateway-reference lookup.
	pi, err := s.store.Get(ev.Reference)
	if err != nil {
		pi, err = s.store.GetByGatewayReference(ev.Reference)
		if err != nil {
			// Unknown order: nothing to update or emit. Acknowledge upstream anyway.
			return
		}
	}
	if pi.Status == string(ev.Status) {
		return // unchanged — idempotent skip on gateway re-send
	}
	pi.Status = string(ev.Status)
	if ev.Type == "payment_intent.payment_failed" {
		// Gateways don't surface a structured decline reason, so the message is
		// generic; the field's presence lets clients guard against nil.
		pi.FailureMessage = "Payment failed; the charge was rejected by the gateway."
	} else if ev.Status == stripe.PaymentIntentStatusSucceeded {
		pi.FailureMessage = "" // a later successful payment clears the prior error
	}
	s.store.Put(pi)

	// PaymentIntent event (always emitted).
	piJSON, _ := json.Marshal(toStripeResponse(pi))
	s.enqueueEvent(ev.Type, pi.ID, piJSON, now)

	// If this intent belongs to a Checkout Session and was just paid, complete the
	// session and emit checkout.session.completed — the event frameworks watch for
	// to mark an order paid. (processEvent only runs on a status change, so this is
	// not duplicated on gateway re-sends.)
	if ev.Status == stripe.PaymentIntentStatusSucceeded {
		if sess, err := s.store.GetSessionByPaymentIntent(pi.ID); err == nil {
			sess.Status = "complete"
			sess.PaymentStatus = "paid"
			s.store.PutSession(sess)
			sessJSON, _ := json.Marshal(toStripeCheckoutSession(sess))
			s.enqueueEvent("checkout.session.completed", sess.ID, sessJSON, now)
		}
	}
}

// enqueueEvent is the legacy non-transactional entry point for outbound events
// (callers not running inside a state-change transaction).
func (s *Server) enqueueEvent(eventType, reference string, objJSON []byte, now int64) {
	s.queueEvent(context.Background(), eventType, reference, objJSON, now)
}

// queueEvent wraps any Stripe-shaped object JSON in an Event envelope and queues
// it for outbound delivery. When the durable OutboundStore is present, the enqueue
// participates in the caller's transaction (pass the tx-bound ctx so emission is
// atomic with the state change); otherwise it appends to the in-memory queue.
func (s *Server) queueEvent(ctx context.Context, eventType, reference string, objJSON []byte, now int64) {
	evt := &stripeEvent{
		ID:         "evt_" + randID(),
		Object:     "event",
		APIVersion: stripeAPIVersion,
		Created:    now,
		Type:       eventType,
		Data:       stripeEventData{Object: objJSON},
	}
	payload, _ := json.Marshal(evt)
	if s.outbound != nil {
		_ = s.outbound.EnqueueOutbound(ctx, &store.OutboundEvent{
			ID:        evt.ID,
			Type:      evt.Type,
			Reference: reference,
			Payload:   payload,
			Created:   now,
		})
		return
	}
	s.store.EnqueueWebhook(&store.WebhookDelivery{
		ID:        evt.ID,
		Type:      evt.Type,
		Reference: reference,
		Payload:   payload,
		Status:    store.DeliveryPending,
		Created:   now,
	})
}

// processSubscriptionEvent handles recurring (Kind="subscription") events:
// authorization completion (activation) and recurring-cycle success. It mirrors
// processEvent's idempotency — unknown references and repeats are no-ops. This
// path runs NO facade-side scheduler: every call is a reaction to an inbound
// gateway push, and the single follow-up ActivateSubscription call is triggered
// by the save-card webhook (the same push model as checkout.session.completed).
// The gateway owns the recurring clock.
func (s *Server) processSubscriptionEvent(ev gateway.WebhookEvent, now int64) {
	sg, ok := s.subscriptionGW()
	if !ok {
		return
	}
	switch {
	case ev.SavedToken != "":
		// The authorization save-card charge succeeded → register the schedule.
		s.activateSubscription(sg, ev, now)
	case ev.GatewaySubID != "" || ev.Type == "invoice.payment_succeeded":
		// A recurring cycle charged successfully → record a paid invoice.
		s.recordSubscriptionCycle(ev, now)
	}
}

// activateSubscription marks the authorization charge succeeded and registers
// the recurring schedule on the gateway using the saved token (one follow-up
// call, pushed by the save-card webhook). On success the subscription flips to
// active and the first invoice is paid; customer.subscription.updated and
// invoice.payment_succeeded are emitted.
func (s *Server) activateSubscription(sg gateway.SubscriptionGateway, ev gateway.WebhookEvent, now int64) {
	sub, err := s.store.GetSubscriptionByAuthPI(ev.Reference)
	if err != nil {
		return // unknown authorization — nothing to activate
	}
	// Idempotent re-apply guard: if the gateway schedule was already registered
	// (a prior run committed sub.GatewayID, possibly after a crash+reclaim), do NOT
	// call ActivateSubscription again — that would double-register the schedule.
	if sub.GatewayID != "" || sub.Status == string(stripe.SubscriptionStatusActive) {
		return
	}
	// The save-card charge is now succeeded.
	if pi, err := s.store.Get(ev.Reference); err == nil {
		pi.Status = string(stripe.PaymentIntentStatusSucceeded)
		pi.FailureMessage = ""
		s.store.Put(pi)
	}

	var cust *gateway.Customer
	if sub.CustomerID != "" {
		if c, err := s.store.GetCustomer(sub.CustomerID); err == nil {
			cust = &gateway.Customer{ID: c.ID, Email: c.Email, Name: c.Name, Phone: c.Phone}
		}
	}
	res, err := sg.ActivateSubscription(context.Background(), &gateway.ActivateSubscriptionInput{
		FacadeSubID:   sub.ID,
		SavedTokenID:  ev.SavedToken,
		AmountMinor:   sub.AmountMinor,
		Currency:      sub.Currency,
		Name:          subName(s, sub),
		Customer:      cust,
		Interval:      sub.Interval,
		IntervalCount: sub.IntervalCount,
	})
	if err != nil {
		// Activation failed: leave the subscription incomplete; no success event.
		return
	}
	sub.Status = string(stripe.SubscriptionStatusActive)
	sub.GatewayID = res.GatewayID
	if res.CurrentPeriodEnd != 0 {
		sub.CurrentPeriodEnd = res.CurrentPeriodEnd
	}
	s.store.PutSubscription(sub)

	// The first invoice is paid (the authorization charge covered it).
	inv, err := s.store.GetInvoice(sub.LatestInvoiceID)
	if err != nil {
		return
	}
	inv.Status = string(stripe.InvoiceStatusPaid)
	s.store.PutInvoice(inv)

	subJSON, _ := json.Marshal(s.toStripeSubscription(sub, false))
	s.enqueueEvent("customer.subscription.updated", sub.ID, subJSON, now)
	invJSON, _ := json.Marshal(toStripeInvoice(inv))
	s.enqueueEvent("invoice.payment_succeeded", inv.ID, invJSON, now)
}

// recordSubscriptionCycle records a paid invoice for a recurring-cycle
// notification, keyed idempotently on the cycle's order_id (a re-sent
// notification finds the invoice already recorded and is a no-op).
func (s *Server) recordSubscriptionCycle(ev gateway.WebhookEvent, now int64) {
	sub, err := s.store.GetSubscriptionByGatewayID(ev.GatewaySubID)
	if err != nil {
		return // unknown subscription — cannot correlate the cycle
	}
	if _, err := s.store.GetInvoiceByPI(ev.Reference); err == nil {
		return // already recorded — idempotent skip on gateway re-send
	}
	amount := ev.AmountMinor
	if amount == 0 {
		amount = sub.AmountMinor
	}
	inv := &store.Invoice{
		ID:              "in_" + randID(),
		AccountID:       sub.AccountID,
		SubscriptionID:  sub.ID,
		PaymentIntentID: ev.Reference,
		CustomerID:      sub.CustomerID,
		AmountMinor:     amount,
		Currency:        sub.Currency,
		Status:          string(stripe.InvoiceStatusPaid),
		BillingReason:   "subscription_cycle",
		Created:         now,
		Livemode:        sub.Livemode,
	}
	s.store.PutInvoice(inv)
	invJSON, _ := json.Marshal(toStripeInvoice(inv))
	s.enqueueEvent("invoice.payment_succeeded", inv.ID, invJSON, now)
}

// subName returns a display name for a subscription's recurring price.
func subName(s *Server, sub *store.Subscription) string {
	if price, err := s.store.GetPrice(sub.PriceID); err == nil {
		return productName(s, price)
	}
	return "Subscription"
}
