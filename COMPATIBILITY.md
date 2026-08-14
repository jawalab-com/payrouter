# Stripe API Compatibility & Gateway Matrix

This document provides the definitive, master compatibility matrix for PayRouter. It details all implemented **Stripe REST API endpoints**, system health routes, hosted checkout UI paths, payment channels, dynamic least-cost routing, and webhook security mechanisms across all four Indonesian payment gateways (**Midtrans**, **Xendit**, **DOKU**, and **Mayar**).

---

## 1. Master Feature & Endpoint Compatibility Matrix

### A. Stripe REST API Surface (`/v1/*`)

| Category | Stripe API Endpoint / Method | Midtrans | Xendit | DOKU | Mayar | Notes & Behavioral Description |
| :--- | :--- | :---: | :---: | :---: | :---: | :--- |
| **Payment Intents** | `POST /v1/payment_intents` | ✅ | ✅ | ✅ | ✅ | Creates payment intent; returns `client_secret` and `next_action.redirect_to_url` |
| | `GET /v1/payment_intents/{id}` | ✅ | ✅ | ✅ | ✅ | Retrieves payment intent state from store (memory / Postgres) |
| | `GET /v1/payment_intents` | ✅ | ✅ | ✅ | ✅ | Lists payment intents scoped to authenticated account |
| | `POST /v1/payment_intents/{id}/confirm` | ✅ | ✅ | ✅ | ✅ | Triggers or confirms gateway payment authorization |
| **Checkout Sessions** | `POST /v1/checkout/sessions` | ✅ | ✅ | ✅ | ✅ | Creates hosted checkout session; returns `session.url` |
| | `GET /v1/checkout/sessions/{id}` | ✅ | ✅ | ✅ | ✅ | Retrieves checkout session state and resolution status |
| | `GET /v1/checkout/sessions` | ✅ | ✅ | ✅ | ✅ | Lists checkout sessions scoped to account |
| **Customers** | `POST /v1/customers` | ✅ | ✅ | ✅ | ✅ | Creates customer record (`cus_...`) |
| | `GET /v1/customers/{id}` | ✅ | ✅ | ✅ | ✅ | Retrieves customer record by ID (404 on unknown) |
| | `GET /v1/customers` | ✅ | ✅ | ✅ | ✅ | Lists customers for authenticated account |
| **Products & Prices** | `POST /v1/products` | ✅ | ✅ | ✅ | ✅ | Creates product catalog item (`prod_...`) |
| | `GET /v1/products/{id}` | ✅ | ✅ | ✅ | ✅ | Retrieves product by ID |
| | `POST /v1/prices` | ✅ | ✅ | ✅ | ✅ | Creates one-time or recurring price interval (`price_...`) |
| | `GET /v1/prices/{id}` | ✅ | ✅ | ✅ | ✅ | Retrieves price record by ID |
| **Refunds** | `POST /v1/refunds` | ✅ | ✅ | ❌ | ❌ | Issues full or partial refund (DOKU & Mayar return `refund_not_supported`) |
| | `GET /v1/refunds/{id}` | ✅ | ✅ | ❌ | ❌ | Retrieves Stripe-shaped `refund` object (`re_...`) |
| **Subscriptions** | `POST /v1/subscriptions` | ✅ | ❌ | ❌ | ❌ | **Midtrans Only:** Manages gateway-side auto-debit clock (`sub_...`) |
| | `GET /v1/subscriptions/{id}` | ✅ | ❌ | ❌ | ❌ | Retrieves subscription status (`active`, `incomplete`, `canceled`) |
| | `DELETE /v1/subscriptions/{id}` | ✅ | ❌ | ❌ | ❌ | Cancels subscription recurring schedule on Midtrans |
| | `GET /v1/invoices/{id}` | ✅ | ❌ | ❌ | ❌ | Retrieves invoice (`in_...`) generated for subscription cycles |
| **Inbound Webhooks**| `POST /v1/webhooks/{gateway}` | ✅ | ✅ | ✅ | ✅ | Unauthenticated listener; verifies provider signature before updating store |

---

### B. System Health & Infrastructure Probes

| Route | Method | Purpose | Auth | Expected Response |
| :--- | :---: | :--- | :---: | :--- |
| `/healthz` | `GET` | Container Liveness probe (process is running) | None | `{"status":"ok"}` (200 OK) |
| `/health/ready` | `GET` | Readiness probe (keys configured, DB migrated) | None | `{"status":"ready"}` (200) / `{"status":"not_ready"}` (503) |

---

### C. Hosted Checkout UI Routes (`PAYMENT_CHECKOUT_UI=true`)

| Route | Method | Purpose | Auth | Notes |
| :--- | :---: | :--- | :---: | :--- |
| `/checkout/{id}` | `GET` | Interactive browser checkout page | None | Customer-facing HTML UI with QR code and VA display |
| `/checkout/{id}/pay` | `POST` | Select payment method and generate instrument | None | Form POST triggering direct issuance or redirect |
| `/checkout/{id}/status`| `GET` | Real-time status polling for browser | None | Returns payment state (`pending`, `paid`, `expired`) |

---

## 2. Payment Method & Channel Mapping

When clients pass `payment_method_types` in Stripe requests, the facade maps them to the corresponding Indonesian gateway channels:

| Stripe Payment Method Code | Target Channel Description | Midtrans | Xendit | DOKU | Mayar | Gateway Channel Key |
| :--- | :--- | :---: | :---: | :---: | :---: | :--- |
| `card` / `credit_card` / `id_card` | Credit / Debit Cards (Visa, MasterCard, JCB) | ✅ | ✅ | ✅ | ✅ | `credit_card` |
| `id_qris` / `qris` | Bank Indonesia QRIS Standard | ✅ | ✅ | ✅ | ✅ | `qris` |
| `id_virtual_account` / `bank_transfer` | Bank Virtual Accounts (BCA, Mandiri, BNI, BRI, Permata) | ✅ | ✅ | ✅ | ✅ | `virtual_account` |
| `ewallet_gopay` / `gopay` | GoPay Direct / QR | ✅ | ✅ | ❌ | ✅ | `ewallet_gopay` |
| `ewallet_shopeepay` / `shopeepay` | ShopeePay Direct / QR | ✅ | ✅ | ❌ | ✅ | `ewallet_shopeepay` |
| `over_the_counter` / `convenience_store` / `id_retail` | Indomaret / Alfamart Retail Outlets | ✅ | ✅ | ✅ | ✅ | `retail_outlet` |

---

## 3. Dynamic Least-Cost Routing & Circuit Breaker Architecture

The orchestrator (`PAYMENT_GATEWAY=auto`) calculates net transaction costs in IDR before routing:

$$\text{Final Fee} = \begin{cases} \text{Base Fee} & \text{if vat\_included = true (e.g. Midtrans QRIS)} \\ \text{Base Fee} \times (1 + \text{vat\_rate}) & \text{if vat\_included = false} \end{cases}$$

$$\text{Mayar Total Fee} = \text{Base Fee} + (\text{Amount} \times \text{platform\_fee\_pct})$$

* **In-Memory Circuit Breaker:**
  * Tracks consecutive failures per gateway (threshold: 3 consecutive network/5xx failures).
  * Automatically trips degraded gateways to `StateOpen` for 30s cooldown.
  * **In-Flight Failover:** If the cheapest gateway returns an upstream server error, PayRouter immediately catches the error and executes the next cheapest candidate in the same request.
* **Routing Metadata:** Every response carries `payment_gateway_selected` and `payment_routing_mode` (`least_cost` or `fallback_priority`).

---

## 4. Webhook Security & Signature Protocol

```text
[Indonesian Gateway Callback] 
      │
      ▼ (Verified via Gateway Scheme: SHA-512 / HMAC / Token)
[PayRouter Facade Engine]
      │
      ▼ (Re-signed via HMAC-SHA256 with WEBHOOK_SIGNING_SECRET)
[Stripe-Signature Header] ──> Sent to PAYMENT_WEBHOOK_URL
```

* **Midtrans:** Verified via SHA-512 signature key (`order_id + status_code + gross_amount + ServerKey`).
* **Xendit:** Verified via constant-time comparison against `x-callback-token` header.
* **DOKU:** Verified via HMAC-SHA256 canonical digest header signature using `DOKU_SECRET_KEY`.
* **Mayar:** Verified via constant-time query parameter token comparison (`?token=MAYAR_WEBHOOK_TOKEN`).
* **Outbound Forwarding:** Re-signed using standard Stripe format: `Stripe-Signature: t=<timestamp>,v1=<hmac_sha256_hex>`.
