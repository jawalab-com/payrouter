package config

import (
	"strings"
	"testing"
)

func TestProductionRequiresDurableStorage(t *testing.T) {
	t.Setenv("FACADE_APP_ENV", "production")
	t.Setenv("FACADE_API_KEY", "sk_test_config")
	t.Setenv("FACADE_DATABASE_URL", "")
	t.Setenv("WEBHOOK_SIGNING_SECRET", "whsec_config_test")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "FACADE_DATABASE_URL") {
		t.Fatalf("Load() error = %v, want missing database error", err)
	}
}

func TestProductionRequiresStableWebhookSecret(t *testing.T) {
	t.Setenv("FACADE_APP_ENV", "production")
	t.Setenv("FACADE_API_KEY", "sk_test_config")
	t.Setenv("FACADE_DATABASE_URL", "postgres://example")
	t.Setenv("WEBHOOK_SIGNING_SECRET", "")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "WEBHOOK_SIGNING_SECRET") {
		t.Fatalf("Load() error = %v, want missing webhook secret error", err)
	}
}
