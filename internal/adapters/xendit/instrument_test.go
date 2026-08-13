package xendit

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jawalab-com/payrouter/internal/gateway"
)

// captured records what the adapter sent, so request shape can be asserted —
// these payloads move money, and a wrong field is a wrong charge.
type captured struct {
	path    string
	headers http.Header
	body    map[string]any
}

func newInstrumentServer(t *testing.T, status int, respBody string, got *captured) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path = r.URL.Path
		got.headers = r.Header.Clone()
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got.body)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, respBody)
	}))
}

func TestIssueQRISReturnsRawPayload(t *testing.T) {
	var got captured
	srv := newInstrumentServer(t, http.StatusCreated, `{
		"id": "qr_9bd909fd-422a",
		"reference_id": "pi_abc",
		"qr_string": "00020101021226620014COM.GO-JEK.WWW",
		"status": "ACTIVE",
		"amount": 100000,
		"expires_at": "2026-08-14T10:00:00Z"
	}`, &got)
	defer srv.Close()

	a := newForTest("xnd_development_x", "tok", srv.URL, srv.Client())
	res, err := a.IssueInstrument(context.Background(), &gateway.CreatePaymentInput{
		Reference: "pi_abc", AmountMinor: 100000, Currency: "idr",
		PaymentMethodType: gateway.IDQRIS, Description: "Order 1",
	})
	if err != nil {
		t.Fatalf("IssueInstrument: %v", err)
	}

	if got.path != qrCodesPath {
		t.Errorf("path = %q, want %q", got.path, qrCodesPath)
	}
	// The QR Codes API rejects calls without an explicit api-version.
	if v := got.headers.Get("api-version"); v != qrAPIVersion {
		t.Errorf("api-version header = %q, want %q", v, qrAPIVersion)
	}
	if got.body["reference_id"] != "pi_abc" {
		t.Errorf("reference_id = %v, want pi_abc", got.body["reference_id"])
	}
	// A STATIC code would be reusable and not amount-bound — wrong for checkout.
	if got.body["type"] != "DYNAMIC" {
		t.Errorf("type = %v, want DYNAMIC", got.body["type"])
	}
	if got.body["amount"] != float64(100000) {
		t.Errorf("amount = %v, want 100000", got.body["amount"])
	}

	if res.Display == nil {
		t.Fatal("Display is nil; the instrument cannot be rendered")
	}
	if res.Display.Kind != gateway.InstrumentQRIS {
		t.Errorf("Kind = %q, want qris", res.Display.Kind)
	}
	if res.Display.QRIS == nil || res.Display.QRIS.Payload != "00020101021226620014COM.GO-JEK.WWW" {
		t.Errorf("QRIS payload not carried through: %+v", res.Display.QRIS)
	}
	if res.GatewayReference != "qr_9bd909fd-422a" {
		t.Errorf("GatewayReference = %q", res.GatewayReference)
	}
	if res.Display.ExpiresAt == 0 {
		t.Error("ExpiresAt not set; a QR with no expiry stays payable indefinitely")
	}
}

func TestIssueVirtualAccountReturnsAccountNumber(t *testing.T) {
	var got captured
	srv := newInstrumentServer(t, http.StatusCreated, `{
		"id": "va_123",
		"external_id": "pi_def",
		"bank_code": "BCA",
		"account_number": "8808123456789",
		"name": "Budi Santoso",
		"expected_amount": 250000,
		"expiration_date": "2026-08-14T10:00:00Z"
	}`, &got)
	defer srv.Close()

	a := newForTest("xnd_development_x", "tok", srv.URL, srv.Client())
	res, err := a.IssueInstrument(context.Background(), &gateway.CreatePaymentInput{
		Reference: "pi_def", AmountMinor: 250000, Currency: "idr",
		PaymentMethodType: gateway.IDVirtualAccount,
		MethodParams:      map[string]any{"bank": "bca"},
		Customer:          &gateway.Customer{Name: "Budi Santoso"},
	})
	if err != nil {
		t.Fatalf("IssueInstrument: %v", err)
	}

	if got.path != vaPath {
		t.Errorf("path = %q, want %q", got.path, vaPath)
	}
	if got.body["bank_code"] != "BCA" {
		t.Errorf("bank_code = %v, want BCA (upper-cased)", got.body["bank_code"])
	}
	// An open VA would let the customer pay any amount, breaking reconciliation.
	if got.body["is_closed"] != true {
		t.Errorf("is_closed = %v, want true", got.body["is_closed"])
	}
	if got.body["expected_amount"] != float64(250000) {
		t.Errorf("expected_amount = %v, want 250000", got.body["expected_amount"])
	}

	va := res.Display.VirtualAccount
	if va == nil {
		t.Fatal("VirtualAccount instruction is nil")
	}
	if va.AccountNumber != "8808123456789" {
		t.Errorf("AccountNumber = %q", va.AccountNumber)
	}
	// Normalized to lowercase so the UI can map it to an asset without casing rules.
	if va.Bank != "bca" {
		t.Errorf("Bank = %q, want bca", va.Bank)
	}
}

// TestUnsupportedMethodIsDistinguishable: the caller must be able to tell "this
// gateway can't issue that instrument" (fall back to hosted redirect) from a
// real failure (surface the error).
func TestUnsupportedMethodIsDistinguishable(t *testing.T) {
	a := newForTest("xnd_development_x", "tok", "http://unused", nil)

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

	for _, m := range []gateway.IDPaymentMethodType{gateway.IDQRIS, gateway.IDVirtualAccount} {
		if !a.SupportsInstrument(m) {
			t.Errorf("SupportsInstrument(%s) = false, want true", m)
		}
	}
}

func TestVirtualAccountRequiresBank(t *testing.T) {
	a := newForTest("xnd_development_x", "tok", "http://unused", nil)
	_, err := a.IssueInstrument(context.Background(), &gateway.CreatePaymentInput{
		Reference: "pi_x", AmountMinor: 1000, PaymentMethodType: gateway.IDVirtualAccount,
	})
	if err == nil || !strings.Contains(err.Error(), "bank") {
		t.Errorf("err = %v, want a message naming the missing bank param", err)
	}
}

// TestEmptyPayloadIsRejected: a 2xx with no qr_string would otherwise produce a
// blank QR on the checkout page — an unpayable order that looks fine.
func TestEmptyPayloadIsRejected(t *testing.T) {
	var got captured
	srv := newInstrumentServer(t, http.StatusCreated, `{"id":"qr_1","status":"ACTIVE"}`, &got)
	defer srv.Close()

	a := newForTest("xnd_development_x", "tok", srv.URL, srv.Client())
	_, err := a.IssueInstrument(context.Background(), &gateway.CreatePaymentInput{
		Reference: "pi_x", AmountMinor: 1000, PaymentMethodType: gateway.IDQRIS,
	})
	if err == nil {
		t.Fatal("expected an error when qr_string is absent")
	}
}

func TestInstrumentErrorsSurfaceHTTPStatus(t *testing.T) {
	var got captured
	srv := newInstrumentServer(t, http.StatusBadRequest, `{"error_code":"INVALID"}`, &got)
	defer srv.Close()

	a := newForTest("xnd_development_x", "tok", srv.URL, srv.Client())
	_, err := a.IssueInstrument(context.Background(), &gateway.CreatePaymentInput{
		Reference: "pi_x", AmountMinor: 1000, PaymentMethodType: gateway.IDQRIS,
	})
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Errorf("err = %v, want the HTTP status surfaced", err)
	}
}

// TestAdapterSatisfiesInstrumentGateway pins the capability contract, so the
// optional interface cannot silently stop being implemented.
func TestAdapterSatisfiesInstrumentGateway(t *testing.T) {
	var _ gateway.InstrumentGateway = (*Adapter)(nil)
}
