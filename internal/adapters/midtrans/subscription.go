package midtrans

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jawalab-com/payrouter/internal/gateway"
	stripe "github.com/stripe/stripe-go/v81"
)

// This file implements gateway.SubscriptionGateway for Midtrans. The recurring
// CLOCK is owned by Midtrans (Core API /v1/subscriptions); the facade only
// starts authorization (a Snap save-card charge) and, once the saved token
// arrives via webhook, registers the schedule. No facade-side scheduler exists.
//
// Endpoints (Core API host = apiBase, Basic auth with the Server Key — same
// doJSON plumbing as one-time payments):
//   POST   /v1/subscriptions              create (ActivateSubscription)
//   POST   /v1/subscriptions/{id}/cancel  cancel (CancelSubscription)
// Authorization is a Snap transaction (snapBase + snapPath) with save_card:true.

// CreateSubscription starts authorization by creating a Snap transaction that
// vaults the customer's card (credit_card.save_card + user_id). The saved token
// arrives on the save-card webhook; until then the subscription is incomplete.
// The redirect URL is the customer's next action.
func (a *Adapter) CreateSubscription(ctx context.Context, in *gateway.CreateSubscriptionInput) (*gateway.SubscriptionResult, error) {
	if in == nil || in.Reference == "" {
		return nil, errors.New("midtrans: CreateSubscription requires a reference (order_id)")
	}
	if in.AmountMinor <= 0 {
		return nil, errors.New("midtrans: CreateSubscription requires a positive amount")
	}
	body := map[string]any{
		"transaction_details": map[string]any{
			"order_id":     in.Reference,
			"gross_amount": in.AmountMinor,
		},
		"credit_card": map[string]any{
			"secure":    true,
			"save_card": true, // vault the card → saved_token_id on the notification
		},
	}
	if in.Customer != nil {
		switch {
		case in.Customer.ID != "":
			body["user_id"] = in.Customer.ID // Midtrans scopes saved cards by user_id
		case in.Customer.Email != "":
			body["user_id"] = in.Customer.Email
		}
		if cd := customerDetails(in.Customer); cd != nil {
			body["customer_details"] = cd
		}
	}
	if in.Name != "" {
		body["item_details"] = []map[string]any{{
			"id":       "subscription",
			"name":     truncate(in.Name, 50),
			"price":    in.AmountMinor,
			"quantity": 1,
		}}
	}
	if in.ReturnURL != "" {
		body["callbacks"] = map[string]any{"finish_url": in.ReturnURL}
	}

	var resp snapResponse
	if err := a.doJSON(ctx, http.MethodPost, a.snapBase+snapPath, body, &resp); err != nil {
		return nil, err
	}
	return &gateway.SubscriptionResult{
		FacadeID:      in.Reference,
		AuthReference: in.Reference,
		Status:        stripe.SubscriptionStatusIncomplete,
		NextAction: &stripe.PaymentIntentNextAction{
			Type: stripe.PaymentIntentNextActionTypeRedirectToURL,
			RedirectToURL: &stripe.PaymentIntentNextActionRedirectToURL{
				URL:       resp.RedirectURL,
				ReturnURL: in.ReturnURL,
			},
		},
		Raw: resp,
	}, nil
}

// ActivateSubscription registers the recurring schedule on Midtrans using the
// saved token from the authorization webhook. Midtrans then fires every cycle.
func (a *Adapter) ActivateSubscription(ctx context.Context, in *gateway.ActivateSubscriptionInput) (*gateway.SubscriptionResult, error) {
	if in == nil || in.SavedTokenID == "" {
		return nil, errors.New("midtrans: ActivateSubscription requires a saved token")
	}
	interval, unit := midtransSchedule(in.Interval, in.IntervalCount)
	maxInterval := in.MaxInterval
	if maxInterval <= 0 {
		// Midtrans has no true infinite schedule; use a large cap. Recreate-on-
		// expiry is a future enhancement (documented in plans.md).
		maxInterval = 999
	}
	schedule := map[string]any{
		"interval":      interval,
		"interval_unit": unit,
		"max_interval":  maxInterval,
	}
	if in.StartTime != "" {
		schedule["start_time"] = in.StartTime
	}
	body := map[string]any{
		"name":         subscriptionName(in.Name, in.FacadeSubID),
		"amount":       strconv.FormatInt(in.AmountMinor, 10),
		"currency":     strings.ToUpper(currencyCode(in.Currency)),
		"payment_type": "credit_card",
		"token":        in.SavedTokenID,
		"schedule":     schedule,
	}
	if in.Customer != nil {
		if cd := customerDetails(in.Customer); cd != nil {
			body["customer_details"] = cd
		}
	}
	if in.FacadeSubID != "" {
		body["metadata"] = map[string]any{"PAYMENT_sub_id": in.FacadeSubID}
	}

	var resp subscriptionResponse
	if err := a.doJSON(ctx, http.MethodPost, a.apiBase+"/v1/subscriptions", body, &resp); err != nil {
		return nil, err
	}
	return &gateway.SubscriptionResult{
		FacadeID:         in.FacadeSubID,
		GatewayID:        resp.ID,
		Status:           mapSubStatus(resp.Status),
		CurrentPeriodEnd: parseMidtransTime(resp.Schedule.NextExecutionAt),
		Raw:              resp,
	}, nil
}

// CancelSubscription cancels the Midtrans subscription so future cycles don't charge.
func (a *Adapter) CancelSubscription(ctx context.Context, gatewaySubscriptionID string) (*gateway.SubscriptionResult, error) {
	if gatewaySubscriptionID == "" {
		return nil, errors.New("midtrans: CancelSubscription requires a subscription id")
	}
	var resp subscriptionResponse
	if err := a.doJSON(ctx, http.MethodPost, a.apiBase+"/v1/subscriptions/"+gatewaySubscriptionID+"/cancel", map[string]any{}, &resp); err != nil {
		return nil, err
	}
	return &gateway.SubscriptionResult{
		GatewayID: gatewaySubscriptionID,
		Status:    mapSubStatus(resp.Status),
		Raw:       resp,
	}, nil
}

// --- Subscription mapping helpers -------------------------------------------

// subscriptionResponse is the Midtrans /v1/subscriptions response.
type subscriptionResponse struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Schedule struct {
		NextExecutionAt string `json:"next_execution_at"`
	} `json:"schedule"`
}

// midtransSchedule maps a Stripe (interval, interval_count) pair onto Midtrans'
// (interval, interval_unit). Stripe "year" becomes 12×month (Midtrans has no year).
func midtransSchedule(interval string, count int64) (int64, string) {
	if count < 1 {
		count = 1
	}
	switch interval {
	case "day":
		return count, "day"
	case "week":
		return count, "week"
	case "year":
		return 12 * count, "month"
	default: // "month" and unknown → monthly
		return count, "month"
	}
}

// mapSubStatus maps a Midtrans subscription status onto a Stripe status.
func mapSubStatus(s string) stripe.SubscriptionStatus {
	switch strings.ToLower(s) {
	case "active":
		return stripe.SubscriptionStatusActive
	default:
		return stripe.SubscriptionStatusCanceled
	}
}

func subscriptionName(name, fallback string) string {
	if name != "" {
		return truncate(name, 255)
	}
	if fallback != "" {
		return fallback
	}
	return "Subscription"
}

func currencyCode(c string) string {
	if c == "" {
		return "IDR"
	}
	return c
}

// parseMidtransTime parses Midtrans' "2020-07-22 07:25:01 +0700" timestamp
// (or RFC3339) into unix seconds; 0 on failure.
func parseMidtransTime(s string) int64 {
	if s == "" {
		return 0
	}
	for _, layout := range []string{"2006-01-02 15:04:05 -0700", time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Unix()
		}
	}
	return 0
}
