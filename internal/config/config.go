// Package config holds runtime configuration, loaded from the environment.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
)

// Config holds runtime configuration.
type Config struct {
	AppEnv        string // development | production
	Addr          string // HTTP listen address, e.g. ":8787"
	APIKey        string // secret a Stripe client must present (sk_test_... / sk_live_...); also seeds the default account key when DatabaseURL is set
	Livemode      bool   // true when APIKey is a live key; test keys target gateway sandboxes
	ActiveGateway string // which adapter to use: "stub" (M0), "midtrans" (M1), "xendit" (M4), "doku" (M5), or "mayar" (M5)
	DatabaseURL   string // when non-empty, the facade runs against PostgreSQL (durable); empty => in-memory (tests/dev)
	Midtrans      MidtransConfig
	Xendit        XenditConfig
	Doku          DokuConfig
	Mayar         MayarConfig
	Webhook       WebhookConfig
}

// WebhookConfig holds the outbound webhook (re-sign + deliver) settings.
type WebhookConfig struct {
	SigningSecret     string // whsec_... merchants use to verify our Stripe-Signature
	SigningSecretAuto bool   // true when SigningSecret was generated at startup (dev convenience)
	DeliverURL        string // merchant endpoint to forward events to; empty = receive but don't forward
}

// MidtransConfig holds the merchant-supplied (BYO) Midtrans credentials.
type MidtransConfig struct {
	ServerKey string // merchant Midtrans Server Key; required when ActiveGateway == "midtrans"
	Sandbox   bool   // true to target the Midtrans sandbox (default true for safety)
	SnapURL   string // optional override for the Snap base URL (local/self-hosted testing)
	APIURL    string // optional override for the Core API base URL
}

// XenditConfig holds the merchant-supplied (BYO) Xendit credentials. Unlike
// Midtrans, Xendit uses two distinct secrets: the Secret API Key (API auth) and
// the Webhook Verification Token (x-callback-token verification). Both are
// required when ActiveGateway == "xendit".
type XenditConfig struct {
	SecretKey    string // merchant Xendit Secret API Key
	WebhookToken string // merchant Xendit Webhook Verification Token
	BaseURL      string // optional override for the API base URL (local/self-hosted testing)
}

// DokuConfig holds the merchant-supplied (BYO) DOKU credentials: the Client-Id
// (merchant identifier) and Secret Key (HMAC key for the per-call signature). Both
// are required when ActiveGateway == "doku".
type DokuConfig struct {
	ClientID  string // merchant DOKU Client-Id
	SecretKey string // merchant DOKU Secret Key (HMAC key)
	Sandbox   bool   // true to target the DOKU sandbox host (default true for safety)
	BaseURL   string // optional override for the API base URL
}

// MayarConfig holds the merchant-supplied (BYO) Mayar credentials: the API Key
// (Bearer auth) and a shared Webhook Token (verified against the ?token= query
// param on callbacks — Mayar does not sign payloads). Both are required when
// ActiveGateway == "mayar".
type MayarConfig struct {
	APIKey       string // merchant Mayar API Key
	WebhookToken string // shared secret verified against ?token= on callbacks
	Sandbox      bool   // true to target the Mayar sandbox host (default true for safety)
	BaseURL      string // optional override for the API base URL
}

// getenvDual checks primary env key (e.g. PAYMENT_ADDR), then fallback key (FACADE_ADDR), then defaultVal.
func getenvDual(primaryKey, fallbackKey, defaultVal string) string {
	if v := os.Getenv(primaryKey); v != "" {
		return v
	}
	if v := os.Getenv(fallbackKey); v != "" {
		return v
	}
	return defaultVal
}

// Load reads configuration from environment variables with sensible defaults.
func Load() (Config, error) {
	apiKey := getenvDual("PAYMENT_API_KEY", "FACADE_API_KEY", "")
	dbURL := strings.TrimSpace(getenvDual("PAYMENT_DATABASE_URL", "FACADE_DATABASE_URL", ""))
	webhookURL := strings.TrimSpace(getenvDual("PAYMENT_WEBHOOK_URL", "FACADE_WEBHOOK_URL", ""))

	c := Config{
		AppEnv:        getenvDual("PAYMENT_APP_ENV", "FACADE_APP_ENV", "development"),
		Addr:          getenvDual("PAYMENT_ADDR", "FACADE_ADDR", ":8787"),
		APIKey:        apiKey,
		ActiveGateway: getenvDual("PAYMENT_GATEWAY", "FACADE_GATEWAY", "stub"),
		DatabaseURL:   dbURL,
		Midtrans: MidtransConfig{
			ServerKey: os.Getenv("MIDTRANS_SERVER_KEY"),
			Sandbox:   getenv("MIDTRANS_SANDBOX", "true") != "false", // sandbox unless explicitly "false"
			SnapURL:   strings.TrimSpace(os.Getenv("MIDTRANS_SNAP_URL")),
			APIURL:    strings.TrimSpace(os.Getenv("MIDTRANS_API_URL")),
		},
		Xendit: XenditConfig{
			SecretKey:    os.Getenv("XENDIT_SECRET_KEY"),
			WebhookToken: os.Getenv("XENDIT_WEBHOOK_TOKEN"),
			BaseURL:      strings.TrimSpace(os.Getenv("XENDIT_BASE_URL")),
		},
		Doku: DokuConfig{
			ClientID:  os.Getenv("DOKU_CLIENT_ID"),
			SecretKey: os.Getenv("DOKU_SECRET_KEY"),
			Sandbox:   getenv("DOKU_SANDBOX", "true") != "false",
			BaseURL:   strings.TrimSpace(os.Getenv("DOKU_BASE_URL")),
		},
		Mayar: MayarConfig{
			APIKey:       os.Getenv("MAYAR_API_KEY"),
			WebhookToken: os.Getenv("MAYAR_WEBHOOK_TOKEN"),
			Sandbox:      getenv("MAYAR_SANDBOX", "true") != "false",
			BaseURL:      strings.TrimSpace(os.Getenv("MAYAR_BASE_URL")),
		},
		Webhook: WebhookConfig{
			SigningSecret: strings.TrimSpace(os.Getenv("WEBHOOK_SIGNING_SECRET")),
			DeliverURL:    webhookURL,
		},
	}
	if c.Webhook.SigningSecret == "" {
		// Dev convenience: mint an ephemeral secret so webhooks work out of the
		// box. Production deployments should set WEBHOOK_SIGNING_SECRET explicitly.
		s, err := randomSigningSecret()
		if err != nil {
			return Config{}, fmt.Errorf("generate webhook secret: %w", err)
		}
		c.Webhook.SigningSecret = s
		c.Webhook.SigningSecretAuto = true
	}
	c.Livemode = strings.HasPrefix(c.APIKey, "sk_live_")
	if c.APIKey == "" {
		return Config{}, fmt.Errorf("FACADE_API_KEY must be set (use a Stripe-style key, e.g. sk_test_...)")
	}
	if c.AppEnv == "production" && c.DatabaseURL == "" {
		return Config{}, fmt.Errorf("FACADE_DATABASE_URL must be set in production; memory storage is test/development only")
	}
	if c.AppEnv == "production" && c.Webhook.SigningSecretAuto {
		return Config{}, fmt.Errorf("WEBHOOK_SIGNING_SECRET must be set in production")
	}
	if c.ActiveGateway == "midtrans" && c.Midtrans.ServerKey == "" {
		return Config{}, fmt.Errorf("MIDTRANS_SERVER_KEY must be set when FACADE_GATEWAY=midtrans")
	}
	if c.ActiveGateway == "xendit" && (c.Xendit.SecretKey == "" || c.Xendit.WebhookToken == "") {
		return Config{}, fmt.Errorf("XENDIT_SECRET_KEY and XENDIT_WEBHOOK_TOKEN must be set when FACADE_GATEWAY=xendit")
	}
	if c.ActiveGateway == "doku" && (c.Doku.ClientID == "" || c.Doku.SecretKey == "") {
		return Config{}, fmt.Errorf("DOKU_CLIENT_ID and DOKU_SECRET_KEY must be set when FACADE_GATEWAY=doku")
	}
	if c.ActiveGateway == "mayar" && (c.Mayar.APIKey == "" || c.Mayar.WebhookToken == "") {
		return Config{}, fmt.Errorf("MAYAR_API_KEY and MAYAR_WEBHOOK_TOKEN must be set when FACADE_GATEWAY=mayar")
	}
	return c, nil
}

// randomSigningSecret generates a whsec_-prefixed 32-byte random signing secret.
func randomSigningSecret() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "whsec_" + hex.EncodeToString(b[:]), nil
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
