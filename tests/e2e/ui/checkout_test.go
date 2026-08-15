//go:build e2e_ui

package ui

import (
	"strings"
	"testing"

	"github.com/mxschmitt/playwright-go"
)

// checkoutURL is the public page a customer's browser loads.
func (e *checkoutEnv) checkoutURL(sessionID string) string {
	return e.baseURL + "/checkout/" + sessionID
}

// qrisButton locates the QRIS method's submit button (the form carrying the
// id_qris hidden input), robust to markup reordering.
func qrisButton(page playwright.Page) playwright.Locator {
	return page.Locator("form:has(input[name='method'][value='id_qris']) button")
}

// vaBankButton locates a specific bank's direct 1-click submit button.
func vaBankButton(page playwright.Page, bank string) playwright.Locator {
	return page.Locator("form:has(input[name='option'][value='" + bank + "']) button")
}

// TestCheckout_PickerRenders: loading a fresh session shows the merchant, the
// formatted total, and payable methods with crisp brand logos.
func TestCheckout_PickerRenders(t *testing.T) {
	env := newCheckoutEnv(t)
	page := newPage(t)
	id := env.createSession(t)

	if _, err := page.Goto(env.checkoutURL(id)); err != nil {
		t.Fatalf("goto: %v", err)
	}
	snap(t, page, "01-picker")

	expect := expect()
	expect.Page(page).ToHaveTitle(playwright.String("Toko Playwright — Pembayaran Aman"))
	expect.Locator(page.Locator(".brand")).ToHaveText(playwright.String("Toko Playwright"))
	expect.Locator(page.Locator(".total")).ToHaveText(playwright.String("Rp 1.250.000"))
	expect.Locator(page.Locator(".desc")).ToHaveText(playwright.String("Paket Pro"))
	expect.Locator(qrisButton(page)).ToBeVisible()
	expect.Locator(page.Locator("#open-va-list")).ToBeVisible()
}

// TestCheckout_QRISFlow: the core hosted-checkout promise — pick QRIS, get a
// rendered QR with no gateway redirect, then watch the live status line flip to
// paid when the order is settled. One customer, one uninterrupted journey.
func TestCheckout_QRISFlow(t *testing.T) {
	env := newCheckoutEnv(t)
	page := newPage(t)
	id := env.createSession(t)

	if _, err := page.Goto(env.checkoutURL(id)); err != nil {
		t.Fatalf("goto: %v", err)
	}
	if err := qrisButton(page).Click(); err != nil {
		t.Fatalf("click QRIS: %v", err)
	}

	// POST/303/GET lands on the instructions page with a self-rendered QR.
	expect := expect()
	expect.Locator(page.Locator("img.qr")).ToBeVisible()
	src, err := page.Locator("img.qr").GetAttribute("src")
	if err != nil || !strings.HasPrefix(src, "data:image/png") {
		t.Errorf("QR src = %q, want a data:image/png; err=%v", src, err)
	}
	expect.Locator(page.Locator("#statustext")).ToContainText(playwright.String("Menunggu verifikasi pembayaran"))
	snap(t, page, "02-qris-waiting")

	// No redirect happened: we are still on our own checkout page.
	if got := strings.TrimPrefix(page.URL(), env.baseURL); got != "/checkout/"+id {
		t.Errorf("after QRIS, URL = %s, want to stay on /checkout/%s", page.URL(), id)
	}

	// The status JSON the page polls must still be unpaid and must not leak the
	// success URL (the contract the waiting UI depends on).
	if body := env.statusJSON(t, id); body["paid"] != false || body["expired"] != false {
		t.Errorf("waiting status = %v, want paid=false expired=false", body)
	}

	// Settle the order and watch the polling line update.
	env.markPaid(t, id)
	expect.Locator(page.Locator("#statustext")).ToContainText(playwright.String("Pembayaran Diterima!"))
	expect.Locator(page.Locator("#status")).ToHaveClass(playwright.String("paid"))
	snap(t, page, "03-qris-paid")

	if body := env.statusJSON(t, id); body["paid"] != true {
		t.Errorf("paid status = %v, want paid=true", body)
	}
}

// TestCheckout_VirtualAccountFlow: choosing a bank yields a VA number the
// customer can copy. Selecting BNI proves the 1-click bank selector drives
// the issued bank.
func TestCheckout_VirtualAccountFlow(t *testing.T) {
	env := newCheckoutEnv(t)
	page := newPage(t)
	id := env.createSession(t)

	if _, err := page.Goto(env.checkoutURL(id)); err != nil {
		t.Fatalf("goto: %v", err)
	}
	if err := page.Locator("#open-va-list").Click(); err != nil {
		t.Fatalf("open VA list: %v", err)
	}
	if err := vaBankButton(page, "bni").Click(); err != nil {
		t.Fatalf("click BNI VA: %v", err)
	}

	expect := expect()
	expect.Locator(page.Locator("#vanum")).ToBeVisible()
	expect.Locator(page.Locator(".card")).ToContainText(playwright.String("BNI Virtual Account"))
	num, err := page.Locator("#vanum").InnerText()
	if err != nil || num == "" {
		t.Errorf("VA number = %q, err=%v", num, err)
	}
	snap(t, page, "04-va")

	// Clicking copy button confirms the number to clipboard and flips label to "Tersalin".
	copy := page.Locator(".copy")
	expect.Locator(copy).ToBeVisible()
	if attr, _ := copy.GetAttribute("data-copy"); attr != num {
		t.Errorf("data-copy = %q, want the VA number %q", attr, num)
	}
	if err := copy.Click(); err != nil {
		t.Fatalf("click copy: %v", err)
	}
	expect.Locator(copy).ToContainText(playwright.String("Tersalin"))
}

// TestCheckout_ExpiredTransition: an order going stale while waiting is reported
// as expired, both over the polling JSON and on the page.
func TestCheckout_ExpiredTransition(t *testing.T) {
	env := newCheckoutEnv(t)
	page := newPage(t)
	id := env.createSession(t)

	if _, err := page.Goto(env.checkoutURL(id)); err != nil {
		t.Fatalf("goto: %v", err)
	}
	if err := qrisButton(page).Click(); err != nil {
		t.Fatalf("click QRIS: %v", err)
	}

	expect := expect()
	expect.Locator(page.Locator("img.qr")).ToBeVisible()

	// Advance time past the session expiry on the store side.
	env.markExpired(t, id)

	// The polling endpoint reflects expired immediately.
	if body := env.statusJSON(t, id); body["expired"] != true {
		t.Fatalf("status = %v, want expired=true", body)
	}

	// And the page status line updates without a manual reload.
	expect.Locator(page.Locator("#statustext")).ToContainText(playwright.String("Kedaluwarsa"))
	snap(t, page, "05-expired")
}
