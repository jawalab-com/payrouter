package orchestrator

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/jawalab-com/payrouter/internal/gateway"
	stripe "github.com/stripe/stripe-go/v81"
)

// mockGateway is a stub that records calls without hitting any real HTTP endpoint.
type mockGateway struct {
	name string
}

func (m *mockGateway) Name() string { return m.name }

func (m *mockGateway) CreatePayment(_ context.Context, in *gateway.CreatePaymentInput) (*gateway.PaymentResult, error) {
	return &gateway.PaymentResult{
		Reference:        fmt.Sprintf("pf_%s_%s", gatewayCode(m.name), in.Reference),
		GatewayReference: "gw_ref_" + m.name,
		Status:           stripe.PaymentIntentStatusRequiresAction,
		NextAction: &stripe.PaymentIntentNextAction{
			RedirectToURL: &stripe.PaymentIntentNextActionRedirectToURL{
				URL: "https://" + m.name + ".local/pay/" + in.Reference,
			},
		},
	}, nil
}

func (m *mockGateway) GetStatus(_ context.Context, _ string) (*gateway.PaymentResult, error) {
	return &gateway.PaymentResult{Status: stripe.PaymentIntentStatusSucceeded}, nil
}

func (m *mockGateway) Refund(_ context.Context, _ *gateway.RefundInput) (*gateway.RefundResult, error) {
	return &gateway.RefundResult{Reference: "re_mock", Status: "succeeded"}, nil
}

func (m *mockGateway) ParseWebhook(_ context.Context, _ *http.Request) ([]gateway.WebhookEvent, error) {
	return nil, nil
}

// gatewayCode maps the gateway name to its 3-letter code matching orchestrator prefix logic.
func gatewayCode(name string) string {
	switch name {
	case "xendit":
		return "xnd"
	case "mayar":
		return "myr"
	case "midtrans":
		return "mdt"
	case "doku":
		return "dku"
	}
	return name
}

func TestOrchestratedGatewayLeastCostRouting(t *testing.T) {
	rootCfg := DefaultConfig()

	adapters := map[string]gateway.Gateway{
		"xendit": &mockGateway{name: "xendit"},
		"mayar":  &mockGateway{name: "mayar"},
	}

	og := NewOrchestratedGateway(rootCfg, adapters)

	if og.Name() != "orchestrated" {
		t.Errorf("name = %s; want orchestrated", og.Name())
	}

	// For QRIS 100k:
	// Xendit QRIS fee = (100,000 * 0.007) * 1.11 = 777 IDR
	// Mayar QRIS fee = ((100,000 * 0.007) + 1500 platform fee) * 1.11 = 2442 IDR
	// Orchestrator must choose Xendit (cheapest).
	res, err := og.CreatePayment(context.Background(), &gateway.CreatePaymentInput{
		Reference:         "pi_orch_qris_test",
		AmountMinor:       100000,
		Currency:          "idr",
		Description:       "Orchestration Unit Test",
		PaymentMethodType: gateway.IDQRIS,
		ReturnURL:         "https://example.com/success",
	})
	if err != nil {
		t.Fatalf("CreatePayment: %v", err)
	}
	if res == nil || res.Reference == "" {
		t.Fatalf("CreatePayment returned empty result")
	}

	// Reference must be prefixed with 'pf_xnd_' — proves xendit was chosen.
	if len(res.Reference) < 7 || res.Reference[:7] != "pf_xnd_" {
		t.Errorf("expected reference prefix 'pf_xnd_' (xendit selected as cheapest), got: %s", res.Reference)
	}
}
