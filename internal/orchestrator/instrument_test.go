package orchestrator

import (
	"context"
	"errors"
	"testing"

	"github.com/jawalab-com/payrouter/internal/gateway"
	stripe "github.com/stripe/stripe-go/v81"
)

// issuingGateway is a mockGateway that also implements InstrumentGateway for a
// declared set of methods, so eligibility filtering can be exercised.
type issuingGateway struct {
	mockGateway
	supports map[gateway.IDPaymentMethodType]bool
	issued   *gateway.CreatePaymentInput
	failWith error
}

func newIssuer(name string, methods ...gateway.IDPaymentMethodType) *issuingGateway {
	g := &issuingGateway{
		mockGateway: mockGateway{name: name},
		supports:    map[gateway.IDPaymentMethodType]bool{},
	}
	for _, m := range methods {
		g.supports[m] = true
	}
	return g
}

func (g *issuingGateway) SupportsInstrument(m gateway.IDPaymentMethodType) bool { return g.supports[m] }

func (g *issuingGateway) IssueInstrument(_ context.Context, in *gateway.CreatePaymentInput) (*gateway.PaymentResult, error) {
	if g.failWith != nil {
		return nil, g.failWith
	}
	g.issued = in
	return &gateway.PaymentResult{
		Reference:        in.Reference,
		GatewayReference: "gw_" + g.name,
		Status:           stripe.PaymentIntentStatusRequiresAction,
		Display: &gateway.DisplayInstructions{
			Kind: gateway.InstrumentQRIS,
			QRIS: &gateway.QRISInstruction{Payload: "qr-from-" + g.name},
		},
	}, nil
}

// TestInstrumentRoutingIsLeastCost is the payoff: with a concrete method the
// channel resolves, so selection is an actual fee comparison rather than the
// priority fallback a hosted checkout is stuck with.
func TestInstrumentRoutingIsLeastCost(t *testing.T) {
	og := NewOrchestratedGateway(DefaultConfig(), map[string]gateway.Gateway{
		"midtrans": newIssuer("midtrans", gateway.IDQRIS),
		"xendit":   newIssuer("xendit", gateway.IDQRIS),
	})

	res, err := og.IssueInstrument(context.Background(), &gateway.CreatePaymentInput{
		Reference: "pi_abc", AmountMinor: 100000, Currency: "idr",
		PaymentMethodType: gateway.IDQRIS,
	})
	if err != nil {
		t.Fatalf("IssueInstrument: %v", err)
	}
	if res.Routing == nil {
		t.Fatal("no routing decision attached")
	}
	if res.Routing.Basis != gateway.RoutingLeastCost {
		t.Errorf("basis = %q, want least_cost — a concrete method must be priceable", res.Routing.Basis)
	}
	if res.Routing.Channel != "qris" {
		t.Errorf("channel = %q, want qris", res.Routing.Channel)
	}
	// Midtrans includes PPN on QRIS (700) and beats Xendit (777).
	if res.Routing.Gateway != "midtrans" {
		t.Errorf("gateway = %q, want midtrans (cheapest for QRIS)", res.Routing.Gateway)
	}
	if res.Display == nil || res.Display.QRIS.Payload != "qr-from-midtrans" {
		t.Errorf("instrument did not come from the selected gateway: %+v", res.Display)
	}
}

// TestIneligibleGatewayCannotWinOnPrice is the eligibility guarantee. The
// cheapest gateway for a channel is irrelevant if it cannot issue the
// instrument — selecting it would fail at charge time, after the customer has
// already chosen a method.
func TestIneligibleGatewayCannotWinOnPrice(t *testing.T) {
	// Midtrans is cheapest for QRIS but cannot issue it here; only Xendit can.
	og := NewOrchestratedGateway(DefaultConfig(), map[string]gateway.Gateway{
		"midtrans": newIssuer("midtrans", gateway.IDVirtualAccount), // not QRIS
		"xendit":   newIssuer("xendit", gateway.IDQRIS),
	})

	res, err := og.IssueInstrument(context.Background(), &gateway.CreatePaymentInput{
		Reference: "pi_abc", AmountMinor: 100000, PaymentMethodType: gateway.IDQRIS,
	})
	if err != nil {
		t.Fatalf("IssueInstrument: %v", err)
	}
	if res.Routing.Gateway != "xendit" {
		t.Errorf("gateway = %q, want xendit — midtrans is cheaper but cannot issue QRIS", res.Routing.Gateway)
	}
	// The reported fee must be Xendit's, not the cheaper ineligible one.
	if res.Routing.FeeMinor != 777 {
		t.Errorf("fee = %v, want 777 (Xendit's QRIS fee), not the ineligible gateway's", res.Routing.FeeMinor)
	}
}

// TestAdapterWithoutCapabilityIsSkipped covers a plain Gateway that does not
// implement the optional interface at all — Mayar's situation.
func TestAdapterWithoutCapabilityIsSkipped(t *testing.T) {
	og := NewOrchestratedGateway(DefaultConfig(), map[string]gateway.Gateway{
		"mayar":  &mockGateway{name: "mayar"}, // no InstrumentGateway
		"xendit": newIssuer("xendit", gateway.IDQRIS),
	})

	if !og.SupportsInstrument(gateway.IDQRIS) {
		t.Fatal("SupportsInstrument = false despite one capable adapter")
	}
	res, err := og.IssueInstrument(context.Background(), &gateway.CreatePaymentInput{
		Reference: "pi_abc", AmountMinor: 100000, PaymentMethodType: gateway.IDQRIS,
	})
	if err != nil {
		t.Fatalf("IssueInstrument: %v", err)
	}
	if res.Routing.Gateway != "xendit" {
		t.Errorf("gateway = %q, want xendit", res.Routing.Gateway)
	}
}

// TestNoEligibleGatewayFallsBackToHosted: the caller must be able to tell "no
// one can issue this" from a real failure, so it can redirect instead.
func TestNoEligibleGatewayFallsBackToHosted(t *testing.T) {
	og := NewOrchestratedGateway(DefaultConfig(), map[string]gateway.Gateway{
		"mayar":    &mockGateway{name: "mayar"},
		"midtrans": newIssuer("midtrans", gateway.IDQRIS),
	})

	if og.SupportsInstrument(gateway.IDRetail) {
		t.Error("SupportsInstrument(retail) = true, but nothing issues it")
	}
	_, err := og.IssueInstrument(context.Background(), &gateway.CreatePaymentInput{
		Reference: "pi_abc", AmountMinor: 100000, PaymentMethodType: gateway.IDRetail,
	})
	if !errors.Is(err, gateway.ErrInstrumentUnsupported) {
		t.Errorf("err = %v, want ErrInstrumentUnsupported so the caller can redirect", err)
	}
}

// TestEWalletResolvesThroughMethodParams verifies the wallet-specific channel
// survives into the cost comparison rather than degrading to a fallback.
func TestEWalletResolvesThroughMethodParams(t *testing.T) {
	og := NewOrchestratedGateway(DefaultConfig(), map[string]gateway.Gateway{
		"midtrans": newIssuer("midtrans", gateway.IDEWallet),
		"xendit":   newIssuer("xendit", gateway.IDEWallet),
	})

	res, err := og.IssueInstrument(context.Background(), &gateway.CreatePaymentInput{
		Reference: "pi_abc", AmountMinor: 100000,
		PaymentMethodType: gateway.IDEWallet,
		MethodParams:      map[string]any{"wallet": "gopay"},
	})
	if err != nil {
		t.Fatalf("IssueInstrument: %v", err)
	}
	if res.Routing.Channel != "ewallet_gopay" {
		t.Errorf("channel = %q, want ewallet_gopay", res.Routing.Channel)
	}
	if res.Routing.Basis != gateway.RoutingLeastCost {
		t.Errorf("basis = %q, want least_cost", res.Routing.Basis)
	}
}

// TestReferenceIsGatewayTagged keeps stateless callback routing working: the
// prefix is how an inbound webhook is resolved back to the issuing gateway.
func TestReferenceIsGatewayTagged(t *testing.T) {
	og := NewOrchestratedGateway(DefaultConfig(), map[string]gateway.Gateway{
		"xendit": newIssuer("xendit", gateway.IDQRIS),
	})

	res, err := og.IssueInstrument(context.Background(), &gateway.CreatePaymentInput{
		Reference: "pi_abc", AmountMinor: 100000, PaymentMethodType: gateway.IDQRIS,
	})
	if err != nil {
		t.Fatalf("IssueInstrument: %v", err)
	}
	gw, raw := ExtractGatewayFromOrderID(res.Reference)
	if gw != "xendit" || raw != "pi_abc" {
		t.Errorf("reference %q decodes to (%q, %q), want (xendit, pi_abc)", res.Reference, gw, raw)
	}
}

// TestIssuerErrorPropagates: a genuine failure must not be mistaken for
// "unsupported", which would silently redirect instead of surfacing the problem.
func TestIssuerErrorPropagates(t *testing.T) {
	failing := newIssuer("xendit", gateway.IDQRIS)
	failing.failWith = errors.New("xendit: HTTP 500")

	og := NewOrchestratedGateway(DefaultConfig(), map[string]gateway.Gateway{"xendit": failing})

	_, err := og.IssueInstrument(context.Background(), &gateway.CreatePaymentInput{
		Reference: "pi_abc", AmountMinor: 100000, PaymentMethodType: gateway.IDQRIS,
	})
	if err == nil {
		t.Fatal("issuer error was swallowed")
	}
	if errors.Is(err, gateway.ErrInstrumentUnsupported) {
		t.Error("a transport failure was reported as ErrInstrumentUnsupported")
	}
}

func TestOrchestratorSatisfiesInstrumentGateway(t *testing.T) {
	var _ gateway.InstrumentGateway = (*OrchestratedGateway)(nil)
}
