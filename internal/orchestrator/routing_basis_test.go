package orchestrator

import (
	"context"
	"testing"

	"github.com/jawalab-com/payrouter/internal/gateway"
)

// TestEveryPaymentMethodTypeResolves pins the mapping between the documented
// gateway.IDPaymentMethodType constants and the config fee-table keys.
//
// Regression: id_card, id_retail, id_ewallet and id_hosted previously fell
// through NormalizeChannel's default branch and matched no provider's fee table,
// so four of six method types silently skipped least-cost routing entirely.
func TestEveryPaymentMethodTypeResolves(t *testing.T) {
	cases := []struct {
		method  gateway.IDPaymentMethodType
		params  map[string]any
		want    string
		wantOK  bool
		comment string
	}{
		{gateway.IDCard, nil, "credit_card", true, "cards must price against credit_card"},
		{gateway.IDQRIS, nil, "qris", true, ""},
		{gateway.IDVirtualAccount, nil, "virtual_account", true, ""},
		{gateway.IDRetail, nil, "retail_outlet", true, "retail must price against retail_outlet"},
		{gateway.IDEWallet, map[string]any{"wallet": "gopay"}, "ewallet_gopay", true, "wallet param disambiguates"},
		{gateway.IDEWallet, map[string]any{"wallet": "shopeepay"}, "ewallet_shopeepay", true, ""},
		// Structurally unresolvable, and that must stay explicit rather than
		// silently defaulting to some arbitrary channel.
		{gateway.IDEWallet, nil, "", false, "bare ewallet has no wallet to price"},
		{gateway.IDHosted, nil, "", false, "hosted picks the method after the gateway"},
	}

	for _, tc := range cases {
		got, ok := ResolveChannel(string(tc.method), tc.params)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("ResolveChannel(%q, %v) = (%q, %v), want (%q, %v) — %s",
				tc.method, tc.params, got, ok, tc.want, tc.wantOK, tc.comment)
		}
	}
}

// TestResolvedChannelsAreActuallyPriced closes the loop: a channel that resolves
// must also be quotable by at least one provider, otherwise the mapping is
// cosmetic and routing still falls back to priority order.
func TestResolvedChannelsAreActuallyPriced(t *testing.T) {
	r := NewRouter(DefaultConfig())
	candidates := []string{"midtrans", "xendit", "doku", "mayar"}

	for _, m := range []gateway.IDPaymentMethodType{
		gateway.IDCard, gateway.IDQRIS, gateway.IDVirtualAccount, gateway.IDRetail,
	} {
		ch, ok := ResolveChannel(string(m), nil)
		if !ok {
			t.Fatalf("%s should resolve", m)
		}
		info := r.SelectBest(candidates, ch, 100_000)
		if info.Basis != gateway.RoutingLeastCost {
			t.Errorf("%s (channel %q): basis = %q, want %q — no provider priced it",
				m, ch, info.Basis, gateway.RoutingLeastCost)
		}
		if info.Compared == 0 {
			t.Errorf("%s (channel %q): compared 0 candidates", m, ch)
		}
	}
}

// TestHostedReportsFallbackNotLeastCost is the honesty guarantee: a hosted
// checkout cannot be cost-routed, and the decision must say so rather than
// masquerading as a least-cost win.
func TestHostedReportsFallbackNotLeastCost(t *testing.T) {
	r := NewRouter(DefaultConfig())
	info := r.SelectBest([]string{"midtrans", "xendit"}, string(gateway.IDHosted), 100_000)

	if info.Basis != gateway.RoutingFallbackPriority {
		t.Errorf("basis = %q, want %q", info.Basis, gateway.RoutingFallbackPriority)
	}
	if info.Compared != 0 {
		t.Errorf("compared = %d, want 0 (nothing is priceable for hosted)", info.Compared)
	}
	if info.Gateway == "" {
		t.Error("fallback must still yield a usable gateway")
	}
}

// TestFallbackStaysWithinCandidates guards a subtler bug in the old code: the
// fallback returned FallbackPriority[0] unconditionally, which could name a
// gateway that has no loaded adapter (no credentials configured) and fail the
// payment outright.
func TestFallbackStaysWithinCandidates(t *testing.T) {
	// Only mayar has credentials; midtrans heads the fallback priority list.
	info := NewRouter(DefaultConfig()).SelectBest([]string{"mayar"}, string(gateway.IDHosted), 50_000)

	if info.Gateway != "mayar" {
		t.Errorf("gateway = %q, want %q — fallback must stay within loaded adapters",
			info.Gateway, "mayar")
	}
}

// TestFeeTiesBreakOnOperatorPriority pins tie-breaking behavior. Published MDR
// is frequently identical across gateways (virtual_account is Rp4.000 + PPN on
// both Midtrans and DOKU), so ties are the common case, not an edge case. They
// must resolve to the operator's configured priority rather than to map
// iteration order or alphabetical accident.
func TestFeeTiesBreakOnOperatorPriority(t *testing.T) {
	cfg := DefaultConfig()
	r := NewRouter(cfg)

	// midtrans and doku both quote 4000 * 1.11 = 4440 for virtual_account.
	mdt, _ := r.CalculateCost("midtrans", "virtual_account", 100_000)
	dku, _ := r.CalculateCost("doku", "virtual_account", 100_000)
	if mdt != dku {
		t.Fatalf("fixture no longer a tie (midtrans=%v doku=%v); pick another channel", mdt, dku)
	}

	// midtrans precedes doku in the default fallback_priority list.
	if info := r.SelectBest([]string{"doku", "midtrans"}, "virtual_account", 100_000); info.Gateway != "midtrans" {
		t.Errorf("tie went to %q, want midtrans (earlier in fallback_priority)", info.Gateway)
	}

	// Reversing the operator's stated preference must reverse the winner.
	cfg.Orchestrator.FallbackPriority = []string{"doku", "midtrans", "xendit", "mayar"}
	if info := NewRouter(cfg).SelectBest([]string{"doku", "midtrans"}, "virtual_account", 100_000); info.Gateway != "doku" {
		t.Errorf("tie went to %q, want doku after reordering fallback_priority", info.Gateway)
	}
}

// TestLeastCostPicksCheapest verifies the routing still does its actual job, and
// that it now reaches a channel (credit_card) that was previously unreachable
// through the IDPaymentMethodType path.
func TestLeastCostPicksCheapest(t *testing.T) {
	r := NewRouter(DefaultConfig())

	// QRIS: Midtrans includes PPN (700) vs others at 777.
	if info := r.SelectBest(nil, "qris", 100_000); info.Gateway != "midtrans" || info.FeeMinor != 700 {
		t.Errorf("qris: got %s @ %v, want midtrans @ 700", info.Gateway, info.FeeMinor)
	}

	// Credit card via the id_card alias — the path that was silently broken.
	ch, _ := ResolveChannel(string(gateway.IDCard), nil)
	info := r.SelectBest([]string{"doku", "mayar"}, ch, 100_000)
	if info.Basis != gateway.RoutingLeastCost {
		t.Fatalf("id_card basis = %q, want least_cost", info.Basis)
	}
	// DOKU 2.8%+2000 with PPN beats Mayar 2.6%+2000 plus a 1.5% platform fee.
	if info.Gateway != "doku" {
		t.Errorf("id_card: got %s @ %v, want doku (mayar's platform fee makes it dearer)",
			info.Gateway, info.FeeMinor)
	}
}

// TestCreatePaymentAttachesRoutingDecision verifies the decision survives out to
// the PaymentResult, which is what the server reports as payment_routing_mode.
func TestCreatePaymentAttachesRoutingDecision(t *testing.T) {
	adapters := map[string]gateway.Gateway{
		"midtrans": &mockGateway{name: "midtrans"},
		"xendit":   &mockGateway{name: "xendit"},
	}
	og := NewOrchestratedGateway(DefaultConfig(), adapters)

	t.Run("qris is least_cost", func(t *testing.T) {
		res, err := og.CreatePayment(context.Background(), &gateway.CreatePaymentInput{
			Reference: "pi_1", AmountMinor: 100_000, Currency: "idr",
			PaymentMethodType: gateway.IDQRIS,
		})
		if err != nil {
			t.Fatalf("CreatePayment: %v", err)
		}
		if res.Routing == nil {
			t.Fatal("Routing must be attached so the server can report it")
		}
		if res.Routing.Basis != gateway.RoutingLeastCost {
			t.Errorf("basis = %q, want least_cost", res.Routing.Basis)
		}
		if res.Routing.Channel != "qris" {
			t.Errorf("channel = %q, want qris", res.Routing.Channel)
		}
	})

	t.Run("hosted is fallback_priority", func(t *testing.T) {
		res, err := og.CreatePayment(context.Background(), &gateway.CreatePaymentInput{
			Reference: "pi_2", AmountMinor: 100_000, Currency: "idr",
			PaymentMethodType: gateway.IDHosted,
		})
		if err != nil {
			t.Fatalf("CreatePayment: %v", err)
		}
		if res.Routing == nil {
			t.Fatal("Routing must be attached even on the fallback path")
		}
		if res.Routing.Basis != gateway.RoutingFallbackPriority {
			t.Errorf("basis = %q, want fallback_priority — hosted cannot be cost-routed",
				res.Routing.Basis)
		}
	})
}
