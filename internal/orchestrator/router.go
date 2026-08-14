package orchestrator

import (
	"math"
	"sort"
	"strings"

	"github.com/jawalab-com/payrouter/internal/gateway"
)

// Router handles least-cost gateway selection and fee math
type Router struct {
	config  *RootConfig
	breaker *CircuitBreaker
}

// NewRouter creates a new Router instance with the given RootConfig and default CircuitBreaker.
func NewRouter(cfg *RootConfig) *Router {
	return &Router{
		config:  cfg,
		breaker: NewCircuitBreaker(DefaultBreakerConfig()),
	}
}

// WithCircuitBreaker sets a custom circuit breaker on the Router.
func (r *Router) WithCircuitBreaker(cb *CircuitBreaker) *Router {
	r.breaker = cb
	return r
}

// CircuitBreaker returns the router's active circuit breaker instance.
func (r *Router) CircuitBreaker() *CircuitBreaker {
	return r.breaker
}


// NormalizeChannel maps Stripe / SDK / gateway.IDPaymentMethodType aliases onto
// config fee-table channel keys. The second return is false when the input does
// not correspond to a priceable channel — either because it is unknown, or
// because it is inherently method-agnostic (IDHosted, IDEWallet without a wallet
// name). Callers MUST honor it: a false here means no cost comparison is
// possible, not that the zero value is a safe channel to price against.
func NormalizeChannel(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "card", "credit_card", "id_card":
		return "credit_card", true
	case "qris", "id_qris":
		return "qris", true
	case "virtual_account", "id_virtual_account", "bank_transfer":
		return "virtual_account", true
	case "ewallet_gopay", "gopay":
		return "ewallet_gopay", true
	case "ewallet_shopeepay", "shopeepay":
		return "ewallet_shopeepay", true
	case "retail_outlet", "over_the_counter", "convenience_store", "id_retail":
		return "retail_outlet", true
	default:
		// Includes "id_hosted" (method chosen later, on the gateway's page) and
		// bare "id_ewallet" (wallet not yet named) — see ResolveChannel.
		return "", false
	}
}

// ResolveChannel determines the fee-table channel for a payment, consulting the
// method params when the payment method type alone is ambiguous. Bare
// "id_ewallet" carries the wallet in MethodParams{"wallet": "gopay"}, so it is
// only priceable once that is supplied.
//
// It returns false for IDHosted by design. A hosted checkout page lets the
// customer choose the method AFTER the gateway has been selected, so there is no
// single channel to price — see the note on Router.SelectBest.
func ResolveChannel(methodType string, params map[string]any) (string, bool) {
	if ch, ok := NormalizeChannel(methodType); ok {
		return ch, true
	}
	if strings.ToLower(strings.TrimSpace(methodType)) == "id_ewallet" {
		if w, _ := params["wallet"].(string); w != "" {
			return NormalizeChannel(w)
		}
	}
	return "", false
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

	channel, ok := NormalizeChannel(rawChannel)
	if !ok {
		return 0, false
	}
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

// RankCandidates returns candidate providers ranked by priority and health.
// Healthy candidates with calculated lowest cost come first, followed by priority fallbacks,
// with degraded/circuit-open candidates demoted to emergency last resort.
func (r *Router) RankCandidates(candidates []string, channel string, amount float64) []gateway.RoutingInfo {
	resolved, _ := NormalizeChannel(channel)

	if r.config == nil {
		return []gateway.RoutingInfo{{
			Gateway: "midtrans",
			Channel: resolved,
			Basis:   gateway.RoutingFallbackPriority,
		}}
	}

	targetProviders := candidates
	if len(targetProviders) == 0 {
		for name := range r.config.Providers {
			targetProviders = append(targetProviders, name)
		}
	}
	ordered := r.inPriorityOrder(targetProviders)

	type candidateCost struct {
		info      gateway.RoutingInfo
		hasCost   bool
		isHealthy bool
	}

	var list []candidateCost
	var comparedCount int

	// First pass: calculate costs and check circuit health
	for _, name := range ordered {
		isHealthy := r.breaker == nil || r.breaker.Allow(name)
		cost, ok := r.CalculateCost(name, channel, amount)
		info := gateway.RoutingInfo{
			Gateway: name,
			Channel: resolved,
		}
		if ok {
			comparedCount++
			info.FeeMinor = cost
			info.Basis = gateway.RoutingLeastCost
		} else {
			info.Basis = gateway.RoutingFallbackPriority
		}
		list = append(list, candidateCost{
			info:      info,
			hasCost:   ok,
			isHealthy: isHealthy,
		})
	}

	// Update compared count on all least-cost entries
	for i := range list {
		if list[i].hasCost {
			list[i].info.Compared = comparedCount
		}
	}

	// Sort candidates:
	// 1. Healthy before Unhealthy
	// 2. Both healthy & priced: lower cost first
	// 3. One priced, one unpriced: priced first
	// 4. Stable tie-break by existing operator priority order
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if a.isHealthy != b.isHealthy {
			return a.isHealthy // healthy candidates first
		}
		if a.hasCost && b.hasCost {
			if a.info.FeeMinor != b.info.FeeMinor {
				return a.info.FeeMinor < b.info.FeeMinor
			}
			return false // tie kept in priority order
		}
		if a.hasCost != b.hasCost {
			return a.hasCost // priced candidates before unpriced
		}
		return false
	})

	results := make([]gateway.RoutingInfo, len(list))
	for i, c := range list {
		results[i] = c.info
	}

	if len(results) == 0 {
		defaultGW := "midtrans"
		if len(r.config.Orchestrator.FallbackPriority) > 0 {
			defaultGW = r.config.Orchestrator.FallbackPriority[0]
		}
		return []gateway.RoutingInfo{{
			Gateway: defaultGW,
			Channel: resolved,
			Basis:   gateway.RoutingFallbackPriority,
		}}
	}

	return results
}

// SelectBest evaluates candidate providers for a channel and reports both the
// winner and WHY it won.
func (r *Router) SelectBest(candidates []string, channel string, amount float64) gateway.RoutingInfo {
	ranked := r.RankCandidates(candidates, channel, amount)
	return ranked[0]
}

// inPriorityOrder sorts candidates by the configured fallback priority, with any
// provider absent from that list appended alphabetically. This makes both the
// tie-break and the fallback pick deterministic and operator-controlled, and it
// keeps the fallback within the set of loaded adapters — naming a gateway whose
// credentials were never configured would fail the payment outright.
func (r *Router) inPriorityOrder(candidates []string) []string {
	rank := make(map[string]int, len(r.config.Orchestrator.FallbackPriority))
	for i, name := range r.config.Orchestrator.FallbackPriority {
		rank[name] = i
	}
	ordered := append([]string(nil), candidates...)
	sort.SliceStable(ordered, func(i, j int) bool {
		ri, iOK := rank[ordered[i]]
		rj, jOK := rank[ordered[j]]
		switch {
		case iOK && jOK:
			return ri < rj
		case iOK != jOK:
			return iOK // ranked providers come before unranked ones
		default:
			return ordered[i] < ordered[j]
		}
	})
	return ordered
}

// SelectBestGateway is the legacy positional form of SelectBest, kept so
// existing callers and tests compile. Prefer SelectBest: this form cannot
// express whether the result came from a fee comparison or a priority fallback.
func (r *Router) SelectBestGateway(candidates []string, channel string, amount float64) (string, float64, error) {
	info := r.SelectBest(candidates, channel, amount)
	return info.Gateway, info.FeeMinor, nil
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
