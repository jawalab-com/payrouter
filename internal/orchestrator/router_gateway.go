package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jawalab-com/payrouter/internal/config"
	"github.com/jawalab-com/payrouter/internal/gateway"
)

// OrchestratedGateway wraps multiple provider adapters (Midtrans, Xendit, DOKU, Mayar)
// and dynamically routes incoming transactions to the adapter with the lowest fee.
type OrchestratedGateway struct {
	router   *Router
	adapters map[string]gateway.Gateway
}

// NewOrchestratedGateway creates an OrchestratedGateway containing only the adapters
// whose merchant keys are present in cfg.
func NewOrchestratedGateway(rootCfg *RootConfig, adapters map[string]gateway.Gateway) *OrchestratedGateway {
	return &OrchestratedGateway{
		router:   NewRouter(rootCfg),
		adapters: adapters,
	}
}

func (og *OrchestratedGateway) Name() string {
	return "orchestrated"
}

// CreatePayment selects the best (least-cost) gateway from configured candidates
// and delegates payment creation to that adapter.
func (og *OrchestratedGateway) CreatePayment(ctx context.Context, in *gateway.CreatePaymentInput) (*gateway.PaymentResult, error) {
	if len(og.adapters) == 0 {
		return nil, errors.New("orchestrator: no gateway credentials configured in environment")
	}

	var methodType string
	var methodParams map[string]any
	var amount float64
	if in != nil {
		methodType = string(in.PaymentMethodType)
		methodParams = in.MethodParams
		amount = float64(in.AmountMinor)
	}

	// Resolve the fee-table channel. An unresolved channel is priced by nobody, so
	// SelectBest will fall back on priority order — we surface that here rather
	// than letting it pass as a cost decision.
	channel, resolved := ResolveChannel(methodType, methodParams)
	if !resolved {
		channel = methodType
	}

	// Filter candidate names to only adapters that are loaded with non-empty keys
	var candidates []string
	for name := range og.adapters {
		candidates = append(candidates, name)
	}

	info := og.router.SelectBest(candidates, channel, amount)
	if info.Basis != gateway.RoutingLeastCost {
		// Loud on purpose: least-cost routing is the product's headline behavior,
		// so every payment that does NOT get it should be traceable in the logs.
		slog.Warn("orchestrator: no fee data for channel, falling back to priority order",
			"payment_method_type", methodType,
			"resolved_channel", info.Channel,
			"selected_gateway", info.Gateway,
			"candidates", candidates,
			"hint", hostedHint(methodType))
	} else {
		slog.Debug("orchestrator: least-cost selection",
			"channel", info.Channel,
			"selected_gateway", info.Gateway,
			"fee_minor", info.FeeMinor,
			"compared", info.Compared)
	}

	adapter, ok := og.adapters[info.Gateway]
	if !ok {
		return nil, fmt.Errorf("orchestrator selected gateway %s but adapter is not loaded", info.Gateway)
	}

	res, err := adapter.CreatePayment(ctx, in)
	if err != nil {
		return nil, err
	}

	// Tag reference with gateway prefix for stateless 0-DB routing on callbacks
	if res != nil {
		if res.Reference != "" {
			res.Reference = FormatOrderID(info.Gateway, res.Reference)
		}
		routing := info
		res.Routing = &routing
	}

	return res, nil
}

// hostedHint explains the one unresolvable case that is structural rather than a
// misconfiguration, so the warning does not read as a missing fee table.
func hostedHint(methodType string) string {
	if strings.EqualFold(strings.TrimSpace(methodType), string(gateway.IDHosted)) {
		return "hosted checkout selects the payment method after gateway selection, so no single channel can be priced; add a fee entry per method and route at confirm time to enable least-cost here"
	}
	return "add a fee entry for this channel in config.yaml to enable least-cost routing"
}

// GetStatus extracts the gateway prefix from gatewayRef and routes query to target adapter.
func (og *OrchestratedGateway) GetStatus(ctx context.Context, gatewayRef string) (*gateway.PaymentResult, error) {
	gwName, rawID := ExtractGatewayFromOrderID(gatewayRef)
	if adapter, ok := og.adapters[gwName]; ok {
		return adapter.GetStatus(ctx, rawID)
	}

	// Fallback: try all adapters
	for _, adapter := range og.adapters {
		res, err := adapter.GetStatus(ctx, gatewayRef)
		if err == nil {
			return res, nil
		}
	}

	return nil, fmt.Errorf("orchestrator: cannot resolve payment reference %s", gatewayRef)
}

// Refund routes refund requests to the gateway matching the reference prefix.
func (og *OrchestratedGateway) Refund(ctx context.Context, in *gateway.RefundInput) (*gateway.RefundResult, error) {
	if in == nil {
		return nil, errors.New("orchestrator: Refund input required")
	}

	gwName, rawID := ExtractGatewayFromOrderID(in.PaymentReference)
	if adapter, ok := og.adapters[gwName]; ok {
		inCopy := *in
		inCopy.PaymentReference = rawID
		return adapter.Refund(ctx, &inCopy)
	}

	return nil, fmt.Errorf("orchestrator: cannot route refund for reference %s", in.PaymentReference)
}

// ParseWebhook routes incoming webhooks to the corresponding adapter if gateway name is matched.
func (og *OrchestratedGateway) ParseWebhook(ctx context.Context, r *http.Request) ([]gateway.WebhookEvent, error) {
	for _, adapter := range og.adapters {
		events, err := adapter.ParseWebhook(ctx, r)
		if err == nil && len(events) > 0 {
			return events, nil
		}
	}
	return nil, errors.New("orchestrator: unable to parse webhook across candidate adapters")
}

// BuildCandidateAdapters inspects application config and instantiates adapters
// ONLY for gateways that have valid secret keys supplied in environment.
func BuildCandidateAdapters(cfg config.Config, buildAdapterFunc func(name string) (gateway.Gateway, bool)) map[string]gateway.Gateway {
	adapters := make(map[string]gateway.Gateway)

	if cfg.Midtrans.ServerKey != "" {
		if gw, ok := buildAdapterFunc("midtrans"); ok {
			adapters["midtrans"] = gw
		}
	}
	if cfg.Xendit.SecretKey != "" {
		if gw, ok := buildAdapterFunc("xendit"); ok {
			adapters["xendit"] = gw
		}
	}
	if cfg.Doku.ClientID != "" && cfg.Doku.SecretKey != "" {
		if gw, ok := buildAdapterFunc("doku"); ok {
			adapters["doku"] = gw
		}
	}
	if cfg.Mayar.APIKey != "" {
		if gw, ok := buildAdapterFunc("mayar"); ok {
			adapters["mayar"] = gw
		}
	}

	return adapters
}
