package mayar

// Direct instrument issuance for Mayar via the v2 Headless API.
//
// CreatePayment uses the same v2 /payments/create endpoint WITHOUT a paymentMethod,
// returning only a hosted link. The methods here instead pin a paymentMethod up
// front, so Mayar returns the instrument embedded in data.paymentDetail and
// PayRouter can render the QR or VA number on its own checkout page — the same UX
// Xendit and Midtrans already provide.
//
// Endpoint: POST /hl/v2/payments/create
//   paymentMethod: "<type>/<CODE>" — qris/QRIS, va/MANDIRI, ewallet/DANA, ...
//
// The paymentDetail shape below is taken from Mayar's dashboard guide (the formal
// reference omits it entirely). Verified shapes:
//
//	QRIS            data.paymentDetail.type == "QR_CODE"
//	                .qr_code.channel_properties.qr_string  -> raw EMVCo payload
//	Virtual account data.paymentDetail.type == "VIRTUAL_ACCOUNT"
//	                .virtual_account.channel_properties.virtual_account_number
//	                .virtual_account.channel_code          -> bank (MANDIRI, ...)
//
// E-wallets (type "EWALLET") are NOT issued here: they return only AUTH redirect
// URLs (web + mobile deeplink), not a self-renderable instrument, so they stay on
// the hosted link. Cards/retail/paylater channels are similarly hosted-only.
//
// Issuance is OPT-IN via EnableInstruments (MAYAR_INSTRUMENT_METHODS): until the
// operator validates a channel on the Mayar dashboard, SupportsInstrument reports
// false and callers fall back to the hosted link. If a listed method is requested
// before its channel is actually live, Mayar returns 400, which IssueInstrument
// maps to ErrInstrumentUnsupported so the checkout falls back to redirect.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jawalab-com/payrouter/internal/gateway"
	stripe "github.com/stripe/stripe-go/v81"
)

// defaultInstrumentTTL bounds how long an issued instrument stays payable. Mayar
// applies its own default expiry; we send an explicit one so instruments are
// reconcilable rather than open-ended.
const defaultInstrumentTTL = 24 * time.Hour

// mayarVABanks maps our normalized lowercase bank code to Mayar's uppercase VA
// channel code. Mayar does NOT support bca (it 400s); callers asking for bca get
// ErrInstrumentUnsupported so another gateway handles it. FLIP and "other" are
// Mayar-specific fallback channels.
var mayarVABanks = map[string]string{
	"mandiri": "MANDIRI",
	"bni":     "BNI",
	"bsi":     "BSI",
	"bri":     "BRI",
	"bjb":     "BJB",
	"cimb":    "CIMB",
	"permata": "PERMATA",
	"flip":    "FLIP",
	"other":   "other",
}

// SupportsInstrument reports the methods this adapter can issue directly. It is
// gated by EnableInstruments (driven by MAYAR_INSTRUMENT_METHODS): until the
// operator has validated a channel on the Mayar dashboard it reports false, so
// the orchestrator never selects Mayar for that method and today's hosted-redirect
// behavior is unchanged.
func (a *Adapter) SupportsInstrument(method gateway.IDPaymentMethodType) bool {
	switch method {
	case gateway.IDQRIS, gateway.IDVirtualAccount:
		return a.instruments[method]
	default:
		// E-wallets, cards and retail outlets stay on the hosted payment link.
		return false
	}
}

// IssueInstrument creates a directly-renderable payment instrument via the v2
// API. It returns gateway.ErrInstrumentUnsupported for methods Mayar cannot
// issue this way (or that are not enabled), so the caller can fall back to the
// hosted redirect.
func (a *Adapter) IssueInstrument(ctx context.Context, in *gateway.CreatePaymentInput) (*gateway.PaymentResult, error) {
	if in == nil || in.Reference == "" {
		return nil, errors.New("mayar: IssueInstrument requires a reference")
	}
	if in.AmountMinor <= 0 {
		return nil, errors.New("mayar: IssueInstrument requires a positive amount")
	}

	switch in.PaymentMethodType {
	case gateway.IDQRIS:
		if !a.instruments[gateway.IDQRIS] {
			return nil, fmt.Errorf("mayar: %w: qris not enabled (set MAYAR_INSTRUMENT_METHODS)", gateway.ErrInstrumentUnsupported)
		}
		return a.issueQRIS(ctx, in)
	case gateway.IDVirtualAccount:
		if !a.instruments[gateway.IDVirtualAccount] {
			return nil, fmt.Errorf("mayar: %w: virtual_account not enabled (set MAYAR_INSTRUMENT_METHODS)", gateway.ErrInstrumentUnsupported)
		}
		return a.issueVirtualAccount(ctx, in)
	default:
		return nil, fmt.Errorf("mayar: %w: %s", gateway.ErrInstrumentUnsupported, in.PaymentMethodType)
	}
}

// issueQRIS pins paymentMethod=qris/QRIS and renders the raw EMVCo payload Mayar
// returns at paymentDetail.qr_code.channel_properties.qr_string. Mayar hands back
// the raw payload (not just an image), so the QR can be rendered at any size and
// offered as copyable text.
func (a *Adapter) issueQRIS(ctx context.Context, in *gateway.CreatePaymentInput) (*gateway.PaymentResult, error) {
	resp, err := a.createV2(ctx, in, "qris/QRIS")
	if err != nil {
		return nil, err
	}

	pd, ok := parsePaymentDetail(resp.Data.PaymentDetail)
	if !ok || pd.QRCode == nil || pd.QRCode.ChannelProperties.QRString == "" {
		// No renderable QR. The invoice may still exist (Mayar returns 200 with an
		// incomplete paymentDetail for misconfigured channels), but we cannot render
		// it on our page — fall back to the hosted link rather than stranding the
		// customer.
		return nil, fmt.Errorf("mayar: %w: qris response carried no qr_string in paymentDetail", gateway.ErrInstrumentUnsupported)
	}

	expires := expiryUnix(resp.Data.ExpiredAt)
	return &gateway.PaymentResult{
		Reference:        in.Reference,
		GatewayReference: resp.Data.ID,
		Status:           stripe.PaymentIntentStatusRequiresAction,
		ExpiresAt:        expires,
		Display: &gateway.DisplayInstructions{
			Kind:      gateway.InstrumentQRIS,
			QRIS:      &gateway.QRISInstruction{Payload: pd.QRCode.ChannelProperties.QRString},
			ExpiresAt: expires,
		},
		Raw: resp,
	}, nil
}

// issueVirtualAccount pins paymentMethod=va/<CODE> and renders the VA number Mayar
// assigns. The bank comes from the response's channel_code (authoritative),
// falling back to the bank the caller requested.
func (a *Adapter) issueVirtualAccount(ctx context.Context, in *gateway.CreatePaymentInput) (*gateway.PaymentResult, error) {
	bank := normalizeBank(in.MethodParams)
	if bank == "" {
		return nil, errors.New("mayar: virtual account requires a bank (method param \"bank\", e.g. bsi, bni, mandiri)")
	}
	code, ok := mayarVABanks[bank]
	if !ok {
		return nil, fmt.Errorf("mayar: %w: Mayar does not issue virtual accounts for bank %q (supported: mandiri, bni, bsi, bri, bjb, cimb, permata, flip)",
			gateway.ErrInstrumentUnsupported, bank)
	}

	resp, err := a.createV2(ctx, in, "va/"+code)
	if err != nil {
		return nil, err
	}

	pd, ok := parsePaymentDetail(resp.Data.PaymentDetail)
	if !ok || pd.VirtualAccount == nil || pd.VirtualAccount.ChannelProperties.VirtualAccountNumber == "" {
		return nil, fmt.Errorf("mayar: %w: va response carried no account number in paymentDetail", gateway.ErrInstrumentUnsupported)
	}

	// channel_code is authoritative; fall back to the requested bank when absent.
	resolvedBank := strings.ToLower(pd.VirtualAccount.ChannelCode)
	if resolvedBank == "" {
		resolvedBank = bank
	}
	expires := expiryUnix(pd.VirtualAccount.ChannelProperties.ExpiresAt, resp.Data.ExpiredAt)

	return &gateway.PaymentResult{
		Reference:        in.Reference,
		GatewayReference: resp.Data.ID,
		Status:           stripe.PaymentIntentStatusRequiresAction,
		ExpiresAt:        expires,
		Display: &gateway.DisplayInstructions{
			Kind: gateway.InstrumentVirtualAccount,
			VirtualAccount: &gateway.VirtualAccountInstruction{
				Bank:          resolvedBank,
				AccountNumber: pd.VirtualAccount.ChannelProperties.VirtualAccountNumber,
				AccountName:   pd.VirtualAccount.ChannelProperties.CustomerName,
			},
			ExpiresAt: expires,
		},
		Raw: resp,
	}, nil
}

// createV2 is the shared v2 create call for a pinned paymentMethod. A "payment
// channel not configured" rejection is mapped to ErrInstrumentUnsupported so the
// caller falls back to the hosted redirect rather than failing the checkout —
// this is the safety net for the gap between MAYAR_INSTRUMENT_METHODS (our flag)
// and the channel actually being live on the Mayar dashboard.
func (a *Adapter) createV2(ctx context.Context, in *gateway.CreatePaymentInput, paymentMethod string) (*v2CreateResponse, error) {
	body := map[string]any{
		"name":          instrumentName(in),
		"amount":        in.AmountMinor, // IDR is zero-decimal: minor units == rupiah
		"description":   descriptionOr(in.Description, "Payment"),
		"paymentMethod": paymentMethod,
		"expiredAt":     time.Now().Add(defaultInstrumentTTL).UTC().Format(time.RFC3339),
	}
	if in.Customer != nil {
		// email is REQUIRED: without it Mayar creates no transaction and silently
		// ignores paymentMethod (per the dashboard guide).
		if in.Customer.Email != "" {
			body["email"] = in.Customer.Email
		}
		if in.Customer.Phone != "" {
			body["mobile"] = in.Customer.Phone
		}
		if in.Customer.Name != "" {
			body["name"] = in.Customer.Name
		}
	}

	var resp v2CreateResponse
	if err := a.doJSON(ctx, v2CreatePath, body, &resp); err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.channelUnavailable() {
			return nil, fmt.Errorf("mayar: %w: %s channel not enabled on the Mayar dashboard", gateway.ErrInstrumentUnsupported, paymentMethod)
		}
		return nil, err
	}
	if resp.Data.ID == "" {
		return nil, fmt.Errorf("mayar: v2 create returned no correlatable id for %s", paymentMethod)
	}
	return &resp, nil
}

// instrumentName picks a payer name for the request, defaulting to a stable label
// when the customer is anonymous.
func instrumentName(in *gateway.CreatePaymentInput) string {
	if in.Customer != nil && in.Customer.Name != "" {
		return in.Customer.Name
	}
	return "PayRouter"
}

// paymentDetail is the typed view of data.paymentDetail. Only the QR and VA
// variants are consumed; e-wallet actions are ignored (e-wallets stay hosted).
type paymentDetail struct {
	ID     string `json:"id"`
	Type   string `json:"type"` // QR_CODE | VIRTUAL_ACCOUNT | EWALLET
	QRCode *struct {
		ChannelProperties struct {
			QRString string `json:"qr_string"`
		} `json:"channel_properties"`
	} `json:"qr_code,omitempty"`
	VirtualAccount *struct {
		ChannelCode       string `json:"channel_code"`
		ChannelProperties struct {
			CustomerName         string `json:"customer_name"`
			VirtualAccountNumber string `json:"virtual_account_number"`
			ExpiresAt            string `json:"expires_at"`
		} `json:"channel_properties"`
	} `json:"virtual_account,omitempty"`
	Actions []struct {
		Action  string `json:"action"`
		URLType string `json:"url_type"`
		URL     string `json:"url"`
	} `json:"actions"`
}

// parsePaymentDetail decodes the raw paymentDetail, returning ok=false when it is
// absent or malformed (callers treat that as "nothing to render -> redirect").
func parsePaymentDetail(raw json.RawMessage) (paymentDetail, bool) {
	var pd paymentDetail
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return pd, false
	}
	if err := json.Unmarshal(raw, &pd); err != nil {
		return pd, false
	}
	return pd, true
}

// normalizeBank extracts the bank code from method params, accepting the common
// spellings a caller might send, lowercased.
func normalizeBank(params map[string]any) string {
	for _, key := range []string{"bank", "bank_code", "channel_code"} {
		if v, _ := params[key].(string); v != "" {
			return strings.ToLower(strings.TrimSpace(v))
		}
	}
	return ""
}

// descriptionOr substitutes a default for an empty description.
func descriptionOr(description, def string) string {
	if strings.TrimSpace(description) == "" {
		return def
	}
	return description
}

// expiryUnix returns the first parseable RFC3339 timestamp in priority order,
// falling back to now + defaultInstrumentTTL when none parse. Used because VA
// expiry can appear in two places (the VA channel_properties and the top-level
// expiredAt), with different reliability.
func expiryUnix(times ...string) int64 {
	for _, ts := range times {
		if ts != "" {
			if t, err := time.Parse(time.RFC3339, ts); err == nil {
				return t.Unix()
			}
		}
	}
	return time.Now().Add(defaultInstrumentTTL).Unix()
}

// compile-time assertion: the adapter satisfies the optional capability once enabled.
var _ gateway.InstrumentGateway = (*Adapter)(nil)
