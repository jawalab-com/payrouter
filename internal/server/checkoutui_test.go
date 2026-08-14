package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/jawalab-com/payrouter/internal/adapters/stub"
	"github.com/jawalab-com/payrouter/internal/config"
	"github.com/jawalab-com/payrouter/internal/store"
)

func newUIServer(t *testing.T) *Server {
	t.Helper()
	return New(config.Config{
		APIKey: "sk_test_x", ActiveGateway: "stub",
		CheckoutUI: true, PublicURL: "https://pay.test", BrandName: "Toko Test",
	}, store.NewMemory(), stub.New())
}

// newSession creates a checkout session through the API and returns its id.
func newSession(t *testing.T, srv *Server) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/checkout/sessions", strings.NewReader(
		"mode=payment&success_url=https://shop.test/ok"+
			"&line_items[0][price_data][currency]=idr"+
			"&line_items[0][price_data][unit_amount]=1250000"+
			"&line_items[0][price_data][product_data][name]=Paket Pro"+
			"&line_items[0][quantity]=1"))
	req.Header.Set("Authorization", "Bearer sk_test_x")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create session: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode session: %v", err)
	}
	if out.ID == "" {
		t.Fatalf("no session id in %s", rec.Body.String())
	}
	return out.ID
}

func uiGet(t *testing.T, srv *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func uiPost(t *testing.T, srv *Server, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// TestCheckoutRoutesAbsentWhenDisabled is the optionality guarantee: with the
// flag off the public surface does not exist at all.
func TestCheckoutRoutesAbsentWhenDisabled(t *testing.T) {
	srv := newTestServer(t) // CheckoutUI false

	for _, path := range []string{"/checkout/cs_x", "/checkout/cs_x/status"} {
		if rec := uiGet(t, srv, path); rec.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404 when the UI is disabled", path, rec.Code)
		}
	}
}

// TestSessionURLPointsAtOurPage: with the UI on, the session must not hand the
// customer a gateway URL — that is what defers gateway selection until the
// method is known.
func TestSessionURLPointsAtOurPage(t *testing.T) {
	srv := newUIServer(t)
	id := newSession(t, srv)

	sess, err := srv.store.GetSession(id)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if want := "https://pay.test/checkout/" + id; sess.URL != want {
		t.Errorf("session URL = %q, want %q", sess.URL, want)
	}

	// No gateway was contacted, so the intent has no method and no gateway ref.
	pi, _ := srv.store.Get(sess.PaymentIntentID)
	if pi.Status != "requires_payment_method" {
		t.Errorf("intent status = %q, want requires_payment_method", pi.Status)
	}
	if pi.GatewayReference != "" {
		t.Errorf("gateway reference = %q, want empty — no gateway should have been called yet", pi.GatewayReference)
	}
}

func TestPickerRendersOfferedMethods(t *testing.T) {
	srv := newUIServer(t)
	id := newSession(t, srv)

	rec := uiGet(t, srv, "/checkout/"+id)
	if rec.Code != http.StatusOK {
		t.Fatalf("picker = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"Toko Test", "Rp 1.250.000", "Paket Pro", "QRIS", "Virtual Account"} {
		if !strings.Contains(body, want) {
			t.Errorf("picker missing %q", want)
		}
	}
}

// TestPayIssuesInstrumentAndRedirects covers the POST/redirect/GET flow: a
// refresh after paying must not resubmit the method choice.
func TestPayIssuesInstrumentAndRedirects(t *testing.T) {
	srv := newUIServer(t)
	id := newSession(t, srv)

	rec := uiPost(t, srv, "/checkout/"+id+"/pay", url.Values{"method": {"id_qris"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("pay = %d, want 303: %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/checkout/"+id {
		t.Errorf("Location = %q, want the checkout page", loc)
	}

	page := uiGet(t, srv, "/checkout/"+id)
	if !strings.Contains(page.Body.String(), "data:image/png;base64,") {
		t.Error("instructions page has no inline QR image")
	}
}

// TestInstrumentIsNotReissuedOnRepeatSubmit: a customer who double-submits or
// navigates back must get the same numbers, not a second instrument they might
// pay into while the first is being reconciled.
func TestInstrumentIsNotReissuedOnRepeatSubmit(t *testing.T) {
	srv := newUIServer(t)
	id := newSession(t, srv)

	uiPost(t, srv, "/checkout/"+id+"/pay", url.Values{"method": {"id_virtual_account"}, "option": {"bca"}})
	sess, _ := srv.store.GetSession(id)
	pi, _ := srv.store.Get(sess.PaymentIntentID)
	first := pi.DisplayJSON
	if first == "" {
		t.Fatal("no instrument stored after first submit")
	}

	// Submit again, this time choosing a different method entirely.
	uiPost(t, srv, "/checkout/"+id+"/pay", url.Values{"method": {"id_qris"}})
	pi, _ = srv.store.Get(sess.PaymentIntentID)
	if pi.DisplayJSON != first {
		t.Error("a second submit replaced the issued instrument; the customer could pay the wrong one")
	}
}

// TestCheckoutPageLeaksNothingSensitive: these pages are public and
// unauthenticated, so anything rendered is effectively disclosed.
func TestCheckoutPageLeaksNothingSensitive(t *testing.T) {
	srv := newUIServer(t)
	id := newSession(t, srv)
	uiPost(t, srv, "/checkout/"+id+"/pay", url.Values{"method": {"id_qris"}})

	sess, _ := srv.store.GetSession(id)
	pi, _ := srv.store.Get(sess.PaymentIntentID)
	body := uiGet(t, srv, "/checkout/"+id).Body.String()

	for name, secret := range map[string]string{
		"client secret":     pi.ClientSecret,
		"API key":           "sk_test_x",
		"gateway reference": pi.GatewayReference,
		"intent id":         pi.ID,
	} {
		if secret != "" && strings.Contains(body, secret) {
			t.Errorf("checkout page exposes the %s", name)
		}
	}
	// Routing metadata names which gateway won; that is merchant information.
	if strings.Contains(body, "payment_routing_mode") || strings.Contains(body, "payment_gateway_selected") {
		t.Error("checkout page exposes routing metadata")
	}
}

func TestCheckoutSecurityHeaders(t *testing.T) {
	srv := newUIServer(t)
	id := newSession(t, srv)
	rec := uiGet(t, srv, "/checkout/"+id)

	h := rec.Header()
	if csp := h.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") ||
		!strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("weak or missing CSP: %q", csp)
	}
	if !strings.Contains(h.Get("Cache-Control"), "no-store") {
		t.Error("payment page is cacheable")
	}
	if !strings.Contains(h.Get("X-Robots-Tag"), "noindex") {
		t.Error("payment page is indexable")
	}
	if h.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing nosniff")
	}
}

// TestUnknownSessionIsIndistinguishable: a wrong id and a session that exists
// but is not payable must look identical, so the endpoint cannot be used to
// enumerate valid session ids.
func TestUnknownSessionIsIndistinguishable(t *testing.T) {
	srv := newUIServer(t)

	a := uiGet(t, srv, "/checkout/cs_doesnotexist")
	b := uiGet(t, srv, "/checkout/cs_alsomissing")
	if a.Code != http.StatusNotFound || b.Code != http.StatusNotFound {
		t.Fatalf("codes = %d/%d, want 404/404", a.Code, b.Code)
	}
	if a.Body.String() != b.Body.String() {
		t.Error("responses differ between unknown sessions, leaking which ids exist")
	}
}

func TestStatusEndpointIsMinimal(t *testing.T) {
	srv := newUIServer(t)
	id := newSession(t, srv)

	rec := uiGet(t, srv, "/checkout/"+id+"/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"paid":false`) {
		t.Errorf("body = %s, want a paid flag", body)
	}
	// An unpaid session must not disclose where a successful payment would go.
	if strings.Contains(body, "redirect_url") {
		t.Error("unpaid status exposes the success URL")
	}
}

func TestInvalidMethodReturnsToPicker(t *testing.T) {
	srv := newUIServer(t)
	id := newSession(t, srv)

	rec := uiPost(t, srv, "/checkout/"+id+"/pay", url.Values{"method": {"id_bogus"}})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "QRIS") {
		t.Error("error response should re-render the picker so the customer can retry")
	}
}

func TestVirtualAccountRequiresBankChoice(t *testing.T) {
	srv := newUIServer(t)
	id := newSession(t, srv)

	rec := uiPost(t, srv, "/checkout/"+id+"/pay", url.Values{"method": {"id_virtual_account"}})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 when no bank is chosen", rec.Code)
	}
}

// TestAmountFormatting pins the local convention; a misgrouped total is the
// fastest way to lose a customer's trust on a payment page.
func TestAmountFormatting(t *testing.T) {
	cases := map[int64]string{
		0: "Rp 0", 500: "Rp 500", 1000: "Rp 1.000",
		99000: "Rp 99.000", 1250000: "Rp 1.250.000", 1000000000: "Rp 1.000.000.000",
	}
	for minor, want := range cases {
		if got := formatAmount(minor, "idr"); got != want {
			t.Errorf("formatAmount(%d) = %q, want %q", minor, got, want)
		}
	}
	if got := formatAmount(1500, "usd"); got != "USD 1500" {
		t.Errorf("non-IDR fallback = %q", got)
	}
}
