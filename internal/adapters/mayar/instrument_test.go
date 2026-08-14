package mayar

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jawalab-com/payrouter/internal/gateway"
	stripe "github.com/stripe/stripe-go/v81"
)

// TestMayarSatisfiesInstrumentGateway pins the capability contract: Mayar now
// implements direct instrument issuance via the v2 API (correlatable ids confirmed
// against the live endpoint). The previous "deliberately does not issue" decision
// only applied to the standalone /qrcode/create endpoints, which remain unused.
func TestMayarSatisfiesInstrumentGateway(t *testing.T) {
	var _ gateway.InstrumentGateway = (*Adapter)(nil)
}

// TestMayarStillSatisfiesGateway guards the base capability: the hosted payment
// link remains the supported path regardless of instrument support.
func TestMayarStillSatisfiesGateway(t *testing.T) {
	var _ gateway.Gateway = (*Adapter)(nil)
}

func TestSupportsInstrumentGatedByEnable(t *testing.T) {
	a := newForTest(testAPIKey, testToken, "http://unused", nil)

	// Off by default: today's hosted-redirect behavior is unchanged until the
	// operator validates channels and lists them in MAYAR_INSTRUMENT_METHODS.
	if a.SupportsInstrument(gateway.IDQRIS) || a.SupportsInstrument(gateway.IDVirtualAccount) {
		t.Fatal("instrument support must be opt-in (off until EnableInstruments)")
	}
	if a.SupportsInstrument(gateway.IDEWallet) {
		t.Error("e-wallets are never issued directly")
	}

	a.EnableInstruments(gateway.IDQRIS, gateway.IDVirtualAccount)
	if !a.SupportsInstrument(gateway.IDQRIS) || !a.SupportsInstrument(gateway.IDVirtualAccount) {
		t.Error("EnableInstruments should grant QRIS and VA support")
	}
	if a.SupportsInstrument(gateway.IDEWallet) {
		t.Error("e-wallets must still go through the hosted link")
	}
}

// v2Server returns a test server that records the request and replies with the
// given data object. For status >= 400 the "__message" key is used as the
// envelope's messages field (the body Mayar sends on a business rejection).
func v2Server(t *testing.T, status int, data map[string]any) (*httptest.Server, *map[string]any, *string) {
	t.Helper()
	var gotBody map[string]any
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		resp := map[string]any{"statusCode": status}
		if status >= 400 {
			resp["messages"] = data["__message"]
			delete(data, "__message")
		} else {
			resp["messages"] = "success"
		}
		resp["data"] = data
		_ = json.NewEncoder(w).Encode(resp)
	}))
	return srv, &gotBody, &gotPath
}

// qrisDetail builds the real QR_CODE paymentDetail shape Mayar returns.
func qrisDetail(qrString string) map[string]any {
	return map[string]any{
		"id":   "pm-qris",
		"type": "QR_CODE",
		"qr_code": map[string]any{
			"channel_properties": map[string]any{"qr_string": qrString},
		},
		"actions": []any{},
	}
}

// vaDetail builds the real VIRTUAL_ACCOUNT paymentDetail shape Mayar returns.
func vaDetail(channelCode, number, customer string) map[string]any {
	return map[string]any{
		"id":   "pm-va",
		"type": "VIRTUAL_ACCOUNT",
		"virtual_account": map[string]any{
			"channel_code": channelCode,
			"channel_properties": map[string]any{
				"customer_name":          customer,
				"virtual_account_number": number,
				"expires_at":             "2026-08-14T02:12:14Z",
			},
		},
		"actions": []any{},
	}
}

func TestIssueQRIS(t *testing.T) {
	srv, gotBody, gotPath := v2Server(t, http.StatusOK, map[string]any{
		"id":            "pay-123",
		"transactionId": "tx-456",
		"paymentLinkId": "pl-789",
		"link":          "https://aifarm.myr.id/invoices/abc",
		"expiredAt":     "2026-08-14T02:12:14Z",
		"paymentDetail": qrisDetail("00020101021226570011..."),
	})
	defer srv.Close()

	a := newForTest(testAPIKey, testToken, srv.URL, nil)
	a.EnableInstruments(gateway.IDQRIS)

	res, err := a.IssueInstrument(context.Background(), &gateway.CreatePaymentInput{
		Reference:         "pi_test",
		AmountMinor:       500,
		Currency:          "idr",
		PaymentMethodType: gateway.IDQRIS,
		Customer:          &gateway.Customer{Name: "Arief", Email: "a@b.test", Phone: "62856"},
	})
	if err != nil {
		t.Fatalf("IssueInstrument: %v", err)
	}
	if *gotPath != "/hl/v2/payments/create" {
		t.Errorf("path = %q; want /hl/v2/payments/create", *gotPath)
	}
	if (*gotBody)["paymentMethod"] != "qris/QRIS" {
		t.Errorf("paymentMethod = %v; want qris/QRIS", (*gotBody)["paymentMethod"])
	}
	if (*gotBody)["amount"] != float64(500) {
		t.Errorf("amount = %v; want 500", (*gotBody)["amount"])
	}
	if (*gotBody)["email"] != "a@b.test" || (*gotBody)["mobile"] != "62856" {
		t.Errorf("customer fields not forwarded: %v", *gotBody)
	}

	if res.GatewayReference != "pay-123" {
		t.Errorf("gateway ref = %q; want data.id", res.GatewayReference)
	}
	if res.Status != stripe.PaymentIntentStatusRequiresAction {
		t.Errorf("status = %s; want requires_action", res.Status)
	}
	if res.Display == nil || res.Display.QRIS == nil {
		t.Fatalf("QRIS display missing")
	}
	if res.Display.QRIS.Payload != "00020101021226570011..." {
		t.Errorf("payload = %q", res.Display.QRIS.Payload)
	}
	if res.Display.QRIS.ImageURL != "" {
		t.Errorf("image url should be empty (Mayar returns raw payload); got %q", res.Display.QRIS.ImageURL)
	}
	if res.ExpiresAt == 0 {
		t.Error("expiry should be parsed")
	}
}

func TestIssueQRISMissingQRString(t *testing.T) {
	// QR_CODE type but the qr_string is absent: cannot render -> redirect fallback.
	srv, _, _ := v2Server(t, http.StatusOK, map[string]any{
		"id": "pay-empty", "transactionId": "tx-empty",
		"paymentDetail": map[string]any{"id": "pm", "type": "QR_CODE", "qr_code": map[string]any{}, "actions": []any{}},
	})
	defer srv.Close()

	a := newForTest(testAPIKey, testToken, srv.URL, nil)
	a.EnableInstruments(gateway.IDQRIS)

	_, err := a.IssueInstrument(context.Background(), &gateway.CreatePaymentInput{
		Reference: "pi_test", AmountMinor: 500, Currency: "idr", PaymentMethodType: gateway.IDQRIS,
	})
	if !errors.Is(err, gateway.ErrInstrumentUnsupported) {
		t.Errorf("err = %v; want ErrInstrumentUnsupported when qr_string missing", err)
	}
}

func TestIssueVirtualAccount(t *testing.T) {
	srv, gotBody, gotPath := v2Server(t, http.StatusOK, map[string]any{
		"id": "pay-va", "transactionId": "tx-va",
		"paymentDetail": vaDetail("BSI", "8890812345678", "Toko Anda"),
	})
	defer srv.Close()

	a := newForTest(testAPIKey, testToken, srv.URL, nil)
	a.EnableInstruments(gateway.IDVirtualAccount)

	res, err := a.IssueInstrument(context.Background(), &gateway.CreatePaymentInput{
		Reference:         "pi_test",
		AmountMinor:       500,
		Currency:          "idr",
		PaymentMethodType: gateway.IDVirtualAccount,
		MethodParams:      map[string]any{"bank": "BSI"},
	})
	if err != nil {
		t.Fatalf("IssueInstrument: %v", err)
	}
	if *gotPath != "/hl/v2/payments/create" {
		t.Errorf("path = %q", *gotPath)
	}
	if (*gotBody)["paymentMethod"] != "va/BSI" {
		t.Errorf("paymentMethod = %v; want va/BSI", (*gotBody)["paymentMethod"])
	}
	if res.Display == nil || res.Display.VirtualAccount == nil {
		t.Fatal("VA display missing")
	}
	if res.Display.VirtualAccount.Bank != "bsi" {
		t.Errorf("bank = %q; want bsi (lowercased channel_code)", res.Display.VirtualAccount.Bank)
	}
	if res.Display.VirtualAccount.AccountNumber != "8890812345678" {
		t.Errorf("account = %q", res.Display.VirtualAccount.AccountNumber)
	}
	if res.Display.VirtualAccount.AccountName != "Toko Anda" {
		t.Errorf("account name = %q; want customer_name", res.Display.VirtualAccount.AccountName)
	}
}

func TestIssueVirtualAccountUnsupportedBank(t *testing.T) {
	// Mayar does not issue bca virtual accounts: fall back to redirect, no HTTP call.
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
	}))
	defer srv.Close()

	a := newForTest(testAPIKey, testToken, srv.URL, nil)
	a.EnableInstruments(gateway.IDVirtualAccount)

	_, err := a.IssueInstrument(context.Background(), &gateway.CreatePaymentInput{
		Reference:         "pi_test",
		AmountMinor:       500,
		PaymentMethodType: gateway.IDVirtualAccount,
		MethodParams:      map[string]any{"bank": "bca"},
	})
	if !errors.Is(err, gateway.ErrInstrumentUnsupported) {
		t.Errorf("err = %v; want ErrInstrumentUnsupported", err)
	}
	if called {
		t.Error("no HTTP call should be made for an unsupported bank")
	}
}

func TestIssueInstrumentRequiresBank(t *testing.T) {
	a := newForTest(testAPIKey, testToken, "http://unused", nil)
	a.EnableInstruments(gateway.IDVirtualAccount)

	_, err := a.IssueInstrument(context.Background(), &gateway.CreatePaymentInput{
		Reference:         "pi_test",
		AmountMinor:       500,
		PaymentMethodType: gateway.IDVirtualAccount,
	})
	if err == nil {
		t.Fatal("VA issuance without a bank should error")
	}
	if errors.Is(err, gateway.ErrInstrumentUnsupported) {
		t.Error("missing-bank is a caller error, not an unsupported-method fallback")
	}
}

// TestIssueInstrumentChannelNotConfigured is the runtime safety net: the operator
// listed the method in MAYAR_INSTRUMENT_METHODS but the channel is not yet live on
// the Mayar dashboard. Mayar returns 400, and we map it to ErrInstrumentUnsupported
// so the caller falls back to the hosted redirect instead of failing the checkout.
func TestIssueInstrumentChannelNotConfigured(t *testing.T) {
	srv, _, _ := v2Server(t, http.StatusBadRequest, map[string]any{
		"__message": "Payment channel configuration not found",
	})
	defer srv.Close()

	a := newForTest(testAPIKey, testToken, srv.URL, nil)
	a.EnableInstruments(gateway.IDQRIS)

	_, err := a.IssueInstrument(context.Background(), &gateway.CreatePaymentInput{
		Reference: "pi_test", AmountMinor: 500, Currency: "idr", PaymentMethodType: gateway.IDQRIS,
	})
	if !errors.Is(err, gateway.ErrInstrumentUnsupported) {
		t.Errorf("err = %v; want ErrInstrumentUnsupported (graceful fallback)", err)
	}
}

func TestIssueInstrumentNotEnabled(t *testing.T) {
	// Without EnableInstruments, issuance is off — regardless of the API's ability.
	a := newForTest(testAPIKey, testToken, "http://unused", nil)
	if a.SupportsInstrument(gateway.IDQRIS) {
		t.Fatal("QRIS should not be supported before EnableInstruments")
	}
	_, err := a.IssueInstrument(context.Background(), &gateway.CreatePaymentInput{
		Reference: "pi_test", AmountMinor: 500, PaymentMethodType: gateway.IDQRIS,
	})
	if !errors.Is(err, gateway.ErrInstrumentUnsupported) {
		t.Errorf("err = %v; want ErrInstrumentUnsupported when not enabled", err)
	}
}

func TestIssueInstrumentUnsupportedMethod(t *testing.T) {
	a := newForTest(testAPIKey, testToken, "http://unused", nil)
	a.EnableInstruments(gateway.IDQRIS, gateway.IDVirtualAccount)

	_, err := a.IssueInstrument(context.Background(), &gateway.CreatePaymentInput{
		Reference:         "pi_test",
		AmountMinor:       500,
		PaymentMethodType: gateway.IDEWallet,
	})
	if !errors.Is(err, gateway.ErrInstrumentUnsupported) {
		t.Errorf("err = %v; want ErrInstrumentUnsupported for ewallet", err)
	}
}

func TestIssueInstrumentValidation(t *testing.T) {
	a := newForTest(testAPIKey, testToken, "http://unused", nil)
	a.EnableInstruments(gateway.IDQRIS)

	if _, err := a.IssueInstrument(context.Background(), nil); err == nil {
		t.Error("nil input should error")
	}
	if _, err := a.IssueInstrument(context.Background(), &gateway.CreatePaymentInput{Reference: "pi_"}); err == nil {
		t.Error("non-positive amount should error")
	}
}

func TestIssueQRISNoPaymentDetail(t *testing.T) {
	// paymentDetail absent entirely: cannot render on our page -> redirect fallback.
	srv, _, _ := v2Server(t, http.StatusOK, map[string]any{
		"id": "pay-none", "transactionId": "tx-none",
	})
	defer srv.Close()

	a := newForTest(testAPIKey, testToken, srv.URL, nil)
	a.EnableInstruments(gateway.IDQRIS)

	_, err := a.IssueInstrument(context.Background(), &gateway.CreatePaymentInput{
		Reference: "pi_test", AmountMinor: 500, Currency: "idr", PaymentMethodType: gateway.IDQRIS,
	})
	if !errors.Is(err, gateway.ErrInstrumentUnsupported) {
		t.Errorf("err = %v; want ErrInstrumentUnsupported when nothing to render", err)
	}
}
