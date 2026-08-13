// Package stub is a no-op Gateway used for the M0 scaffold and tests. It never
// calls a real payment gateway; it returns canned data so the HTTP layer can be
// developed and verified end-to-end before Midtrans is wired in (M1).
package stub

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/jawalab-com/payrouter/internal/gateway"
	stripe "github.com/stripe/stripe-go/v81"
)

// Gateway is a stub adapter that satisfies gateway.Gateway.
type Gateway struct{}

// New returns a stub Gateway.
func New() *Gateway { return &Gateway{} }

// Name returns the adapter identifier.
func (g *Gateway) Name() string { return "stub" }

// CreatePayment returns a canned requires_action result with a fake redirect URL.
func (g *Gateway) CreatePayment(_ context.Context, in *gateway.CreatePaymentInput) (*gateway.PaymentResult, error) {
	ref := fmt.Sprintf("stub_%s", in.Reference)
	return &gateway.PaymentResult{
		Reference:        in.Reference,
		GatewayReference: ref,
		Status:           stripe.PaymentIntentStatusRequiresAction,
		NextAction: &stripe.PaymentIntentNextAction{
			Type: "redirect_to_url",
			RedirectToURL: &stripe.PaymentIntentNextActionRedirectToURL{
				URL:       fmt.Sprintf("https://stub.local/pay/%s", ref),
				ReturnURL: in.ReturnURL,
			},
		},
	}, nil
}

// GetStatus returns a canned requires_action result.
func (g *Gateway) GetStatus(_ context.Context, gatewayRef string) (*gateway.PaymentResult, error) {
	return &gateway.PaymentResult{
		Reference:        gatewayRef,
		GatewayReference: gatewayRef,
		Status:           stripe.PaymentIntentStatusRequiresAction,
	}, nil
}

// Refund returns a canned succeeded refund.
func (g *Gateway) Refund(_ context.Context, in *gateway.RefundInput) (*gateway.RefundResult, error) {
	return &gateway.RefundResult{
		Reference:        in.Reference,
		GatewayReference: fmt.Sprintf("stub_%s", in.PaymentReference),
		AmountMinor:      in.AmountMinor,
		Status:           "succeeded",
	}, nil
}

// ParseWebhook is not implemented for the stub.
func (g *Gateway) ParseWebhook(_ context.Context, _ *http.Request) ([]gateway.WebhookEvent, error) {
	return nil, nil
}

// --- SubscriptionGateway (v2 recurring) -------------------------------------
//
// Canned subscription results so the server layer can be developed and tested
// without a live gateway. The stub never parses recurring webhooks; server-level
// recurring tests build gateway.WebhookEvent values directly (see surface_test.go).

// CreateSubscription returns a canned incomplete subscription with a redirect.
func (g *Gateway) CreateSubscription(_ context.Context, in *gateway.CreateSubscriptionInput) (*gateway.SubscriptionResult, error) {
	ref := in.Reference
	return &gateway.SubscriptionResult{
		FacadeID:      ref,
		AuthReference: ref,
		Status:        stripe.SubscriptionStatusIncomplete,
		NextAction: &stripe.PaymentIntentNextAction{
			Type: "redirect_to_url",
			RedirectToURL: &stripe.PaymentIntentNextActionRedirectToURL{
				URL:       fmt.Sprintf("https://stub.local/sub/%s", ref),
				ReturnURL: in.ReturnURL,
			},
		},
	}, nil
}

// ActivateSubscription returns a canned active subscription (the gateway owns the clock).
func (g *Gateway) ActivateSubscription(_ context.Context, in *gateway.ActivateSubscriptionInput) (*gateway.SubscriptionResult, error) {
	return &gateway.SubscriptionResult{
		FacadeID:         in.FacadeSubID,
		GatewayID:        fmt.Sprintf("stub-sub-%s", in.FacadeSubID),
		Status:           stripe.SubscriptionStatusActive,
		CurrentPeriodEnd: time.Now().Add(30 * 24 * time.Hour).Unix(),
	}, nil
}

// CancelSubscription returns a canned canceled subscription.
func (g *Gateway) CancelSubscription(_ context.Context, gatewaySubscriptionID string) (*gateway.SubscriptionResult, error) {
	return &gateway.SubscriptionResult{
		GatewayID: gatewaySubscriptionID,
		Status:    stripe.SubscriptionStatusCanceled,
	}, nil
}
