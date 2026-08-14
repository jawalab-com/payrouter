package doku

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jawalab-com/payrouter/internal/gateway"
	stripe "github.com/stripe/stripe-go/v81"
)

// Virtual account issuance over SNAP.
//
// Contrary to DOKU's older non-SNAP APIs, which exposed a separate endpoint per
// bank, SNAP uses ONE endpoint and selects the bank with additionalInfo.channel.
const (
	createVAPath = "/virtual-accounts/bi-snap-va/v1.1/transfer-va/create-va"

	// vaTrxTypeClosed fixes the payable amount. Open ("O") and bill-variable
	// ("V") types let the customer choose what to pay, which cannot be reconciled
	// against a fixed order total.
	vaTrxTypeClosed = "C"

	// partnerServiceIDWidth is the field width SNAP mandates. Note the two
	// paddings differ and are not interchangeable — see buildVANumber.
	partnerServiceIDWidth = 8
)

// vaChannels maps a bank to DOKU's SNAP channel code.
//
// The naming is NOT uniform — BCA and BNI are VIRTUAL_ACCOUNT_<BANK> while CIMB
// is VIRTUAL_ACCOUNT_BANK_CIMB — so codes are only listed here once verified
// against DOKU's documentation for that bank. Guessing a code produces a request
// DOKU rejects, or worse, routes to the wrong bank. Banks absent from this map
// can still be used by passing an explicit "channel" method param.
var vaChannels = map[string]string{
	"bca":  "VIRTUAL_ACCOUNT_BCA",
	"bni":  "VIRTUAL_ACCOUNT_BNI",
	"cimb": "VIRTUAL_ACCOUNT_BANK_CIMB",
}

// issueVirtualAccount creates a closed-amount virtual account.
func (a *Adapter) issueVirtualAccount(ctx context.Context, in *gateway.CreatePaymentInput) (*gateway.PaymentResult, error) {
	if !a.snap.supportsVA() {
		return nil, fmt.Errorf("doku: %w: virtual accounts need DOKU_PARTNER_SERVICE_ID",
			gateway.ErrInstrumentUnsupported)
	}

	bank := strings.ToLower(strings.TrimSpace(bankParam(in.MethodParams)))
	if bank == "" {
		return nil, errors.New("doku: virtual account requires a bank (method param \"bank\", e.g. bca)")
	}
	channel, err := vaChannel(bank, in.MethodParams)
	if err != nil {
		return nil, err
	}

	token, err := a.accessToken(ctx)
	if err != nil {
		return nil, err
	}

	customerNo := customerNumber(in.Reference)
	vaNumber := buildVANumber(a.snap.partnerServiceID, customerNo)
	expiry := a.now().Add(qrValiditySeconds * time.Second)

	name := "PayRouter"
	if in.Customer != nil && in.Customer.Name != "" {
		name = truncate(in.Customer.Name, 255)
	}

	body, err := marshalJSON(map[string]any{
		// The field itself is space-padded; the VA number embeds a zero-padded
		// copy. They are different renderings of the same value.
		"partnerServiceId":   padLeft(a.snap.partnerServiceID, partnerServiceIDWidth, ' '),
		"customerNo":         customerNo,
		"virtualAccountNo":   vaNumber,
		"virtualAccountName": name,
		// trxId carries our pi_ reference. The VA number alone would correlate a
		// callback, but sending it too means DOKU's dashboard and notifications
		// both show an id that matches our PaymentIntent.
		"trxId":                 in.Reference,
		"virtualAccountTrxType": vaTrxTypeClosed,
		"totalAmount": map[string]any{
			"value":    snapAmount(in.AmountMinor),
			"currency": currencyOr(in.Currency, "IDR"),
		},
		"expiredDate":    expiry.Format(snapTimeFormat),
		"additionalInfo": map[string]any{"channel": channel},
	})
	if err != nil {
		return nil, err
	}

	var resp createVAResponse
	if err := a.doSNAP(ctx, http.MethodPost, createVAPath, token, body, &resp); err != nil {
		return nil, err
	}
	if err := snapResponseError(resp.ResponseCode, resp.ResponseMessage); err != nil {
		return nil, err
	}

	// Prefer the number DOKU echoes back; fall back to the one we constructed
	// only if the response omits it.
	issued := resp.VirtualAccountData.VirtualAccountNo
	if issued == "" {
		issued = vaNumber
	}

	return &gateway.PaymentResult{
		Reference:        in.Reference,
		GatewayReference: issued,
		Status:           stripe.PaymentIntentStatusRequiresAction,
		ExpiresAt:        expiry.Unix(),
		Display: &gateway.DisplayInstructions{
			Kind: gateway.InstrumentVirtualAccount,
			VirtualAccount: &gateway.VirtualAccountInstruction{
				Bank:          bank,
				AccountNumber: issued,
				AccountName:   resp.VirtualAccountData.VirtualAccountName,
			},
			ExpiresAt: expiry.Unix(),
		},
		Raw: resp,
	}, nil
}

// createVAResponse is the subset of the create-va response we act on.
type createVAResponse struct {
	ResponseCode       string `json:"responseCode"`
	ResponseMessage    string `json:"responseMessage"`
	VirtualAccountData struct {
		VirtualAccountNo      string `json:"virtualAccountNo"`
		VirtualAccountName    string `json:"virtualAccountName"`
		VirtualAccountTrxType string `json:"virtualAccountTrxType"`
		TotalAmount           struct {
			Value    string `json:"value"`
			Currency string `json:"currency"`
		} `json:"totalAmount"`
	} `json:"virtualAccountData"`
}

// vaChannel resolves the SNAP channel code for a bank. An explicit "channel"
// method param wins, so a bank not yet verified here can be used without a code
// change; otherwise only verified codes are accepted.
func vaChannel(bank string, params map[string]any) (string, error) {
	if v, _ := params["channel"].(string); strings.TrimSpace(v) != "" {
		return strings.ToUpper(strings.TrimSpace(v)), nil
	}
	if code, ok := vaChannels[bank]; ok {
		return code, nil
	}
	known := make([]string, 0, len(vaChannels))
	for b := range vaChannels {
		known = append(known, b)
	}
	sortStrings(known)
	return "", fmt.Errorf("doku: no verified SNAP channel code for bank %q (verified: %s); "+
		"pass an explicit \"channel\" method param to use it", bank, strings.Join(known, ", "))
}

// buildVANumber concatenates the prefix and customer number into the account
// number the customer transfers to.
//
// The padding here is ZERO, while the standalone partnerServiceId field is
// padded with SPACES. DOKU specifies them separately and they are not
// interchangeable: swapping them yields a VA number that either fails validation
// or silently addresses a different account.
func buildVANumber(partnerServiceID, customerNo string) string {
	return padLeft(partnerServiceID, partnerServiceIDWidth, '0') + customerNo
}

// padLeft left-pads s to width with pad. A value already at or over width is
// returned unchanged, since truncating an identifier would corrupt it.
func padLeft(s string, width int, pad byte) string {
	if len(s) >= width {
		return s
	}
	return strings.Repeat(string(pad), width-len(s)) + s
}

// customerNumber derives the numeric customer number SNAP requires from our
// pi_ reference, which is hexadecimal and cannot be used directly.
//
// It is deterministic so a retried payment maps to the same virtual account
// rather than stranding the customer with a second number for one order. Width
// is bounded to 20 digits, SNAP's maximum, which a uint64 always satisfies.
func customerNumber(reference string) string {
	sum := sha256.Sum256([]byte(reference))
	return strconv.FormatUint(binary.BigEndian.Uint64(sum[:8]), 10)
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

// sortStrings sorts in place; kept local to avoid importing sort for one call.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
