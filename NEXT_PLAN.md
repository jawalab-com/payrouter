# Stateless Least-Cost Payment Routing (Orchestration Plan)

This document outlines the architecture, configuration schema, and exact fee structure for adding **Stateless Least-Cost Routing (Orchestration)** to `payment-facade` across Indonesian payment gateways (**Midtrans**, **Xendit**, **Mayar**, and **DOKU**).

---

## 1. Architectural Principles

1. **Zero Database (100% Stateless):**
   - Routing decisions are calculated deterministically on each request based on requested payment method, gross amount, and fee schedules defined in `config.yaml`.
2. **Secrets vs. Rules Separation:**
   - `.env` contains strictly API keys, secrets, environment flags, and optional **negotiated fee overrides**.
   - `config.yaml` contains default gateway capability rules, payment method MDRs, fixed fees, and tax multipliers.
3. **Hierarchy of Configuration (ENV > YAML > Code Defaults):**
   - `config.yaml` provides standard default gateway rates.
   - Any merchant with **negotiated custom rates** (e.g. Xendit VA at Rp 2,500 instead of Rp 4,000) or deploying in containerized environments (Kubernetes/Docker) can set optional environment variables (e.g. `XENDIT_FEE_BANK_TRANSFER_FLAT=2500`) to override `config.yaml` without editing files.
4. **Self-Describing Webhook Order IDs:**
   - To route callbacks back to the correct gateway adapter without a database, the facade prepends a 3-letter gateway prefix to all outbound gateway order IDs:
     - `Midtrans`: `pf_mdt_<uuid>`
     - `Xendit`: `pf_xnd_<uuid>`
     - `Mayar`: `pf_myr_<uuid>`
     - `DOKU`: `pf_dku_<uuid>`
   - Webhook handlers parse `pf_<gw>_` from incoming payload order IDs to immediately invoke the right gateway adapter and signature verification routine.
5. **PPN (11% Indonesian VAT) Inclusion Formula:**
   $$\text{Total Gateway Cost} = \left( (\text{Gross Amount} \times \text{MDR Pct}) + \text{Flat Fee} \right) \times (1 + \text{PPN Rate})$$
   *(Note: Certain channels like Midtrans QRIS/GoPay already include VAT in their headline rates, configured via `vat_included: true`).*

---

## 2. Gateway Fee Schedule Comparison (Official Standard Rates)

| Channel / Gateway | Midtrans | Xendit | Mayar | DOKU |
| :--- | :--- | :--- | :--- | :--- |
| **QRIS** | 0.7% *(VAT incl)* | 0.7% + 11% PPN | 0.7% + 11% PPN | 0.7% + 11% PPN |
| **Virtual Account (VA)** | Rp 4.000 + 11% PPN | Rp 4.000 + 11% PPN | Rp 4.000 + 11% PPN | Rp 4.000 + 11% PPN |
| **Credit Card (Domestic Visa/MC)** | 2.9% + Rp 2.000 + PPN | 2.9% + Rp 2.000 + PPN | 2.6% + Rp 2.000 + PPN *(Best %)* | 2.8% + Rp 2.000 + PPN *(Better than Midtrans/Xendit)* |
| **E-Wallet (GoPay)** | 2.0% *(VAT incl)* | 2.0% + 11% PPN | 2.0% + 11% PPN | 2.0% + 11% PPN |
| **E-Wallet (ShopeePay/DANA/OVO)** | 1.5% *(VAT incl)* | 1.5% + 11% PPN | 1.5% + 11% PPN | 1.5% + 11% PPN |
| **Retail (Indomaret/Alfamart)** | Rp 5.000 + PPN | Rp 4.000 + PPN *(Best)* | Rp 5.000 - Rp 7.500 | Rp 5.000 + PPN |

---

## 3. Proposed `.env` Configuration

Add the following environment variables to `.env.example` and your production environment.

```bash
# ==========================================
# FACADE SERVER & ROUTING CONFIG
# ==========================================
FACADE_API_KEY=sk_test_facade_secret_key
FACADE_ADDR=:8787
FACADE_CONFIG_PATH=./config.yaml
FACADE_ROUTING_MODE=least_cost # options: least_cost, fallback, manual

# ==========================================
# GATEWAY MERCHANT CREDENTIALS
# ==========================================
# Midtrans (Merchant BYO)
MIDTRANS_SERVER_KEY=SB-Mid-server-YOUR_SERVER_KEY
MIDTRANS_SANDBOX=true

# Xendit (Merchant BYO)
XENDIT_SECRET_KEY=xnd_development_YOUR_SECRET_KEY
XENDIT_WEBHOOK_TOKEN=YOUR_XENDIT_CALLBACK_TOKEN

# DOKU (Merchant BYO)
DOKU_CLIENT_ID=MCH-YOUR_CLIENT_ID
DOKU_SECRET_KEY=SK-YOUR_SECRET_KEY
DOKU_SANDBOX=true

# Mayar (Merchant BYO)
MAYAR_API_KEY=mayar_sec_YOUR_API_KEY
MAYAR_WEBHOOK_TOKEN=mayar_wh_YOUR_WEBHOOK_TOKEN
MAYAR_SANDBOX=true

# ==========================================
# OPTIONAL: NEGOTIATED FEE OVERRIDES VIA .ENV
# Format: <GATEWAY>_FEE_<CHANNEL>_<FLAT|PCT>
# Unset = fallback to config.yaml default.
# ==========================================
# DOKU Negotiated Fee Overrides
# DOKU_FEE_BANK_TRANSFER_FLAT=3500       # e.g. negotiated VA rate Rp 3,500
# DOKU_FEE_CREDIT_CARD_PCT=0.025         # e.g. negotiated CC rate 2.5%

# Xendit Negotiated Fee Overrides
# XENDIT_FEE_BANK_TRANSFER_FLAT=2500     # e.g. negotiated VA rate Rp 2,500
# XENDIT_FEE_QRIS_PCT=0.0045             # e.g. negotiated QRIS rate 0.45%

# Midtrans Negotiated Fee Overrides
# MIDTRANS_FEE_BANK_TRANSFER_FLAT=3500
# MIDTRANS_FEE_CREDIT_CARD_PCT=0.022

# Mayar Negotiated Fee Overrides
# MAYAR_FEE_CREDIT_CARD_PCT=0.024

# ==========================================
# OUTBOUND STRIPE WEBHOOK FORWARDING
# ==========================================
WEBHOOK_SIGNING_SECRET=whsec_your_stripe_compatible_signing_secret
FACADE_WEBHOOK_URL=https://your-app.example.com/api/stripe-webhook
```

---

## 4. Proposed `config.yaml` Structure

Save this file as `config.yaml` in the root of `payment-facade`.

```yaml
version: "1.0"
orchestration:
  enabled: true
  strategy: "least_cost" # options: "least_cost", "round_robin", "priority"
  default_ppn_rate: 0.11 # 11% Indonesian VAT (PPN)
  
  # Fallback gateway order if top-choice fails or is disabled
  fallback_priority:
    - midtrans
    - xendit
    - doku
    - mayar

gateways:
  midtrans:
    enabled: true
    code: "mdt"
    currency: "IDR"
    channels:
      qris:
        pct: 0.007
        flat: 0
        vat_included: true
      bank_transfer:
        pct: 0.0
        flat: 4000
        vat_included: false
      credit_card:
        pct: 0.029
        flat: 2000
        vat_included: false
      ewallet_gopay:
        pct: 0.020
        flat: 0
        vat_included: true
      ewallet_shopeepay:
        pct: 0.015
        flat: 0
        vat_included: true
      over_the_counter:
        pct: 0.0
        flat: 5000
        vat_included: false

  xendit:
    enabled: true
    code: "xnd"
    currency: "IDR"
    channels:
      qris:
        pct: 0.007
        flat: 0
        vat_included: false
      bank_transfer:
        pct: 0.0
        flat: 4000
        vat_included: false
      credit_card:
        pct: 0.029
        flat: 2000
        vat_included: false
      ewallet_gopay:
        pct: 0.020
        flat: 0
        vat_included: false
      ewallet_shopeepay:
        pct: 0.015
        flat: 0
        vat_included: false
      over_the_counter:
        pct: 0.0
        flat: 4000
        vat_included: false

  mayar:
    enabled: true
    code: "myr"
    currency: "IDR"
    channels:
      qris:
        pct: 0.007
        flat: 0
        vat_included: false
      bank_transfer:
        pct: 0.0
        flat: 4000
        vat_included: false
      credit_card:
        pct: 0.026 # Mayar headline rate is 2.6% (lowest default CC % rate)
        flat: 2000
        vat_included: false
      ewallet_gopay:
        pct: 0.020
        flat: 0
        vat_included: false
      ewallet_shopeepay:
        pct: 0.015
        flat: 0
        vat_included: false
      over_the_counter:
        pct: 0.0
        flat: 5000
        vat_included: false

  doku:
    enabled: true
    code: "dku"
    currency: "IDR"
    channels:
      qris:
        pct: 0.007
        flat: 0
        vat_included: false
      bank_transfer:
        pct: 0.0
        flat: 4000 # DOKU official headline rate is Rp 4.000
        vat_included: false
      credit_card:
        pct: 0.028 # DOKU headline rate is 2.8% (better than Midtrans/Xendit 2.9%)
        flat: 2000
        vat_included: false
      over_the_counter:
        pct: 0.0
        flat: 5000
        vat_included: false
```

---

## 5. Go Orchestrator Engine Snippet (`internal/orchestrator/router.go`)

```go
package orchestrator

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
)

type ChannelFee struct {
	Pct         float64 `yaml:"pct"`
	Flat        float64 `yaml:"flat"`
	VatIncluded bool    `yaml:"vat_included"`
}

type GatewayConfig struct {
	Enabled  bool                  `yaml:"enabled"`
	Code     string                `yaml:"code"`
	Channels map[string]ChannelFee `yaml:"channels"`
}

type Config struct {
	Orchestration struct {
		Enabled        bool     `yaml:"enabled"`
		Strategy       string   `yaml:"strategy"`
		DefaultPpnRate float64  `yaml:"default_ppn_rate"`
		Fallback       []string `yaml:"fallback_priority"`
	} `yaml:"orchestration"`
	Gateways map[string]GatewayConfig `yaml:"gateways"`
}

// GetEffectiveFee retrieves channel fee, applying ENV overrides if present
func (c *Config) GetEffectiveFee(gatewayName, channel string) (ChannelFee, bool) {
	gw, exists := c.Gateways[gatewayName]
	if !exists || !gw.Enabled {
		return ChannelFee{}, false
	}
	fee, supported := gw.Channels[channel]
	if !supported {
		return ChannelFee{}, false
	}

	// 1. Check for flat fee ENV override (e.g. DOKU_FEE_BANK_TRANSFER_FLAT)
	envFlatKey := fmt.Sprintf("%s_FEE_%s_FLAT", strings.ToUpper(gatewayName), strings.ToUpper(channel))
	if val := os.Getenv(envFlatKey); val != "" {
		if parsedFlat, err := strconv.ParseFloat(val, 64); err == nil {
			fee.Flat = parsedFlat
		}
	}

	// 2. Check for percentage ENV override (e.g. DOKU_FEE_CREDIT_CARD_PCT)
	envPctKey := fmt.Sprintf("%s_FEE_%s_PCT", strings.ToUpper(gatewayName), strings.ToUpper(channel))
	if val := os.Getenv(envPctKey); val != "" {
		if parsedPct, err := strconv.ParseFloat(val, 64); err == nil {
			fee.Pct = parsedPct
		}
	}

	return fee, true
}

// CalculateCost calculates total cost in IDR for a given gross amount and channel
func (c *Config) CalculateCost(gatewayName, channel string, amount float64) (float64, bool) {
	fee, ok := c.GetEffectiveFee(gatewayName, channel)
	if !ok {
		return 0, false
	}

	rawCost := (amount * fee.Pct) + fee.Flat
	if fee.VatIncluded {
		return rawCost, true
	}

	totalCost := rawCost * (1.0 + c.Orchestration.DefaultPpnRate)
	return math.Round(totalCost), true
}

// SelectBestGateway evaluates enabled gateways and returns the adapter name with the lowest total fee
func (c *Config) SelectBestGateway(channel string, amount float64) string {
	bestGw := ""
	minCost := math.MaxFloat64

	for gwName := range c.Gateways {
		cost, ok := c.CalculateCost(gwName, channel, amount)
		if ok && cost < minCost {
			minCost = cost
			bestGw = gwName
		}
	}

	if bestGw == "" && len(c.Orchestration.Fallback) > 0 {
		return c.Orchestration.Fallback[0]
	}
	return bestGw
}

// ExtractGatewayFromOrderID parses 'pf_mdt_12345' -> returns 'midtrans', '12345'
func ExtractGatewayFromOrderID(orderID string) (string, string) {
	parts := strings.Split(orderID, "_")
	if len(parts) >= 3 && parts[0] == "pf" {
		switch parts[1] {
		case "mdt":
			return "midtrans", strings.Join(parts[2:], "_")
		case "xnd":
			return "xendit", strings.Join(parts[2:], "_")
		case "myr":
			return "mayar", strings.Join(parts[2:], "_")
		case "dku":
			return "doku", strings.Join(parts[2:], "_")
		}
	}
	return "unknown", orderID
}
```
