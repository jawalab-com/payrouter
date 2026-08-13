package orchestrator

import (
	"math"
	"strings"
)

// Router handles least-cost gateway selection and fee math
type Router struct {
	config *RootConfig
}

// NewRouter creates a new Router instance with the given RootConfig
func NewRouter(cfg *RootConfig) *Router {
	return &Router{config: cfg}
}

// NormalizeChannel maps Stripe / SDK payment method aliases to config channel keys
func NormalizeChannel(raw string) string {
	ch := strings.ToLower(strings.TrimSpace(raw))
	switch ch {
	case "card", "credit_card":
		return "credit_card"
	case "qris", "id_qris":
		return "qris"
	case "virtual_account", "id_virtual_account", "bank_transfer":
		return "virtual_account"
	case "ewallet_gopay", "gopay":
		return "ewallet_gopay"
	case "ewallet_shopeepay", "shopeepay":
		return "ewallet_shopeepay"
	case "retail_outlet", "over_the_counter", "convenience_store":
		return "retail_outlet"
	default:
		return ch
	}
}

// CalculateCost calculates total transaction fee in IDR (including PPN and platform modifiers)
func (r *Router) CalculateCost(providerName, rawChannel string, amount float64) (float64, bool) {
	if r.config == nil || r.config.Providers == nil {
		return 0, false
	}

	p, exists := r.config.Providers[providerName]
	if !exists || !p.Enabled {
		return 0, false
	}

	channel := NormalizeChannel(rawChannel)
	fee, supported := p.Fees[channel]
	if !supported {
		return 0, false
	}

	// Step 1: Calculate Base Fee by Type
	var baseFee float64
	switch fee.Type {
	case "fixed":
		baseFee = fee.Fixed
	case "percentage":
		baseFee = amount * fee.Percentage
	case "mixed":
		baseFee = (amount * fee.Percentage) + fee.Fixed
	default:
		// Fallback for direct calculation
		if fee.Percentage > 0 && fee.Fixed > 0 {
			baseFee = (amount * fee.Percentage) + fee.Fixed
		} else if fee.Percentage > 0 {
			baseFee = amount * fee.Percentage
		} else {
			baseFee = fee.Fixed
		}
	}

	// Step 2: Apply Platform Modifiers (e.g. Mayar Platform Fee)
	if p.PlatformFeePct > 0 {
		baseFee += (amount * p.PlatformFeePct)
	}

	// Step 3: Calculate Final Taxed Fee (PPN / VAT)
	var finalFee float64
	if fee.VatIncluded {
		finalFee = baseFee
	} else {
		vatRate := r.config.Orchestrator.VatRate
		if vatRate <= 0 {
			vatRate = 0.11
		}
		finalFee = baseFee * (1.0 + vatRate)
	}

	return math.Round(finalFee*100) / 100, true
}

// SelectBestGateway evaluates candidate providers and returns the provider name with the lowest total fee
func (r *Router) SelectBestGateway(candidates []string, channel string, amount float64) (string, float64, error) {
	if r.config == nil {
		return "midtrans", 0, nil
	}

	bestProvider := ""
	minCost := math.MaxFloat64

	// If no candidates provided, evaluate all enabled providers in config
	targetProviders := candidates
	if len(targetProviders) == 0 {
		for name := range r.config.Providers {
			targetProviders = append(targetProviders, name)
		}
	}

	for _, name := range targetProviders {
		cost, ok := r.CalculateCost(name, channel, amount)
		if ok && cost < minCost {
			minCost = cost
			bestProvider = name
		}
	}

	// Fallback to priority queue if no candidate matched channel
	if bestProvider == "" && len(r.config.Orchestrator.FallbackPriority) > 0 {
		for _, name := range r.config.Orchestrator.FallbackPriority {
			cost, ok := r.CalculateCost(name, channel, amount)
			if ok {
				return name, cost, nil
			}
		}
		return r.config.Orchestrator.FallbackPriority[0], 0, nil
	}

	if bestProvider == "" {
		return "midtrans", 0, nil
	}
	return bestProvider, minCost, nil
}

// FormatOrderID prepends gateway prefix to order ID for stateless webhook routing (e.g. pf_mdt_123)
func FormatOrderID(providerName, rawID string) string {
	prefix := "mdt"
	switch strings.ToLower(providerName) {
	case "xendit":
		prefix = "xnd"
	case "doku":
		prefix = "dku"
	case "mayar":
		prefix = "myr"
	case "midtrans":
		prefix = "mdt"
	}
	return "pf_" + prefix + "_" + rawID
}

// ExtractGatewayFromOrderID parses 'pf_mdt_cs_123' -> returns ('midtrans', 'cs_123')
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
