//go:build e2e_ui

// Package ui is an OPTIONAL, opt-in browser e2e layer for the hosted checkout
// UI. It is built with the `e2e_ui` tag and is NOT compiled or run by the
// default `tests/e2e/run.sh` (which stays bash-only). Run it via run-ui.sh.
//
// The layer drives a real Chromium through playwright-go against an in-process
// PayRouter server (stub gateway, PAYMENT_CHECKOUT_UI=true). The stub issues
// deterministic, scannable-but-non-payable QRIS/VA instruments, so the full
// surface — method picker, rendered QR/VA, copy button, live status poll, paid
// and expired transitions — is exercised without live gateway credentials or a
// separate process.
//
// Screenshot capture and a cursor/ripple overlay are OFF by default for fast,
// headless CI. Two independent flags:
//   - E2E_UI_SCREENSHOTS=1 captures a screenshot per step silently, headless —
//     no browser window ever opens. Use this in CI to attach artifact PNGs to
//     the report.
//   - E2E_UI_RECORD=1 is for live supervision: the browser runs headed, every
//     step is screenshotted, and a fake cursor + click ripples are injected so
//     a human watching can follow what Playwright is doing. (Implies screenshots.)
package ui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jawalab-com/payrouter/internal/adapters/stub"
	"github.com/jawalab-com/payrouter/internal/config"
	"github.com/jawalab-com/payrouter/internal/server"
	"github.com/jawalab-com/payrouter/internal/store"
	"github.com/mxschmitt/playwright-go"
)

// --- recording / global state ----------------------------------------------

var (
	pw           *playwright.Playwright
	record       bool // E2E_UI_RECORD=1: headed + screenshots + cursor (live supervision)
	screenshots  bool // E2E_UI_SCREENSHOTS=1: capture screenshots silently, headless
	slowMo       float64
	artifactsDir string
)

func TestMain(m *testing.M) {
	record = os.Getenv("E2E_UI_RECORD") == "1"
	// Screenshots are enabled by default so artifacts/ is always fresh.
	screenshots = os.Getenv("E2E_UI_SCREENSHOTS") != "0"
	if v := os.Getenv("E2E_UI_SLOWMO"); v != "" {
		fmt.Sscanf(v, "%f", &slowMo)
	} else if record {
		slowMo = 120 // slow interactions so the cursor overlay is watchable
	}
	artifactsDir = filepath.Join(".", "artifacts")
	_ = os.MkdirAll(artifactsDir, 0o755)

	// Optional self-install: run-ui.sh installs browsers itself, but this lets
	// `go test -tags e2e_ui` work standalone once E2E_UI_INSTALL=1 is set.
	if os.Getenv("E2E_UI_INSTALL") == "1" {
		if err := playwright.Install(&playwright.RunOptions{Browsers: []string{"chromium"}}); err != nil {
			fmt.Fprintf(os.Stderr, "playwright install failed: %v\n", err)
			os.Exit(1)
		}
	}

	var err error
	pw, err = playwright.Run()
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"playwright driver unavailable: %v\n"+
				"hint: run ./tests/e2e/ui/run-ui.sh (it installs chromium) or set E2E_UI_INSTALL=1\n", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = pw.Stop()
	os.Exit(code)
}

// expect returns the assertions facade with a UI-friendly default timeout so the
// status-poll transitions (every 3s) resolve without a brittle 5s ceiling.
func expect() playwright.PlaywrightAssertions { return playwright.NewPlaywrightAssertions(15000) }

// --- in-process PayRouter ---------------------------------------------------

// checkoutEnv is a live PayRouter serving the checkout UI on a random local port.
type checkoutEnv struct {
	t       *testing.T
	server  *httptest.Server
	store   *store.Memory
	baseURL string
	apiKey  string
}

func newCheckoutEnv(t *testing.T) *checkoutEnv {
	t.Helper()
	mem := store.NewMemory()
	cfg := config.Config{
		APIKey:        "sk_test_ui",
		ActiveGateway: "stub",
		CheckoutUI:    true,
		PublicURL:     "http://127.0.0.1:0", // the browser uses the real httptest address directly
		BrandName:     "Toko Playwright",
	}
	ts := httptest.NewServer(server.New(cfg, mem, stub.New()))
	env := &checkoutEnv{t: t, server: ts, store: mem, baseURL: ts.URL, apiKey: "sk_test_ui"}
	t.Cleanup(ts.Close)
	return env
}

// createSession creates a checkout session through the public Stripe-compatible
// API (the same path a merchant frontend uses) and returns its id.
func (e *checkoutEnv) createSession(t *testing.T) string {
	t.Helper()
	form := url.Values{
		"mode":                                   {"payment"},
		"success_url":                            {"https://shop.test/ok"},
		"line_items[0][price_data][currency]":    {"idr"},
		"line_items[0][price_data][unit_amount]": {"1250000"},
		"line_items[0][price_data][product_data][name]": {"Paket Pro"},
		"line_items[0][quantity]":                       {"1"},
	}
	req, err := http.NewRequest(http.MethodPost, e.baseURL+"/v1/checkout/sessions", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+e.apiKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create session: HTTP %d", resp.StatusCode)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode session: %v", err)
	}
	if out.ID == "" {
		t.Fatal("create session returned no id")
	}
	return out.ID
}

// statusJSON GETs the polling endpoint the page itself polls, returning the raw
// {paid, expired, redirect_url?} body. This is the contract the browser relies on.
func (e *checkoutEnv) statusJSON(t *testing.T, sessionID string) map[string]any {
	t.Helper()
	resp, err := http.Get(e.baseURL + "/checkout/" + sessionID + "/status")
	if err != nil {
		t.Fatalf("status get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: HTTP %d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	return body
}

// markPaid flips the session/intent to succeeded in-process. The stub gateway's
// ParseWebhook is a no-op, so this stands in for a gateway "payment received"
// callback — the same effect the webhook path would have on the stored intent.
func (e *checkoutEnv) markPaid(t *testing.T, sessionID string) {
	t.Helper()
	sess, err := e.store.GetSession(sessionID)
	if err != nil {
		t.Fatalf("markPaid: GetSession: %v", err)
	}
	pi, err := e.store.Get(sess.PaymentIntentID)
	if err != nil {
		t.Fatalf("markPaid: Get intent: %v", err)
	}
	pi.Status = "succeeded"
	e.store.Put(pi)
	sess.PaymentStatus = "paid"
	e.store.PutSession(sess)
}

// markExpired pushes the session expiry into the past so the status endpoint
// reports expired:true, exactly as a real order going stale would.
func (e *checkoutEnv) markExpired(t *testing.T, sessionID string) {
	t.Helper()
	sess, err := e.store.GetSession(sessionID)
	if err != nil {
		t.Fatalf("markExpired: GetSession: %v", err)
	}
	sess.ExpiresAt = time.Now().Unix() - 1
	e.store.PutSession(sess)
}

// --- browser ----------------------------------------------------------------

// newPage launches Chromium and returns a page with clipboard permission. In
// record mode the supervision cursor is injected too (it is only meaningful when
// a human is watching the headed browser; silent screenshot runs skip it so the
// PNGs are clean). The browser/context/page are closed on test cleanup.
func newPage(t *testing.T) playwright.Page {
	t.Helper()
	browser, err := pw.Chromium.Launch(playwright.BrowserTypeLaunchOptions{
		Headless: playwright.Bool(!record),
		SlowMo:   playwright.Float(slowMo),
	})
	if err != nil {
		t.Fatalf("launch browser: %v", err)
	}
	ctx, err := browser.NewContext(playwright.BrowserNewContextOptions{
		Permissions: []string{"clipboard-read", "clipboard-write"},
		Viewport:    &playwright.Size{Width: 1280, Height: 800},
	})
	if err != nil {
		t.Fatalf("new context: %v", err)
	}
	if record {
		if err := ctx.AddInitScript(playwright.Script{Content: playwright.String(supervisionCursor)}); err != nil {
			t.Fatalf("inject cursor: %v", err)
		}
	}
	page, err := ctx.NewPage()
	if err != nil {
		t.Fatalf("new page: %v", err)
	}
	t.Cleanup(func() {
		_ = ctx.Close()
		_ = browser.Close()
	})
	return page
}

// snap writes a full-page screenshot to artifacts/ when screenshot capture is on
// (E2E_UI_SCREENSHOTS=1 or E2E_UI_RECORD=1); a no-op in fast/CI mode so the
// default run adds zero I/O.
func snap(t *testing.T, page playwright.Page, name string) {
	t.Helper()
	if !screenshots {
		return
	}
	img, err := page.Screenshot(playwright.PageScreenshotOptions{FullPage: playwright.Bool(true)})
	if err != nil {
		t.Logf("screenshot %s: %v", name, err)
		return
	}
	path := filepath.Join(artifactsDir, sanitize(strings.ToLower(t.Name()))+"-"+name+".png")
	if err := os.WriteFile(path, img, 0o644); err != nil {
		t.Logf("write screenshot %s: %v", path, err)
	}
}

// sanitize makes a test name filesystem-safe.
func sanitize(s string) string {
	s = strings.ReplaceAll(s, "/", "_")
	return strings.ReplaceAll(s, " ", "_")
}
