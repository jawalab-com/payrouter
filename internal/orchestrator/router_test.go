package orchestrator

import (
	"os"
	"testing"
)

func TestExpandEnvDefaults(t *testing.T) {
	os.Setenv("TEST_VA_FIXED", "2500")
	defer os.Unsetenv("TEST_VA_FIXED")

	input := []byte(`
vat_rate: ${TEST_VAT:0.11}
fixed_fee: ${TEST_VA_FIXED:4000}
`)

	expanded := string(ExpandEnvDefaults(input))

	if !contains(expanded, "vat_rate: 0.11") {
		t.Errorf("expected default vat_rate 0.11, got: %s", expanded)
	}
	if !contains(expanded, "fixed_fee: 2500") {
		t.Errorf("expected env overridden fixed_fee 2500, got: %s", expanded)
	}
}

func TestCalculateCost(t *testing.T) {
	cfg, err := LoadConfig("../../config.yaml")
	if err != nil {
		t.Fatalf("failed to load config.yaml: %v", err)
	}

	router := NewRouter(cfg)

	// Amount: 100,000 IDR
	amount := 100000.0

	// 1. Midtrans QRIS (0.7%, VAT included) -> 100,000 * 0.007 = 700 IDR (no extra PPN)
	costMdtQRIS, ok := router.CalculateCost("midtrans", "qris", amount)
	if !ok || costMdtQRIS != 700 {
		t.Errorf("expected Midtrans QRIS fee 700 IDR, got: %v (ok=%v)", costMdtQRIS, ok)
	}

	// 2. Xendit QRIS (0.7%, VAT NOT included) -> (100,000 * 0.007) * 1.11 = 777 IDR
	costXndQRIS, ok := router.CalculateCost("xendit", "qris", amount)
	if !ok || costXndQRIS != 777 {
		t.Errorf("expected Xendit QRIS fee 777 IDR, got: %v (ok=%v)", costXndQRIS, ok)
	}

	// Midtrans must be cheaper than Xendit for QRIS due to vat_included=true
	if costMdtQRIS >= costXndQRIS {
		t.Errorf("Midtrans QRIS (%v) should be cheaper than Xendit QRIS (%v)", costMdtQRIS, costXndQRIS)
	}

	// 3. Mayar Credit Card (2.6% + 2000 fixed + 1.5% platform fee) + 11% PPN
	// Base CC fee: (100000 * 0.026) + 2000 = 2600 + 2000 = 4600 IDR
	// Platform fee: 100000 * 0.015 = 1500 IDR
	// Total Base: 4600 + 1500 = 6100 IDR
	// Taxed Total: 6100 * 1.11 = 6771 IDR
	costMyrCC, ok := router.CalculateCost("mayar", "credit_card", amount)
	if !ok || costMyrCC != 6771 {
		t.Errorf("expected Mayar CC fee 6771 IDR, got: %v (ok=%v)", costMyrCC, ok)
	}
}

func TestSelectBestGateway(t *testing.T) {
	cfg, err := LoadConfig("../../config.yaml")
	if err != nil {
		t.Fatalf("failed to load config.yaml: %v", err)
	}

	router := NewRouter(cfg)

	// For QRIS 100k, Midtrans (700 IDR) beats Xendit/Mayar/DOKU (777 IDR)
	bestQRIS, fee, _ := router.SelectBestGateway(nil, "qris", 100000)
	if bestQRIS != "midtrans" || fee != 700 {
		t.Errorf("expected Midtrans (700 IDR) to win QRIS, got: %s (fee=%v)", bestQRIS, fee)
	}
}

func TestStatelessOrderID(t *testing.T) {
	formatted := FormatOrderID("xendit", "cs_test_12345")
	if formatted != "pf_xnd_cs_test_12345" {
		t.Errorf("expected pf_xnd_cs_test_12345, got: %s", formatted)
	}

	gw, id := ExtractGatewayFromOrderID("pf_xnd_cs_test_12345")
	if gw != "xendit" || id != "cs_test_12345" {
		t.Errorf("expected ('xendit', 'cs_test_12345'), got: ('%s', '%s')", gw, id)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && (s[:len(substr)] == substr || contains(s[1:], substr)))
}
