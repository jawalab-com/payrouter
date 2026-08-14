// Package server implements the Stripe-compatible HTTP API. It accepts requests
// shaped like Stripe's REST API, routes them through the gateway adapter, and
// returns Stripe-shaped objects so existing Stripe integrations work unchanged.
package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/jawalab-com/payrouter/internal/account"
	"github.com/jawalab-com/payrouter/internal/config"
	"github.com/jawalab-com/payrouter/internal/gateway"
	"github.com/jawalab-com/payrouter/internal/orchestrator"
	"github.com/jawalab-com/payrouter/internal/store"
)

// Server is the Stripe-compatible facade HTTP handler.
type Server struct {
	cfg     config.Config
	store   store.Store
	gw      gateway.Gateway
	mux     *http.ServeMux
	handler http.Handler // mux wrapped in the middleware chain; see buildChain

	// Durable capabilities, resolved from the store in New. They are nil when the
	// store is the in-memory *store.Memory; in that case the legacy code paths run
	// (single-key auth, no idempotency, non-durable reads/writes).
	keys     store.AccountKeyStore
	idem     store.IdempotencyStore
	audit    store.AuditStore
	payments store.PaymentDurable
	inbound  store.InboundStore
	outbound store.OutboundStore
}

// New constructs a Server backed by the given store and gateway adapter. If the
// store implements the durable capability interfaces (i.e. it is the Postgres
// adapter), they are bound here; otherwise they stay nil and legacy paths apply.
func New(cfg config.Config, st store.Store, gw gateway.Gateway) *Server {
	s := &Server{cfg: cfg, store: st, gw: gw, mux: http.NewServeMux()}
	s.keys, _ = st.(store.AccountKeyStore)
	s.idem, _ = st.(store.IdempotencyStore)
	s.audit, _ = st.(store.AuditStore)
	s.payments, _ = st.(store.PaymentDurable)
	s.inbound, _ = st.(store.InboundStore)
	s.outbound, _ = st.(store.OutboundStore)
	s.routes()
	s.handler = s.buildChain()
	return s
}

// buildChain composes the cross-cutting middleware in front of the router.
//
// Order is deliberate and outermost-first:
//
//	request id   — so every line below can be correlated, including panics
//	access log   — outside recovery, so a recovered 500 is still logged
//	recovery     — converts a panic into a Stripe-shaped 500
//	CORS         — answers preflights before auth can reject them
//	rate limit   — sheds load before any handler work or DB round trip
//	body limit   — caps the body before anything reads it
func (s *Server) buildChain() http.Handler {
	var h http.Handler = s.mux
	h = s.limitBody(h)
	if s.cfg.RateLimitRPS > 0 {
		burst := s.cfg.RateLimitBurst
		if burst <= 0 {
			burst = s.cfg.RateLimitRPS
		}
		h = newRateLimiter(s.cfg.RateLimitRPS, burst).middleware(s.cfg.TrustProxyHeaders)(h)
	}
	h = cors(s.cfg.CORSOrigins)(h)
	h = recoverPanics(h)
	h = accessLog(h)
	h = withRequestID(h)
	return h
}

// limitBody caps every request body before a handler can read it. Without it the
// idempotency middleware's io.ReadAll and each handler's ParseForm would read an
// attacker-controlled body into memory with no ceiling. The cap applies to
// gateway callbacks too, which are unauthenticated by design.
func (s *Server) limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, s.maxBodyBytes())
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	s.mux.HandleFunc("GET /health/ready", s.healthReady)

	// Payment Intents (create/retrieve/list/confirm).
	s.mux.HandleFunc("POST /v1/payment_intents", s.auth(s.idempotency("payment_intents.create", s.createPaymentIntent)))
	s.mux.HandleFunc("GET /v1/payment_intents", s.auth(s.listPaymentIntents))
	s.mux.HandleFunc("GET /v1/payment_intents/{id}", s.auth(s.retrievePaymentIntent))
	s.mux.HandleFunc("POST /v1/payment_intents/{id}/confirm", s.auth(s.idempotency("payment_intents.confirm", s.confirmPaymentIntent)))
	s.mux.HandleFunc("POST /v1/refunds", s.auth(s.idempotency("refunds.create", s.createRefund)))
	s.mux.HandleFunc("GET /v1/refunds/{id}", s.auth(s.retrieveRefund))

	// Inbound gateway callbacks are durable as of Phase 5B and are registered in
	// every mode. No Bearer auth — the adapter verifies the gateway signature.
	s.mux.HandleFunc("POST /v1/webhooks/{gateway}", s.handleWebhookIn)

	// Catalog (customers, products, prices) is durable and account-scoped as of
	// Phase 5D part 1, registered in every mode.
	s.mux.HandleFunc("POST /v1/customers", s.auth(s.createCustomer))
	s.mux.HandleFunc("GET /v1/customers", s.auth(s.listCustomers))
	s.mux.HandleFunc("GET /v1/customers/{id}", s.auth(s.retrieveCustomer))
	s.mux.HandleFunc("POST /v1/products", s.auth(s.createProduct))
	s.mux.HandleFunc("GET /v1/products/{id}", s.auth(s.retrieveProduct))
	s.mux.HandleFunc("POST /v1/prices", s.auth(s.createPrice))
	s.mux.HandleFunc("GET /v1/prices/{id}", s.auth(s.retrievePrice))

	// Checkout Sessions (redirect/hosted — the natural analog for all four gateways).
	// Durable and account-scoped as of Phase 5D part 2.
	s.mux.HandleFunc("POST /v1/checkout/sessions", s.auth(s.createCheckoutSession))
	s.mux.HandleFunc("GET /v1/checkout/sessions", s.auth(s.listCheckoutSessions))
	s.mux.HandleFunc("GET /v1/checkout/sessions/{id}", s.auth(s.retrieveCheckoutSession))

	// Subscriptions & Invoices (recurring — durable and account-scoped as of 5D part 2).
	s.mux.HandleFunc("POST /v1/subscriptions", s.auth(s.createSubscription))
	s.mux.HandleFunc("GET /v1/subscriptions/{id}", s.auth(s.retrieveSubscription))
	s.mux.HandleFunc("DELETE /v1/subscriptions/{id}", s.auth(s.cancelSubscription))
	s.mux.HandleFunc("GET /v1/invoices/{id}", s.auth(s.retrieveInvoice))

	// Hosted checkout UI: PUBLIC, unauthenticated routes serving HTML to
	// customers' browsers. Registered only when explicitly enabled, so with the
	// flag off these paths do not exist at all rather than merely 404.
	if s.cfg.CheckoutUI {
		s.registerCheckoutUI()
	}
}

// ServeHTTP implements http.Handler, serving through the middleware chain built
// in New.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

// maxBodyBytes is the per-request body ceiling, from config with a safe default.
func (s *Server) maxBodyBytes() int64 {
	if s.cfg.MaxBodyBytes > 0 {
		return s.cfg.MaxBodyBytes
	}
	return config.DefaultMaxBodyBytes
}

// bodyLimitExceeded reports whether err is the sentinel returned once a request
// body exceeds the MaxBytesReader ceiling, so it can be answered 413 rather than
// being reported as a malformed request.
func bodyLimitExceeded(err error) bool {
	var maxErr *http.MaxBytesError
	return errors.As(err, &maxErr)
}

// parseForm parses a Stripe-style form body and writes the appropriate error
// response on failure, returning false when the caller should stop. An
// over-limit body is reported as 413 rather than 400 so a client can tell "too
// big" from "malformed" — the two need different fixes.
func parseForm(w http.ResponseWriter, r *http.Request) bool {
	err := r.ParseForm()
	if err == nil {
		return true
	}
	if bodyLimitExceeded(err) {
		writeStripeError(w, http.StatusRequestEntityTooLarge, "request_too_large",
			"Request body exceeds the maximum permitted size.")
		return false
	}
	writeStripeError(w, http.StatusBadRequest, "invalid_request_error", "Unable to parse request body.")
	return false
}

// auth validates the Stripe-style "Bearer sk_..." API key. When the store is
// durable (AccountKeyStore present), the key is resolved to an account id and
// stashed in the request context for account-scoped reads/writes; otherwise the
// legacy single-key constant-time compare against cfg.APIKey is used.
func (s *Server) auth(h http.HandlerFunc) http.HandlerFunc {
	const prefix = "Bearer "
	return func(w http.ResponseWriter, r *http.Request) {
		hh := r.Header.Get("Authorization")
		if !strings.HasPrefix(hh, prefix) {
			writeStripeError(w, http.StatusUnauthorized, "authentication_required", "Missing or malformed Authorization header.")
			return
		}
		token := hh[len(prefix):]
		if s.keys != nil {
			accountID, err := s.keys.ResolveAPIKey(r.Context(), token)
			if err != nil {
				writeStripeError(w, http.StatusUnauthorized, "authentication_required", "Invalid API Key provided.")
				return
			}
			r = r.WithContext(account.With(r.Context(), accountID))
		} else if subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.APIKey)) != 1 {
			writeStripeError(w, http.StatusUnauthorized, "authentication_required", "Invalid API Key provided.")
			return
		}
		h(w, r)
	}
}

// idempotency wraps a write handler with Stripe-style Idempotency-Key handling.
// It is a no-op when the store is not durable (no IdempotencyStore) or when no
// Idempotency-Key header is supplied. When active, it claims (account, operation,
// key) against the request body hash: a completed match replays the stored
// response, a different body is a 409 payload mismatch, an in-flight same-body
// request is a 409, and an owned claim runs the handler, stores its response, and
// replays it on retry.
func (s *Server) idempotency(operation string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		if s.idem == nil || key == "" {
			h(w, r)
			return
		}
		accountID := account.From(r.Context())

		body, err := io.ReadAll(r.Body)
		if err != nil {
			if bodyLimitExceeded(err) {
				writeStripeError(w, http.StatusRequestEntityTooLarge, "request_too_large",
					"Request body exceeds the maximum permitted size.")
				return
			}
			writeStripeError(w, http.StatusBadRequest, "invalid_request_error", "Unable to read request body.")
			return
		}
		_ = r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(body))
		canonical := body
		if values, err := url.ParseQuery(string(body)); err == nil {
			canonical = []byte(values.Encode())
		}
		sum := sha256.Sum256([]byte(r.Method + "\n" + r.URL.Path + "\n" + string(canonical)))

		var claim store.ClaimResult
		if err := s.store.RunInTx(r.Context(), func(ctx context.Context) error {
			c, err := s.idem.ClaimIdempotency(ctx, accountID, operation, key, sum[:])
			claim = c
			return err
		}); err != nil {
			switch {
			case errors.Is(err, store.ErrIdempotencyPayloadMismatch):
				writeStripeError(w, http.StatusConflict, "idempotency_payload_mismatch",
					"Keys for idempotent requests can only be used with the same parameters they were first used with.")
			case errors.Is(err, store.ErrIdempotencyInProgress):
				writeStripeError(w, http.StatusConflict, "idempotency_in_progress",
					"A request with the same Idempotency-Key is currently in progress; retry shortly.")
			default:
				writeStripeError(w, http.StatusInternalServerError, "api_error", err.Error())
			}
			return
		}
		if !claim.Owned {
			// Replay the stored response verbatim.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(claim.ResponseStatus)
			_, _ = w.Write(claim.ResponseBody)
			return
		}
		r = r.WithContext(account.WithIdempotency(r.Context(), key))

		// Owned: run the handler against a capturing writer, then store the response.
		rec := newCaptureWriter()
		h(rec, r)
		var finalizeErr error
		if rec.status >= 500 {
			finalizeErr = s.store.RunInTx(r.Context(), func(ctx context.Context) error {
				return s.idem.FailIdempotency(ctx, accountID, operation, key)
			})
		} else {
			finalizeErr = s.store.RunInTx(r.Context(), func(ctx context.Context) error {
				return s.idem.CompleteIdempotency(ctx, accountID, operation, key, rec.status, rec.buf.Bytes())
			})
		}
		if finalizeErr != nil {
			writeStripeError(w, http.StatusInternalServerError, "api_error", "Unable to persist idempotency result.")
			return
		}
		dst := w.Header()
		for k, vs := range rec.h {
			dst[k] = append([]string(nil), vs...)
		}
		w.WriteHeader(rec.status)
		_, _ = w.Write(rec.buf.Bytes())
	}
}

// commandID is deterministic for idempotent commands so a recovered claim uses
// the same provider reference. Requests without a key retain random Stripe IDs.
func commandID(ctx context.Context, prefix, operation string) string {
	key := account.Idempotency(ctx)
	if key == "" {
		return prefix + randID()
	}
	sum := sha256.Sum256([]byte(account.From(ctx) + "\n" + operation + "\n" + key))
	return prefix + hex.EncodeToString(sum[:12])
}

// captureWriter is an http.ResponseWriter that buffers the handler's response so
// the idempotency middleware can store and replay it.
type captureWriter struct {
	h      http.Header
	status int
	buf    bytes.Buffer
}

func newCaptureWriter() *captureWriter {
	return &captureWriter{h: http.Header{}, status: http.StatusOK}
}

func (c *captureWriter) Header() http.Header         { return c.h }
func (c *captureWriter) WriteHeader(status int)      { c.status = status }
func (c *captureWriter) Write(b []byte) (int, error) { return c.buf.Write(b) }

// writeJSON encodes v as a JSON response with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeStripeError emits an error in Stripe's error-envelope shape.
func writeStripeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"type":    stripeErrorType(code),
			"code":    code,
			"message": message,
		},
	})
}

// stripeErrorType maps internal error codes onto Stripe's error "type" values.
func stripeErrorType(code string) string {
	switch {
	case strings.Contains(code, "auth"):
		return "authentication_error"
	case strings.Contains(code, "invalid"), strings.Contains(code, "missing"), strings.Contains(code, "param"):
		return "invalid_request_error"
	default:
		return "api_error"
	}
}

// randID returns a 24-char hex id component (used for pi_/re_ ids and client secrets).
func randID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// formBool reads a Stripe-style boolean form field. The empty string returns def
// (Stripe's default), "true"/"false" are honored, anything else is def.
func formBool(r *http.Request, key string, def bool) bool {
	v := r.PostFormValue(key)
	if v == "" {
		return def
	}
	return v == "true"
}

// healthReady checks operational readiness: the API key is configured, the
// active gateway is named, and (if durable) the signing secret is explicit.
// Returns 200 ready / 503 not ready. Designed for Docker/k8s readiness probes.
func (s *Server) healthReady(w http.ResponseWriter, _ *http.Request) {
	problems := []string{}
	if s.cfg.APIKey == "" {
		problems = append(problems, "api_key_missing")
	}
	if s.cfg.ActiveGateway == "" {
		problems = append(problems, "gateway_not_configured")
	}
	if s.cfg.AppEnv == "production" && s.cfg.Webhook.SigningSecretAuto {
		problems = append(problems, "signing_secret_auto_in_production")
	}
	if len(problems) > 0 {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status":   "not_ready",
			"problems": problems,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// enrichMetadata injects payment_gateway_selected and payment_routing_mode into
// metadata.
//
// payment_routing_mode reports what actually happened, not what was configured.
// Orchestration mode does not guarantee a least-cost decision: when no candidate
// has a fee entry for the resolved channel — every hosted Checkout Session, for
// one, since the method is chosen after the gateway is — the orchestrator falls
// back to priority order. That case reports "fallback_priority", so the metadata
// never claims a fee comparison that did not run.
func (s *Server) enrichMetadata(r *http.Request, res *gateway.PaymentResult) map[string]string {
	m := collectMetadata(r)
	if m == nil {
		m = make(map[string]string)
	}

	var resRef string
	if res != nil {
		resRef = res.Reference
	}
	gwName, _ := orchestrator.ExtractGatewayFromOrderID(resRef)
	if gwName != "" && gwName != "unknown" {
		m["payment_gateway_selected"] = gwName
	} else if s.gw != nil {
		m["payment_gateway_selected"] = s.gw.Name()
	}

	switch {
	case res != nil && res.Routing != nil:
		m["payment_routing_mode"] = string(res.Routing.Basis)
		if res.Routing.Channel != "" {
			m["payment_routing_channel"] = res.Routing.Channel
		}
	case s.isOrchestrated():
		// Orchestrated but the adapter reported no decision (e.g. a direct
		// SubscriptionGateway path): don't assert a basis we can't substantiate.
		m["payment_routing_mode"] = "unknown"
	default:
		m["payment_routing_mode"] = string(gateway.RoutingStatic)
	}

	return m
}

// isOrchestrated reports whether the configured gateway is the dynamic router
// rather than a single named adapter.
func (s *Server) isOrchestrated() bool {
	switch s.cfg.ActiveGateway {
	case "auto", "least_cost", "orchestrated":
		return true
	}
	return false
}
