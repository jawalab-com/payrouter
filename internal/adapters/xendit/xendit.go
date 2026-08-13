// Package xendit implements gateway.Gateway for Xendit, using the Invoice API
// (a hosted checkout page, used to create a payment and get a redirect URL) for
// create + status and the /refunds endpoint for refunds. It is the ONLY
// Xendit-aware code: it accepts Stripe-domain inputs from the facade, calls
// Xendit, and returns Stripe-domain results. Endpoints and the verification
// method are verified against Xendit's official docs — see plans.md §4.2.
//
// Two BYO credentials are used: the merchant Secret API Key (Basic auth for API
// calls) and the merchant Webhook Verification Token (constant-time compared
// against the x-callback-token header on inbound callbacks). Unlike Midtrans,
// Xendit has no separate sandbox host — the API key prefix selects the
// environment. The facade never touches money; it only translates.
package xendit

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/stripe-compatible-facade/internal/gateway"
	stripe "github.com/stripe/stripe-go/v81"
)

// Default Xendit API base. Same host serves development and production; the API
// key prefix (xnd_development_ / xnd_production_) selects the environment.
const (
	DefaultBaseURL = "https://api.xendit.co"

	invoicesPath = "/v2/invoices"
	refundsPath  = "/refunds"
)

// Adapter implements gateway.Gateway against Xendit.
type Adapter struct {
	secretKey    string // confidential; Basic auth for API calls
	webhookToken string // confidential; constant-time compared against x-callback-token
	baseURL      string
	httpClient   *http.Client
}

// New returns an Adapter for the given Xendit Secret API Key and Webhook
// Verification Token (both merchant-supplied, BYO).
func New(secretKey, webhookToken string) *Adapter {
	return &Adapter{
		secretKey:    secretKey,
		webhookToken: webhookToken,
		baseURL:      DefaultBaseURL,
		httpClient:   http.DefaultClient,
	}
}

// newForTest injects a base URL and HTTP client so tests can use httptest.
func newForTest(secretKey, webhookToken, baseURL string, hc *http.Client) *Adapter {
	a := New(secretKey, webhookToken)
	if baseURL != "" {
		a.baseURL = baseURL
	}
	if hc != nil {
		a.httpClient = hc
	}
	return a
}

// SetBaseURL overrides the API base URL (empty ignored). Lets operators point at a
// local or self-hosted Xendit-compatible endpoint.
func (a *Adapter) SetBaseURL(base string) {
	if base != "" {
		a.baseURL = base
	}
}

// Name implements gateway.Gateway.
func (a *Adapter) Name() string { return "xendit" }

// authHeader builds the Basic auth header Xendit expects: base64(SecretKey + ":").
func (a *Adapter) authHeader() string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(a.secretKey+":"))
}

// CreatePayment creates a Xendit Invoice (a hosted checkout page that shows every
// merchant-activated payment channel) and returns its URL as the customer's next
// action. The reference (pi_...) is carried as the Invoice external_id.
func (a *Adapter) CreatePayment(ctx context.Context, in *gateway.CreatePaymentInput) (*gateway.PaymentResult, error) {
	if in == nil || in.Reference == "" {
		return nil, errors.New("xendit: CreatePayment requires a reference (external_id)")
	}
	if in.AmountMinor <= 0 {
		return nil, errors.New("xendit: CreatePayment requires a positive amount")
	}

	body := map[string]any{
		"external_id": in.Reference,
		"amount":      in.AmountMinor, // IDR is zero-decimal: minor units == rupiah
	}
	if currency := strings.ToUpper(in.Currency); currency != "" {
		body["currency"] = currency
	}
	if in.Description != "" {
		body["description"] = truncate(in.Description, 1000)
	}
	if in.Customer != nil && in.Customer.Email != "" {
		body["payer_email"] = in.Customer.Email
	}
	if in.ReturnURL != "" {
		body["success_redirect_url"] = in.ReturnURL
	}

	var resp invoiceResponse
	if err := a.doJSON(ctx, http.MethodPost, a.baseURL+invoicesPath, body, &resp); err != nil {
		return nil, err
	}

	return &gateway.PaymentResult{
		Reference:        in.Reference,
		GatewayReference: resp.ID, // Xendit invoice id; used for status polling
		Status:           mapStatus(resp.Status),
		NextAction: &stripe.PaymentIntentNextAction{
			Type: stripe.PaymentIntentNextActionTypeRedirectToURL,
			RedirectToURL: &stripe.PaymentIntentNextActionRedirectToURL{
				URL:       resp.InvoiceURL,
				ReturnURL: in.ReturnURL,
			},
		},
		Raw: resp,
	}, nil
}

// GetStatus fetches the Invoice by its Xendit id (our stored GatewayReference).
func (a *Adapter) GetStatus(ctx context.Context, gatewayRef string) (*gateway.PaymentResult, error) {
	if gatewayRef == "" {
		return nil, errors.New("xendit: GetStatus requires a gateway reference (invoice id)")
	}
	var inv invoiceResponse
	if err := a.doJSON(ctx, http.MethodGet, a.baseURL+invoicesPath+"/"+gatewayRef, nil, &inv); err != nil {
		return nil, err
	}
	return &gateway.PaymentResult{
		Reference:        inv.ExternalID,
		GatewayReference: inv.ID,
		Status:           mapStatus(inv.Status),
		Raw:              inv,
	}, nil
}

// Refund issues a full (AmountMinor == 0) or partial refund via POST /refunds
// against the payment identified by PaymentReference.
func (a *Adapter) Refund(ctx context.Context, in *gateway.RefundInput) (*gateway.RefundResult, error) {
	if in == nil || in.PaymentReference == "" {
		return nil, errors.New("xendit: Refund requires a payment reference")
	}
	body := map[string]any{
		"payment_id": in.PaymentReference,
	}
	if in.AmountMinor > 0 {
		body["amount"] = in.AmountMinor
	}
	if in.Reference != "" {
		body["external_id"] = in.Reference
	}
	if in.Reason != "" {
		body["reason"] = in.Reason
	}

	var r refundResponse
	if err := a.doJSON(ctx, http.MethodPost, a.baseURL+refundsPath, body, &r); err != nil {
		return nil, err
	}

	amountMinor := in.AmountMinor
	if amountMinor == 0 {
		amountMinor = r.Amount // full refund: use the refunded amount from Xendit
	}
	return &gateway.RefundResult{
		Reference:        in.Reference,
		GatewayReference: r.ID,
		AmountMinor:      amountMinor,
		Status:           strings.ToLower(r.Status),
	}, nil
}

// ParseWebhook verifies the x-callback-token header (constant-time compared to the
// merchant Webhook Verification Token) and translates the Invoice callback into
// canonical Stripe-shaped events. Re-signing as Stripe-Signature and delivery
// happen at the server layer (M2). No Bearer auth — the callback token IS the auth.
func (a *Adapter) ParseWebhook(_ context.Context, r *http.Request) ([]gateway.WebhookEvent, error) {
	// Verify first: never trust an unsigned body.
	if !verifyCallbackToken(r.Header.Get("X-Callback-Token"), a.webhookToken) {
		return nil, gateway.ErrInvalidSignature
	}

	rawBody, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("xendit: read callback body: %w", err)
	}
	var cb invoiceCallback
	if err := json.Unmarshal(rawBody, &cb); err != nil {
		return nil, fmt.Errorf("xendit: cannot parse callback: %w", err)
	}

	status := mapStatus(cb.Status)
	obj, _ := json.Marshal(stripePaymentIntentLike{
		ID:       cb.ExternalID,
		Object:   "payment_intent",
		Amount:   amountValue(cb.Amount, cb.PaidAmount),
		Currency: "idr",
		Status:   string(status),
	})
	return []gateway.WebhookEvent{{
		ProviderEventID: cb.ID,
		Type:            stripeEventType(status),
		Reference:       cb.ExternalID,
		Status:          status,
		ObjectJSON:      obj,
	}}, nil
}

// --- HTTP plumbing ----------------------------------------------------------

// doJSON sends a JSON request and decodes the JSON response. A nil body sends no
// request body (used for GET). Non-2xx HTTP responses are surfaced as errors.
func (a *Adapter) doJSON(ctx context.Context, method, url string, body map[string]any, out any) error {
	var reqBody io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("xendit: marshal request: %w", err)
		}
		reqBody = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return fmt.Errorf("xendit: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", a.authHeader())

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("xendit: call %s %s: %w", method, url, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("xendit: read response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("xendit: %s %s returned HTTP %d", method, url, resp.StatusCode)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("xendit: decode response: %w", err)
		}
	}
	return nil
}

// --- Xendit request/response structs ----------------------------------------

type invoiceResponse struct {
	ID         string `json:"id"`
	ExternalID string `json:"external_id"`
	InvoiceURL string `json:"invoice_url"`
	Status     string `json:"status"`
	Amount     int64  `json:"amount"`
	PaidAmount int64  `json:"paid_amount"`
	ExpiryDate string `json:"expiry_date"`
}

type refundResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Amount int64  `json:"amount"`
}

// invoiceCallback is the subset of Xendit's Invoice payment callback we act on.
// Xendit sends many fields; we keep the ones needed to map state.
type invoiceCallback struct {
	ID         string `json:"id"`
	ExternalID string `json:"external_id"`
	Status     string `json:"status"`
	Amount     int64  `json:"amount"`
	PaidAmount int64  `json:"paid_amount"`
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

// --- Helpers ----------------------------------------------------------------

// amountValue returns the paid amount if present, else the invoice amount.
func amountValue(amount, paid int64) int64 {
	if paid > 0 {
		return paid
	}
	return amount
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
