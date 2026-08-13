// Package midtrans implements gateway.Gateway for Midtrans, combining the Snap
// API (hosted payment page, used to create a transaction and get a redirect URL)
// with the Core API v2 (status + refund). It is the ONLY Midtrans-aware code:
// it accepts Stripe-domain inputs from the facade, calls Midtrans, and returns
// Stripe-domain results. Endpoints and the signature algorithm are verified
// against Midtrans' official docs.
//
// Sandbox vs production is selected by MIDTRANS_SANDBOX (true by default).
package midtrans

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/jawalab-com/payrouter/internal/gateway"
	stripe "github.com/stripe/stripe-go/v81"
)

// Default Midtrans base URLs. Snap and the Core API live on different hosts.
const (
	SnapSandboxURL = "https://app.sandbox.midtrans.com"
	SnapProdURL    = "https://app.midtrans.com"
	APISandboxURL  = "https://api.sandbox.midtrans.com"
	APIProdURL     = "https://api.midtrans.com"

	snapPath = "/snap/v1/transactions" // Snap create-transaction endpoint
)

// Adapter implements gateway.Gateway against Midtrans.
type Adapter struct {
	serverKey  string // confidential; used for Basic auth and signature verification
	snapBase   string // e.g. SnapSandboxURL
	apiBase    string // e.g. APISandboxURL
	httpClient *http.Client
}

// New returns an Adapter for the given Midtrans Server Key and environment.
func New(serverKey string, sandbox bool) *Adapter {
	a := &Adapter{serverKey: serverKey, httpClient: http.DefaultClient}
	if sandbox {
		a.snapBase, a.apiBase = SnapSandboxURL, APISandboxURL
	} else {
		a.snapBase, a.apiBase = SnapProdURL, APIProdURL
	}
	return a
}

// newForTest injects base URLs and an HTTP client so tests can use httptest.
func newForTest(serverKey, snapBase, apiBase string, hc *http.Client) *Adapter {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Adapter{serverKey: serverKey, snapBase: snapBase, apiBase: apiBase, httpClient: hc}
}

// SetBaseURLs overrides the Snap/Core API base URLs (empty values are ignored).
// Lets operators point at a local or self-hosted Midtrans-compatible endpoint.
func (a *Adapter) SetBaseURLs(snapURL, apiURL string) {
	if snapURL != "" {
		a.snapBase = snapURL
	}
	if apiURL != "" {
		a.apiBase = apiURL
	}
}

// Name implements gateway.Gateway.
func (a *Adapter) Name() string { return "midtrans" }

// authHeader builds the Basic auth header Midtrans expects: base64(ServerKey + ":").
func (a *Adapter) authHeader() string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(a.serverKey+":"))
}

// CreatePayment initiates a Snap transaction and returns the hosted redirect URL
// as the customer's next action. The reference (pi_...) is carried as Midtrans'
// order_id.
func (a *Adapter) CreatePayment(ctx context.Context, in *gateway.CreatePaymentInput) (*gateway.PaymentResult, error) {
	if in == nil || in.Reference == "" {
		return nil, errors.New("midtrans: CreatePayment requires a reference (order_id)")
	}
	if in.AmountMinor <= 0 {
		return nil, errors.New("midtrans: CreatePayment requires a positive amount")
	}

	body := map[string]any{
		"transaction_details": map[string]any{
			"order_id":     in.Reference,
			"gross_amount": in.AmountMinor, // IDR is zero-decimal: minor units == rupiah
		},
	}
	if eps := enabledPayments(in.PaymentMethodType, in.MethodParams); len(eps) > 0 {
		body["enabled_payments"] = eps
	}
	if cd := customerDetails(in.Customer); cd != nil {
		body["customer_details"] = cd
	}
	if in.Description != "" {
		body["item_details"] = []map[string]any{{
			"id":       "facade",
			"name":     truncate(in.Description, 50),
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

	return &gateway.PaymentResult{
		Reference:        in.Reference,
		GatewayReference: resp.Token, // Snap token; the real transaction_id is assigned once the customer picks a method
		Status:           stripe.PaymentIntentStatusRequiresAction,
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

// GetStatus fetches the Core API v2 status for an order_id (our pi_ reference).
func (a *Adapter) GetStatus(ctx context.Context, gatewayRef string) (*gateway.PaymentResult, error) {
	if gatewayRef == "" {
		return nil, errors.New("midtrans: GetStatus requires a gateway reference (order_id)")
	}
	var s statusResponse
	if err := a.doJSON(ctx, http.MethodGet, a.apiBase+"/v2/"+gatewayRef+"/status", nil, &s); err != nil {
		return nil, err
	}
	return &gateway.PaymentResult{
		Reference:        s.OrderID,
		GatewayReference: s.TransactionID,
		Status:           mapStatus(s.TransactionStatus, s.FraudStatus),
		Raw:              s,
	}, nil
}

// Refund issues a full (AmountMinor == 0) or partial refund via Core API v2.
func (a *Adapter) Refund(ctx context.Context, in *gateway.RefundInput) (*gateway.RefundResult, error) {
	if in == nil || in.PaymentReference == "" {
		return nil, errors.New("midtrans: Refund requires a payment reference (order_id)")
	}
	body := map[string]any{}
	if in.AmountMinor > 0 {
		body["amount"] = in.AmountMinor
	}
	if in.Reason != "" {
		body["reason"] = in.Reason
	}

	var r refundResponse
	if err := a.doJSON(ctx, http.MethodPost, a.apiBase+"/v2/"+in.PaymentReference+"/refund", body, &r); err != nil {
		return nil, err
	}

	amountMinor := in.AmountMinor
	if amountMinor == 0 {
		amountMinor = grossAmountMinor(r.GrossAmount) // full refund: use the original amount
	}
	st := "succeeded"
	if r.StatusCode != "" && r.StatusCode[0] != '2' {
		st = "failed"
	}
	return &gateway.RefundResult{
		Reference:        in.Reference,
		GatewayReference: r.TransactionID,
		AmountMinor:      amountMinor,
		Status:           st,
	}, nil
}

// ParseWebhook verifies the Midtrans signature_key and translates the
// notification into canonical Stripe-shaped events. Re-signing as
// Stripe-Signature and delivery happen at the server layer (M2).
func (a *Adapter) ParseWebhook(_ context.Context, r *http.Request) ([]gateway.WebhookEvent, error) {
	rawBody, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("midtrans: read notification body: %w", err)
	}
	var n notification
	if err := json.Unmarshal(rawBody, &n); err != nil {
		return nil, fmt.Errorf("midtrans: cannot parse notification: %w", err)
	}
	if !verifySignature(n.SignatureKey, n.OrderID, n.StatusCode, n.GrossAmount, a.serverKey) {
		return nil, gateway.ErrInvalidSignature
	}

	// Recurring notifications are distinguished by payload shape — the signature
	// scheme is identical for one-time and recurring, so one callback path serves
	// both. A save-card success carries saved_token_id (subscription activation
	// trigger); a recurring cycle carries subscription_id.
	if n.SavedTokenID != "" {
		return []gateway.WebhookEvent{{
			ProviderEventID: n.TransactionID,
			Type:            "customer.subscription.activating",
			Kind:            "subscription",
			Reference:       n.OrderID,
			SavedToken:      n.SavedTokenID,
			AmountMinor:     grossAmountMinor(n.GrossAmount),
		}}, nil
	}
	if n.SubscriptionID != "" {
		return []gateway.WebhookEvent{{
			ProviderEventID: n.TransactionID,
			Type:            "invoice.payment_succeeded",
			Kind:            "subscription",
			Reference:       n.OrderID,
			GatewaySubID:    n.SubscriptionID,
			AmountMinor:     grossAmountMinor(n.GrossAmount),
		}}, nil
	}

	status := mapStatus(n.TransactionStatus, n.FraudStatus)
	obj, _ := json.Marshal(stripePaymentIntentLike{
		ID:       n.OrderID,
		Object:   "payment_intent",
		Amount:   grossAmountMinor(n.GrossAmount),
		Currency: "idr",
		Status:   string(status),
	})
	return []gateway.WebhookEvent{{
		ProviderEventID: n.TransactionID,
		Type:            stripeEventType(status),
		Reference:       n.OrderID,
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
			return fmt.Errorf("midtrans: marshal request: %w", err)
		}
		reqBody = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return fmt.Errorf("midtrans: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", a.authHeader())

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("midtrans: call %s %s: %w", method, url, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("midtrans: read response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("midtrans: %s %s returned HTTP %d", method, url, resp.StatusCode)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("midtrans: decode response: %w", err)
		}
	}
	return nil
}

// --- Midtrans request/response structs --------------------------------------

type snapResponse struct {
	Token       string `json:"token"`
	RedirectURL string `json:"redirect_url"`
}

type statusResponse struct {
	StatusCode        string `json:"status_code"`
	TransactionID     string `json:"transaction_id"`
	OrderID           string `json:"order_id"`
	GrossAmount       string `json:"gross_amount"`
	TransactionStatus string `json:"transaction_status"`
	FraudStatus       string `json:"fraud_status"`
	PaymentType       string `json:"payment_type"`
}

type refundResponse struct {
	StatusCode        string `json:"status_code"`
	TransactionID     string `json:"transaction_id"`
	OrderID           string `json:"order_id"`
	GrossAmount       string `json:"gross_amount"`
	TransactionStatus string `json:"transaction_status"`
}

type notification struct {
	StatusCode        string `json:"status_code"`
	TransactionID     string `json:"transaction_id"`
	OrderID           string `json:"order_id"`
	GrossAmount       string `json:"gross_amount"`
	TransactionStatus string `json:"transaction_status"`
	FraudStatus       string `json:"fraud_status"`
	PaymentType       string `json:"payment_type"`
	SignatureKey      string `json:"signature_key"`
	SavedTokenID      string `json:"saved_token_id"`  // present on a save-card success → subscription activation
	SubscriptionID    string `json:"subscription_id"` // present on a recurring-cycle notification
}

// stripePaymentIntentLike is a minimal Stripe-shaped object embedded in webhook
// events. M2 will expand this into the full PaymentIntent envelope.
type stripePaymentIntentLike struct {
	ID       string `json:"id"`
	Object   string `json:"object"`
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
	Status   string `json:"status"`
}

// --- Mapping helpers --------------------------------------------------------

// enabledPayments maps the facade's IDPaymentMethodType (+ method params) onto
// Midtrans Snap "enabled_payments" codes. Snap expects specific codes per bank/
// wallet; "bank_transfer" is a Core API concept, not a Snap enabled_payment.
func enabledPayments(pm gateway.IDPaymentMethodType, params map[string]any) []string {
	switch pm {
	case gateway.IDVirtualAccount:
		if b, _ := params["bank"].(string); b != "" {
			if code, ok := vaCode(b); ok {
				return []string{code}
			}
		}
		return []string{"bca_va", "bni_va", "bri_va", "permata_va", "echannel"}
	case gateway.IDQRIS:
		return []string{"qris"}
	case gateway.IDEWallet:
		if w, _ := params["wallet"].(string); w != "" {
			return []string{strings.ToLower(w)} // gopay, shopeepay
		}
		return []string{"gopay"}
	case gateway.IDRetail:
		if s, _ := params["store"].(string); s != "" {
			return []string{strings.ToLower(s)} // alfamart, indomaret
		}
		return []string{"alfamart"}
	default:
		return nil // let Snap show all merchant-activated methods
	}
}

// vaCode maps a bank alias (from method params) to a Midtrans VA payment code.
func vaCode(bank string) (string, bool) {
	switch strings.ToLower(bank) {
	case "bca":
		return "bca_va", true
	case "bni":
		return "bni_va", true
	case "bri":
		return "bri_va", true
	case "permata":
		return "permata_va", true
	case "mandiri":
		return "echannel", true // Mandiri uses the echannel bill-payment flow
	}
	return "", false
}

// customerDetails builds the Midtrans customer_details object, or nil if empty.
func customerDetails(c *gateway.Customer) map[string]any {
	if c == nil {
		return nil
	}
	cd := map[string]any{}
	if c.Email != "" {
		cd["email"] = c.Email
	}
	if c.Phone != "" {
		cd["phone"] = c.Phone
	}
	if c.Name != "" {
		first, last := splitName(c.Name)
		cd["first_name"] = first
		if last != "" {
			cd["last_name"] = last
		}
	}
	if len(cd) == 0 {
		return nil
	}
	return cd
}

func splitName(name string) (string, string) {
	name = strings.TrimSpace(name)
	if i := strings.IndexByte(name, ' '); i >= 0 {
		return name[:i], strings.TrimSpace(name[i+1:])
	}
	return name, ""
}

// grossAmountMinor converts a Midtrans gross_amount string ("10000.00") to minor
// units. IDR is zero-decimal, so the integer part is already rupiah.
func grossAmountMinor(s string) int64 {
	if i := strings.IndexByte(s, '.'); i >= 0 {
		s = s[:i]
	}
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
