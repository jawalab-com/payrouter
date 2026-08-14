package config

import (
	"strings"
	"testing"
)

func TestProductionRequiresDurableStorage(t *testing.T) {
	t.Setenv("PAYMENT_APP_ENV", "production")
	t.Setenv("PAYMENT_API_KEY", "sk_test_config")
	t.Setenv("PAYMENT_DATABASE_URL", "")
	t.Setenv("WEBHOOK_SIGNING_SECRET", "whsec_config_test")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "PAYMENT_DATABASE_URL") {
		t.Fatalf("Load() error = %v, want missing database error", err)
	}
}

func TestProductionRequiresStableWebhookSecret(t *testing.T) {
	t.Setenv("PAYMENT_APP_ENV", "production")
	t.Setenv("PAYMENT_API_KEY", "sk_test_config")
	t.Setenv("PAYMENT_DATABASE_URL", "postgres://example")
	t.Setenv("WEBHOOK_SIGNING_SECRET", "")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "WEBHOOK_SIGNING_SECRET") {
		t.Fatalf("Load() error = %v, want missing webhook secret error", err)
	}
}

// TestCheckoutUIDefaultsByGatewayMode pins the new posture: the hosted UI is ON
// by default for orchestrated gateways and OFF for a single static one, with
// PAYMENT_CHECKOUT_UI overriding either way. PAYMENT_PUBLIC_URL is no longer
// required, so a missing URL must not fail Load.
func TestCheckoutUIDefaultsByGatewayMode(t *testing.T) {
	cases := []struct {
		gateway  string
		explicit string
		want     bool
	}{
		{"auto", "", true}, {"least_cost", "", true}, {"orchestrated", "", true},
		{"stub", "", false}, {"xendit", "", false}, {"mayar", "", false},
		{"auto", "false", false}, // opt-out keeps the API-only shape even when orchestrated
		{"xendit", "true", true}, // a single-gateway deployment can still opt in
		{"auto", "true", true},
	}
	for _, tc := range cases {
		got := resolveCheckoutUI(tc.gateway, tc.explicit)
		if got != tc.want {
			t.Errorf("resolveCheckoutUI(%q, %q) = %v, want %v", tc.gateway, tc.explicit, got, tc.want)
		}
	}
}

// TestCheckoutUIDoesNotRequirePublicURL: removing the hard-fail is part of the
// contract — orchestration must boot with zero mandatory UI config.
func TestCheckoutUIDoesNotRequirePublicURL(t *testing.T) {
	t.Setenv("PAYMENT_API_KEY", "sk_test_config")
	t.Setenv("PAYMENT_GATEWAY", "auto")
	t.Setenv("PAYMENT_PUBLIC_URL", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() with UI on and no public URL: unexpected error %v", err)
	}
	if !cfg.CheckoutUI {
		t.Error("CheckoutUI should default on for an orchestrated gateway")
	}
	if cfg.PublicURL != "" {
		t.Errorf("PublicURL = %q, want empty (inferred later)", cfg.PublicURL)
	}
}
