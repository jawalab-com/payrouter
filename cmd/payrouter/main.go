// Command payrouter runs the PayRouter HTTP server — a Stripe-compatible
// payment facade with dynamic least-cost gateway orchestration.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"github.com/jawalab-com/payrouter/internal/adapters/doku"
	"github.com/jawalab-com/payrouter/internal/adapters/mayar"
	"github.com/jawalab-com/payrouter/internal/adapters/midtrans"
	"github.com/jawalab-com/payrouter/internal/adapters/stub"
	"github.com/jawalab-com/payrouter/internal/adapters/xendit"
	"github.com/jawalab-com/payrouter/internal/config"
	"github.com/jawalab-com/payrouter/internal/gateway"
	"github.com/jawalab-com/payrouter/internal/orchestrator"
	"github.com/jawalab-com/payrouter/internal/server"
	"github.com/jawalab-com/payrouter/internal/store"
	"github.com/jawalab-com/payrouter/internal/storepg"
	"github.com/jawalab-com/payrouter/internal/webhook"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		slog.Error("config load failed", "error", err)
		os.Exit(1)
	}

	gw, err := selectGateway(cfg)
	if err != nil {
		slog.Error("gateway init failed", "error", err)
		os.Exit(1)
	}

	// Shutdown context, created early so DB bootstrap can honor interrupts too.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Persistence: durable PostgreSQL when PAYMENT_DATABASE_URL is set, otherwise
	// the in-memory store (zero-DB stateless mode for dev/simple deployments).
	var st store.Store = store.NewMemory()
	if cfg.DatabaseURL != "" {
		pool, err := storepg.ConnectPool(ctx, cfg.DatabaseURL)
		if err != nil {
			slog.Error("database connect failed", "error", err)
			os.Exit(1)
		}
		defer pool.Close()
		if err := storepg.Migrate(ctx, pool); err != nil {
			slog.Error("database migration failed", "error", err)
			os.Exit(1)
		}
		if err := storepg.SeedDefaultAccount(ctx, pool, cfg.APIKey); err != nil {
			slog.Error("account seed failed", "error", err)
			os.Exit(1)
		}
		st = storepg.New(pool, gw.Name())
		slog.Info("durable storage enabled", "backend", "postgresql")
	}

	if cfg.Webhook.SigningSecretAuto {
		slog.Warn("WEBHOOK_SIGNING_SECRET not set; generated ephemeral secret — set explicitly in production",
			"secret", cfg.Webhook.SigningSecret)
	}

	// Outbound webhook delivery: receive gateway callbacks, re-sign as Stripe
	// events, and forward to the merchant endpoint. Runs until shutdown.
	go webhook.New(st, cfg.Webhook.DeliverURL, cfg.Webhook.SigningSecret, nil).Start(ctx)

	srv := server.New(cfg, st, gw)
	// Reclaim inbound provider notifications left 'processing' by a crash (durable
	// deployments only; no-ops on the in-memory store).
	go server.NewReconciler(srv).Start(ctx)

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv,
		ReadHeaderTimeout: 5 * time.Second,
	}

	slog.Info("payrouter started",
		"addr", cfg.Addr,
		"gateway", cfg.ActiveGateway,
		"livemode", cfg.Livemode,
	)

	// Serve until interrupted, then shut down gracefully so the deliverer drains.
	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.ListenAndServe() }()
	select {
	case <-ctx.Done():
		slog.Info("shutting down gracefully...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	case err := <-errCh:
		if err != nil {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}
}

// selectGateway builds the adapter named by cfg.ActiveGateway. Options include
// static gateways (midtrans, xendit, doku, mayar, stub) or auto / least_cost
// for dynamic fee-optimized routing across all gateways with valid credentials.
func selectGateway(cfg config.Config) (gateway.Gateway, error) {
	switch cfg.ActiveGateway {
	case "", "stub":
		return stub.New(), nil

	case "auto", "least_cost", "orchestrated":
		// PAYMENT_CONFIG_PATH is documented, so honor it rather than assuming the
		// fee schedule sits in the process working directory.
		orchCfg, err := orchestrator.LoadConfig(cfg.ConfigPath)
		if err != nil {
			// Not fatal — the compiled-in schedule is kept in sync with config.yaml
			// by test — but it means every fee override in that file is being
			// ignored, which is invisible from the outside. Say so plainly.
			slog.Warn("orchestrator: fee schedule not loaded; routing on compiled-in defaults, all config.yaml overrides ignored",
				"path", cfg.ConfigPath,
				"error", err,
				"hint", "set PAYMENT_CONFIG_PATH or mount config.yaml into the container")
			orchCfg = orchestrator.DefaultConfig()
		} else {
			slog.Info("orchestrator: fee schedule loaded", "path", cfg.ConfigPath)
		}

		adapters := orchestrator.BuildCandidateAdapters(cfg, func(name string) (gateway.Gateway, bool) {
			switch name {
			case "midtrans":
				a := midtrans.New(cfg.Midtrans.ServerKey, cfg.Midtrans.Sandbox)
				a.SetBaseURLs(cfg.Midtrans.SnapURL, cfg.Midtrans.APIURL)
				return a, true
			case "xendit":
				a := xendit.New(cfg.Xendit.SecretKey, cfg.Xendit.WebhookToken)
				a.SetBaseURL(cfg.Xendit.BaseURL)
				return a, true
			case "doku":
				a := doku.New(cfg.Doku.ClientID, cfg.Doku.SecretKey, cfg.Doku.Sandbox)
				a.SetBaseURL(cfg.Doku.BaseURL)
				enableDokuSNAP(a, cfg)
				return a, true
			case "mayar":
				a := mayar.New(cfg.Mayar.APIKey, cfg.Mayar.WebhookToken, cfg.Mayar.Sandbox)
				a.SetBaseURL(cfg.Mayar.BaseURL)
				return a, true
			}
			return nil, false
		})

		// Sort for deterministic log output.
		var names []string
		for name := range adapters {
			names = append(names, name)
		}
		sort.Strings(names)
		slog.Info("orchestration mode enabled", "strategy", "least_cost", "gateways", names)
		return orchestrator.NewOrchestratedGateway(orchCfg, adapters), nil

	case "midtrans":
		a := midtrans.New(cfg.Midtrans.ServerKey, cfg.Midtrans.Sandbox)
		a.SetBaseURLs(cfg.Midtrans.SnapURL, cfg.Midtrans.APIURL)
		return a, nil
	case "xendit":
		a := xendit.New(cfg.Xendit.SecretKey, cfg.Xendit.WebhookToken)
		a.SetBaseURL(cfg.Xendit.BaseURL)
		return a, nil
	case "doku":
		a := doku.New(cfg.Doku.ClientID, cfg.Doku.SecretKey, cfg.Doku.Sandbox)
		a.SetBaseURL(cfg.Doku.BaseURL)
		enableDokuSNAP(a, cfg)
		return a, nil
	case "mayar":
		a := mayar.New(cfg.Mayar.APIKey, cfg.Mayar.WebhookToken, cfg.Mayar.Sandbox)
		a.SetBaseURL(cfg.Mayar.BaseURL)
		return a, nil
	default:
		return nil, UnknownGatewayError(cfg.ActiveGateway)
	}
}

// UnknownGatewayError is returned for an unrecognized PAYMENT_GATEWAY value.
type UnknownGatewayError string

func (e UnknownGatewayError) Error() string { return "unknown gateway: " + string(e) }

// enableDokuSNAP turns on DOKU's direct QRIS issuance when SNAP credentials are
// configured. It is deliberately non-fatal: without SNAP the adapter reports no
// instrument support and payments fall back to the hosted Checkout page, so a
// deployment that has not yet registered an RSA key still works.
func enableDokuSNAP(a *doku.Adapter, cfg config.Config) {
	if cfg.Doku.PrivateKeyPEM == "" {
		slog.Debug("doku: SNAP not configured; QRIS will use the hosted checkout page",
			"hint", "set DOKU_PRIVATE_KEY (or DOKU_PRIVATE_KEY_FILE), DOKU_MERCHANT_ID and DOKU_TERMINAL_ID")
		return
	}
	if err := a.EnableSNAP(doku.SNAPConfig{
		PrivateKeyPEM:    []byte(cfg.Doku.PrivateKeyPEM),
		MerchantID:       cfg.Doku.MerchantID,
		TerminalID:       cfg.Doku.TerminalID,
		PartnerServiceID: cfg.Doku.PartnerServiceID,
	}); err != nil {
		slog.Warn("doku: SNAP credentials rejected; falling back to hosted checkout", "error", err)
		return
	}
	// Report what was actually unlocked: QRIS and virtual accounts need different
	// credentials, so a partial setup enables only one.
	var enabled []string
	if a.SupportsInstrument(gateway.IDQRIS) {
		enabled = append(enabled, "qris")
	}
	if a.SupportsInstrument(gateway.IDVirtualAccount) {
		enabled = append(enabled, "virtual_account")
	}
	slog.Info("doku: SNAP enabled", "direct_instruments", enabled)
}
