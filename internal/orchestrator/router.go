package orchestrator

import (
	"math"
	"sort"
	"strings"

	"github.com/jawalab-com/payrouter/internal/gateway"
)

// Router handles least-cost gateway selection and fee math
type Router struct {
	config *RootConfig
}

// NewRouter creates a new Router instance with the given RootConfig
func NewRouter(cfg *RootConfig) *Router {
	return &Router{config: cfg}
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

// SelectBest evaluates candidate providers for a channel and reports both the
// winner and WHY it won.
//
// The Basis field is the point of this function. Previously an unresolvable
// channel silently fell through to FallbackPriority[0] and was indistinguishable
// from a genuine least-cost win, which meant every hosted checkout reported
// least-cost routing while actually being pinned to the first priority entry.
// Callers must branch on Basis rather than assume a fee comparison occurred.
//
// Passing a channel that NormalizeChannel cannot resolve (notably "id_hosted")
// is not an error — it yields RoutingFallbackPriority with Compared == 0.
func (r *Router) SelectBest(candidates []string, channel string, amount float64) gateway.RoutingInfo {
	resolved, _ := NormalizeChannel(channel)
	info := gateway.RoutingInfo{Channel: resolved}

	if r.config == nil {
		info.Gateway, info.Basis = "midtrans", gateway.RoutingFallbackPriority
		return info
	}

	// If no candidates provided, evaluate all enabled providers in config.
	targetProviders := candidates
	if len(targetProviders) == 0 {
		for name := range r.config.Providers {
			targetProviders = append(targetProviders, name)
		}
	}
	// Iterate in the operator's configured preference order. Fee ties are common
	// (identical published MDR across gateways), and on a tie the operator's
	// priority should decide — not map iteration order or the alphabet.
	ordered := r.inPriorityOrder(targetProviders)

	bestProvider := ""
	minCost := math.MaxFloat64
	for _, name := range ordered {
		cost, ok := r.CalculateCost(name, channel, amount)
		if !ok {
			continue
		}
		info.Compared++
		if cost < minCost {
			minCost, bestProvider = cost, name
		}
	}
	if bestProvider != "" {
		info.Gateway, info.FeeMinor, info.Basis = bestProvider, minCost, gateway.RoutingLeastCost
		return info
	}

	// No candidate priced this channel. Fall back to the configured priority
	// order, but report that honestly — this is NOT a least-cost decision.
	info.Basis = gateway.RoutingFallbackPriority
	if len(ordered) > 0 {
		info.Gateway = ordered[0]
		return info
	}
	if len(r.config.Orchestrator.FallbackPriority) > 0 {
		info.Gateway = r.config.Orchestrator.FallbackPriority[0]
		return info
	}
	info.Gateway = "midtrans"
	return info
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
