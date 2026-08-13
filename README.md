# ⚡ PayRouter

> **Stripe-Compatible Payment Facade & Dynamic Least-Cost Gateway Orchestrator**

PayRouter is a high-performance, stateless payment engine written in Go. It accepts requests shaped exactly like Stripe's REST API and dynamically routes payments to local Indonesian payment gateways (**Midtrans**, **Xendit**, **DOKU**, **Mayar**) using dynamic least-cost MDR fee calculation.

Existing frontend applications, mobile apps, and backend services built with official Stripe SDKs (`stripe-go`, `stripe-node`, `stripe-python`, `@stripe/stripe-js`) work **unchanged** without modifying a single line of checkout code.

---

## 🚀 Key Features

* **100% Stripe REST API Surface Compatibility:** Drop-in replacement for Checkout Sessions (`/v1/checkout/sessions`), Payment Intents (`/v1/payment_intents`), Customers (`/v1/customers`), Subscriptions (`/v1/subscriptions`), Refunds (`/v1/refunds`), and Products/Prices (`/v1/products`, `/v1/prices`).
* **Dynamic Least-Cost Fee Orchestrator (`PAYMENT_GATEWAY=auto`):** Evaluates real-time MDR fees, fixed charges, platform modifiers, and 11% PPN tax rules for every payment method to automatically pick the cheapest gateway.
* **Metadata Insights:** Every response automatically enriches Stripe metadata with `payment_gateway_selected` and `payment_routing_mode`.
* **Zero-DB Stateless Default:** Runs in pure 0-DB memory mode out of the box with zero external database dependencies. Optional PostgreSQL driver available for enterprise audit logs.
* **Unified Outbound Webhooks:** Translates gateway callbacks into standard Stripe events (`checkout.session.completed`, `payment_intent.succeeded`, `charge.refunded`) signed with standard `Stripe-Signature` headers.

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
go build -o payrouter.exe ./cmd/facade
./payrouter.exe
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
