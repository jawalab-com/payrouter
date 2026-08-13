package doku

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jawalab-com/payrouter/internal/gateway"
	stripe "github.com/stripe/stripe-go/v81"
)

// Direct instrument issuance for DOKU, over the Bank Indonesia SNAP APIs.
//
// CreatePayment uses the Checkout API, a hosted page returning only a redirect
// URL. The endpoint here issues a QRIS code up front so PayRouter can render it
// on its own checkout page.
//
// This runs on a different protocol from the rest of the adapter — see
// snapauth.go — and requires SNAP to be enabled with an RSA keypair via
// EnableSNAP. Virtual accounts are deliberately not implemented yet: DOKU
// exposes them per bank (/bri-virtual-account/..., /bni-virtual-account/...,
// /mandiri-virtual-account/...), each with its own request shape, so they are a
// separate piece of work rather than a variant of this one.
const (
	qrGeneratePath = "/snap-adapter/b2b/v1.0/qr/qr-mpm-generate"

	// channelID identifies the integration type; H2H is host-to-host.
	channelID = "H2H"

	// qrValiditySeconds bounds how long an issued QR stays payable.
	qrValiditySeconds = 24 * 60 * 60
)

// SupportsInstrument reports which methods this adapter can issue directly.
// It returns false until EnableSNAP has been called, so an adapter without SNAP
// credentials falls back to the hosted Checkout page instead of failing when a
// customer tries to pay.
func (a *Adapter) SupportsInstrument(method gateway.IDPaymentMethodType) bool {
	if a.snap == nil {
		return false
	}
	return method == gateway.IDQRIS
}

// IssueInstrument creates a directly-renderable payment instrument.
func (a *Adapter) IssueInstrument(ctx context.Context, in *gateway.CreatePaymentInput) (*gateway.PaymentResult, error) {
	if in == nil || in.Reference == "" {
		return nil, errors.New("doku: IssueInstrument requires a reference (partnerReferenceNo)")
	}
	if in.AmountMinor <= 0 {
		return nil, errors.New("doku: IssueInstrument requires a positive amount")
	}
	if in.PaymentMethodType != gateway.IDQRIS {
		return nil, fmt.Errorf("doku: %w: %s", gateway.ErrInstrumentUnsupported, in.PaymentMethodType)
	}
	if a.snap == nil {
		return nil, fmt.Errorf("doku: %w: SNAP is not configured (call EnableSNAP)", gateway.ErrInstrumentUnsupported)
	}

	token, err := a.accessToken(ctx)
	if err != nil {
		return nil, err
	}

	body, err := json.Marshal(map[string]any{
		"partnerReferenceNo": in.Reference,
		"amount": map[string]any{
			// SNAP carries amounts as decimal strings, not integers, even for
			// zero-decimal currencies like IDR.
			"value":    snapAmount(in.AmountMinor),
			"currency": currencyOr(in.Currency, "IDR"),
		},
		"merchantId": a.snap.merchantID,
		"terminalId": a.snap.terminalID,
		"validityPeriod": a.now().Add(qrValiditySeconds * time.Second).
			Format(snapTimeFormat),
	})
	if err != nil {
		return nil, fmt.Errorf("doku: marshal QR request: %w", err)
	}

	var resp qrGenerateResponse
	if err := a.doSNAP(ctx, http.MethodPost, qrGeneratePath, token, body, &resp); err != nil {
		return nil, err
	}
	if err := resp.responseError(); err != nil {
		return nil, err
	}
	if resp.QRContent == "" {
		return nil, errors.New("doku: QR response carried no qrContent")
	}

	expires := a.now().Add(qrValiditySeconds * time.Second).Unix()
	if t, err := time.Parse(snapTimeFormat, resp.AdditionalInfo.ValidityPeriod); err == nil {
		expires = t.Unix()
	}

	return &gateway.PaymentResult{
		Reference:        in.Reference,
		GatewayReference: resp.ReferenceNo,
		Status:           stripe.PaymentIntentStatusRequiresAction,
		ExpiresAt:        expires,
		Display: &gateway.DisplayInstructions{
			Kind:      gateway.InstrumentQRIS,
			QRIS:      &gateway.QRISInstruction{Payload: resp.QRContent},
			ExpiresAt: expires,
		},
		Raw: resp,
	}, nil
}

// doSNAP performs a signed SNAP call. The body is passed as bytes and sent
// verbatim: the signature covers a hash of the request body, so re-encoding
// between signing and sending would invalidate it.
func (a *Adapter) doSNAP(ctx context.Context, method, path, token string, body []byte, out any) error {
	timestamp := snapTimestamp(a.now())
	signature := signSymmetric(a.snap.secretKey, method, path, token, body, timestamp)

	req, err := http.NewRequestWithContext(ctx, method, a.baseURL+path, strings.NewReader(string(body)))
	if err != nil {
		return fmt.Errorf("doku: build SNAP request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-TIMESTAMP", timestamp)
	req.Header.Set("X-SIGNATURE", signature)
	req.Header.Set("X-PARTNER-ID", a.snap.clientID)
	req.Header.Set("CHANNEL-ID", channelID)
	// X-EXTERNAL-ID must be unique per day; DOKU rejects a repeat as a duplicate.
	req.Header.Set("X-EXTERNAL-ID", externalID())

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("doku: call %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("doku: read SNAP response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("doku: %s %s returned HTTP %d: %s", method, path, resp.StatusCode, snippet(raw))
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("doku: decode SNAP response: %w", err)
		}
	}
	return nil
}

// qrGenerateResponse is the subset of the qr-mpm-generate response we act on.
type qrGenerateResponse struct {
	ResponseCode       string `json:"responseCode"`
	ResponseMessage    string `json:"responseMessage"`
	ReferenceNo        string `json:"referenceNo"`
	PartnerReferenceNo string `json:"partnerReferenceNo"`
	QRContent          string `json:"qrContent"`
	TerminalID         string `json:"terminalId"`
	AdditionalInfo     struct {
		ValidityPeriod string `json:"validityPeriod"`
	} `json:"additionalInfo"`
}

// responseError converts a non-success SNAP responseCode into an error.
//
// SNAP codes are six digits: HTTP status, service code, then case code. A
// success is 2xxxxxx — anything else is a rejection that may still arrive with
// HTTP 200, so the code must be checked rather than the transport status.
func (r qrGenerateResponse) responseError() error {
	if r.ResponseCode == "" {
		return errors.New("doku: SNAP response carried no responseCode")
	}
	if strings.HasPrefix(r.ResponseCode, "2") {
		return nil
	}
	msg := r.ResponseMessage
	if msg == "" {
		msg = "request rejected"
	}
	return fmt.Errorf("doku: SNAP request failed (responseCode %s): %s", r.ResponseCode, msg)
}

// snapAmount renders a minor-unit amount as the decimal string SNAP expects.
// IDR is zero-decimal, so the minor unit is already whole rupiah.
func snapAmount(minor int64) string {
	return strconv.FormatInt(minor, 10) + ".00"
}

// currencyOr upper-cases the currency, substituting a default when empty.
func currencyOr(currency, def string) string {
	if c := strings.ToUpper(strings.TrimSpace(currency)); c != "" {
		return c
	}
	return def
}
