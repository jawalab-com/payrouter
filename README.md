# ⚡ PayRouter

> **Stripe-Compatible Payment Facade & Dynamic Least-Cost Gateway Orchestrator**

PayRouter is a high-performance, stateless payment engine written in Go. It accepts requests shaped exactly like Stripe's REST API and dynamically routes payments to local Indonesian payment gateways (**Midtrans**, **Xendit**, **DOKU**, **Mayar**) using dynamic least-cost MDR fee calculation.

Existing frontend applications, mobile apps, and backend services built with official Stripe SDKs (`stripe-go`, `stripe-node`, `stripe-python`, `@stripe/stripe-js`) work **unchanged** without modifying a single line of checkout code.

---

## 🚀 Key Features

* **100% Stripe REST API Surface Compatibility:** Drop-in replacement for Checkout Sessions (`/v1/checkout/sessions`), Payment Intents (`/v1/payment_intents`), Customers (`/v1/customers`), Subscriptions (`/v1/subscriptions`), Refunds (`/v1/refunds`), and Products/Prices (`/v1/products`, `/v1/prices`).
* **Dynamic Least-Cost Fee Orchestrator (`PAYMENT_GATEWAY=auto`):** Evaluates real-time MDR fees, fixed charges, platform modifiers, and 11% PPN tax rules for every payment method to automatically pick the cheapest gateway. Requires a known payment method — see [Routing coverage](#-routing-coverage) for where it applies and where it cannot.
* **Metadata Insights:** Every response automatically enriches Stripe metadata with `payment_gateway_selected`, `payment_routing_mode`, and (when a fee comparison ran) `payment_routing_channel`.
* **In-Memory Mode for Development:** Runs with zero external dependencies for local development and evaluation. **PostgreSQL is required for production** and is enforced at startup — see [Storage modes](#-storage-modes) for what memory mode does not provide.
* **Unified Outbound Webhooks:** Translates gateway callbacks into standard Stripe events (`checkout.session.completed`, `payment_intent.succeeded`, `charge.refunded`) signed with standard `Stripe-Signature` headers.

---

## 🗄️ Storage modes

PayRouter runs against an in-memory store by default and PostgreSQL when
`PAYMENT_DATABASE_URL` is set. These are **not** equivalent, and the difference
is about payment correctness, not just persistence:

| Capability | In-memory (default) | PostgreSQL (`PAYMENT_DATABASE_URL`) |
| :--- | :---: | :---: |
| Stripe API surface | ✅ | ✅ |
| Gateway routing & webhooks | ✅ | ✅ |
| **`Idempotency-Key` handling** | ❌ ignored | ✅ enforced |
| **Inbound webhook deduplication** | ❌ | ✅ |
| Crash reconciliation of in-flight callbacks | ❌ | ✅ |
| Multi-tenant (per-account) API keys | ❌ single key | ✅ |
| Survives restart | ❌ | ✅ |

Without PostgreSQL, `Idempotency-Key` is accepted but **not honored**: a client
retrying a timed-out `POST /v1/payment_intents` creates a *second payment*, and a
redelivered gateway callback is reprocessed. That is a duplicate-charge risk, so
memory mode is strictly for development and evaluation.

This is enforced, not merely advised — with `PAYMENT_APP_ENV=production`, startup
fails unless `PAYMENT_DATABASE_URL` is set. Bring your own PostgreSQL (any managed
or self-hosted instance); migrations are applied automatically at boot.

---

## 🧭 Routing coverage

Least-cost routing needs a payment method to price against, because the MDR fee
schedule is per-method. Where the method is known at creation time, routing
compares every configured gateway and picks the cheapest:

| Request | Channel priced | Routing |
| :--- | :--- | :--- |
| `payment_method_types[]=id_qris` | `qris` | ✅ least-cost |
| `payment_method_types[]=id_virtual_account` | `virtual_account` | ✅ least-cost |
| `payment_method_types[]=id_card` | `credit_card` | ✅ least-cost |
| `payment_method_types[]=id_retail` | `retail_outlet` | ✅ least-cost |
| `payment_method_types[]=id_ewallet` + `wallet=gopay` | `ewallet_gopay` | ✅ least-cost |
| `POST /v1/checkout/sessions` (hosted) | — | ⚠️ fallback priority |

**Hosted Checkout Sessions cannot be cost-routed.** A hosted page lets the
customer choose the method *after* the gateway has been selected, so no single
fee applies at selection time. Those sessions fall back to the configured
`fallback_priority` order, log a warning, and report
`payment_routing_mode: fallback_priority` in metadata — the response never claims
a fee comparison that did not happen.

To get least-cost routing on checkout, either create a PaymentIntent with an
explicit `payment_method_types[]`, or collect the method in your own UI before
calling PayRouter.

---

## 💰 Gateway Fee Comparison Matrix (Default `config.yaml`)

PayRouter uses the following base MDR fee schedule (configurable via `config.yaml` or `.env` overrides):

| Payment Channel | Midtrans | Xendit | DOKU | Mayar (Starter Tier) |
| :--- | :--- | :--- | :--- | :--- |
| **QRIS** | **0.70%** *(PPN incl.)* | **0.70%** + 11% PPN | **0.70%** + 11% PPN | **0.70%** + 1.5% platform + 11% PPN |
| **Virtual Account** | **Rp 4.000** + 11% PPN | **Rp 4.000** + 11% PPN | **Rp 4.000** + 11% PPN | **Rp 4.000** + 1.5% platform + 11% PPN |
| **Credit Card** | **2.90% + Rp 2.000** + 11% PPN | **2.90% + Rp 2.000** + 11% PPN | **2.80% + Rp 2.000** + 11% PPN | **2.60% + Rp 2.000** + 1.5% platform + 11% PPN |
| **GoPay (E-Wallet)** | **2.00%** *(PPN incl.)* | **2.00%** + 11% PPN | — | — |
| **ShopeePay (E-Wallet)** | **1.50%** *(PPN incl.)* | **1.50%** + 11% PPN | — | — |
| **Retail Outlet (Alfamart/Indomaret)** | **Rp 5.000** + 11% PPN | **Rp 4.000** + 11% PPN | **Rp 5.000** + 11% PPN | **Rp 5.000** + 1.5% platform + 11% PPN |

*Note: All rates can be overridden in `.env` (e.g. `XENDIT_FEE_VA_FIXED=2500` for custom enterprise pricing).*

---

## 🛠️ Environment Configuration (`.env`)

Copy `.env.example` to `.env` and set your server variables and gateway credentials:

```bash
# Server Settings
PAYMENT_ADDR=:8787
PAYMENT_API_KEY=sk_test_payment_secret_key
PAYMENT_GATEWAY=auto  # Options: auto (least-cost), midtrans, xendit, doku, mayar, stub
PAYMENT_CONFIG_PATH=./config.yaml
PAYMENT_APP_ENV=development       # "production" requires PAYMENT_DATABASE_URL + WEBHOOK_SIGNING_SECRET
PAYMENT_MAX_BODY_BYTES=1048576    # per-request body cap (default 1 MiB); oversized requests get HTTP 413

# Durable storage (REQUIRED in production — see "Storage modes")
PAYMENT_DATABASE_URL=postgres://user:pass@host:5432/payrouter?sslmode=require

# Outbound Stripe Webhook Destination
PAYMENT_WEBHOOK_URL=https://your-app.example.com/api/stripe-webhook
WEBHOOK_SIGNING_SECRET=whsec_your_stripe_compatible_signing_secret

# Active Gateway Credentials (BYO Keys)
XENDIT_SECRET_KEY=xnd_development_...
MAYAR_API_KEY=eyJhbGci...
MAYAR_WEBHOOK_TOKEN=your_mayar_token
```

---

## ⚡ Quickstart

### 1. Run via Go CLI
```bash
# Clone the repository
git clone https://github.com/jawalab-com/payrouter.git
cd payrouter

# Build and run
go build -o payrouter ./cmd/payrouter
./payrouter
```

### 2. Run via Docker
```bash
docker build -t payrouter .
docker run -p 8787:8787 --env-file .env payrouter
```

---

## 📖 API Usage Examples

PayRouter accepts standard Stripe form-encoded HTTP requests.

### Create a Hosted Checkout Session
```bash
curl -X POST http://localhost:8787/v1/checkout/sessions \
  -H "Authorization: Bearer sk_test_payment_secret_key" \
  -d "mode=payment" \
  -d "success_url=https://example.com/success" \
  -d "cancel_url=https://example.com/cancel" \
  -d "line_items[0][price_data][currency]=idr" \
  -d "line_items[0][price_data][unit_amount]=100000" \
  -d "line_items[0][price_data][product_data][name]=Pro Subscription"
```

**Stripe Response Output:**
```json
{
  "id": "cs_6b9bdd1df1a3c096d56b713f",
  "object": "checkout.session",
  "mode": "payment",
  "status": "open",
  "payment_status": "unpaid",
  "amount_total": 100000,
  "currency": "idr",
  "url": "https://checkout-staging.xendit.co/web/6a7d61805a693600d08b6318",
  "metadata": {
    "payment_gateway_selected": "xendit",
    "payment_routing_mode": "least_cost"
  }
}
```

### List All Recent Transactions
```bash
curl -X GET http://localhost:8787/v1/payment_intents \
  -H "Authorization: Bearer sk_test_payment_secret_key"
```

---

## 🧪 Testing

Run the full Go unit test suite across all gateway adapters, server handlers, and fee calculators:

```bash
go test ./...
```

To generate a visual HTML code coverage report:
```bash
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out -o coverage.html
```

---

## 📄 License

PayRouter is open-source software licensed under the [MIT License](LICENSE). Developed by [Jawalab](https://jawalab.com).
