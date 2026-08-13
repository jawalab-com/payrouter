package midtrans

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

// Direct instrument issuance for Midtrans.
//
// CreatePayment uses Snap, a hosted page that returns only a redirect URL: the
// customer picks a method on Midtrans' site and the instrument is issued there.
// The Core API charge endpoint used here issues the instrument up front instead,
// so PayRouter can render it on its own checkout page.
//
// Note this is a different API on a different host (apiBase, not snapBase) and
// it must be enabled on the merchant account — Snap access alone is not enough.
const (
	chargePath = "/v2/charge"

	// defaultInstrumentTTL bounds how long an issued instrument stays payable.
	defaultInstrumentTTL = 24 * time.Hour

	// wibOffset is Indonesia Western Standard Time. Midtrans returns expiry_time
	// as a naive "2006-01-02 15:04:05" string in the merchant's timezone. A fixed
	// offset is used rather than time.LoadLocation because Indonesia observes no
	// DST and the scratch container carries no tzdata.
	wibOffset = 7 * 60 * 60
)

// SupportsInstrument reports the methods this adapter can issue directly.
// E-wallets, cards and retail outlets still go through the hosted Snap page.
func (a *Adapter) SupportsInstrument(method gateway.IDPaymentMethodType) bool {
	switch method {
	case gateway.IDQRIS, gateway.IDVirtualAccount:
		return true
	default:
		return false
	}
}

// IssueInstrument creates a directly-renderable payment instrument via the Core
// API. It returns gateway.ErrInstrumentUnsupported for methods Midtrans cannot
// issue this way, so the caller can fall back to the hosted Snap redirect.
func (a *Adapter) IssueInstrument(ctx context.Context, in *gateway.CreatePaymentInput) (*gateway.PaymentResult, error) {
	if in == nil || in.Reference == "" {
		return nil, errors.New("midtrans: IssueInstrument requires a reference (order_id)")
	}
	if in.AmountMinor <= 0 {
		return nil, errors.New("midtrans: IssueInstrument requires a positive amount")
	}

	switch in.PaymentMethodType {
	case gateway.IDQRIS:
		return a.issueQRIS(ctx, in)
	case gateway.IDVirtualAccount:
		return a.issueVirtualAccount(ctx, in)
	default:
		return nil, fmt.Errorf("midtrans: %w: %s", gateway.ErrInstrumentUnsupported, in.PaymentMethodType)
	}
}

// issueQRIS charges payment_type=qris and returns the raw EMVCo payload, so the
// QR can be rendered at any size and offered as copyable text. Midtrans also
// exposes a hosted image via actions[generate-qr-code], kept as a fallback for
// the rare response that omits qr_string.
func (a *Adapter) issueQRIS(ctx context.Context, in *gateway.CreatePaymentInput) (*gateway.PaymentResult, error) {
	body := a.chargeBody(in, "qris")
	// acquirer selects the QRIS issuer behind the code; gopay is Midtrans' default.
	qris := map[string]any{"acquirer": "gopay"}
	if acq, _ := in.MethodParams["acquirer"].(string); acq != "" {
		qris["acquirer"] = strings.ToLower(acq)
	}
	body["qris"] = qris

	var resp chargeResponse
	if err := a.doJSON(ctx, http.MethodPost, a.apiBase+chargePath, body, &resp); err != nil {
		return nil, err
	}
	if err := resp.chargeError(); err != nil {
		return nil, err
	}

	imageURL := resp.actionURL("generate-qr-code")
	if resp.QRString == "" && imageURL == "" {
		return nil, errors.New("midtrans: QRIS charge returned neither qr_string nor a generate-qr-code action")
	}

	expires := resp.expiryUnix()
	return &gateway.PaymentResult{
		Reference:        in.Reference,
		GatewayReference: resp.TransactionID,
		Status:           stripe.PaymentIntentStatusRequiresAction,
		ExpiresAt:        expires,
		Display: &gateway.DisplayInstructions{
			Kind:      gateway.InstrumentQRIS,
			QRIS:      &gateway.QRISInstruction{Payload: resp.QRString, ImageURL: imageURL},
			ExpiresAt: expires,
		},
		Raw: resp,
	}, nil
}

// issueVirtualAccount charges a bank transfer and normalizes the three distinct
// response shapes Midtrans uses:
//
//	bca/bni/bri  payment_type=bank_transfer -> va_numbers[]
//	permata      payment_type=bank_transfer -> permata_va_number (top level)
//	mandiri      payment_type=echannel      -> biller_code + bill_key (no VA at all)
func (a *Adapter) issueVirtualAccount(ctx context.Context, in *gateway.CreatePaymentInput) (*gateway.PaymentResult, error) {
	bank := strings.ToLower(strings.TrimSpace(bankParam(in.MethodParams)))
	if bank == "" {
		return nil, errors.New("midtrans: virtual account requires a bank (method param \"bank\", e.g. bca)")
	}
	code, ok := vaCode(bank)
	if !ok {
		return nil, fmt.Errorf("midtrans: unsupported virtual account bank %q", bank)
	}

	var body map[string]any
	if code == "echannel" {
		body = a.chargeBody(in, "echannel")
		// Mandiri Bill Payment requires bill descriptors on the request.
		body["echannel"] = map[string]any{
			"bill_info1": "Payment",
			"bill_info2": truncate(descriptionOr(in.Description, "Order"), 30),
		}
	} else {
		body = a.chargeBody(in, "bank_transfer")
		body["bank_transfer"] = map[string]any{"bank": bank}
	}

	var resp chargeResponse
	if err := a.doJSON(ctx, http.MethodPost, a.apiBase+chargePath, body, &resp); err != nil {
		return nil, err
	}
	if err := resp.chargeError(); err != nil {
		return nil, err
	}

	va, err := resp.virtualAccount(bank)
	if err != nil {
		return nil, err
	}
	expires := resp.expiryUnix()
	return &gateway.PaymentResult{
		Reference:        in.Reference,
		GatewayReference: resp.TransactionID,
		Status:           stripe.PaymentIntentStatusRequiresAction,
		ExpiresAt:        expires,
		Display: &gateway.DisplayInstructions{
			Kind:           gateway.InstrumentVirtualAccount,
			VirtualAccount: va,
			ExpiresAt:      expires,
		},
		Raw: resp,
	}, nil
}

// chargeBody builds the fields common to every Core API charge.
func (a *Adapter) chargeBody(in *gateway.CreatePaymentInput, paymentType string) map[string]any {
	body := map[string]any{
		"payment_type": paymentType,
		"transaction_details": map[string]any{
			"order_id":     in.Reference,
			"gross_amount": in.AmountMinor, // IDR is zero-decimal: minor units == rupiah
		},
		// Without an explicit expiry Midtrans applies its account default, which
		// can leave an instrument payable long after the order is abandoned.
		"custom_expiry": map[string]any{
			"expiry_duration": int64(defaultInstrumentTTL / time.Minute),
			"unit":            "minute",
		},
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
	return body
}

// chargeResponse is the subset of the Core API charge response we act on. The
// VA fields are mutually exclusive by bank; see virtualAccount.
type chargeResponse struct {
	StatusCode        string `json:"status_code"`
	StatusMessage     string `json:"status_message"`
	TransactionID     string `json:"transaction_id"`
	OrderID           string `json:"order_id"`
	PaymentType       string `json:"payment_type"`
	TransactionStatus string `json:"transaction_status"`
	ExpiryTime        string `json:"expiry_time"`

	// QRIS
	QRString string `json:"qr_string"`
	Actions  []struct {
		Name   string `json:"name"`
		Method string `json:"method"`
		URL    string `json:"url"`
	} `json:"actions"`

	// Bank transfer (bca/bni/bri)
	VANumbers []struct {
		Bank     string `json:"bank"`
		VANumber string `json:"va_number"`
	} `json:"va_numbers"`

	// Permata returns its VA at the top level rather than in va_numbers.
	PermataVANumber string `json:"permata_va_number"`

	// Mandiri Bill Payment returns a biller code plus a bill key.
	BillerCode string `json:"biller_code"`
	BillKey    string `json:"bill_key"`
}

// chargeError converts a non-success status_code into an error. Midtrans returns
// HTTP 200 with a failing status_code for business-level rejections, so relying
// on the HTTP status alone would treat a declined charge as a success.
func (r chargeResponse) chargeError() error {
	switch r.StatusCode {
	case "200", "201":
		return nil
	case "":
		return errors.New("midtrans: charge response contained no status_code")
	default:
		msg := r.StatusMessage
		if msg == "" {
			msg = "charge rejected"
		}
		return fmt.Errorf("midtrans: charge failed (status_code %s): %s", r.StatusCode, msg)
	}
}

// actionURL returns the URL of the named action, or "" when absent.
func (r chargeResponse) actionURL(name string) string {
	for _, a := range r.Actions {
		if a.Name == name {
			return a.URL
		}
	}
	return ""
}

// virtualAccount normalizes whichever VA shape the bank produced.
func (r chargeResponse) virtualAccount(bank string) (*gateway.VirtualAccountInstruction, error) {
	if r.BillKey != "" || r.BillerCode != "" {
		if r.BillKey == "" || r.BillerCode == "" {
			return nil, errors.New("midtrans: Mandiri bill payment needs both biller_code and bill_key")
		}
		return &gateway.VirtualAccountInstruction{
			Bank:          "mandiri",
			AccountNumber: r.BillKey,
			BillerCode:    r.BillerCode,
		}, nil
	}
	if r.PermataVANumber != "" {
		return &gateway.VirtualAccountInstruction{Bank: "permata", AccountNumber: r.PermataVANumber}, nil
	}
	for _, va := range r.VANumbers {
		if va.VANumber != "" {
			b := strings.ToLower(va.Bank)
			if b == "" {
				b = bank
			}
			return &gateway.VirtualAccountInstruction{Bank: b, AccountNumber: va.VANumber}, nil
		}
	}
	return nil, fmt.Errorf("midtrans: charge for bank %q returned no virtual account number", bank)
}

// expiryUnix parses Midtrans' naive expiry_time, falling back to the TTL we
// requested when it is absent or unparseable.
func (r chargeResponse) expiryUnix() int64 {
	if r.ExpiryTime != "" {
		if t, err := time.Parse("2006-01-02 15:04:05", r.ExpiryTime); err == nil {
			return t.Unix() - wibOffset
		}
	}
	return time.Now().Add(defaultInstrumentTTL).Unix()
}

// bankParam extracts the bank from method params, accepting common spellings.
func bankParam(params map[string]any) string {
	for _, key := range []string{"bank", "bank_code"} {
		if v, _ := params[key].(string); v != "" {
			return v
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
