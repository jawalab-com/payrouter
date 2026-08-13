package orchestrator

import (
	"fmt"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"
)

// FeeConfig defines the rate structure for a specific channel
type FeeConfig struct {
	Type        string  `yaml:"type"`         // fixed | percentage | mixed
	Fixed       float64 `yaml:"fixed"`        // Fixed fee in minor/major currency (IDR)
	Percentage  float64 `yaml:"percentage"`   // Percentage MDR (e.g. 0.007 for 0.7%)
	VatIncluded bool    `yaml:"vat_included"` // If true, PPN is already included in headline fee
}

// ProviderConfig defines gateway settings and fee schedules
type ProviderConfig struct {
	Enabled        bool                 `yaml:"enabled"`
	Code           string               `yaml:"code"`
	PlatformFeePct float64              `yaml:"platform_fee_pct"` // Optional platform tier fee (e.g. Mayar 1.5%)
	Fees           map[string]FeeConfig `yaml:"fees"`
}

// OrchestratorSettings defines engine rules and VAT configuration
type OrchestratorSettings struct {
	Strategy         string   `yaml:"strategy"`          // lowest_fee | round_robin | priority
	VatRate          float64  `yaml:"vat_rate"`          // e.g. 0.11 for 11% PPN
	FallbackPriority []string `yaml:"fallback_priority"` // e.g. ["midtrans", "xendit", "doku", "mayar"]
}

// RootConfig holds the parsed YAML structure
type RootConfig struct {
	Version      string                    `yaml:"version"`
	Orchestrator OrchestratorSettings      `yaml:"orchestrator"`
	Providers    map[string]ProviderConfig `yaml:"providers"`
}

// envPattern matches ${VAR:default} or ${VAR}
var envPattern = regexp.MustCompile(`\$\{([A-Z0-9_]+)(?::([^}]*))?\}`)

// ExpandEnvDefaults replaces ${VAR:default} syntax in YAML with ENV value or default fallback
func ExpandEnvDefaults(raw []byte) []byte {
	expanded := envPattern.ReplaceAllStringFunc(string(raw), func(match string) string {
		submatches := envPattern.FindStringSubmatch(match)
		if len(submatches) < 2 {
			return match
		}
		varName := submatches[1]
		defaultVal := ""
		if len(submatches) >= 3 {
			defaultVal = submatches[2]
		}

		if envVal := os.Getenv(varName); envVal != "" {
			return envVal
		}
		return defaultVal
	})
	return []byte(expanded)
}

// LoadConfig loads and parses config.yaml from path with environment variable interpolation
func LoadConfig(path string) (*RootConfig, error) {
	if path == "" {
		path = "config.yaml"
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: read config file %s: %w", path, err)
	}

	interpolated := ExpandEnvDefaults(data)

	var cfg RootConfig
	if err := yaml.Unmarshal(interpolated, &cfg); err != nil {
		return nil, fmt.Errorf("orchestrator: unmarshal config YAML: %w", err)
	}

	return &cfg, nil
}

// DefaultConfig provides fallback orchestrator configuration matching config.yaml
func DefaultConfig() *RootConfig {
	return &RootConfig{
		Version: "1.0",
		Orchestrator: OrchestratorSettings{
			Strategy:         "lowest_fee",
			VatRate:          0.11,
			FallbackPriority: []string{"midtrans", "xendit", "doku", "mayar"},
		},
		Providers: map[string]ProviderConfig{
			"midtrans": {
				Enabled: true,
				Code:    "mdt",
				Fees: map[string]FeeConfig{
					"virtual_account":   {Type: "fixed", Fixed: 4000, VatIncluded: false},
					"qris":              {Type: "percentage", Percentage: 0.007, VatIncluded: true},
					"credit_card":       {Type: "mixed", Fixed: 2000, Percentage: 0.029, VatIncluded: false},
					"ewallet_gopay":     {Type: "percentage", Percentage: 0.02, VatIncluded: true},
					"ewallet_shopeepay": {Type: "percentage", Percentage: 0.015, VatIncluded: true},
					"retail_outlet":     {Type: "fixed", Fixed: 5000, VatIncluded: false},
				},
			},
			"xendit": {
				Enabled: true,
				Code:    "xnd",
				Fees: map[string]FeeConfig{
					"virtual_account":   {Type: "fixed", Fixed: 4000, VatIncluded: false},
					"qris":              {Type: "percentage", Percentage: 0.007, VatIncluded: false},
					"credit_card":       {Type: "mixed", Fixed: 2000, Percentage: 0.029, VatIncluded: false},
					"ewallet_gopay":     {Type: "percentage", Percentage: 0.02, VatIncluded: false},
					"ewallet_shopeepay": {Type: "percentage", Percentage: 0.015, VatIncluded: false},
					"retail_outlet":     {Type: "fixed", Fixed: 4000, VatIncluded: false},
				},
			},
			"doku": {
				Enabled: true,
				Code:    "dku",
				Fees: map[string]FeeConfig{
					"virtual_account": {Type: "fixed", Fixed: 4000, VatIncluded: false},
					"qris":            {Type: "percentage", Percentage: 0.007, VatIncluded: false},
					"credit_card":     {Type: "mixed", Fixed: 2000, Percentage: 0.028, VatIncluded: false},
					"retail_outlet":   {Type: "fixed", Fixed: 5000, VatIncluded: false},
				},
			},
			"mayar": {
				Enabled:        true,
				Code:           "myr",
				PlatformFeePct: 0.015,
				// No e-wallet entries: Mayar does not offer GoPay/ShopeePay, so it must
				// never be a candidate for those channels.
				Fees: map[string]FeeConfig{
					"virtual_account": {Type: "fixed", Fixed: 4000, VatIncluded: false},
					"qris":            {Type: "percentage", Percentage: 0.007, VatIncluded: false},
					"credit_card":     {Type: "mixed", Fixed: 2000, Percentage: 0.026, VatIncluded: false},
					"retail_outlet":   {Type: "fixed", Fixed: 5000, VatIncluded: false},
				},
			},
		},
	}
}
