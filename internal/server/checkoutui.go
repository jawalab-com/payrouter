package server

import (
	"bytes"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jawalab-com/payrouter/internal/gateway"
	"github.com/jawalab-com/payrouter/internal/store"
	qrcode "github.com/skip2/go-qrcode"
)

// Hosted checkout UI.
//
// These routes are the reason least-cost routing can work on a checkout session
// at all. The Stripe-compatible API creates a session without touching a
// gateway; the customer picks a method here; only then does the orchestrator
// know a concrete channel, price it across gateways, and ask the cheapest one to
// issue an instrument that this page renders.
//
// Everything here is PUBLIC and unauthenticated — a customer's browser cannot
// hold an sk_ key. The UI is ON by default for orchestrated gateways (the
// consistency it gives a multi-gateway checkout is the whole point), OFF for a
// single static gateway, and PAYMENT_CHECKOUT_UI overrides either way; see
// config.resolveCheckoutUI. Either way, enabling it widens the perimeter from a
// machine-to-machine API to a public web surface, so the consequences enforced
// below stand regardless: no session field that is not needed for payment is ever
// rendered, responses are marked non-cacheable and non-indexable, and the page
// ships no external references so a strict CSP holds.

//go:embed checkoutui/page.gohtml
var checkoutTemplates embed.FS

// checkoutTmpl is parsed once at startup; a parse failure is a programming error
// in an embedded asset, so it panics rather than failing per request.
var checkoutTmpl = template.Must(template.ParseFS(checkoutTemplates, "checkoutui/page.gohtml"))

// checkoutBasePath is the public URL prefix for the hosted checkout.
const checkoutBasePath = "/checkout/"

// checkoutBaseURL resolves the absolute base used both to build checkout links
// (handed to a customer's browser) and to recognize our own page on redirect. It
// prefers an explicitly configured PAYMENT_PUBLIC_URL — canonical and immune to
// Host-header spoofing. When that is unset the UI still works with zero config:
// the base is derived from the incoming request, honoring X-Forwarded-Proto and
// X-Forwarded-Host only when the operator has opted into PAYMENT_TRUST_PROXY_HEADERS
// (e.g. behind a reverse proxy that overwrites Host). Production deployments should
// still set PAYMENT_PUBLIC_URL explicitly — main.go warns when it is missing —
// because request-inferred links otherwise depend on headers a client can influence.
func (s *Server) checkoutBaseURL(r *http.Request) string {
	if s.cfg.PublicURL != "" {
		return s.cfg.PublicURL + checkoutBasePath
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	if s.cfg.TrustProxyHeaders {
		if proto := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); proto != "" {
			scheme = proto
		}
		if h := strings.TrimSpace(r.Header.Get("X-Forwarded-Host")); h != "" {
			host = h
		}
	}
	return scheme + "://" + host + checkoutBasePath
}

// checkoutView is the data handed to the templates. It carries only what the
// page needs to display: no metadata, no gateway references, no client secret.
type checkoutView struct {
	Brand       string
	BasePath    string
	AmountText  string
	Description string
	Error       string

	Methods []checkoutMethod

	// Instrument display.
	Content       template.HTML // pre-rendered body, injected into the layout
	QRImage       template.URL  // data: URI of a QR we rendered ourselves
	QRFallbackURL string        // provider-hosted image, when only that is available
	VANumber      string
	BankLabel     string
	AccountName   string
	BillerCode    string
	ExpiresText   string
}

// checkoutMethod is one selectable payment method, with optional sub-choices
// (which bank, which wallet).
type checkoutMethod struct {
	Method  string
	Label   string
	Options []checkoutOption
}

type checkoutOption struct{ Value, Label string }

// registerCheckoutUI adds the public checkout routes. Called only when the UI is
// enabled (default ON for orchestrated gateways, see config.resolveCheckoutUI), so
// with it off these paths simply do not exist.
func (s *Server) registerCheckoutUI() {
	s.mux.HandleFunc("GET /checkout/{id}", s.checkoutPage)
	s.mux.HandleFunc("POST /checkout/{id}/pay", s.checkoutPay)
	s.mux.HandleFunc("GET /checkout/{id}/status", s.checkoutStatus)
}

// checkoutPage renders the method picker, or the instrument if one was already
// issued — so a customer who reloads or returns later sees the same numbers
// rather than being issued a second instrument for one order.
func (s *Server) checkoutPage(w http.ResponseWriter, r *http.Request) {
	sess, pi, ok := s.loadCheckout(w, r)
	if !ok {
		return
	}

	view := s.baseView(sess)
	if display := decodeDisplay(pi); display != nil {
		s.renderInstrument(w, r, view, display)
		return
	}
	view.Methods = s.availableMethods()
	if len(view.Methods) == 0 {
		// Nothing can be issued directly; the hosted redirect is the only path.
		s.redirectToHosted(w, r, sess, pi)
		return
	}
	s.renderCheckout(w, http.StatusOK, "picker", view)
}

// checkoutPay handles the method choice: route it, issue the instrument, store
// it, and show it.
func (s *Server) checkoutPay(w http.ResponseWriter, r *http.Request) {
	sess, pi, ok := s.loadCheckout(w, r)
	if !ok {
		return
	}
	if !parseForm(w, r) {
		return
	}

	// Already issued: never issue a second instrument for the same order, even
	// if the customer double-submits or navigates back.
	if display := decodeDisplay(pi); display != nil {
		s.renderInstrument(w, r, s.baseView(sess), display)
		return
	}

	method, params, err := methodFromForm(r)
	if err != nil {
		view := s.baseView(sess)
		view.Methods = s.availableMethods()
		view.Error = err.Error()
		s.renderCheckout(w, http.StatusBadRequest, "picker", view)
		return
	}

	issuer, _ := s.gw.(gateway.InstrumentGateway)
	if issuer == nil || !issuer.SupportsInstrument(method) {
		s.redirectToHosted(w, r, sess, pi)
		return
	}

	res, err := issuer.IssueInstrument(r.Context(), &gateway.CreatePaymentInput{
		Reference:         pi.ID,
		AmountMinor:       sess.AmountTotal,
		Currency:          sess.Currency,
		Description:       sess.Description,
		PaymentMethodType: method,
		MethodParams:      params,
		ReturnURL:         sess.SuccessURL,
	})
	if err != nil {
		if errors.Is(err, gateway.ErrInstrumentUnsupported) {
			s.redirectToHosted(w, r, sess, pi)
			return
		}
		slog.Error("checkout: instrument issuance failed",
			"request_id", RequestID(r.Context()),
			"session", sess.ID, "method", method, "error", err)

		view := s.baseView(sess)
		view.Methods = s.availableMethods()
		// Deliberately generic: the underlying error can name gateways, endpoints
		// and credentials, none of which belong in front of a customer.
		view.Error = "Metode pembayaran ini sedang tidak tersedia. Silakan pilih metode lain."
		s.renderCheckout(w, http.StatusBadGateway, "picker", view)
		return
	}

	s.persistInstrument(pi, res, method)

	// POST/redirect/GET so a refresh does not resubmit the choice.
	http.Redirect(w, r, checkoutBasePath+sess.ID, http.StatusSeeOther)
}

// checkoutStatus is the polling endpoint behind the live status line. It returns
// the minimum a waiting page needs and nothing else.
func (s *Server) checkoutStatus(w http.ResponseWriter, r *http.Request) {
	sess, pi, ok := s.loadCheckout(w, r)
	if !ok {
		return
	}
	paid := sess.PaymentStatus == "paid" || pi.Status == "succeeded"
	body := map[string]any{
		"paid":    paid,
		"expired": !paid && sess.ExpiresAt > 0 && time.Now().Unix() > sess.ExpiresAt,
	}
	if paid && sess.SuccessURL != "" {
		body["redirect_url"] = sess.SuccessURL
	}
	noStore(w)
	writeJSON(w, http.StatusOK, body)
}

// loadCheckout resolves the session and its intent, rejecting anything not
// currently payable. Failures are deliberately uniform: a wrong id and a closed
// session look identical from outside, so the endpoint cannot be used to probe
// which session ids exist.
func (s *Server) loadCheckout(w http.ResponseWriter, r *http.Request) (*store.Session, *store.PaymentIntent, bool) {
	id := r.PathValue("id")
	sess, err := s.store.GetSession(id)
	if err != nil || sess == nil || sess.Mode != "payment" {
		s.renderNotFound(w)
		return nil, nil, false
	}
	pi, err := s.store.Get(sess.PaymentIntentID)
	if err != nil || pi == nil {
		s.renderNotFound(w)
		return nil, nil, false
	}
	return sess, pi, true
}

// baseView builds the fields every page shares.
func (s *Server) baseView(sess *store.Session) checkoutView {
	return checkoutView{
		Brand:       s.cfg.BrandName,
		BasePath:    checkoutBasePath + sess.ID,
		AmountText:  formatAmount(sess.AmountTotal, sess.Currency),
		Description: sess.Description,
	}
}

// availableMethods lists the methods the configured gateway can actually issue,
// so the page never offers a choice that would fail after the customer taps it.
func (s *Server) availableMethods() []checkoutMethod {
	issuer, ok := s.gw.(gateway.InstrumentGateway)
	if !ok {
		return nil
	}
	var out []checkoutMethod
	if issuer.SupportsInstrument(gateway.IDQRIS) {
		out = append(out, checkoutMethod{Method: string(gateway.IDQRIS), Label: "QRIS"})
	}
	if issuer.SupportsInstrument(gateway.IDVirtualAccount) {
		out = append(out, checkoutMethod{
			Method: string(gateway.IDVirtualAccount),
			Label:  "Transfer Bank (Virtual Account)",
			Options: []checkoutOption{
				{Value: "bca", Label: "BCA"},
				{Value: "bni", Label: "BNI"},
				{Value: "bri", Label: "BRI"},
				{Value: "mandiri", Label: "Mandiri"},
				{Value: "permata", Label: "Permata"},
			},
		})
	}
	return out
}

// methodFromForm validates the submitted choice. Only known methods are
// accepted, and the sub-choice is passed through as an opaque parameter the
// adapter validates against its own supported list.
func methodFromForm(r *http.Request) (gateway.IDPaymentMethodType, map[string]any, error) {
	switch r.PostFormValue("method") {
	case string(gateway.IDQRIS):
		return gateway.IDQRIS, nil, nil
	case string(gateway.IDVirtualAccount):
		bank := strings.ToLower(strings.TrimSpace(r.PostFormValue("option")))
		if bank == "" {
			return "", nil, errors.New("Pilih bank tujuan transfer.")
		}
		return gateway.IDVirtualAccount, map[string]any{"bank": bank}, nil
	default:
		return "", nil, errors.New("Metode pembayaran tidak dikenal.")
	}
}

// persistInstrument records the issued instrument on the intent so the page can
// be reloaded and the webhook path can correlate the payment.
func (s *Server) persistInstrument(pi *store.PaymentIntent, res *gateway.PaymentResult, method gateway.IDPaymentMethodType) {
	if res == nil {
		return
	}
	pi.PaymentMethodType = string(method)
	pi.Status = string(res.Status)
	if res.GatewayReference != "" {
		pi.GatewayReference = res.GatewayReference
	}
	if res.Display != nil {
		if raw, err := json.Marshal(res.Display); err == nil {
			pi.DisplayJSON = string(raw)
		}
	}
	applyNextAction(pi, res.NextAction)
	s.store.Put(pi)
}

// decodeDisplay reads back a stored instrument, returning nil when none was
// issued or the stored value is unusable.
func decodeDisplay(pi *store.PaymentIntent) *gateway.DisplayInstructions {
	if pi == nil || pi.DisplayJSON == "" {
		return nil
	}
	var d gateway.DisplayInstructions
	if err := json.Unmarshal([]byte(pi.DisplayJSON), &d); err != nil {
		return nil
	}
	return &d
}

// renderInstrument fills the view from an issued instrument and renders it.
func (s *Server) renderInstrument(w http.ResponseWriter, r *http.Request, view checkoutView, d *gateway.DisplayInstructions) {
	switch d.Kind {
	case gateway.InstrumentQRIS:
		if d.QRIS == nil {
			s.renderNotFound(w)
			return
		}
		if d.QRIS.Payload != "" {
			// Render the QR ourselves and inline it as a data: URI. Rendering
			// locally keeps the page self-contained: no third-party image load on
			// a payment screen, and nothing breaks if the provider's CDN is down.
			if uri, err := qrDataURI(d.QRIS.Payload); err == nil {
				view.QRImage = template.URL(uri)
			} else {
				slog.Error("checkout: QR encode failed",
					"request_id", RequestID(r.Context()), "error", err)
			}
		}
		if view.QRImage == "" {
			view.QRFallbackURL = d.QRIS.ImageURL
		}
		if view.QRImage == "" && view.QRFallbackURL == "" {
			s.renderNotFound(w)
			return
		}
	case gateway.InstrumentVirtualAccount:
		if d.VirtualAccount == nil {
			s.renderNotFound(w)
			return
		}
		view.VANumber = d.VirtualAccount.AccountNumber
		view.BankLabel = bankLabel(d.VirtualAccount.Bank)
		view.AccountName = d.VirtualAccount.AccountName
		view.BillerCode = d.VirtualAccount.BillerCode
	default:
		s.renderNotFound(w)
		return
	}
	if d.ExpiresAt > 0 {
		view.ExpiresText = formatExpiry(d.ExpiresAt)
	}
	s.renderCheckout(w, http.StatusOK, "instructions", view)
}

// redirectToHosted sends the customer to the gateway's own page. It is the
// fallback whenever no configured gateway can issue an instrument for the
// chosen method — a redirect is always available.
func (s *Server) redirectToHosted(w http.ResponseWriter, r *http.Request, sess *store.Session, pi *store.PaymentIntent) {
	if sess.URL != "" && !strings.HasPrefix(sess.URL, s.checkoutBaseURL(r)) {
		http.Redirect(w, r, sess.URL, http.StatusSeeOther)
		return
	}
	res, err := s.gw.CreatePayment(r.Context(), &gateway.CreatePaymentInput{
		Reference:         pi.ID,
		AmountMinor:       sess.AmountTotal,
		Currency:          sess.Currency,
		Description:       sess.Description,
		PaymentMethodType: gateway.IDHosted,
		ReturnURL:         sess.SuccessURL,
	})
	if err != nil || res == nil || res.NextAction == nil || res.NextAction.RedirectToURL == nil {
		slog.Error("checkout: hosted fallback failed",
			"request_id", RequestID(r.Context()), "session", sess.ID, "error", err)
		view := s.baseView(sess)
		view.Error = "Pembayaran sedang tidak tersedia. Silakan coba beberapa saat lagi."
		s.renderCheckout(w, http.StatusBadGateway, "picker", view)
		return
	}
	s.persistInstrument(pi, res, gateway.IDHosted)
	sess.URL = res.NextAction.RedirectToURL.URL
	s.store.PutSession(sess)
	http.Redirect(w, r, sess.URL, http.StatusSeeOther)
}

// renderCheckout writes one of the page templates with the security headers a
// public payment page needs.
func (s *Server) renderCheckout(w http.ResponseWriter, status int, name string, view checkoutView) {
	// Two passes: render the body, then wrap it in the shared layout. Go
	// templates have no inheritance, and this keeps one <head> and one stylesheet
	// rather than duplicating them per page.
	var body bytes.Buffer
	if err := checkoutTmpl.ExecuteTemplate(&body, name, view); err != nil {
		slog.Error("checkout: template execute failed", "template", name, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	view.Content = template.HTML(body.String())

	var buf bytes.Buffer
	if err := checkoutTmpl.ExecuteTemplate(&buf, "layout", view); err != nil {
		slog.Error("checkout: layout execute failed", "template", name, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	// The page is entirely self-contained — inline CSS/JS and a data: URI image —
	// so it can be locked down to itself with no external origins allowed.
	h.Set("Content-Security-Policy",
		"default-src 'none'; img-src 'self' data: https:; style-src 'unsafe-inline'; "+
			"script-src 'unsafe-inline'; form-action 'self'; connect-src 'self'; "+
			"base-uri 'none'; frame-ancestors 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Robots-Tag", "noindex, nofollow")
	noStore(w)
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

func (s *Server) renderNotFound(w http.ResponseWriter) {
	noStore(w)
	http.Error(w, "Halaman pembayaran tidak ditemukan atau sudah tidak berlaku.", http.StatusNotFound)
}

// noStore prevents a payment page or status response from being cached by a
// browser, proxy, or shared device.
func noStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, private")
	w.Header().Set("Pragma", "no-cache")
}

// qrDataURI encodes a QRIS payload as an inline PNG data URI.
func qrDataURI(payload string) (string, error) {
	// Medium recovery balances resilience against a screen-scanned code with a
	// module count that stays legible on a phone.
	png, err := qrcode.Encode(payload, qrcode.Medium, 512)
	if err != nil {
		return "", fmt.Errorf("encode QR: %w", err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png), nil
}

// formatAmount renders a minor-unit amount for display. IDR is zero-decimal, so
// minor units are whole rupiah and are grouped with dots per local convention.
func formatAmount(minor int64, currency string) string {
	if !strings.EqualFold(currency, "idr") {
		return fmt.Sprintf("%s %d", strings.ToUpper(currency), minor)
	}
	return "Rp " + groupThousands(minor)
}

// groupThousands inserts dot separators, e.g. 1250000 -> "1.250.000".
func groupThousands(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	digits := fmt.Sprintf("%d", n)
	var b strings.Builder
	for i, c := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte('.')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// formatExpiry renders an expiry in WIB, the timezone an Indonesian payer reads
// their banking app in. A fixed offset is used because Indonesia observes no DST
// and the container carries no tzdata.
func formatExpiry(unix int64) string {
	return time.Unix(unix, 0).In(time.FixedZone("WIB", 7*60*60)).Format("2 Jan 2006, 15:04") + " WIB"
}

// bankLabel renders a bank code for display, falling back to upper case for any
// bank not listed.
func bankLabel(code string) string {
	switch strings.ToLower(code) {
	case "bca":
		return "BCA Virtual Account"
	case "bni":
		return "BNI Virtual Account"
	case "bri":
		return "BRI Virtual Account"
	case "mandiri":
		return "Mandiri Bill Payment"
	case "permata":
		return "Permata Virtual Account"
	case "cimb":
		return "CIMB Niaga Virtual Account"
	default:
		return strings.ToUpper(code) + " Virtual Account"
	}
}
