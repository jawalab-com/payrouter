# Stripe API Compatibility & Gateway Matrix

This document provides a single, unified master matrix detailing the implementation status of **Stripe REST API endpoints**, payment channels, stateless orchestration routing, and webhook security mechanisms across all four Indonesian payment gateways (**Midtrans**, **Xendit**, **DOKU**, and **Mayar**).

---

## 1. Master Feature & Endpoint Compatibility Matrix

| Category | Stripe API Endpoint / Feature | Midtrans | Xendit | DOKU | Mayar | Notes & Behavioral Description |
| :--- | :--- | :---: | :---: | :---: | :---: | :--- |
| **Hosted Checkout** | `POST /v1/checkout/sessions` | ✅ | ✅ | ✅ | ✅ | Returns gateway-hosted page URL in `session.url` |
| | `GET /v1/checkout/sessions/{id}` | ✅ | ✅ | ✅ | ✅ | Resolves session state from memory or Postgres store |
| | `POST /v1/checkout/sessions/{id}/expire` | ⚠️ | ⚠️ | ❌ | ❌ | Supported where gateway invoice expiration API exists |
| **Payment Intents** | `POST /v1/payment_intents` | ✅ | ✅ | ✅ | ✅ | Returns authorization link in `next_action.redirect_to_url` |
| | `GET /v1/payment_intents/{id}` | ✅ | ✅ | ✅ | ✅ | Queries local store or polls gateway API |
| | `POST /v1/payment_intents/{id}/confirm` | ✅ | ✅ | ✅ | ✅ | Triggers gateway payment authorization |
| | `POST /v1/payment_intents/{id}/cancel` | ✅ | ✅ | ❌ | ❌ | Cancels active intent on gateway if supported |
| **Refunds** | `POST /v1/refunds` | ✅ | ✅ | ❌ | ❌ | DOKU & Mayar return HTTP 400 (`refund_not_supported`) |
| | `GET /v1/refunds/{id}` | ✅ | ✅ | ❌ | ❌ | Returns Stripe-shaped `refund` JSON object |
| **Subscriptions** | `POST /v1/subscriptions` | ✅ | ❌ | ❌ | ❌ | **Midtrans Only:** Manages gateway-side auto-debit clock |
| | `GET /v1/subscriptions/{id}` | ✅ | ❌ | ❌ | ❌ | Returns active / incomplete subscription state |
| | `DELETE /v1/subscriptions/{id}` | ✅ | ❌ | ❌ | ❌ | Cancels recurring subscription clock on Midtrans |
| | `GET /v1/invoices/{id}` | ✅ | ❌ | ❌ | ❌ | Returns first invoice on activation, then per cycle |
| **Catalog** | `POST /v1/customers` (Create/Retrieve) | ✅ | ✅ | ✅ | ✅ | Universal pass-through customer store |
| | `POST /v1/products` (Create/Retrieve) | ✅ | ✅ | ✅ | ✅ | Universal pass-through product catalog |
| | `POST /v1/prices` (Create/Retrieve) | ✅ | ✅ | ✅ | ✅ | Supports `one_time` and `recurring` price intervals |
| **Orchestration** | Dynamic Least-Cost Routing | ✅ | ✅ | ✅ | ✅ | `internal/orchestrator` selects lowest fee gateway |
| | Stateless Webhook Prefix Routing | `pf_mdt_` | `pf_xnd_` | `pf_dku_` | `pf_myr_` | Prepend prefix to gateway order ID for 0-DB routing |
| **Webhooks** | Inbound Gateway Listener | ✅ | ✅ | ✅ | ✅ | Endpoint path: `/v1/webhooks/{gateway}` |
| | Inbound Signature Verification | SHA-512 | Token | HMAC-SHA256 | `?token=` | Verifies authenticity before processing payload |
| | Outbound Forwarding Signature | `v1=...` | `v1=...` | `v1=...` | `v1=...` | Re-signs payload with standard `Stripe-Signature` |

---

## 2. Payment Method & Channel Mapping

When clients pass `payment_method_types` in Stripe requests, the facade maps them to the corresponding Indonesian gateway channels:

| Stripe Payment Method Code | Target Channel Description | Midtrans | Xendit | DOKU | Mayar | Gateway Channel Name |
| :--- | :--- | :---: | :---: | :---: | :---: | :--- |
| `card` / `credit_card` | Credit / Debit Cards (Visa, MasterCard, JCB) | ✅ | ✅ | ✅ | ✅ | `credit_card` |
| `id_qris` / `qris` | Bank Indonesia QRIS Standard | ✅ | ✅ | ✅ | ✅ | `qris` |
| `id_virtual_account` / `bank_transfer` | Bank Virtual Accounts (BCA, Mandiri, BNI, BRI) | ✅ | ✅ | ✅ | ✅ | `virtual_account` |
| `ewallet_gopay` / `gopay` | GoPay Direct / QR | ✅ | ✅ | ❌ | ✅ | `ewallet_gopay` |
| `ewallet_shopeepay` / `shopeepay` | ShopeePay Direct / QR | ✅ | ✅ | ❌ | ✅ | `ewallet_shopeepay` |
| `over_the_counter` / `convenience_store` | Indomaret / Alfamart Retail Outlets | ✅ | ✅ | ✅ | ✅ | `retail_outlet` |

---

## 3. Stateless Orchestration & Routing Logic

The facade uses **zero-state least-cost calculation** based on `config.yaml` and `.env` overrides:

$$\text{Final Fee} = \begin{cases} \text{Base Fee} & \text{if vat\_included = true (e.g. Midtrans QRIS)} \\ \text{Base Fee} \times (1 + \text{vat\_rate}) & \text{if vat\_included = false} \end{cases}$$

$$\text{Mayar Total Fee} = \text{Base Fee} + (\text{Amount} \times \text{platform\_fee\_pct})$$

---

## 4. Webhook Security & Signature Protocol

```text
[Indonesian Gateway Callback] 
      │
      ▼ (Verified via Gateway Scheme: SHA-512 / HMAC / Token)
[Payment Facade Engine]
      │
      ▼ (Re-signed via HMAC-SHA256 with WEBHOOK_SIGNING_SECRET)
[Stripe-Signature Header] ──> Sent to PAYMENT_WEBHOOK_URL
```

* **Midtrans:** Verified via SHA-512 signature key (`order_id + status_code + gross_amount + ServerKey`).
* **Xendit:** Verified via constant-time comparison against `x-callback-token` header.
* **DOKU:** Verified via HMAC-SHA256 canonical digest header signature using `DOKU_SECRET_KEY`.
* **Mayar:** Verified via constant-time query parameter token comparison (`?token=MAYAR_WEBHOOK_TOKEN`).
