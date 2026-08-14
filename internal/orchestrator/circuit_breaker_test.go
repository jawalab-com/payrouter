package orchestrator

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/jawalab-com/payrouter/internal/gateway"
	stripe "github.com/stripe/stripe-go/v81"
)

func TestCircuitBreaker_BasicLifecycle(t *testing.T) {
	fakeNow := time.Now()
	cb := NewCircuitBreaker(BreakerConfig{
		FailureThreshold: 3,
		CooldownDuration: 10 * time.Second,
	})
	cb.nowFn = func() time.Time { return fakeNow }

	// Initial state: Closed (allowed)
	if !cb.Allow("xendit") {
		t.Fatalf("expected xendit to be allowed initially")
	}
	if st := cb.State("xendit"); st != StateClosed {
		t.Fatalf("expected StateClosed, got %s", st)
	}

	// 2 failures: still closed
	cb.RecordFailure("xendit", errors.New("500 internal server error"))
	cb.RecordFailure("xendit", errors.New("504 gateway timeout"))
	if !cb.Allow("xendit") {
		t.Fatalf("expected xendit to still be allowed after 2 failures")
	}

	// 3rd failure: trips to Open
	cb.RecordFailure("xendit", errors.New("500 internal server error"))
	if cb.Allow("xendit") {
		t.Fatalf("expected xendit to be blocked after 3 failures")
	}
	if st := cb.State("xendit"); st != StateOpen {
		t.Fatalf("expected StateOpen, got %s", st)
	}

	// Time passes, but within cooldown (< 10s): still Open
	fakeNow = fakeNow.Add(5 * time.Second)
	if cb.Allow("xendit") {
		t.Fatalf("expected xendit to remain blocked before cooldown expires")
	}

	// Cooldown expires (>= 10s): transitions to HalfOpen for 1 probe
	fakeNow = fakeNow.Add(6 * time.Second)
	if !cb.Allow("xendit") {
		t.Fatalf("expected xendit probe to be allowed in HalfOpen")
	}
	// Subsequent concurrent call in HalfOpen should be blocked while probe is running
	if cb.Allow("xendit") {
		t.Fatalf("expected subsequent concurrent calls to be blocked during HalfOpen probe")
	}

	// Probe succeeds: circuit closes
	cb.RecordSuccess("xendit")
	if !cb.Allow("xendit") {
		t.Fatalf("expected xendit to be allowed after successful probe")
	}
	if st := cb.State("xendit"); st != StateClosed {
		t.Fatalf("expected StateClosed, got %s", st)
	}
}

func TestCircuitBreaker_RouterFailover(t *testing.T) {
	cfg, err := LoadConfig("../../config.yaml")
	if err != nil {
		t.Fatalf("failed to load config.yaml: %v", err)
	}

	router := NewRouter(cfg)
	cb := router.CircuitBreaker()

	// QRIS 100k IDR:
	// Midtrans is cheapest (700 IDR), Xendit is 2nd cheapest (777 IDR)
	best := router.SelectBest([]string{"midtrans", "xendit"}, "qris", 100_000)
	if best.Gateway != "midtrans" {
		t.Fatalf("expected midtrans to be chosen initially, got %s", best.Gateway)
	}

	// Trip Midtrans circuit
	cb.RecordFailure("midtrans", errors.New("midtrans down"))
	cb.RecordFailure("midtrans", errors.New("midtrans down"))
	cb.RecordFailure("midtrans", errors.New("midtrans down"))

	// Now Midtrans is unhealthy -> Router automatically falls over to Xendit!
	bestAfterTrip := router.SelectBest([]string{"midtrans", "xendit"}, "qris", 100_000)
	if bestAfterTrip.Gateway != "xendit" {
		t.Fatalf("expected failover to xendit, got %s", bestAfterTrip.Gateway)
	}
	if bestAfterTrip.FeeMinor != 777 {
		t.Fatalf("expected xendit fee 777, got %v", bestAfterTrip.FeeMinor)
	}

	// Also trip Xendit circuit: both are now unhealthy
	cb.RecordFailure("xendit", errors.New("xendit down"))
	cb.RecordFailure("xendit", errors.New("xendit down"))
	cb.RecordFailure("xendit", errors.New("xendit down"))

	// When all candidates are tripped, router still returns a candidate rather than crashing
	bestAllTripped := router.SelectBest([]string{"midtrans", "xendit"}, "qris", 100_000)
	if bestAllTripped.Gateway == "" {
		t.Fatalf("expected non-empty gateway fallback even when all candidates tripped")
	}
}

// MockAdapter for in-flight failover testing
type mockGatewayAdapter struct {
	name       string
	shouldFail bool
	errToRet   error
}

func (m *mockGatewayAdapter) Name() string { return m.name }
func (m *mockGatewayAdapter) CreatePayment(ctx context.Context, in *gateway.CreatePaymentInput) (*gateway.PaymentResult, error) {
	if m.shouldFail {
		return nil, m.errToRet
	}
	return &gateway.PaymentResult{
		Reference: in.Reference,
		Status:    stripe.PaymentIntentStatusRequiresAction,
		NextAction: &stripe.PaymentIntentNextAction{
			Type: stripe.PaymentIntentNextActionTypeRedirectToURL,
			RedirectToURL: &stripe.PaymentIntentNextActionRedirectToURL{
				URL: "https://pay.example.com/" + m.name,
			},
		},
	}, nil
}
func (m *mockGatewayAdapter) GetStatus(ctx context.Context, gatewayRef string) (*gateway.PaymentResult, error) {
	return nil, nil
}
func (m *mockGatewayAdapter) Refund(ctx context.Context, in *gateway.RefundInput) (*gateway.RefundResult, error) {
	return nil, nil
}
func (m *mockGatewayAdapter) ParseWebhook(ctx context.Context, r *http.Request) ([]gateway.WebhookEvent, error) {
	return nil, nil
}

func TestOrchestratedGateway_InFlightFailover(t *testing.T) {
	cfg, err := LoadConfig("../../config.yaml")
	if err != nil {
		t.Fatalf("failed to load config.yaml: %v", err)
	}

	// Midtrans is cheapest for QRIS, but its adapter will fail
	mdtMock := &mockGatewayAdapter{name: "midtrans", shouldFail: true, errToRet: errors.New("500 internal server error")}
	xndMock := &mockGatewayAdapter{name: "xendit", shouldFail: false}

	og := NewOrchestratedGateway(cfg, map[string]gateway.Gateway{
		"midtrans": mdtMock,
		"xendit":   xndMock,
	})

	in := &gateway.CreatePaymentInput{
		Reference:         "pi_test_123",
		AmountMinor:       100_000,
		PaymentMethodType: gateway.IDPaymentMethodType("id_qris"),
	}

	res, err := og.CreatePayment(context.Background(), in)
	if err != nil {
		t.Fatalf("expected successful failover to xendit, got error: %v", err)
	}
	if res == nil || res.Routing == nil {
		t.Fatalf("expected valid result and routing info")
	}
	if res.Routing.Gateway != "xendit" {
		t.Fatalf("expected failover to xendit, got %s", res.Routing.Gateway)
	}
}
