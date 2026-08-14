// Package config holds runtime configuration, loaded from the environment.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// DefaultMaxBodyBytes caps a single request body at 1 MiB. Stripe-shaped form
// payloads are kilobytes at most, and gateway callbacks are smaller still, so
// this leaves generous headroom while bounding memory per request.
const DefaultMaxBodyBytes int64 = 1 << 20

// Default per-caller rate limits. Generous enough that ordinary merchant traffic
// never notices, low enough to blunt credential stuffing and runaway retry loops.
// The limit is per instance and per caller — set PAYMENT_RATE_LIMIT_RPS=0 to
// disable when an upstream gateway or load balancer already enforces quotas.
const (
	DefaultRateLimitRPS   float64 = 50
	DefaultRateLimitBurst float64 = 100
)

// Config holds runtime configuration.
type Config struct {
	AppEnv        string // development | production
	Addr          string // HTTP listen address, e.g. ":8787"
	APIKey        string // secret a Stripe client must present (sk_test_... / sk_live_...); also seeds the default account key when DatabaseURL is set
	Livemode      bool   // true when APIKey is a live key; test keys target gateway sandboxes
	ActiveGateway string // which adapter to use: "stub" (M0), "midtrans" (M1), "xendit" (M4), "doku" (M5), or "mayar" (M5)
	DatabaseURL   string // when non-empty, the facade runs against PostgreSQL (durable); empty => in-memory (tests/dev)
	MaxBodyBytes  int64  // per-request body ceiling; 0 => DefaultMaxBodyBytes
	ConfigPath    string // path to the orchestrator fee schedule (config.yaml)

	// HTTP edge controls. Rate limiting is per-instance and per-caller (API key
	// when presented, client IP otherwise); 0 rps disables it. CORS is off unless
	// origins are listed, and only exact origins are ever allowed.
	RateLimitRPS      float64
	RateLimitBurst    float64
	CORSOrigins       []string
	TrustProxyHeaders bool // honor X-Forwarded-For / X-Real-Ip; only true behind a proxy you control

	// Hosted checkout UI. Defaults ON for orchestrated gateways (auto /
	// least_cost), where a single self-hosted page is what makes multi-gateway
	// routing feel consistent and trustworthy to a customer, and OFF for a single
	// static gateway (one gateway's own hosted page is already consistent).
	// PAYMENT_CHECKOUT_UI=true|false overrides either way. Note the security
	// implication either way: when on, PayRouter serves HTML to the public's
	// browsers on unauthenticated URLs, from the same process that holds every
	// merchant's gateway credentials.
	CheckoutUI bool
	PublicURL  string // external base URL for checkout links; optional — inferred from the request when unset (see server.checkoutBaseURL)
	BrandName  string // merchant name shown on the checkout page
	Midtrans   MidtransConfig
	Xendit     XenditConfig
	Doku       DokuConfig
	Mayar      MayarConfig
	Webhook    WebhookConfig
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

	// SNAP (Bank Indonesia standard) credentials, required only for direct QRIS
	// issuance. DOKU's SNAP APIs authenticate with an RSA keypair whose public
	// half is registered in the DOKU dashboard — the Checkout API's Client-Id and
	// Secret Key alone are not sufficient. When these are absent the adapter keeps
	// using the hosted Checkout page instead.
	PrivateKeyPEM    string // PEM-encoded RSA private key (PKCS#1 or PKCS#8)
	MerchantID       string // merchant identifier sent on QR requests
	TerminalID       string // terminal identifier sent on QR requests
	PartnerServiceID string // DOKU-assigned virtual account prefix (company code / BIN)
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

	// InstrumentMethods opts Mayar into direct instrument issuance (the v2
	// /payments/create API) for the named methods. Accepts a comma list of
	// "qris" and/or "virtual_account". Empty (default) keeps Mayar on the hosted
	// payment link — set it only once the matching channel has been validated on
	// the Mayar dashboard, since issuance otherwise falls back to redirect.
	InstrumentMethods []string
}

// getenv returns os.Getenv(key) if non-empty, otherwise defaultVal.
func getenv(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

// getenvFloat reads a non-negative float from the environment, falling back to
// defaultVal when unset or unparseable. Zero is a meaningful value (it disables
// rate limiting) so it is preserved rather than treated as absent.
func getenvFloat(key string, defaultVal float64) float64 {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return defaultVal
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v < 0 {
		return defaultVal
	}
	return v
}

// readKeyMaterial resolves PEM key material from either an inline value or a
// file path, preferring inline.
//
// Environment variables cannot hold real newlines, so an inline PEM arrives with
// literal backslash-n sequences; those are restored here. A file path suits
// deployments that mount the key as a secret volume. A path that cannot be read
// yields "" rather than an error: the adapter then reports no instrument support
// and falls back to hosted checkout, which is preferable to refusing to boot.
func readKeyMaterial(inline, path string) string {
	if v := strings.TrimSpace(inline); v != "" {
		return strings.ReplaceAll(v, `\n`, "\n")
	}
	if p := strings.TrimSpace(path); p != "" {
		if data, err := os.ReadFile(p); err == nil {
			return string(data)
		}
	}
	return ""
}

// resolveCheckoutUI decides whether the hosted checkout UI is enabled. An explicit
// PAYMENT_CHECKOUT_UI value wins ("true"/"false" and common aliases). When unset,
// the UI is ON by default for orchestrated gateways — where a single self-hosted
// page is what makes least-cost routing across gateways look consistent to a
// customer — and OFF for a single static gateway.
func resolveCheckoutUI(activeGateway, explicit string) bool {
	switch strings.ToLower(strings.TrimSpace(explicit)) {
	case "true", "1", "yes", "on":
		return true
	case "false", "0", "no", "off":
		return false
	default:
		return isOrchestrated(activeGateway)
	}
}

// isOrchestrated reports whether the active gateway is the dynamic least-cost
// orchestrator rather than a single static adapter.
func isOrchestrated(activeGateway string) bool {
	switch strings.ToLower(strings.TrimSpace(activeGateway)) {
	case "auto", "least_cost", "orchestrated":
		return true
	default:
		return false
	}
}

// splitList parses a comma-separated env value into trimmed, non-empty entries.
func splitList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// getenvInt64 reads a positive int64 from the environment, falling back to
// defaultVal when unset, unparseable, or non-positive.
func getenvInt64(key string, defaultVal int64) int64 {
	v, err := strconv.ParseInt(strings.TrimSpace(os.Getenv(key)), 10, 64)
	if err != nil || v <= 0 {
		return defaultVal
	}
	return v
}

// loadDotEnv parses key=val lines from a local .env file without overwriting existing env vars.
func loadDotEnv(filename string) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return
	}
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		k := strings.TrimSpace(parts[0])
		v := strings.TrimSpace(parts[1])
		v = strings.Trim(v, `"'`)
		if os.Getenv(k) == "" {
			_ = os.Setenv(k, v)
		}
	}
}

// Load reads configuration from environment variables with sensible defaults.
func Load() (Config, error) {
	loadDotEnv(".env")
	c := Config{
		AppEnv:        getenv("PAYMENT_APP_ENV", "development"),
		Addr:          getenv("PAYMENT_ADDR", ":8787"),
		APIKey:        os.Getenv("PAYMENT_API_KEY"),
		ActiveGateway: getenv("PAYMENT_GATEWAY", "stub"),
		DatabaseURL:   strings.TrimSpace(os.Getenv("PAYMENT_DATABASE_URL")),
		MaxBodyBytes:  getenvInt64("PAYMENT_MAX_BODY_BYTES", DefaultMaxBodyBytes),
		ConfigPath:    getenv("PAYMENT_CONFIG_PATH", "config.yaml"),

		RateLimitRPS:      getenvFloat("PAYMENT_RATE_LIMIT_RPS", DefaultRateLimitRPS),
		RateLimitBurst:    getenvFloat("PAYMENT_RATE_LIMIT_BURST", DefaultRateLimitBurst),
		CORSOrigins:       splitList(os.Getenv("PAYMENT_CORS_ORIGINS")),
		TrustProxyHeaders: os.Getenv("PAYMENT_TRUST_PROXY_HEADERS") == "true",

		CheckoutUI: resolveCheckoutUI(getenv("PAYMENT_GATEWAY", "stub"), os.Getenv("PAYMENT_CHECKOUT_UI")),
		PublicURL:  strings.TrimRight(strings.TrimSpace(os.Getenv("PAYMENT_PUBLIC_URL")), "/"),
		BrandName:  getenv("PAYMENT_BRAND_NAME", "Checkout"),
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
			// The key may be supplied inline (with literal \n escapes, as env vars
			// cannot carry real newlines) or as a path to a PEM file.
			PrivateKeyPEM:    readKeyMaterial(os.Getenv("DOKU_PRIVATE_KEY"), os.Getenv("DOKU_PRIVATE_KEY_FILE")),
			MerchantID:       strings.TrimSpace(os.Getenv("DOKU_MERCHANT_ID")),
			TerminalID:       strings.TrimSpace(os.Getenv("DOKU_TERMINAL_ID")),
			PartnerServiceID: strings.TrimSpace(os.Getenv("DOKU_PARTNER_SERVICE_ID")),
		},
		Mayar: MayarConfig{
			APIKey:            os.Getenv("MAYAR_API_KEY"),
			WebhookToken:      os.Getenv("MAYAR_WEBHOOK_TOKEN"),
			Sandbox:           getenv("MAYAR_SANDBOX", "true") != "false",
			BaseURL:           strings.TrimSpace(os.Getenv("MAYAR_BASE_URL")),
			InstrumentMethods: splitList(os.Getenv("MAYAR_INSTRUMENT_METHODS")),
		},
		Webhook: WebhookConfig{
			SigningSecret: strings.TrimSpace(os.Getenv("WEBHOOK_SIGNING_SECRET")),
			DeliverURL:    strings.TrimSpace(os.Getenv("PAYMENT_WEBHOOK_URL")),
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
		return Config{}, fmt.Errorf("PAYMENT_API_KEY must be set (use a Stripe-style key, e.g. sk_test_...)")
	}
	if c.AppEnv == "production" && c.DatabaseURL == "" {
		return Config{}, fmt.Errorf("PAYMENT_DATABASE_URL must be set in production; memory storage is test/development only")
	}
	if c.AppEnv == "production" && c.Webhook.SigningSecretAuto {
		return Config{}, fmt.Errorf("WEBHOOK_SIGNING_SECRET must be set in production")
	}
	// The public URL is optional: when the checkout UI is on and PAYMENT_PUBLIC_URL
	// is unset, the server infers checkout links from the incoming request instead
	// of failing to boot (see server.checkoutBaseURL). Production should still set
	// it explicitly — main.go warns when it is missing — because request-inferred
	// links depend on Host headers a client can influence.
	if c.ActiveGateway == "midtrans" && c.Midtrans.ServerKey == "" {
		return Config{}, fmt.Errorf("MIDTRANS_SERVER_KEY must be set when PAYMENT_GATEWAY=midtrans")
	}
	if c.ActiveGateway == "xendit" && (c.Xendit.SecretKey == "" || c.Xendit.WebhookToken == "") {
		return Config{}, fmt.Errorf("XENDIT_SECRET_KEY and XENDIT_WEBHOOK_TOKEN must be set when PAYMENT_GATEWAY=xendit")
	}
	if c.ActiveGateway == "doku" && (c.Doku.ClientID == "" || c.Doku.SecretKey == "") {
		return Config{}, fmt.Errorf("DOKU_CLIENT_ID and DOKU_SECRET_KEY must be set when PAYMENT_GATEWAY=doku")
	}
	if c.ActiveGateway == "mayar" && (c.Mayar.APIKey == "" || c.Mayar.WebhookToken == "") {
		return Config{}, fmt.Errorf("MAYAR_API_KEY and MAYAR_WEBHOOK_TOKEN must be set when PAYMENT_GATEWAY=mayar")
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
