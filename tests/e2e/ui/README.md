# Checkout UI e2e (optional, Playwright)

A browser-driven e2e layer for the hosted checkout UI. It is **opt-in and
separate** from the bash/curl harness in [`../`](../): the default
[`../run.sh`](../run.sh) stays pure bash and runs anywhere, while this layer
needs Chromium and is built behind the `e2e_ui` Go build tag so it never
compiles or runs unless asked.

## What it covers

A single customer journey, end to end, against the real checkout template with a
deterministic stub gateway (scannable-but-non-payable instruments — no live
credentials, no real money, no separate process):

| Test | Journey step |
|------|--------------|
| `TestCheckout_PickerRenders` | Load the page → merchant brand, formatted total, both methods present |
| `TestCheckout_QRISFlow` | Pick QRIS → QR renders (data-URI, no gateway redirect) → status JSON unpaid → settle order → status line flips to **Pembayaran diterima** |
| `TestCheckout_VirtualAccountFlow` | Pick a bank (BNI) → VA number + bank label render → copy button works |
| `TestCheckout_ExpiredTransition` | Order goes stale → status JSON `expired:true` → page shows **Pembayaran kedaluwarsa** |
| `TestCheckout_UnknownSessionIs404` | Bogus session id returns 404 (no enumeration) |

It exercises the actual code paths that matter: `server.registerCheckoutUI`,
instrument issuance, the `/checkout/{id}/status` polling contract, the
progressive-enhancement copy button, and the live-status JS.

## Run

```bash
# First time only — downloads Chromium (~150MB)
./tests/e2e/ui/run-ui.sh --install

# Headless, fast (CI default)
./tests/e2e/ui/run-ui.sh

# Headless, but capture a screenshot per step (silent — no browser window opens)
./tests/e2e/ui/run-ui.sh --screenshots

# Headed + screenshots + a fake cursor/click-ripple overlay, so a human
# can watch and follow what Playwright does
./tests/e2e/ui/run-ui.sh --record
```

Screenshots land in `artifacts/` (gitignored) and are produced with either
`--screenshots` or `--record`.

## Environment variables

| Var | Effect |
|-----|--------|
| `E2E_UI_INSTALL=1` | Install Chromium (same as `--install`) |
| `E2E_UI_SCREENSHOTS=1` | Headless + a screenshot per step (same as `--screenshots`) |
| `E2E_UI_RECORD=1` | Headed + screenshots + cursor (same as `--record`; implies screenshots) |
| `E2E_UI_SLOWMO=N` | Slow each Playwright step by N ms (default 120 in record mode) |

## How it works

`harness_test.go` spins up an in-process PayRouter (`server.New` + stub gateway +
`CheckoutUI: true`) on an `httptest.Server`, creates a checkout session through
the public `/v1/checkout/sessions` API, and points a real Chromium at
`/checkout/{id}`. Because the stub gateway's `ParseWebhook` is a no-op, the paid
and expired transitions are driven by mutating the in-process store directly —
the same state change a real gateway callback would produce.

Recording and the supervision cursor are **off by default** so the common run is
fast and headless. Turning on `E2E_UI_RECORD` switches to headed mode, captures a
screenshot per step, and injects a mouse-following dot plus click ripples
(`cursor_test.go`) so a headed run is easy to follow.

## Relationship to the bash harness

The bash [`../run.sh`](../run.sh) is the contract/regression net for the
Stripe-compatible **API**; this layer is the net for the **UI**. They are
intentionally independent: API changes don't need a browser, and UI changes don't
need to touch bash.
