# Payment facade E2E scenarios

Black-box HTTP scenarios for the Go facade, mirroring `scripts/e2e/` on the Rust
platform side: real `curl` requests against a running facade process, asserting
on the Stripe-shaped JSON it returns. Nothing here imports Go packages or talks
to PostgreSQL directly — it only exercises the public `/v1/*` surface, so it
works unmodified once `apps/payment-facade` is split into its own repository.

Prerequisites: `bash`, `curl`, `jq`.

```bash
./tests/e2e/run.sh
```

## Combined report (both layers)

`run-all.sh` runs the API layer above **and** the optional Playwright UI layer
(`ui/`), then generates a combined report — JUnit XML for CI and a
self-contained HTML page for humans. Unlike `run.sh`, it does **not** fail-fast:
each layer runs to completion and its exit code is captured, so a failure in one
layer still leaves you a full report covering both.

```bash
./tests/e2e/run-all.sh                 # API + UI (headless)
./tests/e2e/run-all.sh --record        # UI headed, with screenshots/video
./tests/e2e/run-all.sh --install       # first run: download Chromium for the UI layer
./tests/e2e/run-all.sh --open          # open the HTML report in a browser when done
E2E_SKIP_UI=1  ./tests/e2e/run-all.sh  # API only (no Chromium needed)
E2E_SKIP_API=1 ./tests/e2e/run-all.sh  # UI only
```

| Variable | Default | Notes |
| --- | --- | --- |
| `E2E_REPORT_DIR` | `tests/e2e/report/out` | where `e2e-report.html` / `.xml` are written |
| `E2E_SKIP_UI` | unset | `1` skips the Playwright layer |
| `E2E_SKIP_API` | unset | `1` skips the bash API layer |

The report generator (`tests/e2e/report`) consumes the bash layer's `api.jsonl`
(per-scenario JSON emitted by `lib/report.sh`) and the UI layer's `go test -json`
output, links UI screenshots/video into the HTML, and exits non-zero if any case
failed. It can also be run standalone:

```bash
go run ./tests/e2e/report \
  -api tests/e2e/report/out/api.jsonl \
  -ui  tests/e2e/report/out/ui.json \
  -artifacts tests/e2e/ui/artifacts \
  -out tests/e2e/report/out
```

## Targeting a running facade

| Variable | Default | Notes |
| --- | --- | --- |
| `PAYMENT_BASE_URL` | `http://localhost:8787` | facade base URL |
| `PAYMENT_API_KEY` | `sk_test_local_vamios` | must match the facade's `PAYMENT_API_KEY` (matches `infra/compose.yaml`'s default) |
| `PAYMENT_GATEWAY` | `stub` | informational — only changes which assertions the webhook/subscription scenarios expect; set it to match the server's actual `PAYMENT_GATEWAY` |

Start the facade locally with the stub gateway (no credentials, canned
responses — the default the suite is written against):

```bash
PAYMENT_API_KEY=sk_test_local_vamios PAYMENT_GATEWAY=stub go run ./cmd/facade
./tests/e2e/run.sh
```

## Coverage

- `health.sh` — liveness/readiness.
- `auth.sh` — missing/unknown Bearer key rejection.
- `customers.sh` — create/retrieve, 404 on unknown id.
- `catalog.sh` — products, one-time and recurring prices (inline and referenced
  product), validation errors.
- `payment_intents.sh` — create/retrieve/confirm, validation errors, and
  `Idempotency-Key` replay + payload-mismatch conflict.
- `refunds.sh` — full/partial refund, validation errors.
- `checkout.sh` — hosted Checkout Session in `mode=payment`.
- `subscriptions.sh` — create/retrieve/cancel; auto-skips with an explanation
  if the active gateway doesn't implement `SubscriptionGateway` (Xendit, DOKU,
  and Mayar don't yet — see `plans.md`).
- `webhooks.sh` — routing only (`404` for a gateway name that isn't active).
  Signature verification for a real provider isn't exercised — see below.

## Idempotency-Key behavior depends on storage mode

`Idempotency-Key` handling (`payment_intents.sh`) is only active when the
facade runs with `PAYMENT_DATABASE_URL` set (durable/PostgreSQL storage); the
in-memory store treats the header as a no-op. The scenario detects this from
the replay response itself and prints `SKIP` with an explanation rather than
failing when run against the in-memory store — this is expected in that mode,
not a bug.

## Testing against a real gateway sandbox (e.g. your own Xendit dev key)

Because every scenario here talks to the facade's Stripe-shaped `/v1/*` API —
never to a specific gateway's API directly — pointing the *facade* at a real
sandbox and re-running this same suite against it is a legitimate way to
exercise the Xendit adapter's translation logic:

```bash
PAYMENT_API_KEY=sk_test_local_vamios \
PAYMENT_GATEWAY=xendit \
XENDIT_SECRET_KEY=xnd_development_...   \
XENDIT_WEBHOOK_TOKEN=...                 \
go run ./cmd/facade

PAYMENT_GATEWAY=xendit ./tests/e2e/run.sh
```

What this does and doesn't prove:

- **Does** exercise real request/response translation for payment intents,
  checkout sessions, and refunds against Xendit's actual test-mode API —
  useful for catching adapter bugs (wrong field mapping, wrong status
  mapping, auth handling) that a mocked test can't.
- **Does not** exercise inbound webhook signature verification end-to-end,
  since that requires a `XENDIT_WEBHOOK_TOKEN` generated in the Xendit
  dashboard (separate from the Secret Key) and a publicly reachable URL for
  Xendit to call back to (e.g. via `ngrok http 8787`, with the tunnel URL
  registered as the Xendit callback URL). `webhooks.sh` only checks routing.
- **Subscriptions will skip**: the Xendit adapter doesn't implement
  `SubscriptionGateway` yet, so `subscriptions.sh` reports `SKIP` rather than
  a failure.
- Xendit enforces gateway-specific minimum amounts per payment method (e.g.
  virtual account minimums); the suite's default amounts were chosen against
  the stub gateway and may need bumping if a scenario gets a `502` from a
  live sandbox call.
- A **development/test-mode key never touches real money**, but it does hit
  Xendit's real infrastructure over the network — treat sandbox credentials
  with the same care as production ones (don't commit them; pass via
  environment only).
