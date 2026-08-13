package xendit

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jawalab-com/payrouter/internal/gateway"
	stripe "github.com/stripe/stripe-go/v81"
)

// Direct instrument issuance for Xendit.
//
// The Invoice API used by CreatePayment returns only a hosted redirect URL. The
// endpoints here instead ask Xendit to issue a specific instrument up front, so
// PayRouter can render the QR or VA number on its own checkout page:
//
//	QRIS            POST /qr_codes                  -> qr_string (raw EMVCo payload)
//	Virtual account POST /callback_virtual_accounts -> account_number
const (
	qrCodesPath = "/qr_codes"
	vaPath      = "/callback_virtual_accounts"

	// Xendit requires an explicit API version header on the QR Codes API.
	qrAPIVersion = "2022-07-31"

	// defaultInstrumentTTL bounds how long an issued instrument stays payable.
	// Xendit defaults QR codes to no expiry, which would leave stale QR codes
	// payable indefinitely; an explicit window keeps them reconcilable.
	defaultInstrumentTTL = 24 * time.Hour
)

// SupportsInstrument reports the methods this adapter can issue directly.
// E-wallets and retail outlets still go through the hosted Invoice page.
func (a *Adapter) SupportsInstrument(method gateway.IDPaymentMethodType) bool {
	switch method {
	case gateway.IDQRIS, gateway.IDVirtualAccount:
		return true
	default:
		return false
	}
}

// IssueInstrument creates a directly-renderable payment instrument. It returns
// gateway.ErrInstrumentUnsupported for methods Xendit cannot issue this way, so
// the caller can fall back to the hosted redirect.
func (a *Adapter) IssueInstrument(ctx context.Context, in *gateway.CreatePaymentInput) (*gateway.PaymentResult, error) {
	if in == nil || in.Reference == "" {
		return nil, errors.New("xendit: IssueInstrument requires a reference (external_id)")
	}
	if in.AmountMinor <= 0 {
		return nil, errors.New("xendit: IssueInstrument requires a positive amount")
	}

	switch in.PaymentMethodType {
	case gateway.IDQRIS:
		return a.issueQRIS(ctx, in)
	case gateway.IDVirtualAccount:
		return a.issueVirtualAccount(ctx, in)
	default:
		return nil, fmt.Errorf("xendit: %w: %s", gateway.ErrInstrumentUnsupported, in.PaymentMethodType)
	}
}

// issueQRIS creates a dynamic QRIS code. Xendit returns the raw EMVCo payload,
// so the QR can be rendered at any size and offered as copyable text — no
// dependence on a provider-hosted image.
func (a *Adapter) issueQRIS(ctx context.Context, in *gateway.CreatePaymentInput) (*gateway.PaymentResult, error) {
	expiry := time.Now().Add(defaultInstrumentTTL).UTC()
	body := map[string]any{
		"reference_id": in.Reference,
		"type":         "DYNAMIC", // amount fixed at creation, as opposed to a reusable STATIC code
		"currency":     currencyOr(in.Currency, "IDR"),
		"amount":       in.AmountMinor, // IDR is zero-decimal: minor units == rupiah
		"channel_code": "ID_DANA",      // Xendit's channel code for Indonesian QRIS
		"expires_at":   expiry.Format(time.RFC3339),
	}
	if in.Description != "" {
		body["description"] = truncate(in.Description, 1000)
	}

	var resp qrCodeResponse
	if err := a.doJSONWithHeaders(ctx, http.MethodPost, a.baseURL+qrCodesPath, body,
		map[string]string{"api-version": qrAPIVersion}, &resp); err != nil {
		return nil, err
	}
	if resp.QRString == "" {
		return nil, errors.New("xendit: QR code response contained no qr_string")
	}

	return &gateway.PaymentResult{
		Reference:        in.Reference,
		GatewayReference: resp.ID,
		Status:           stripe.PaymentIntentStatusRequiresAction,
		ExpiresAt:        parseUnix(resp.ExpiresAt, expiry),
		Display: &gateway.DisplayInstructions{
			Kind:      gateway.InstrumentQRIS,
			QRIS:      &gateway.QRISInstruction{Payload: resp.QRString},
			ExpiresAt: parseUnix(resp.ExpiresAt, expiry),
		},
		Raw: resp,
	}, nil
}

// issueVirtualAccount creates a closed VA fixed to this payment's amount, so a
// customer cannot underpay or overpay into it.
func (a *Adapter) issueVirtualAccount(ctx context.Context, in *gateway.CreatePaymentInput) (*gateway.PaymentResult, error) {
	bank := normalizeBank(in.MethodParams)
	if bank == "" {
		return nil, errors.New("xendit: virtual account requires a bank (method param \"bank\", e.g. bca)")
	}
	expiry := time.Now().Add(defaultInstrumentTTL).UTC()

	name := "PayRouter"
	if in.Customer != nil && in.Customer.Name != "" {
		name = truncate(in.Customer.Name, 255)
	}
	body := map[string]any{
		"external_id":     in.Reference,
		"bank_code":       strings.ToUpper(bank),
		"name":            name,
		"expected_amount": in.AmountMinor,
		// A closed VA accepts exactly expected_amount, and is single-use.
		"is_closed":       true,
		"is_single_use":   true,
		"expiration_date": expiry.Format(time.RFC3339),
	}

	var resp vaResponse
	if err := a.doJSONWithHeaders(ctx, http.MethodPost, a.baseURL+vaPath, body, nil, &resp); err != nil {
		return nil, err
	}
	if resp.AccountNumber == "" {
		return nil, errors.New("xendit: virtual account response contained no account_number")
	}

	return &gateway.PaymentResult{
		Reference:        in.Reference,
		GatewayReference: resp.ID,
		Status:           stripe.PaymentIntentStatusRequiresAction,
		ExpiresAt:        parseUnix(resp.ExpirationDate, expiry),
		Display: &gateway.DisplayInstructions{
			Kind: gateway.InstrumentVirtualAccount,
			VirtualAccount: &gateway.VirtualAccountInstruction{
				Bank:          strings.ToLower(resp.BankCode),
				AccountNumber: resp.AccountNumber,
				AccountName:   resp.Name,
			},
			ExpiresAt: parseUnix(resp.ExpirationDate, expiry),
		},
		Raw: resp,
	}, nil
}

// qrCodeResponse is the subset of Xendit's QR Codes API response we act on.
type qrCodeResponse struct {
	ID          string `json:"id"`
	ReferenceID string `json:"reference_id"`
	QRString    string `json:"qr_string"`
	Status      string `json:"status"`
	Amount      int64  `json:"amount"`
	ExpiresAt   string `json:"expires_at"`
}

// vaResponse is the subset of Xendit's Virtual Account API response we act on.
type vaResponse struct {
	ID             string `json:"id"`
	ExternalID     string `json:"external_id"`
	BankCode       string `json:"bank_code"`
	AccountNumber  string `json:"account_number"`
	Name           string `json:"name"`
	ExpectedAmount int64  `json:"expected_amount"`
	ExpirationDate string `json:"expiration_date"`
}

// normalizeBank extracts the bank code from method params, accepting the common
// spellings a caller might send.
func normalizeBank(params map[string]any) string {
	for _, key := range []string{"bank", "bank_code", "channel_code"} {
		if v, _ := params[key].(string); v != "" {
			return strings.ToLower(strings.TrimSpace(v))
		}
	}
	return ""
}

// currencyOr upper-cases the currency, substituting a default when empty.
func currencyOr(currency, def string) string {
	if c := strings.ToUpper(strings.TrimSpace(currency)); c != "" {
		return c
	}
	return def
}

// parseUnix converts an RFC3339 timestamp to unix seconds, falling back to the
// expiry we requested when the provider echoes nothing parseable.
func parseUnix(ts string, fallback time.Time) int64 {
	if ts != "" {
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			return t.Unix()
		}
	}
	return fallback.Unix()
}
