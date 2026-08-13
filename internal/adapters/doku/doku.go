// Package doku implements gateway.Gateway for DOKU, using the Checkout API (a
// hosted payment page, used to create a payment and get a redirect URL). It is
// the ONLY DOKU-aware code. Endpoints and the signature algorithm are verified
// against DOKU's official docs.
//
// DOKU authenticates every request with a per-call HMAC-SHA256 signature built
// from a canonical string of headers plus a base64 SHA-256 body digest. Two BYO
// credentials are used: the Client-Id (merchant identifier, sent in a header) and
// the Secret Key (the HMAC key, never sent). Inbound notifications are verified
// the same way, with the Request-Target set to the merchant Notification URL path
// we expose (/v1/webhooks/doku). The facade never touches money; it only translates.
package doku

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

// Default DOKU base URLs. Sandbox and production are separate hosts.
const (
	SandboxBaseURL = "https://api-sandbox.doku.com"
	ProdBaseURL    = "https://api.doku.com"

	checkoutPath     = "/checkout/v1/payment"
	notificationPath = "/v1/webhooks/doku" // Request-Target DOKU signs inbound notifications with
)

// Adapter implements gateway.Gateway against DOKU.
type Adapter struct {
	clientID        string // merchant Client-Id (sent in a header)
	secretKey       string // merchant Secret Key (HMAC key, never sent)
	baseURL         string
	notificationURL string // Request-Target used to verify inbound notifications
	httpClient      *http.Client
}

// New returns an Adapter for the given DOKU Client-Id and Secret Key (both
// merchant-supplied, BYO). sandbox selects the sandbox host.
func New(clientID, secretKey string, sandbox bool) *Adapter {
	a := &Adapter{
		clientID:        clientID,
		secretKey:       secretKey,
		notificationURL: notificationPath,
		httpClient:      http.DefaultClient,
	}
	if sandbox {
		a.baseURL = SandboxBaseURL
	} else {
		a.baseURL = ProdBaseURL
	}
	return a
}

// newForTest injects a base URL and HTTP client so tests can use httptest.
func newForTest(clientID, secretKey, baseURL string, hc *http.Client) *Adapter {
	a := New(clientID, secretKey, true)
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

// SetNotificationPath overrides the Request-Target used to verify inbound
// notifications, in case the merchant exposes the callback under a different path.
func (a *Adapter) SetNotificationPath(p string) {
	if p != "" {
		a.notificationURL = p
	}
}

// Name implements gateway.Gateway.
func (a *Adapter) Name() string { return "doku" }

// signedHeaders builds the per-call DOKU auth headers for a POST to target with
// the given JSON body: Client-Id, Request-Id, Request-Timestamp, Digest, Signature.
func (a *Adapter) signedHeaders(target string, body []byte) http.Header {
	reqID := uuidv4()
	ts := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	dgst := digest(body)
	h := http.Header{}
	h.Set("Client-Id", a.clientID)
	h.Set("Request-Id", reqID)
	h.Set("Request-Timestamp", ts)
	h.Set("Digest", dgst)
	h.Set("Signature", sign(canonical(a.clientID, reqID, ts, target, dgst), a.secretKey))
	return h
}

// CreatePayment creates a DOKU Checkout session (a hosted payment page) and
// returns its URL as the customer's next action. The reference (pi_...) is
// carried as the order invoice_number, which DOKU echoes back in notifications.
func (a *Adapter) CreatePayment(ctx context.Context, in *gateway.CreatePaymentInput) (*gateway.PaymentResult, error) {
	if in == nil || in.Reference == "" {
		return nil, errors.New("doku: CreatePayment requires a reference (invoice_number)")
	}
	if in.AmountMinor <= 0 {
		return nil, errors.New("doku: CreatePayment requires a positive amount")
	}

	currency := strings.ToUpper(in.Currency)
	if currency == "" {
		currency = "IDR"
	}
	order := map[string]any{
		"amount":         in.AmountMinor, // IDR is zero-decimal: minor units == rupiah
		"invoice_number": in.Reference,
		"currency":       currency,
	}
	if in.ReturnURL != "" {
		order["callback_url"] = in.ReturnURL
		order["auto_redirect"] = true
	}
	body := map[string]any{
		"order":   order,
		"payment": map[string]any{"payment_due_date": 60}, // minutes
	}
	if in.Customer != nil {
		c := map[string]any{}
		if in.Customer.Name != "" {
			c["name"] = in.Customer.Name
		}
		if in.Customer.Email != "" {
			c["email"] = in.Customer.Email
		}
		if in.Customer.Phone != "" {
			c["phone"] = in.Customer.Phone
		}
		if in.Customer.ID != "" {
			c["id"] = in.Customer.ID
		}
		if len(c) > 0 {
			body["customer"] = c
		}
	}

	var resp checkoutResponse
	if err := a.doJSON(ctx, checkoutPath, body, &resp); err != nil {
		return nil, err
	}

	return &gateway.PaymentResult{
		Reference:        in.Reference,
		GatewayReference: resp.Payment.TokenID, // DOKU checkout token
		Status:           stripe.PaymentIntentStatusRequiresAction,
		NextAction: &stripe.PaymentIntentNextAction{
			Type: stripe.PaymentIntentNextActionTypeRedirectToURL,
			RedirectToURL: &stripe.PaymentIntentNextActionRedirectToURL{
				URL:       resp.Payment.URL,
				ReturnURL: in.ReturnURL,
			},
		},
		Raw: resp,
	}, nil
}

// GetStatus is not supported: DOKU's Checkout flow is push-only — status arrives
// via the payment notification. Wire the Notification URL and use ParseWebhook.
func (a *Adapter) GetStatus(_ context.Context, _ string) (*gateway.PaymentResult, error) {
	return nil, errors.New("doku: GetStatus not supported — DOKU delivers status via push notification; use ParseWebhook")
}

// Refund is not supported in this adapter version: DOKU refunds are
// channel-specific and not part of the unified Checkout flow.
func (a *Adapter) Refund(_ context.Context, _ *gateway.RefundInput) (*gateway.RefundResult, error) {
	return nil, errors.New("doku: Refund not supported in this adapter version (DOKU refunds are channel-specific)")
}

// ParseWebhook verifies the DOKU Signature header (HMAC-SHA256 over the canonical
// header string + body digest, with the notification URL path as Request-Target)
// and translates the payment notification into canonical Stripe-shaped events.
// No Bearer auth — the signature IS the auth.
func (a *Adapter) ParseWebhook(_ context.Context, r *http.Request) ([]gateway.WebhookEvent, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("doku: read notification body: %w", err)
	}

	// Recompute the Digest from the received body (also detects tampering) and
	// verify the Signature over the canonical string DOKU signs for notifications.
	dgst := digest(body)
	if !verify(r.Header.Get("Signature"), r.Header.Get("Client-Id"), r.Header.Get("Request-Id"),
		r.Header.Get("Request-Timestamp"), a.notificationURL, dgst, a.secretKey) {
		return nil, gateway.ErrInvalidSignature
	}

	// Freshness check: reject notifications whose Request-Timestamp is outside a
	// ±5-minute window. The timestamp is cryptographically bound into the HMAC
	// above, so an attacker cannot forge a fresh one without the key — this guard
	// stops wholesale-replay of a captured valid notification.
	if ts := r.Header.Get("Request-Timestamp"); ts != "" {
		if parsed, err := time.Parse(time.RFC3339, ts); err == nil {
			if diff := time.Since(parsed); diff > 5*time.Minute || diff < -5*time.Minute {
				return nil, gateway.ErrStaleTimestamp
			}
		}
	}

	var n notification
	if err := json.Unmarshal(body, &n); err != nil {
		return nil, fmt.Errorf("doku: cannot parse notification: %w", err)
	}

	status := mapStatus(n.Transaction.Status)
	obj, _ := json.Marshal(stripePaymentIntentLike{
		ID:       n.Order.InvoiceNumber,
		Object:   "payment_intent",
		Amount:   n.Order.Amount,
		Currency: "idr",
		Status:   string(status),
	})
	return []gateway.WebhookEvent{{
		ProviderEventID: n.Order.InvoiceNumber,
		Type:            stripeEventType(status),
		Reference:       n.Order.InvoiceNumber,
		Status:          status,
		ObjectJSON:      obj,
	}}, nil
}

// --- HTTP plumbing ----------------------------------------------------------

// doJSON sends a signed POST and decodes the JSON response. Non-2xx responses are
// surfaced as errors.
func (a *Adapter) doJSON(ctx context.Context, target string, reqBody map[string]any, out any) error {
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("doku: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+target, bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("doku: build request: %w", err)
	}
	req.Header = a.signedHeaders(target, bodyBytes)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("doku: call POST %s: %w", target, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("doku: read response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("doku: POST %s returned HTTP %d", target, resp.StatusCode)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("doku: decode response: %w", err)
		}
	}
	return nil
}

// --- DOKU request/response structs ------------------------------------------

type checkoutResponse struct {
	Payment struct {
		URL     string `json:"url"`
		TokenID string `json:"token_id"`
		Expired string `json:"expired_date"`
	} `json:"payment"`
}

// notification is the subset of DOKU's payment notification we act on.
type notification struct {
	Order struct {
		InvoiceNumber string `json:"invoice_number"`
		Amount        int64  `json:"amount"`
	} `json:"order"`
	Transaction struct {
		Status string `json:"status"`
	} `json:"transaction"`
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

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
