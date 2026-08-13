package orchestrator

import "testing"

// TestDefaultConfigMatchesShippedYAML keeps the compiled-in fallback fee schedule
// identical to config.yaml.
//
// DefaultConfig() is used whenever config.yaml cannot be read — which is the norm
// in a container image that ships only the binary. When the two disagree, those
// deployments silently route on a different fee schedule than the documented one,
// with no error and no way to notice from the outside. Divergences found when this
// test was written included Xendit VA priced Rp500 high, DOKU cards 0.1pp high,
// and Mayar offered as a candidate for GoPay/ShopeePay, which it does not support.
//
// config.yaml is authoritative: it is what operators read, edit and override.
func TestDefaultConfigMatchesShippedYAML(t *testing.T) {
	shipped, err := LoadConfig("../../config.yaml")
	if err != nil {
		t.Fatalf("load config.yaml: %v", err)
	}
	fallback := DefaultConfig()

	for name, want := range shipped.Providers {
		got, ok := fallback.Providers[name]
		if !ok {
			t.Errorf("provider %q in config.yaml but missing from DefaultConfig()", name)
			continue
		}
		if got.PlatformFeePct != want.PlatformFeePct {
			t.Errorf("%s platform_fee_pct: DefaultConfig()=%v config.yaml=%v",
				name, got.PlatformFeePct, want.PlatformFeePct)
		}
		for channel, wantFee := range want.Fees {
			gotFee, ok := got.Fees[channel]
			if !ok {
				t.Errorf("%s/%s: in config.yaml but missing from DefaultConfig()", name, channel)
				continue
			}
			if gotFee != wantFee {
				t.Errorf("%s/%s: DefaultConfig()=%+v config.yaml=%+v", name, channel, gotFee, wantFee)
			}
		}
		// The reverse direction matters just as much: an extra channel in the
		// fallback makes a gateway eligible for a method it does not support.
		for channel := range got.Fees {
			if _, ok := want.Fees[channel]; !ok {
				t.Errorf("%s/%s: in DefaultConfig() but not config.yaml — would route a method the gateway may not offer",
					name, channel)
			}
		}
	}
	for name := range fallback.Providers {
		if _, ok := shipped.Providers[name]; !ok {
			t.Errorf("provider %q in DefaultConfig() but not config.yaml", name)
		}
	}
	if fallback.Orchestrator.VatRate != shipped.Orchestrator.VatRate {
		t.Errorf("vat_rate: DefaultConfig()=%v config.yaml=%v",
			fallback.Orchestrator.VatRate, shipped.Orchestrator.VatRate)
	}
}
