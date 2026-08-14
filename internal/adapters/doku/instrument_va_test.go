package doku

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/jawalab-com/payrouter/internal/gateway"
)

// vaResponseBody is the shape DOKU returns from create-va.
const vaResponseBody = `{
	"responseCode": "2002700",
	"responseMessage": "Successful",
	"virtualAccountData": {
		"virtualAccountNo": "0008888112345678901",
		"virtualAccountName": "Budi Santoso",
		"virtualAccountTrxType": "C",
		"totalAmount": {"value": "250000.00", "currency": "IDR"}
	}
}`

// withVA extends the SNAP stub server to answer create-va.
func withVA(t *testing.T, respBody string) (*snapServer, *http.Header, *[]byte) {
	t.Helper()
	srv := newSNAPServer(t)
	var vaHeaders http.Header
	var vaBody []byte

	base := srv.Config.Handler
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == createVAPath {
			vaHeaders = r.Header.Clone()
			vaBody, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, respBody)
			return
		}
		base.ServeHTTP(w, r)
	})
	return srv, &vaHeaders, &vaBody
}

func TestIssueVirtualAccountReturnsNumber(t *testing.T) {
	srv, vaHeaders, vaBody := withVA(t, vaResponseBody)
	_, keyPEM := testKey(t)
	a := newSNAPAdapter(t, srv, keyPEM)

	res, err := a.IssueInstrument(context.Background(), &gateway.CreatePaymentInput{
		Reference: "pi_abc", AmountMinor: 250000, Currency: "idr",
		PaymentMethodType: gateway.IDVirtualAccount,
		MethodParams:      map[string]any{"bank": "bca"},
		Customer:          &gateway.Customer{Name: "Budi Santoso"},
	})
	if err != nil {
		t.Fatalf("IssueInstrument: %v", err)
	}

	va := res.Display.VirtualAccount
	if va == nil {
		t.Fatal("no VirtualAccount instruction")
	}
	if va.AccountNumber != "0008888112345678901" {
		t.Errorf("AccountNumber = %q, want the number DOKU echoed back", va.AccountNumber)
	}
	if va.Bank != "bca" {
		t.Errorf("Bank = %q, want bca", va.Bank)
	}

	var body map[string]any
	if err := json.Unmarshal(*vaBody, &body); err != nil {
		t.Fatalf("request body not JSON: %v", err)
	}
	// A closed amount is the only type reconcilable against a fixed order total.
	if body["virtualAccountTrxType"] != vaTrxTypeClosed {
		t.Errorf("virtualAccountTrxType = %v, want C", body["virtualAccountTrxType"])
	}
	if body["trxId"] != "pi_abc" {
		t.Errorf("trxId = %v, want our pi_ reference", body["trxId"])
	}
	info, _ := body["additionalInfo"].(map[string]any)
	if info["channel"] != "VIRTUAL_ACCOUNT_BCA" {
		t.Errorf("channel = %v, want VIRTUAL_ACCOUNT_BCA", info["channel"])
	}
	amount, _ := body["totalAmount"].(map[string]any)
	if amount["value"] != "250000.00" {
		t.Errorf("totalAmount.value = %v, want a 2-decimal string", amount["value"])
	}
	if (*vaHeaders).Get("Authorization") != "Bearer tok-abc" {
		t.Error("create-va did not carry the B2B token")
	}
}

// TestPartnerServiceIDPaddingDiffersBetweenFieldAndVANumber is the subtle one.
// SNAP space-pads partnerServiceId in its own field but zero-pads the same value
// when it is embedded in virtualAccountNo. Using one padding for both yields a
// VA number that fails validation or addresses a different account.
func TestPartnerServiceIDPaddingDiffersBetweenFieldAndVANumber(t *testing.T) {
	srv, _, vaBody := withVA(t, vaResponseBody)
	_, keyPEM := testKey(t)
	a := newSNAPAdapter(t, srv, keyPEM) // PartnerServiceID "88881", 5 chars

	if _, err := a.IssueInstrument(context.Background(), &gateway.CreatePaymentInput{
		Reference: "pi_abc", AmountMinor: 1000,
		PaymentMethodType: gateway.IDVirtualAccount,
		MethodParams:      map[string]any{"bank": "bca"},
	}); err != nil {
		t.Fatalf("IssueInstrument: %v", err)
	}

	var body map[string]any
	_ = json.Unmarshal(*vaBody, &body)

	psid, _ := body["partnerServiceId"].(string)
	if psid != "   88881" {
		t.Errorf("partnerServiceId = %q, want %q (8 wide, SPACE padded)", psid, "   88881")
	}

	customerNo, _ := body["customerNo"].(string)
	vaNo, _ := body["virtualAccountNo"].(string)
	wantVA := "00088881" + customerNo
	if vaNo != wantVA {
		t.Errorf("virtualAccountNo = %q, want %q (8 wide, ZERO padded, then customerNo)", vaNo, wantVA)
	}
	// Guard the mistake directly: the two paddings must not be the same string.
	if strings.HasPrefix(vaNo, psid) {
		t.Error("virtualAccountNo embeds the space-padded prefix; it must be zero-padded")
	}
}

// TestCustomerNumberIsDeterministic: a retried payment must map to the same
// virtual account, or the customer ends up with two numbers for one order and
// whichever they pay into may not be the one being reconciled.
func TestCustomerNumberIsDeterministic(t *testing.T) {
	first := customerNumber("pi_abc")
	if first != customerNumber("pi_abc") {
		t.Error("customerNumber is not deterministic for the same reference")
	}
	if first == customerNumber("pi_def") {
		t.Error("different references produced the same customer number")
	}
	if len(first) > 20 {
		t.Errorf("customerNumber is %d digits, exceeding SNAP's 20-digit maximum", len(first))
	}
	for _, c := range first {
		if c < '0' || c > '9' {
			t.Fatalf("customerNumber contains a non-digit: %q", first)
		}
	}
}

// TestUnverifiedBankIsRejectedNotGuessed: DOKU's channel codes are not uniform
// (VIRTUAL_ACCOUNT_BCA but VIRTUAL_ACCOUNT_BANK_CIMB), so an unknown bank must
// fail loudly rather than have a code inferred for it.
func TestUnverifiedBankIsRejectedNotGuessed(t *testing.T) {
	srv, _, _ := withVA(t, vaResponseBody)
	_, keyPEM := testKey(t)
	a := newSNAPAdapter(t, srv, keyPEM)

	_, err := a.IssueInstrument(context.Background(), &gateway.CreatePaymentInput{
		Reference: "pi_abc", AmountMinor: 1000,
		PaymentMethodType: gateway.IDVirtualAccount,
		MethodParams:      map[string]any{"bank": "mandiri"},
	})
	if err == nil {
		t.Fatal("an unverified bank was accepted; its channel code would have been guessed")
	}
	if !strings.Contains(err.Error(), "mandiri") || !strings.Contains(err.Error(), "channel") {
		t.Errorf("err = %v, want the bank named and the override explained", err)
	}
}

// TestExplicitChannelOverrideIsHonored lets an operator use a bank before its
// code is verified here, without waiting on a code change.
func TestExplicitChannelOverrideIsHonored(t *testing.T) {
	srv, _, vaBody := withVA(t, vaResponseBody)
	_, keyPEM := testKey(t)
	a := newSNAPAdapter(t, srv, keyPEM)

	if _, err := a.IssueInstrument(context.Background(), &gateway.CreatePaymentInput{
		Reference: "pi_abc", AmountMinor: 1000,
		PaymentMethodType: gateway.IDVirtualAccount,
		MethodParams: map[string]any{
			"bank": "mandiri", "channel": "VIRTUAL_ACCOUNT_BANK_MANDIRI",
		},
	}); err != nil {
		t.Fatalf("IssueInstrument: %v", err)
	}

	var body map[string]any
	_ = json.Unmarshal(*vaBody, &body)
	info, _ := body["additionalInfo"].(map[string]any)
	if info["channel"] != "VIRTUAL_ACCOUNT_BANK_MANDIRI" {
		t.Errorf("channel = %v, want the explicit override", info["channel"])
	}
}

// TestVACapabilityNeedsPartnerServiceID: QRIS and VA need different credentials,
// so a partially configured adapter must enable only what it can actually do.
func TestVACapabilityNeedsPartnerServiceID(t *testing.T) {
	srv := newSNAPServer(t)
	_, keyPEM := testKey(t)

	a := newForTest("client-1", "secret-1", srv.URL, srv.Client())
	a.nowFn = fixedTime
	if err := a.EnableSNAP(SNAPConfig{
		PrivateKeyPEM: keyPEM, MerchantID: "m", TerminalID: "t", // no PartnerServiceID
	}); err != nil {
		t.Fatalf("EnableSNAP: %v", err)
	}

	if !a.SupportsInstrument(gateway.IDQRIS) {
		t.Error("QRIS should be supported with merchant/terminal ids")
	}
	if a.SupportsInstrument(gateway.IDVirtualAccount) {
		t.Error("VA reported as supported without a partner service id")
	}
	_, err := a.IssueInstrument(context.Background(), &gateway.CreatePaymentInput{
		Reference: "pi_x", AmountMinor: 1000,
		PaymentMethodType: gateway.IDVirtualAccount,
		MethodParams:      map[string]any{"bank": "bca"},
	})
	if !errors.Is(err, gateway.ErrInstrumentUnsupported) {
		t.Errorf("err = %v, want ErrInstrumentUnsupported so the caller falls back", err)
	}
}

// TestQRISCapabilityNeedsMerchantAndTerminal is the mirror case.
func TestQRISCapabilityNeedsMerchantAndTerminal(t *testing.T) {
	srv := newSNAPServer(t)
	_, keyPEM := testKey(t)

	a := newForTest("client-1", "secret-1", srv.URL, srv.Client())
	a.nowFn = fixedTime
	if err := a.EnableSNAP(SNAPConfig{
		PrivateKeyPEM: keyPEM, PartnerServiceID: "88881", // no merchant/terminal
	}); err != nil {
		t.Fatalf("EnableSNAP: %v", err)
	}

	if a.SupportsInstrument(gateway.IDQRIS) {
		t.Error("QRIS reported as supported without merchant/terminal ids")
	}
	if !a.SupportsInstrument(gateway.IDVirtualAccount) {
		t.Error("VA should be supported with a partner service id")
	}
}

// TestEnableSNAPRejectsAConfigThatEnablesNothing surfaces a misconfiguration
// rather than silently starting with no capabilities.
func TestEnableSNAPRejectsAConfigThatEnablesNothing(t *testing.T) {
	_, keyPEM := testKey(t)
	a := newForTest("client-1", "secret-1", "http://unused", nil)

	if err := a.EnableSNAP(SNAPConfig{PrivateKeyPEM: keyPEM}); err == nil {
		t.Fatal("expected an error when neither QRIS nor VA credentials are supplied")
	}
}

// TestVAFailureCodeIsDetected: SNAP returns rejections with a non-2xx
// responseCode, sometimes still under HTTP 200.
func TestVAFailureCodeIsDetected(t *testing.T) {
	srv, _, _ := withVA(t, `{"responseCode":"4092700","responseMessage":"Conflict duplicate VA"}`)
	_, keyPEM := testKey(t)
	a := newSNAPAdapter(t, srv, keyPEM)

	_, err := a.IssueInstrument(context.Background(), &gateway.CreatePaymentInput{
		Reference: "pi_abc", AmountMinor: 1000,
		PaymentMethodType: gateway.IDVirtualAccount,
		MethodParams:      map[string]any{"bank": "bca"},
	})
	if err == nil {
		t.Fatal("failing responseCode treated as success")
	}
	if !strings.Contains(err.Error(), "4092700") {
		t.Errorf("err = %v, want the responseCode surfaced", err)
	}
}
