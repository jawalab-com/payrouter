// Package gateway defines the adapter seam between the Stripe-compatible facade
// and the underlying Indonesian payment gateways. It is the ONLY gateway-aware
// code; everything else speaks Stripe's domain.
package gateway

import (
	"context"
	"errors"
	"net/http"

	stripe "github.com/stripe/stripe-go/v81"
)

// ErrInvalidSignature is returned by ParseWebhook when the gateway callback's
// signature does not verify. Adapters should wrap or return it directly so the
// HTTP layer can distinguish a spoof/replay (HTTP 401) from malformed input.
var ErrInvalidSignature = errors.New("gateway: invalid webhook signature")

// ErrStaleTimestamp is returned when a gateway callback's timestamp is outside
// the acceptable freshness window. Mapped to HTTP 401 in the webhook handler.
var ErrStaleTimestamp = errors.New("gateway: stale webhook timestamp")

// Gateway is implemented once per payment processor. Inputs and outputs are
// expressed in Stripe-domain terms, so adapters are isolated and the rest of the
// system stays gateway-agnostic. Deliberately smaller than an orchestrator
// interface: no capture/void/mandate/3DS/routing in v1. Recurring billing is an
// OPTIONAL capability declared by the separate SubscriptionGateway interface
// below (implemented only by gateways that run the recurring clock themselves).
type Gateway interface {
	// Name returns the gateway identifier, e.g. "stub", "midtrans", "xendit".
	Name() string

	// CreatePayment initiates a one-time payment and returns the action the
	// customer must take to complete it (e.g. a redirect URL for VA/QRIS/ewallet).
	CreatePayment(ctx context.Context, in *CreatePaymentInput) (*PaymentResult, error)

	// GetStatus retrieves the current status of a payment by its gateway reference
	// (used for polling and reconciliation).
	GetStatus(ctx context.Context, gatewayRef string) (*PaymentResult, error)

	// Refund issues a full (AmountMinor == 0) or partial refund.
	Refund(ctx context.Context, in *RefundInput) (*RefundResult, error)

	// ParseWebhook verifies the gateway signature and translates its callback into
	// canonical (Stripe-shaped) events. It receives the full request so adapters
	// can read the body, headers, and (for gateways like Mayar) the query string
	// used for token verification. Wired up in M2.
	ParseWebhook(ctx context.Context, r *http.Request) ([]WebhookEvent, error)
}

// IDPaymentMethodType is our documented extension of Stripe's payment method
// types for Indonesian methods (see COMPATIBILITY.md).
type IDPaymentMethodType string

const (
	IDCard           IDPaymentMethodType = "id_card"
	IDVirtualAccount IDPaymentMethodType = "id_virtual_account"
	IDEWallet        IDPaymentMethodType = "id_ewallet"
	IDQRIS           IDPaymentMethodType = "id_qris"
	IDRetail         IDPaymentMethodType = "id_retail"
	IDHosted         IDPaymentMethodType = "id_hosted" // show all merchant-activated methods (Checkout/hosted pages)
)

// CreatePaymentInput carries a Stripe-shaped request down to the gateway.
type CreatePaymentInput struct {
	Reference         string // our PaymentIntent id (pi_...), carried as the gateway order_id/external_id
	AmountMinor       int64  // smallest currency unit (IDR is zero-decimal, i.e. whole rupiah)
	Currency          string // "idr" in v1
	Description       string
	Customer          *Customer
	PaymentMethodType IDPaymentMethodType
	MethodParams      map[string]any // e.g. {"bank":"bca"}, {"wallet":"gopay"}
	ReturnURL         string
}

// Customer is the minimal customer data passed to a gateway.
type Customer struct {
	ID    string
	Email string
	Name  string
	Phone string
}

// PaymentResult is the gateway's outcome, normalized to Stripe-domain terms.
// Status reuses stripe-go's canonical PaymentIntentStatus so values never drift
// from Stripe's.
type PaymentResult struct {
	Reference        string                          // pi_...
	GatewayReference string                          // gateway transaction id
	Status           stripe.PaymentIntentStatus      // StatusRequiresAction, StatusSucceeded, ...
	NextAction       *stripe.PaymentIntentNextAction // redirect_to_url for VA/QRIS/ewallet/hosted
	ExpiresAt        int64                           // unix seconds (VA/QRIS expire)
	Routing          *RoutingInfo                    // how the gateway was chosen; nil for direct (non-orchestrated) adapters
	Display          *DisplayInstructions            // instrument to render ourselves; nil for hosted-redirect results
	Raw              any                             // gateway payload, debugging only
}

// RoutingBasis records WHY a gateway was chosen, so callers never report a
// least-cost decision that did not happen. Reported verbatim as the
// payment_routing_mode metadata value.
type RoutingBasis string

const (
	// RoutingLeastCost means real fee data was found for the resolved channel and
	// the cheapest candidate won.
	RoutingLeastCost RoutingBasis = "least_cost"
	// RoutingFallbackPriority means no candidate had a fee entry for the channel
	// (or the channel could not be resolved at all), so the configured priority
	// order picked the gateway. NO cost comparison took place.
	RoutingFallbackPriority RoutingBasis = "fallback_priority"
	// RoutingStatic means a single gateway was configured directly; the
	// orchestrator was never consulted.
	RoutingStatic RoutingBasis = "static"
)

// RoutingInfo describes how the orchestrator selected a gateway for one payment.
// Channel is the resolved fee-table key ("qris", "virtual_account", ...) and is
// empty when the requested payment method type could not be mapped to one —
// notably IDHosted, where the method is not yet known at selection time.
type RoutingInfo struct {
	Gateway  string       // chosen gateway name
	Channel  string       // resolved fee-table channel; "" when unresolved
	Basis    RoutingBasis // why this gateway won
	FeeMinor float64      // computed fee in minor units; meaningful only when Basis is RoutingLeastCost
	Compared int          // number of candidates that produced a real fee quote
}

// --- Direct instrument issuance --------------------------------------------
//
// The Gateway interface above creates payments through each provider's HOSTED
// page (Midtrans Snap, Xendit Invoice, DOKU Checkout, Mayar payment link). Those
// endpoints return only a redirect URL: the customer chooses a method on the
// provider's site, and the instrument (VA number, QR payload) is issued there,
// after the fact. Nothing in a hosted response can be rendered by us.
//
// InstrumentGateway is the opposite: it asks the provider to issue a specific
// instrument up front so PayRouter can display it on its own page. That keeps
// checkout consistent no matter which gateway least-cost routing selects, and it
// requires the caller to have already chosen a payment method — which is also
// what lets the router price a real channel instead of falling back.
//
// It is an OPTIONAL capability, declared the same way as SubscriptionGateway:
// adapters that cannot issue an instrument for a method simply return
// ErrInstrumentUnsupported and the caller falls back to the hosted redirect.

// ErrInstrumentUnsupported is returned by IssueInstrument when an adapter cannot
// issue the requested method directly. It is not a failure — callers treat it as
// "fall back to the hosted checkout page for this one".
var ErrInstrumentUnsupported = errors.New("gateway: direct instrument issuance not supported for this method")

// InstrumentGateway is implemented by adapters that can issue a payment
// instrument directly, without sending the customer to a hosted page.
type InstrumentGateway interface {
	// IssueInstrument creates a payment and returns the instrument to display.
	// The payment method type in the input is REQUIRED and must be concrete
	// (IDQRIS, IDVirtualAccount, ...) — IDHosted is meaningless here.
	IssueInstrument(ctx context.Context, in *CreatePaymentInput) (*PaymentResult, error)

	// SupportsInstrument reports whether this adapter can issue the given method
	// directly, so a caller can filter candidates before attempting a charge.
	SupportsInstrument(method IDPaymentMethodType) bool
}

// InstrumentKind discriminates the populated variant of DisplayInstructions.
type InstrumentKind string

const (
	InstrumentQRIS           InstrumentKind = "qris"
	InstrumentVirtualAccount InstrumentKind = "virtual_account"
)

// DisplayInstructions carries everything needed to render a payment instrument
// on our own checkout page. Exactly one variant is populated, per Kind.
type DisplayInstructions struct {
	Kind           InstrumentKind
	QRIS           *QRISInstruction
	VirtualAccount *VirtualAccountInstruction
	ExpiresAt      int64 // unix seconds; 0 when the provider states no expiry
}

// QRISInstruction is a QRIS payment to display.
//
// Payload and ImageURL are BOTH optional individually but at least one is always
// set, because providers differ in what they hand back: Xendit and Midtrans
// return the raw EMVCo payload string (which we can render into a QR ourselves,
// at any size, and offer as copyable text), while Mayar returns only a URL to a
// pre-rendered image. Renderers must therefore prefer Payload and fall back to
// ImageURL rather than assuming either is present.
type QRISInstruction struct {
	Payload  string // raw EMVCo QR string, e.g. "00020101021226..."; "" if provider gives image only
	ImageURL string // provider-hosted QR image; "" when Payload is available
}

// VirtualAccountInstruction is a bank transfer destination to display.
type VirtualAccountInstruction struct {
	Bank          string // normalized lowercase bank code, e.g. "bca", "bni", "bri", "permata", "mandiri"
	AccountNumber string // the VA number the customer transfers to
	AccountName   string // display name on the account, when the provider supplies one
}

// RefundInput describes a refund to issue.
type RefundInput struct {
	Reference        string // our refund id (re_...)
	PaymentReference string // pi_... being refunded
	AmountMinor      int64  // 0 = full refund
	Reason           string
}

// RefundResult is the outcome of a refund.
type RefundResult struct {
	Reference        string
	GatewayReference string
	AmountMinor      int64
	Status           string // succeeded/failed/canceled (canonicalized to stripe.Charge refund status in M4)
}

// WebhookEvent is a single canonical event emitted from a gateway callback. The
// HTTP layer dispatches on Kind: "" (the zero value) is a one-time payment event
// — it applies Status to the stored PaymentIntent; "subscription" is a recurring
// event (authorization/activation or a cycle), handled by the subscription path.
// Both kinds are then wrapped in a Stripe Event envelope and re-signed for
// outbound delivery (M2). Existing one-time ParseWebhook output leaves the new
// fields zero, so it routes to the PaymentIntent path unchanged.
type WebhookEvent struct {
	ProviderEventID string                     // stable provider transaction/delivery id for durable deduplication
	Type            string                     // Stripe event type, e.g. "payment_intent.succeeded"
	Reference       string                     // pi_ / sub_ / in_ ...
	Kind            string                     // "" (=payment_intent) | "subscription" — dispatcher discriminator
	Status          stripe.PaymentIntentStatus // Kind=="": applied to the PaymentIntent
	SubStatus       stripe.SubscriptionStatus  // Kind=="subscription": applied to the Subscription
	SavedToken      string                     // gateway saved_token_id (subscription activation trigger)
	GatewaySubID    string                     // gateway subscription id (recurring-cycle correlation)
	AmountMinor     int64                      // invoice amount on a subscription cycle/activation
	ObjectJSON      []byte                     // raw Stripe-shaped object payload (fallback/seed)
}

// --- Recurring billing (v2) ------------------------------------------------
//
// Subscriptions are viable as pure translation ONLY when the gateway runs the
// recurring clock itself (gateway-side auto-debit). The facade never schedules a
// charge. SubscriptionGateway is an optional capability: adapters that don't
// support recurring simply omit it, and the server returns a "not supported"
// Stripe error (mirroring how DOKU/Mayar decline refunds today).

// SubscriptionGateway is implemented by adapters whose gateway owns the recurring
// clock. CreateSubscription starts authorization (a hosted save-card page);
// ActivateSubscription registers the schedule once the saved token arrives via
// webhook; the gateway then fires every cycle. No facade-side scheduler exists.
type SubscriptionGateway interface {
	// CreateSubscription starts customer authorization: it creates the gateway's
	// save-card / hosted-registration page and returns a redirect (the subscription
	// is incomplete until the customer authorizes). No recurring schedule is
	// registered yet — the saved token is unavailable until the hosted page is
	// completed (verified vs Midtrans docs: subscriptions need a saved_token_id).
	CreateSubscription(ctx context.Context, in *CreateSubscriptionInput) (*SubscriptionResult, error)

	// ActivateSubscription registers the recurring schedule on the gateway using
	// the saved token from the authorization webhook. THE GATEWAY OWNS THE CLOCK
	// from here on; the facade never schedules a charge.
	ActivateSubscription(ctx context.Context, in *ActivateSubscriptionInput) (*SubscriptionResult, error)

	// CancelSubscription cancels the gateway subscription so the customer is not
	// charged in future cycles.
	CancelSubscription(ctx context.Context, gatewaySubscriptionID string) (*SubscriptionResult, error)
}

// CreateSubscriptionInput starts authorization. Reference is the facade's auth
// PaymentIntent id (pi_...), carried as the gateway order_id so the save-card
// webhook can correlate back to the pending subscription.
type CreateSubscriptionInput struct {
	Reference     string // pi_... (authorization charge), carried as the gateway order_id
	AmountMinor   int64  // first-cycle (authorization) amount; IDR whole rupiah
	Currency      string // "idr"
	Name          string // subscription display name
	Customer      *Customer
	Interval      string // Stripe interval: day | week | month | year
	IntervalCount int64  // 1 default
	ReturnURL     string // success_url → hosted finish_url
}

// ActivateSubscriptionInput registers the recurring schedule once the saved
// token is in hand. The gateway runs all subsequent cycles.
type ActivateSubscriptionInput struct {
	FacadeSubID   string // sub_... (carried in gateway metadata for traceability)
	SavedTokenID  string // from the authorization webhook
	AmountMinor   int64
	Currency      string
	Name          string
	Customer      *Customer
	Interval      string
	IntervalCount int64
	StartTime     string // ISO8601 +offset (Midtrans start_time); "" = now
	MaxInterval   int64  // cap on cycle count (Midtrans has no true infinite)
}

// SubscriptionResult is the gateway's subscription outcome, normalized to
// Stripe-domain terms. Status reuses stripe-go's SubscriptionStatus so values
// never drift from Stripe's.
type SubscriptionResult struct {
	FacadeID         string                          // sub_... we generated
	GatewayID        string                          // gateway subscription id (empty until activated)
	AuthReference    string                          // pi_... of the authorization charge (create phase)
	Status           stripe.SubscriptionStatus       // Incomplete (create) | Active (activate) | Canceled
	NextAction       *stripe.PaymentIntentNextAction // redirect for the authorization page (create phase)
	CurrentPeriodEnd int64                           // unix seconds (from the gateway schedule)
	AmountMinor      int64
	Raw              any
}
