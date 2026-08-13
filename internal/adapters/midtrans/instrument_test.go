package midtrans

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jawalab-com/payrouter/internal/gateway"
)

type chargeCapture struct {
	path string
	body map[string]any
}

func newChargeServer(t *testing.T, respBody string, got *chargeCapture) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got.body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respBody)
	}))
}

// issueVia runs IssueInstrument against a stub Core API returning respBody.
func issueVia(t *testing.T, respBody string, in *gateway.CreatePaymentInput) (*gateway.PaymentResult, *chargeCapture, error) {
	t.Helper()
	var got chargeCapture
	srv := newChargeServer(t, respBody, &got)
	defer srv.Close()

	// snapBase is deliberately unroutable: instrument issuance must use the Core
	// API host, never Snap.
	a := newForTest("SB-Mid-server-x", "http://snap.invalid", srv.URL, srv.Client())
	res, err := a.IssueInstrument(context.Background(), in)
	return res, &got, err
}

func TestIssueQRISReturnsPayloadAndImage(t *testing.T) {
	res, got, err := issueVia(t, `{
		"status_code": "201",
		"transaction_id": "tx-123",
		"order_id": "pi_abc",
		"payment_type": "qris",
		"transaction_status": "pending",
		"qr_string": "00020101021226620014COM.GO-JEK.WWW",
		"expiry_time": "2026-08-14 17:00:00",
		"actions": [{"name":"generate-qr-code","method":"GET","url":"https://api.midtrans.com/v2/qris/tx-123/qr-code"}]
	}`, &gateway.CreatePaymentInput{
		Reference: "pi_abc", AmountMinor: 100000, Currency: "idr",
		PaymentMethodType: gateway.IDQRIS,
	})
	if err != nil {
		t.Fatalf("IssueInstrument: %v", err)
	}

	if got.path != chargePath {
		t.Errorf("path = %q, want %q (Core API, not Snap)", got.path, chargePath)
	}
	if got.body["payment_type"] != "qris" {
		t.Errorf("payment_type = %v, want qris", got.body["payment_type"])
	}
	if _, ok := got.body["custom_expiry"]; !ok {
		t.Error("custom_expiry absent; the instrument would inherit the account default")
	}

	q := res.Display.QRIS
	if q == nil || q.Payload != "00020101021226620014COM.GO-JEK.WWW" {
		t.Fatalf("QRIS payload not carried through: %+v", q)
	}
	// Both are populated so a renderer can prefer the payload but still fall back.
	if q.ImageURL == "" {
		t.Error("generate-qr-code action URL not captured as ImageURL")
	}
	if res.GatewayReference != "tx-123" {
		t.Errorf("GatewayReference = %q, want the transaction_id", res.GatewayReference)
	}
}

// TestVirtualAccountShapesAreNormalized is the point of this adapter: Midtrans
// returns three structurally different payloads depending on the bank, and a UI
// must not have to know that.
func TestVirtualAccountShapesAreNormalized(t *testing.T) {
	cases := []struct {
		name           string
		bank           string
		resp           string
		wantBank       string
		wantAccount    string
		wantBillerCode string
		wantPaymentTyp string
	}{
		{
			name:     "bca uses va_numbers",
			bank:     "bca",
			resp:     `{"status_code":"201","transaction_id":"tx-1","va_numbers":[{"bank":"bca","va_number":"12345678901"}]}`,
			wantBank: "bca", wantAccount: "12345678901", wantPaymentTyp: "bank_transfer",
		},
		{
			name:     "permata returns a top-level field",
			bank:     "permata",
			resp:     `{"status_code":"201","transaction_id":"tx-2","permata_va_number":"8778123456789"}`,
			wantBank: "permata", wantAccount: "8778123456789", wantPaymentTyp: "bank_transfer",
		},
		{
			name:     "mandiri is bill payment, not a virtual account",
			bank:     "mandiri",
			resp:     `{"status_code":"201","transaction_id":"tx-3","biller_code":"70012","bill_key":"820849"}`,
			wantBank: "mandiri", wantAccount: "820849", wantBillerCode: "70012", wantPaymentTyp: "echannel",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, got, err := issueVia(t, tc.resp, &gateway.CreatePaymentInput{
				Reference: "pi_x", AmountMinor: 50000, Currency: "idr",
				PaymentMethodType: gateway.IDVirtualAccount,
				MethodParams:      map[string]any{"bank": tc.bank},
			})
			if err != nil {
				t.Fatalf("IssueInstrument: %v", err)
			}
			if got.body["payment_type"] != tc.wantPaymentTyp {
				t.Errorf("payment_type = %v, want %s", got.body["payment_type"], tc.wantPaymentTyp)
			}

			va := res.Display.VirtualAccount
			if va == nil {
				t.Fatal("VirtualAccount instruction is nil")
			}
			if va.Bank != tc.wantBank {
				t.Errorf("Bank = %q, want %q", va.Bank, tc.wantBank)
			}
			if va.AccountNumber != tc.wantAccount {
				t.Errorf("AccountNumber = %q, want %q", va.AccountNumber, tc.wantAccount)
			}
			if va.BillerCode != tc.wantBillerCode {
				t.Errorf("BillerCode = %q, want %q", va.BillerCode, tc.wantBillerCode)
			}
		})
	}
}

// TestMandiriRequiresBothNumbers: a bill key without its biller code cannot be
// paid, so a partial response must fail rather than render half the input.
func TestMandiriRequiresBothNumbers(t *testing.T) {
	_, _, err := issueVia(t, `{"status_code":"201","transaction_id":"tx-4","bill_key":"820849"}`,
		&gateway.CreatePaymentInput{
			Reference: "pi_x", AmountMinor: 50000,
			PaymentMethodType: gateway.IDVirtualAccount,
			MethodParams:      map[string]any{"bank": "mandiri"},
		})
	if err == nil || !strings.Contains(err.Error(), "biller_code") {
		t.Errorf("err = %v, want a complaint about the missing biller_code", err)
	}
}

// TestBusinessFailureOnHTTP200IsDetected: Midtrans answers business-level
// rejections with HTTP 200 and a failing status_code. Trusting the HTTP status
// would record a declined charge as a live payment instrument.
func TestBusinessFailureOnHTTP200IsDetected(t *testing.T) {
	_, _, err := issueVia(t, `{"status_code":"402","status_message":"Insufficient merchant balance"}`,
		&gateway.CreatePaymentInput{
			Reference: "pi_x", AmountMinor: 50000, PaymentMethodType: gateway.IDQRIS,
		})
	if err == nil {
		t.Fatal("HTTP 200 with status_code 402 was treated as success")
	}
	if !strings.Contains(err.Error(), "402") || !strings.Contains(err.Error(), "Insufficient") {
		t.Errorf("err = %v, want the status_code and message surfaced", err)
	}
}

func TestEmptyInstrumentResponsesRejected(t *testing.T) {
	if _, _, err := issueVia(t, `{"status_code":"201","transaction_id":"tx-5"}`,
		&gateway.CreatePaymentInput{Reference: "pi_x", AmountMinor: 1000, PaymentMethodType: gateway.IDQRIS}); err == nil {
		t.Error("QRIS: expected an error when neither qr_string nor an action is present")
	}
	if _, _, err := issueVia(t, `{"status_code":"201","transaction_id":"tx-6"}`,
		&gateway.CreatePaymentInput{
			Reference: "pi_x", AmountMinor: 1000, PaymentMethodType: gateway.IDVirtualAccount,
			MethodParams: map[string]any{"bank": "bca"},
		}); err == nil {
		t.Error("VA: expected an error when no account number is returned")
	}
}

func TestUnsupportedBankIsRejected(t *testing.T) {
	_, _, err := issueVia(t, `{"status_code":"201"}`, &gateway.CreatePaymentInput{
		Reference: "pi_x", AmountMinor: 1000, PaymentMethodType: gateway.IDVirtualAccount,
		MethodParams: map[string]any{"bank": "not_a_bank"},
	})
	if err == nil || !strings.Contains(err.Error(), "not_a_bank") {
		t.Errorf("err = %v, want the unsupported bank named", err)
	}
}

func TestUnsupportedMethodsFallBack(t *testing.T) {
	a := newForTest("SB-Mid-server-x", "http://snap.invalid", "http://api.invalid", nil)

	for _, m := range []gateway.IDPaymentMethodType{gateway.IDEWallet, gateway.IDRetail, gateway.IDCard, gateway.IDHosted} {
		if a.SupportsInstrument(m) {
			t.Errorf("SupportsInstrument(%s) = true, want false", m)
		}
		_, err := a.IssueInstrument(context.Background(), &gateway.CreatePaymentInput{
			Reference: "pi_x", AmountMinor: 1000, PaymentMethodType: m,
		})
		if !errors.Is(err, gateway.ErrInstrumentUnsupported) {
			t.Errorf("%s: err = %v, want ErrInstrumentUnsupported", m, err)
		}
	}
}

func TestExpiryTimeParsedAsWIB(t *testing.T) {
	res, _, err := issueVia(t, `{
		"status_code":"201","transaction_id":"tx-7","qr_string":"00020101",
		"expiry_time":"2026-08-14 17:00:00"
	}`, &gateway.CreatePaymentInput{
		Reference: "pi_x", AmountMinor: 1000, PaymentMethodType: gateway.IDQRIS,
	})
	if err != nil {
		t.Fatalf("IssueInstrument: %v", err)
	}
	// Midtrans sends a naive local timestamp. Interpreting it as UTC would put
	// the expiry 7 hours late, keeping an instrument payable past its real
	// deadline, so it must be read as WIB (UTC+7): 17:00 WIB is 10:00 UTC.
	want := time.Date(2026, 8, 14, 17, 0, 0, 0, time.FixedZone("WIB", wibOffset)).Unix()
	if res.Display.ExpiresAt != want {
		t.Errorf("ExpiresAt = %d (%s), want %d (%s)",
			res.Display.ExpiresAt, time.Unix(res.Display.ExpiresAt, 0).UTC(),
			want, time.Unix(want, 0).UTC())
	}
	if res.ExpiresAt != want {
		t.Errorf("PaymentResult.ExpiresAt = %d, want %d (must match the display expiry)", res.ExpiresAt, want)
	}
}

func TestMidtransSatisfiesInstrumentGateway(t *testing.T) {
	var _ gateway.InstrumentGateway = (*Adapter)(nil)
}
