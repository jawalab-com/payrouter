# stripe-compatible-facade

A thin, open-source **translation layer** that exposes a Stripe-compatible API in
front of Indonesian payment gateways (Midtrans, Xendit, DOKU, Mayar, …).

> **Philosophy:** translate, don't orchestrate. The merchant supplies *their own*
> gateway keys. This service never touches money — it translates Stripe-shaped
> requests into gateway calls and gateway events back into Stripe-shaped webhooks.
> Point an existing Stripe integration at it with only a base-URL + key change.

Status: **v2 — Subscriptions (Midtrans)**, on top of the M6 one-time surface. The
facade exposes the endpoints the widest range of libraries and frameworks depend on —
Checkout Sessions, Customers, Products/Prices, Refunds, and PaymentIntents
create/retrieve/confirm — plus recurring Prices, Subscriptions (create/retrieve/cancel),
and Invoice retrieve. One-time payments work on all four gateway adapters (Midtrans,
Xendit, DOKU, Mayar); subscriptions are Midtrans-only for now (Xendit/DOKU/Mayar return
"not supported"). The official `stripe-go` SDK drives the facade and verifies forwarded
webhooks via its own `webhook.ConstructEvent` (`internal/compat`), proving the drop-in
claim for the full surface. See [`plans.md`](./plans.md) for the full design, milestones,
and scope.

## Run

```bash
# Stub gateway (no credentials, returns canned data — good for UI dev):
export FACADE_API_KEY=sk_test_...   # any Stripe-style key clients must present
export FACADE_GATEWAY=stub
go run ./cmd/facade                 # listens on :8787 by default (FACADE_ADDR)
```

```bash
# Midtrans gateway (merchant supplies their own sandbox Server Key):
export FACADE_API_KEY=sk_test_...
export FACADE_GATEWAY=midtrans
export MIDTRANS_SERVER_KEY=SB-Mid-server-...   # BYO merchant key
export MIDTRANS_SANDBOX=true                   # false -> production endpoints
go run ./cmd/facade
```

```bash
# Xendit gateway (merchant supplies their own Secret Key + Webhook Token):
export FACADE_API_KEY=sk_test_...
export FACADE_GATEWAY=xendit
export XENDIT_SECRET_KEY=xnd_development_...   # BYO merchant key
export XENDIT_WEBHOOK_TOKEN=...                # BYO webhook verification token
go run ./cmd/facade                             # env selected by key prefix
```

```bash
# DOKU gateway (merchant supplies their own Client-Id + Secret Key):
export FACADE_API_KEY=sk_test_...
export FACADE_GATEWAY=doku
export DOKU_CLIENT_ID=MCH-...                  # BYO merchant Client-Id
export DOKU_SECRET_KEY=SK-...                  # BYO merchant Secret Key (HMAC)
export DOKU_SANDBOX=true                       # false -> production endpoints
go run ./cmd/facade
```

```bash
# Mayar gateway (merchant supplies their own API Key + Webhook Token):
export FACADE_API_KEY=sk_test_...
export FACADE_GATEWAY=mayar
export MAYAR_API_KEY=...                       # BYO merchant API Key
export MAYAR_WEBHOOK_TOKEN=...                 # shared secret; append ?token= to the webhook URL
export MAYAR_SANDBOX=true                      # false -> production endpoints
go run ./cmd/facade
```

### Configuration

| Variable | Default | Notes |
| --- | --- | --- |
| `FACADE_API_KEY` | *(required)* | Stripe-style key clients must present (`sk_test_…` / `sk_live_…`) |
| `FACADE_ADDR` | `:8787` | listen address |
| `FACADE_GATEWAY` | `stub` | `stub`, `midtrans`, `xendit`, `doku`, or `mayar` |
| `MIDTRANS_SERVER_KEY` | *(required if gateway=midtrans)* | merchant Midtrans Server Key (BYO) |
| `MIDTRANS_SANDBOX` | `true` | `false` targets Midtrans production endpoints |
| `MIDTRANS_SNAP_URL` / `MIDTRANS_API_URL` | *(Midtrans default)* | override Snap / Core API base URLs (local or self-hosted Midtrans) |
| `XENDIT_SECRET_KEY` | *(required if gateway=xendit)* | merchant Xendit Secret API Key (BYO) |
| `XENDIT_WEBHOOK_TOKEN` | *(required if gateway=xendit)* | merchant Xendit Webhook Verification Token (BYO); compared against `x-callback-token` |
| `XENDIT_BASE_URL` | *(Xendit default)* | override the API base URL (local/self-hosted Xendit) |
| `DOKU_CLIENT_ID` / `DOKU_SECRET_KEY` | *(required if gateway=doku)* | merchant DOKU Client-Id + Secret Key (BYO) |
| `DOKU_SANDBOX` | `true` | `false` targets DOKU production endpoints |
| `DOKU_BASE_URL` | *(DOKU default)* | override the API base URL |
| `MAYAR_API_KEY` / `MAYAR_WEBHOOK_TOKEN` | *(required if gateway=mayar)* | merchant Mayar API Key + shared webhook secret (BYO) |
| `MAYAR_SANDBOX` | `true` | `false` targets Mayar production endpoints |
| `MAYAR_BASE_URL` | *(Mayar default)* | override the API base URL |
| `WEBHOOK_SIGNING_SECRET` | *(random, per process)* | `whsec_…` merchants use to verify our outbound `Stripe-Signature`. Set explicitly in production. |
| `FACADE_WEBHOOK_URL` | *(unset)* | merchant endpoint to forward Stripe events to. Unset = receive gateway notifications but don't forward. |

## Webhooks

The facade is a **two-way translator**: it receives gateway callbacks and re-emits
them as Stripe events.

1. Point your gateway's callback/Notification URL at the facade. The path carries the
   gateway name, so the facade verifies each callback with the right scheme:
   - **Midtrans:** set the Notification URL in the Midtrans dashboard to
     `https://<your-facade>/v1/webhooks/midtrans`. Verified via the body's `signature_key`
     (SHA-512 over `order_id + status_code + gross_amount + ServerKey`) using `MIDTRANS_SERVER_KEY`.
     For recurring billing, also set the **Recurring Notification URL** to the same
     `https://<your-facade>/v1/webhooks/midtrans` — the signature scheme is identical, and
     the facade tells a save-card / recurring-cycle notification apart by its payload shape.
   - **Xendit:** set the Callback URL in the Xendit dashboard to
     `https://<your-facade>/v1/webhooks/xendit`. Verified via the `x-callback-token` header
     (constant-time compared to `XENDIT_WEBHOOK_TOKEN`).
   - **DOKU:** set the Notification URL in the DOKU dashboard to
     `https://<your-facade>/v1/webhooks/doku`. Verified via the `Signature` header
     (HMAC-SHA256 over a canonical string of `Client-Id`/`Request-Id`/
     `Request-Timestamp`/`Request-Target`/`Digest` headers + base64 body digest) using
     `DOKU_SECRET_KEY`.
   - **Mayar:** Mayar does not sign payloads. Register a tokenized URL —
     `https://<your-facade>/v1/webhooks/mayar?token=<MAYAR_WEBHOOK_TOKEN>` — as the
     webhook in your Mayar settings. Verified via the `?token=` query param
     (constant-time compared to `MAYAR_WEBHOOK_TOKEN`).
2. The facade maps the status, wraps the PaymentIntent in a Stripe `Event`, and POSTs it
   to `FACADE_WEBHOOK_URL`, signed with a `Stripe-Signature` header (`t=…,v1=…`) using
   `WEBHOOK_SIGNING_SECRET`.
3. Your existing Stripe webhook handler verifies that signature and handles
   `payment_intent.succeeded` / `.canceled` / `.payment_failed` (and `refund.created`
   for refunds) exactly as it would for Stripe. For Midtrans subscriptions it also
   receives `customer.subscription.created` / `.updated` / `.deleted` and
   `invoice.payment_succeeded`.

```bash
export FACADE_WEBHOOK_URL=https://your-app.example.com/stripe-webhook
export WEBHOOK_SIGNING_SECRET=whsec_share_this_with_your_handler
```

> **Note (M2):** the delivery queue is in-memory — queued events are lost on restart.
> By design: this is a translation layer with **no database**. Re-sent notifications
> are handled idempotently (a status that hasn't changed doesn't re-emit).

> **Note (M5) — gateway coverage:** all four adapters implement create → hosted
> redirect → payment-notification → Stripe event. Midtrans and Xendit additionally
> support status polling (`GetStatus`) and refunds. DOKU (push-only checkout) and
> Mayar (no refund/status API) return a clear error for those two operations;
> their status arrives exclusively via the notification webhook.

## Stripe API surface

The facade implements the most-used Stripe endpoints for one-time payments and
(Midtrans) recurring billing, so an existing Stripe integration (stripe-go, stripe-js,
Next.js, Laravel Cashier, Django, Stripe CLI, …) works with only a base-URL + key
change. All endpoints accept Stripe-shaped form-encoded params and return Stripe-shaped
objects.

| Endpoint | Notes |
| --- | --- |
| `POST /v1/checkout/sessions` · `GET /v1/checkout/sessions/:id` | Redirect to the gateway hosted page — the natural fit for all four gateways. `line_items` from inline `price_data` or a `price` id; `mode=payment` (one-time) or `mode=subscription` (recurring, Midtrans). Payment mode emits `checkout.session.completed`. |
| `POST /v1/payment_intents` · `GET /v1/payment_intents/:id` · `POST /v1/payment_intents/:id/confirm` | Manual flow; `confirm` surfaces the redirect `next_action`. |
| `POST /v1/subscriptions` · `GET /v1/subscriptions/:id` · `DELETE /v1/subscriptions/:id` | Recurring billing (Midtrans). Create returns `status=incomplete` with the authorization redirect at `latest_invoice.payment_intent.next_action.redirect_to_url`; the save-card webhook then activates it. Emits `customer.subscription.created` / `.updated` / `.deleted`. |
| `GET /v1/invoices/:id` | Retrieve an invoice — the first on activation, then one per recurring cycle. |
| `POST /v1/refunds` · `GET /v1/refunds/:id` | Full or partial. Supported by Midtrans/Xendit; DOKU/Mayar return a clear error. |
| `POST /v1/customers` · `GET /v1/customers/:id` | Minimal store/passthrough. |
| `POST /v1/products` · `GET /v1/products/:id` · `POST /v1/prices` · `GET /v1/prices/:id` | Minimal catalog so Checkout/Subscriptions can reference price ids. Prices support `type=one_time` (default) and `type=recurring` (`recurring[interval]` ∈ day/week/month/year). |
| `POST /v1/webhooks/:gateway` | Inbound gateway callback (no Bearer auth; verified by the adapter). Also serves recurring notifications. |

One-time payments work on all four gateways. Subscriptions are **Midtrans-only** for now
(the gateway runs the recurring clock via gateway-side auto-debit); Xendit/DOKU/Mayar
return a "not supported" error on the subscription endpoints, and Mayar is one-time-only
by design (no gateway-side auto-debit). The facade runs **no scheduler** — every recurring
charge is fired by the gateway, and the only follow-up call the facade makes is the single
schedule-registration triggered by the save-card webhook.

## Try it

```bash
# create a payment intent (form-encoded, like the Stripe SDK sends)
curl http://localhost:8787/v1/payment_intents \
  -H "Authorization: Bearer sk_test_..." \
  -d amount=50000 \
  -d currency=idr \
  -d 'payment_method_types[]=id_virtual_account' \
  -d 'metadata[x_bank]=bca' \
  -d return_url=https://app.test/done

# retrieve it
curl http://localhost:8787/v1/payment_intents/pi_... \
  -H "Authorization: Bearer sk_test_..."

# create a Checkout Session: the customer is redirected to the gateway hosted page.
# line_items can use inline price_data (below) or a price id (line_items[0][price]=...).
curl http://localhost:8787/v1/checkout/sessions \
  -H "Authorization: Bearer sk_test_..." \
  --data-urlencode mode=payment \
  --data-urlencode success_url=https://app.test/done \
  --data-urlencode 'line_items[0][price_data][currency]=idr' \
  --data-urlencode 'line_items[0][price_data][unit_amount]=50000' \
  --data-urlencode 'line_items[0][price_data][product_data][name]=T-shirt' \
  --data-urlencode 'line_items[0][quantity]=2'
# -> { "id": "cs_…", "object": "checkout.session", "url": "https://<gateway-hosted-page>",
#      "payment_intent": "pi_…", "amount_total": 100000, "status": "open", … }

# create a subscription (Midtrans): returns incomplete + the authorization redirect.
# items[0][price] may reference a recurring price id, or use inline price_data as here.
curl http://localhost:8787/v1/subscriptions \
  -H "Authorization: Bearer sk_test_..." \
  --data-urlencode customer=cus_... \
  --data-urlencode 'items[0][price_data][currency]=idr' \
  --data-urlencode 'items[0][price_data][unit_amount]=75000' \
  --data-urlencode 'items[0][price_data][product_data][name]=Pro plan' \
  --data-urlencode 'items[0][price_data][recurring][interval]=month'
# -> { "id": "sub_…", "object": "subscription", "status": "incomplete",
#      "latest_invoice": { "payment_intent": { "next_action": { "redirect_to_url": { … } } } } }
# The customer completes the hosted save-card page; Midtrans pushes the save-card
# notification -> the facade registers the schedule -> status flips to "active" and
# customer.subscription.updated + invoice.payment_succeeded are forwarded.
```

## Test

```bash
go test ./...
```

## Layout

```
cmd/facade/            entrypoint
internal/config/       env-based config
internal/gateway/      the Gateway adapter interface (the only gateway-aware seam)
internal/adapters/     gateway implementations (stub, midtrans, xendit, doku, mayar)
internal/store/        in-memory state needed for translation (intents, webhook queue; no database)
internal/server/       Stripe-compatible HTTP API
```

License: Apache-2.0 (vendored Stripe types, when used, retain their MIT notice).
