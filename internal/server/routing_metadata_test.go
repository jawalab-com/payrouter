package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jawalab-com/payrouter/internal/config"
	"github.com/jawalab-com/payrouter/internal/gateway"
	"github.com/jawalab-com/payrouter/internal/store"
	stripe "github.com/stripe/stripe-go/v81"
)

// routingStubGateway returns a fixed routing decision so the server's metadata
// reporting can be asserted without standing up the real orchestrator.
type routingStubGateway struct {
	routing *gateway.RoutingInfo
}

func (g *routingStubGateway) Name() string { return "orchestrated" }

func (g *routingStubGateway) CreatePayment(_ context.Context, in *gateway.CreatePaymentInput) (*gateway.PaymentResult, error) {
	return &gateway.PaymentResult{
		Reference:        "pf_xnd_" + in.Reference,
		GatewayReference: "gw_ref",
		Status:           stripe.PaymentIntentStatusRequiresAction,
		NextAction: &stripe.PaymentIntentNextAction{
			RedirectToURL: &stripe.PaymentIntentNextActionRedirectToURL{URL: "https://pay.test/x"},
		},
		Routing: g.routing,
	}, nil
}

func (g *routingStubGateway) GetStatus(_ context.Context, _ string) (*gateway.PaymentResult, error) {
	return &gateway.PaymentResult{Status: stripe.PaymentIntentStatusSucceeded}, nil
}

func (g *routingStubGateway) Refund(_ context.Context, _ *gateway.RefundInput) (*gateway.RefundResult, error) {
	return &gateway.RefundResult{Reference: "re_x", Status: "succeeded"}, nil
}

func (g *routingStubGateway) ParseWebhook(_ context.Context, _ *http.Request) ([]gateway.WebhookEvent, error) {
	return nil, nil
}

func createCheckoutWith(t *testing.T, routing *gateway.RoutingInfo) string {
	t.Helper()
	srv := New(
		config.Config{APIKey: "sk_test_x", ActiveGateway: "auto"},
		store.NewMemory(),
		&routingStubGateway{routing: routing},
	)

	req := httptest.NewRequest(http.MethodPost, "/v1/checkout/sessions", strings.NewReader(
		"mode=payment&success_url=https://a.test/ok"+
			"&line_items[0][price_data][currency]=idr"+
			"&line_items[0][price_data][unit_amount]=100000"+
			"&line_items[0][price_data][product_data][name]=Test"+
			"&line_items[0][quantity]=1",
	))
	req.Header.Set("Authorization", "Bearer sk_test_x")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("checkout create: %d %s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// TestHostedCheckoutDoesNotClaimLeastCost is the reporting half of the routing
// fix. Orchestration mode previously stamped payment_routing_mode=least_cost on
// every response regardless of what the router did, so a hosted session — which
// cannot be cost-routed at all — asserted a fee comparison that never ran.
func TestHostedCheckoutDoesNotClaimLeastCost(t *testing.T) {
	body := createCheckoutWith(t, &gateway.RoutingInfo{
		Gateway: "midtrans",
		Basis:   gateway.RoutingFallbackPriority,
	})

	if strings.Contains(body, `"payment_routing_mode":"least_cost"`) {
		t.Errorf("metadata claims least_cost for a fallback decision: %s", body)
	}
	if !strings.Contains(body, `"payment_routing_mode":"fallback_priority"`) {
		t.Errorf("expected payment_routing_mode=fallback_priority: %s", body)
	}
}

// TestGenuineLeastCostIsReported confirms the honest path still reports the win,
// including the channel the decision was priced against.
func TestGenuineLeastCostIsReported(t *testing.T) {
	body := createCheckoutWith(t, &gateway.RoutingInfo{
		Gateway:  "xendit",
		Channel:  "qris",
		Basis:    gateway.RoutingLeastCost,
		FeeMinor: 777,
		Compared: 4,
	})

	if !strings.Contains(body, `"payment_routing_mode":"least_cost"`) {
		t.Errorf("expected payment_routing_mode=least_cost: %s", body)
	}
	if !strings.Contains(body, `"payment_routing_channel":"qris"`) {
		t.Errorf("expected payment_routing_channel=qris: %s", body)
	}
}

// TestOrchestratedWithoutDecisionIsNotClaimedLeastCost covers adapters that
// bypass the router (e.g. the subscription authorization path): absent a
// decision, the server must not invent one.
func TestOrchestratedWithoutDecisionIsNotClaimedLeastCost(t *testing.T) {
	body := createCheckoutWith(t, nil)

	if strings.Contains(body, `"payment_routing_mode":"least_cost"`) {
		t.Errorf("claimed least_cost with no routing decision attached: %s", body)
	}
	if !strings.Contains(body, `"payment_routing_mode":"unknown"`) {
		t.Errorf("expected payment_routing_mode=unknown: %s", body)
	}
}

// TestStaticGatewayStillReportsStatic guards the non-orchestrated path.
func TestStaticGatewayStillReportsStatic(t *testing.T) {
	srv := newTestServer(t) // ActiveGateway: "stub"

	req := httptest.NewRequest(http.MethodPost, "/v1/payment_intents",
		strings.NewReader("amount=50000&currency=idr&payment_method_types[]=id_qris"))
	req.Header.Set("Authorization", "Bearer sk_test_x")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), `"payment_routing_mode":"static"`) {
		t.Errorf("expected static: %s", rec.Body.String())
	}
}
