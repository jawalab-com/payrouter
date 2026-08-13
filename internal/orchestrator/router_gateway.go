package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/stripe-compatible-facade/internal/config"
	"github.com/stripe-compatible-facade/internal/gateway"
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

	channel := "virtual_account" // Default channel
	if in != nil && string(in.PaymentMethodType) != "" {
		channel = string(in.PaymentMethodType)
	}

	// Filter candidate names to only adapters that are loaded with non-empty keys
	var candidates []string
	for name := range og.adapters {
		candidates = append(candidates, name)
	}

	var amount float64
	if in != nil {
		amount = float64(in.AmountMinor)
	}

	bestGW, _, err := og.router.SelectBestGateway(candidates, channel, amount)
	if err != nil {
		return nil, fmt.Errorf("orchestrator selection error: %w", err)
	}

	adapter, ok := og.adapters[bestGW]
	if !ok {
		return nil, fmt.Errorf("orchestrator selected gateway %s but adapter is not loaded", bestGW)
	}

	res, err := adapter.CreatePayment(ctx, in)
	if err != nil {
		return nil, err
	}

	// Tag reference with gateway prefix for stateless 0-DB routing on callbacks
	if res != nil && res.Reference != "" {
		res.Reference = FormatOrderID(bestGW, res.Reference)
	}

	return res, nil
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
