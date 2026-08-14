// Package mayar implements gateway.Gateway for Mayar.id, using the Headless API
// (a hosted payment link, used to create a payment and get a redirect URL). It is
// the ONLY Mayar-aware code. Endpoints and the verification pattern are verified
// against Mayar's official docs.
//
// Two BYO credentials are used: the API Key (Bearer auth for API calls) and a
// shared Webhook Token (constant-time compared against the ?token= query param
// Mayar echoes on each callback — Mayar does not sign payloads). Note that Mayar
// generates its own payment id and has no field for our pi_ reference, so its
// callbacks carry Mayar's id and are resolved to an intent via the store's
// gateway-reference index. The facade never touches money; it only translates.
//
// # Direct instrument issuance (v2 Headless API)
//
// The standalone QR endpoints — POST /hl/v1/qrcode/create and the v2
// /qr-codes/create — remain UNUSABLE: both accept only an amount and return only
// {url, amount}, with no transaction id and no reference field, so a QR issued
// through them cannot be correlated to its webhook (the customer pays, the order
// is never marked paid). They are deliberately NOT used here.
//
// The v2 single-payment endpoint, POST /hl/v2/payments/create, does not have
// that problem. Verified against the live API: it accepts a paymentMethod
// ("qris", "va/bsi", "ewallet/dana", ...) and returns data.id, data.transactionId
// and data.paymentLinkId — three correlation handles — exactly what this adapter
// needs to resolve payment.received callbacks through the store's reverse index.
// Direct issuance therefore goes through that endpoint (see instrument.go).
//
// Issuance is OPT-IN: Mayar's payment channels (QRIS, VA, e-wallet) must be
// enabled on the merchant dashboard first, and validation takes time. Until the
// operator lists the validated methods in MAYAR_INSTRUMENT_METHODS, SupportsInstrument
// reports false and callers fall back to the hosted link below — today's behavior
// unchanged. If a listed method is requested before its channel is actually live,
// Mayar returns 400 "Payment channel configuration not found", which IssueInstrument
// maps to ErrInstrumentUnsupported so the caller falls back to redirect rather
// than failing the checkout.
package mayar

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jawalab-com/payrouter/internal/gateway"
	stripe "github.com/stripe/stripe-go/v81"
)

// Default Mayar Headless API base URLs. Production (.id) and sandbox (.io).
const (
	ProdBaseURL    = "https://api.mayar.id"
	SandboxBaseURL = "https://api.mayar.io"

	// v2CreatePath is the Mayar Headless v2 single-payment endpoint. It serves
	// both the hosted link (CreatePayment, no paymentMethod) and direct instrument
	// issuance (IssueInstrument, with paymentMethod). Mayar is standardized on v2.
	v2CreatePath = "/hl/v2/payments/create"
)

// Adapter implements gateway.Gateway against Mayar.
type Adapter struct {
	apiKey       string // Bearer token for API calls
	webhookToken string // shared secret verified against ?token= on callbacks
	baseURL      string
	httpClient   *http.Client

	// instruments is the opt-in set of methods this adapter may issue directly
	// via the v2 API. Empty until EnableInstruments is called, which keeps direct
	// issuance off until the operator has validated the Mayar payment channels.
	instruments map[gateway.IDPaymentMethodType]bool
}

// New returns an Adapter for the given Mayar API Key and Webhook Token (both
// merchant-supplied, BYO). sandbox selects the sandbox host.
func New(apiKey, webhookToken string, sandbox bool) *Adapter {
	a := &Adapter{apiKey: apiKey, webhookToken: webhookToken, httpClient: http.DefaultClient}
	if sandbox {
		a.baseURL = SandboxBaseURL
	} else {
		a.baseURL = ProdBaseURL
	}
	return a
}

// newForTest injects a base URL and HTTP client so tests can use httptest.
func newForTest(apiKey, webhookToken, baseURL string, hc *http.Client) *Adapter {
	a := New(apiKey, webhookToken, true)
	if baseURL != "" {
		a.baseURL = baseURL
	}
	if hc != nil {
		a.httpClient = hc
	}
	return a
}

// SetBaseURL overrides the API base URL (empty ignored).
func (a *Adapter) SetBaseURL(base string) {
	if base != "" {
		a.baseURL = base
	}
}

// EnableInstruments opts the adapter into direct issuance for the given methods
// via the v2 API. It mirrors DOKU's EnableSNAP: non-fatal and additive. A method
// is only eligible for instrument routing once it is both passed here AND its
// payment channel is live on the Mayar dashboard (see the package doc comment).
func (a *Adapter) EnableInstruments(methods ...gateway.IDPaymentMethodType) {
	if a.instruments == nil {
		a.instruments = make(map[gateway.IDPaymentMethodType]bool, len(methods))
	}
	for _, m := range methods {
		a.instruments[m] = true
	}
}

// Name implements gateway.Gateway.
func (a *Adapter) Name() string { return "mayar" }

// CreatePayment creates a Mayar payment link (a hosted checkout page) via the v2
// API and returns its URL as the customer's next action. No paymentMethod is
// pinned, so the customer chooses a method on Mayar's page. Mayar generates its
// own payment id (data.id); our pi_ reference is not sent (Mayar has no
// external-id field), so the stored GatewayReference is Mayar's id and inbound
// callbacks resolve via the store's gateway-reference index.
func (a *Adapter) CreatePayment(ctx context.Context, in *gateway.CreatePaymentInput) (*gateway.PaymentResult, error) {
	if in == nil {
		return nil, errors.New("mayar: CreatePayment requires a reference")
	}
	if in.AmountMinor <= 0 {
		return nil, errors.New("mayar: CreatePayment requires a positive amount")
	}

	description := in.Description
	if description == "" {
		description = "Payment"
	}
	body := map[string]any{
		"amount":      in.AmountMinor, // IDR is zero-decimal: minor units == rupiah
		"redirectUrl": in.ReturnURL,
		"description": description,
		"expiredAt":   time.Now().Add(24 * time.Hour).Format(time.RFC3339), // Mayar requires an expiry
	}
	if in.Customer != nil {
		if in.Customer.Name != "" {
			body["name"] = in.Customer.Name
		}
		if in.Customer.Email != "" {
			body["email"] = in.Customer.Email
		}
		if in.Customer.Phone != "" {
			body["mobile"] = in.Customer.Phone
		}
	}

	var resp v2CreateResponse
	if err := a.doJSON(ctx, v2CreatePath, body, &resp); err != nil {
		return nil, err
	}

	return &gateway.PaymentResult{
		Reference:        in.Reference,
		GatewayReference: resp.Data.ID, // Mayar's payment request id; callbacks carry this
		Status:           stripe.PaymentIntentStatusRequiresAction,
		NextAction: &stripe.PaymentIntentNextAction{
			Type: stripe.PaymentIntentNextActionTypeRedirectToURL,
			RedirectToURL: &stripe.PaymentIntentNextActionRedirectToURL{
				URL:       resp.Data.Link,
				ReturnURL: in.ReturnURL,
			},
		},
		Raw: resp,
	}, nil
}

// GetStatus is not supported: Mayar's Headless API does not expose a payment
// status lookup; status arrives via the webhook callback.
func (a *Adapter) GetStatus(_ context.Context, _ string) (*gateway.PaymentResult, error) {
	return nil, errors.New("mayar: GetStatus not supported — Mayar delivers status via webhook; use ParseWebhook")
}

// Refund is not supported in this adapter version: Mayar's Headless API does not
// expose a refund endpoint.
func (a *Adapter) Refund(_ context.Context, _ *gateway.RefundInput) (*gateway.RefundResult, error) {
	return nil, errors.New("mayar: Refund not supported in this adapter version")
}

// ParseWebhook verifies the ?token= query param and translates a Mayar payment
// callback into a canonical Stripe-shaped event. Mayar's terminal payment event
// is "payment.received"; only a confirmed-paid callback (data.status == true)
// flips the intent to succeeded. No Bearer auth — the token IS the auth.
func (a *Adapter) ParseWebhook(_ context.Context, r *http.Request) ([]gateway.WebhookEvent, error) {
	if !verifyToken(r.URL.Query().Get("token"), a.webhookToken) {
		return nil, gateway.ErrInvalidSignature
	}

	rawBody, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("mayar: read webhook body: %w", err)
	}
	var wh callback
	if err := json.Unmarshal(rawBody, &wh); err != nil {
		return nil, fmt.Errorf("mayar: cannot parse webhook: %w", err)
	}

	// Only act on confirmed payment receipt; ignore reminders/membership events and
	// ambiguous (status != true) callbacks rather than risk a false failure.
	if wh.Event != "payment.received" || !wh.Data.Status {
		return nil, nil
	}

	status := stripe.PaymentIntentStatusSucceeded
	obj, _ := json.Marshal(stripePaymentIntentLike{
		ID:       wh.Data.ID,
		Object:   "payment_intent",
		Amount:   wh.Data.Amount,
		Currency: "idr",
		Status:   string(status),
	})
	return []gateway.WebhookEvent{{
		ProviderEventID: wh.Data.ID,
		Type:            stripeEventType(status),
		Reference:       wh.Data.ID, // Mayar's id -> resolved to an intent via the store reverse-index
		Status:          status,
		ObjectJSON:      obj,
	}}, nil
}

// --- HTTP plumbing ----------------------------------------------------------

// doJSON sends a Bearer-authenticated POST and decodes the JSON response.
func (a *Adapter) doJSON(ctx context.Context, target string, reqBody map[string]any, out any) error {
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("mayar: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+target, bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("mayar: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.apiKey)

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("mayar: call POST %s: %w", target, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("mayar: read response: %w", err)
	}
	if resp.StatusCode >= 400 {
		// Surface Mayar's own status/message so callers can branch on business
		// outcomes (e.g. IssueInstrument maps "channel configuration not found"
		// to ErrInstrumentUnsupported rather than a hard failure).
		var env struct {
			StatusCode int    `json:"statusCode"`
			Messages   string `json:"messages"`
		}
		_ = json.Unmarshal(raw, &env) // best-effort: envelope may be absent on transport errors
		return &apiError{
			target:  target,
			http:    resp.StatusCode,
			status:  env.StatusCode,
			message: env.Messages,
		}
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("mayar: decode response: %w", err)
		}
	}
	return nil
}

// --- Mayar request/response structs -----------------------------------------

// apiError carries Mayar's own envelope status alongside the HTTP status, so
// callers can distinguish business-level rejections (e.g. an unconfigured
// payment channel) from transport failures.
type apiError struct {
	target  string
	http    int
	status  int
	message string
}

func (e *apiError) Error() string {
	if e.message != "" {
		return fmt.Sprintf("mayar: POST %s returned HTTP %d (statusCode %d): %s", e.target, e.http, e.status, e.message)
	}
	return fmt.Sprintf("mayar: POST %s returned HTTP %d", e.target, e.http)
}

// channelUnavailable reports whether this error is Mayar signalling that the
// requested payment channel is not enabled — which means "fall back to redirect",
// not "fail the checkout".
func (e *apiError) channelUnavailable() bool {
	m := strings.ToLower(e.message)
	return strings.Contains(m, "not available or disabled") ||
		strings.Contains(m, "channel configuration not found") ||
		strings.Contains(m, "payment channel configuration not found")
}

// v2CreateResponse is the subset of Mayar's v2 /payments/create response we act
// on. The same endpoint serves hosted links (CreatePayment) and pinned-method
// issuance (IssueInstrument); the difference is whether paymentMethod was sent,
// which determines whether PaymentDetail is populated. PaymentDetail is kept raw
// because its shape varies by method — it is interpreted in instrument.go.
type v2CreateResponse struct {
	StatusCode int    `json:"statusCode"`
	Messages   string `json:"messages"`
	Data       struct {
		ID            string          `json:"id"`
		TransactionID string          `json:"transactionId"`
		PaymentLinkID string          `json:"paymentLinkId"`
		Link          string          `json:"link"`
		Amount        int64           `json:"amount"`
		Status        string          `json:"status"`
		ExpiredAt     string          `json:"expiredAt"`
		PaymentDetail json.RawMessage `json:"paymentDetail"`
	} `json:"data"`
}

// callback is the subset of Mayar's webhook payload we act on.
type callback struct {
	Event string `json:"event"`
	Data  struct {
		ID     string `json:"id"`
		Status bool   `json:"status"`
		Amount int64  `json:"amount"`
	} `json:"data"`
}

// stripePaymentIntentLike is a minimal Stripe-shaped object embedded in webhook
// events (expanded into the full PaymentIntent envelope by the M2 server layer).
type stripePaymentIntentLike struct {
	ID       string `json:"id"`
	Object   string `json:"object"`
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
	Status   string `json:"status"`
}

// stripeEventType maps a canonical status to the Stripe event type emitted on
// webhook translation.
func stripeEventType(status stripe.PaymentIntentStatus) string {
	switch status {
	case stripe.PaymentIntentStatusSucceeded:
		return "payment_intent.succeeded"
	case stripe.PaymentIntentStatusRequiresPaymentMethod:
		return "payment_intent.payment_failed"
	case stripe.PaymentIntentStatusCanceled:
		return "payment_intent.canceled"
	default:
		return "payment_intent.requires_action"
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
