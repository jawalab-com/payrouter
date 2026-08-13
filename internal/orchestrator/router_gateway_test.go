package orchestrator

import (
	"context"
	"testing"

	"github.com/stripe-compatible-facade/internal/adapters/mayar"
	"github.com/stripe-compatible-facade/internal/adapters/xendit"
	"github.com/stripe-compatible-facade/internal/gateway"
)

func TestOrchestratedGatewayLeastCostRouting(t *testing.T) {
	rootCfg := DefaultConfig()

	// Use real key if available or mock adapter
	key := "xnd_development_lWLDVy9WxoHGnFt5FOPdnBwBXsXyGEAKWvdcgRCwX4n8o0tPCaIXnS1ga33DRXF"

	adapters := map[string]gateway.Gateway{
		"xendit": xendit.New(key, "token_test"),
		"mayar":  mayar.New("mayar_sec_test", "token_test", true),
	}

	og := NewOrchestratedGateway(rootCfg, adapters)

	if og.Name() != "orchestrated" {
		t.Errorf("name = %s; want orchestrated", og.Name())
	}

	// For QRIS 100k:
	// Xendit QRIS fee = (100,000 * 0.007) * 1.11 = 777 IDR
	// Mayar QRIS fee = ((100,000 * 0.007) + 1500 platform fee) * 1.11 = 2442 IDR
	// Dynamic orchestrator must choose Xendit!
	res, err := og.CreatePayment(context.Background(), &gateway.CreatePaymentInput{
		Reference:         "pi_orch_qris_test",
		AmountMinor:       100000,
		Currency:          "idr",
		Description:       "Orchestration Test",
		PaymentMethodType: gateway.IDQRIS,
		ReturnURL:         "https://example.com/success",
	})
	if err != nil {
		t.Fatalf("CreatePayment: %v", err)
	}

	if res == nil || res.Reference == "" {
		t.Fatalf("CreatePayment returned empty result")
	}

	// Reference must be formatted with 'pf_xnd_' prefix!
	if len(res.Reference) < 7 || res.Reference[:7] != "pf_xnd_" {
		t.Errorf("expected reference prefix 'pf_xnd_', got: %s", res.Reference)
	}
}
