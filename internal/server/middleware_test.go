package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jawalab-com/payrouter/internal/adapters/stub"
	"github.com/jawalab-com/payrouter/internal/config"
	"github.com/jawalab-com/payrouter/internal/store"
)

// serveThrough runs one request through a middleware decorator.
func serveThrough(mw func(http.Handler) http.Handler, h http.HandlerFunc, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	mw(h).ServeHTTP(rec, r)
	return rec
}

// --- panic recovery -------------------------------------------------------

// TestPanicBecomesStripeError is the reason this middleware exists: stdlib
// net/http survives a handler panic but closes the connection with no response,
// so a client mid-payment sees a transport error rather than a parseable one.
func TestPanicBecomesStripeError(t *testing.T) {
	chain := func(h http.Handler) http.Handler { return accessLog(recoverPanics(h)) }
	rec := serveThrough(chain, func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}, httptest.NewRequest(http.MethodGet, "/v1/payment_intents", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"error"`) || !strings.Contains(body, `"type":"api_error"`) {
		t.Errorf("want a Stripe error envelope, got: %s", body)
	}
	// The panic value must not reach the client.
	if strings.Contains(body, "boom") {
		t.Errorf("panic detail leaked to client: %s", body)
	}
}

// TestPanicAfterPartialWriteDoesNotCorruptBody guards against appending an error
// envelope onto a response the handler already began.
func TestPanicAfterPartialWriteDoesNotCorruptBody(t *testing.T) {
	chain := func(h http.Handler) http.Handler { return accessLog(recoverPanics(h)) }
	rec := serveThrough(chain, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"pi_partial"`))
		panic("late failure")
	}, httptest.NewRequest(http.MethodGet, "/x", nil))

	if strings.Contains(rec.Body.String(), "api_error") {
		t.Errorf("error envelope appended to an already-started response: %s", rec.Body.String())
	}
}

// TestErrAbortHandlerIsNotSwallowed keeps the stdlib's deliberate-abort contract
// intact rather than converting it into a 500.
func TestErrAbortHandlerIsNotSwallowed(t *testing.T) {
	defer func() {
		if rec := recover(); rec != http.ErrAbortHandler {
			t.Errorf("recovered %v, want http.ErrAbortHandler to propagate", rec)
		}
	}()
	rec := httptest.NewRecorder()
	recoverPanics(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	})).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	t.Fatal("expected ErrAbortHandler to propagate")
}

// --- request id -----------------------------------------------------------

func TestRequestIDGeneratedAndEchoed(t *testing.T) {
	var seen string
	rec := serveThrough(withRequestID, func(_ http.ResponseWriter, r *http.Request) {
		seen = RequestID(r.Context())
	}, httptest.NewRequest(http.MethodGet, "/x", nil))

	if seen == "" {
		t.Fatal("no request id in context")
	}
	if got := rec.Header().Get("X-Request-Id"); got != seen {
		t.Errorf("echoed %q, context has %q", got, seen)
	}
}

func TestInboundRequestIDHonoredAndSanitized(t *testing.T) {
	cases := []struct{ in, want string }{
		{"trace-abc_123.4", "trace-abc_123.4"},              // preserved verbatim
		{"bad\nvalue\rhere", "badvaluehere"},                // CRLF stripped: log injection
		{"drop\x00null", "dropnull"},                        // control chars stripped
		{strings.Repeat("a", 200), strings.Repeat("a", 64)}, // truncated
		{"!!!", ""}, // nothing usable -> regenerate
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set("X-Request-Id", tc.in)

		var seen string
		serveThrough(withRequestID, func(_ http.ResponseWriter, r *http.Request) {
			seen = RequestID(r.Context())
		}, req)

		if tc.want == "" {
			if !strings.HasPrefix(seen, "req_") {
				t.Errorf("input %q: want a generated id, got %q", tc.in, seen)
			}
			continue
		}
		if seen != tc.want {
			t.Errorf("input %q: got %q, want %q", tc.in, seen, tc.want)
		}
	}
}

// --- rate limiting --------------------------------------------------------

func TestRateLimitAllowsBurstThenRejects(t *testing.T) {
	l := newRateLimiter(1, 3)
	now := time.Now()

	for i := range 3 {
		if !l.allow("k", now) {
			t.Fatalf("request %d within burst was rejected", i+1)
		}
	}
	if l.allow("k", now) {
		t.Error("burst exhausted but request still allowed")
	}
	// Tokens refill at 1/sec.
	if !l.allow("k", now.Add(time.Second)) {
		t.Error("token should have refilled after 1s")
	}
}

func TestRateLimitIsPerCaller(t *testing.T) {
	l := newRateLimiter(1, 1)
	now := time.Now()

	if !l.allow("merchant_a", now) || l.allow("merchant_a", now) {
		t.Fatal("bucket for merchant_a behaved unexpectedly")
	}
	// A different merchant must be unaffected by the first one's flood.
	if !l.allow("merchant_b", now) {
		t.Error("one caller's exhaustion leaked into another's bucket")
	}
}

func TestRateLimitReturns429WithRetryAfter(t *testing.T) {
	mw := newRateLimiter(1, 1).middleware(false)
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	req := httptest.NewRequest(http.MethodPost, "/v1/payment_intents", nil)

	first := httptest.NewRecorder()
	mw(h).ServeHTTP(first, req)
	second := httptest.NewRecorder()
	mw(h).ServeHTTP(second, req)

	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", second.Code)
	}
	if second.Header().Get("Retry-After") == "" {
		t.Error("429 without Retry-After")
	}
	if !strings.Contains(second.Body.String(), "rate_limit_error") {
		t.Errorf("want a Stripe rate_limit_error envelope: %s", second.Body.String())
	}
}

// TestRateLimitKeyPrefersAPIKey verifies callers are distinguished by key, and
// that the raw secret is never used as the map key.
func TestRateLimitKeyPrefersAPIKey(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer sk_test_supersecret")
	req.RemoteAddr = "10.0.0.1:1234"

	key := rateLimitKey(req, false)
	if !strings.HasPrefix(key, "key:") {
		t.Errorf("key = %q, want the API key to take precedence over IP", key)
	}
	if strings.Contains(key, "supersecret") {
		t.Errorf("raw API key retained in limiter state: %q", key)
	}

	anon := httptest.NewRequest(http.MethodGet, "/x", nil)
	anon.RemoteAddr = "10.0.0.1:1234"
	if got := rateLimitKey(anon, false); got != "ip:10.0.0.1" {
		t.Errorf("unauthenticated key = %q, want ip:10.0.0.1", got)
	}
}

// TestStaleBucketsAreEvicted: the limiter map is keyed by untrusted input, so
// unbounded growth would itself be a memory-exhaustion vector.
func TestStaleBucketsAreEvicted(t *testing.T) {
	l := newRateLimiter(1, 1)
	old := time.Now()
	l.allow("ancient", old)

	// A new caller seen well after the TTL triggers the sweep.
	l.allow("fresh", old.Add(staleBucketTTL+time.Minute))

	l.mu.Lock()
	_, stillThere := l.buckets["ancient"]
	l.mu.Unlock()
	if stillThere {
		t.Error("stale bucket was not evicted")
	}
}

// --- proxy headers --------------------------------------------------------

// TestClientIPIgnoresSpoofedHeadersByDefault: trusting X-Forwarded-For
// unconditionally would let any caller pick their own rate-limit bucket.
func TestClientIPIgnoresSpoofedHeadersByDefault(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.RemoteAddr = "10.0.0.9:5555"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")

	if got := clientIP(req, false); got != "10.0.0.9" {
		t.Errorf("clientIP = %q, want the socket address when proxies are untrusted", got)
	}
	if got := clientIP(req, true); got != "1.2.3.4" {
		t.Errorf("clientIP = %q, want the forwarded address when proxies are trusted", got)
	}
}

func TestClientIPUsesLeftmostForwardedEntry(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.RemoteAddr = "10.0.0.9:5555"
	req.Header.Set("X-Forwarded-For", "203.0.113.7, 70.41.3.18, 150.172.238.178")

	if got := clientIP(req, true); got != "203.0.113.7" {
		t.Errorf("clientIP = %q, want the original client (leftmost)", got)
	}
}

// --- CORS -----------------------------------------------------------------

func TestCORSDisabledByDefault(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/payment_intents", nil)
	req.Header.Set("Origin", "https://evil.test")

	rec := serveThrough(cors(nil), func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}, req)

	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("CORS headers emitted with no origins configured")
	}
}

func TestCORSAllowsOnlyListedOrigins(t *testing.T) {
	mw := cors([]string{"https://shop.test"})

	for _, tc := range []struct{ origin, want string }{
		{"https://shop.test", "https://shop.test"},
		{"https://evil.test", ""},
		// Prefix/suffix lookalikes must not match.
		{"https://shop.test.evil.test", ""},
		{"https://notshop.test", ""},
	} {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set("Origin", tc.origin)
		rec := serveThrough(mw, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}, req)

		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != tc.want {
			t.Errorf("origin %q: allow-origin = %q, want %q", tc.origin, got, tc.want)
		}
	}
}

// TestCORSNeverEmitsWildcard: a wildcard on an API authenticated with secret
// keys would invite putting those keys into browser code.
func TestCORSNeverEmitsWildcard(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Origin", "https://shop.test")
	rec := serveThrough(cors([]string{"https://shop.test"}), func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}, req)

	if rec.Header().Get("Access-Control-Allow-Origin") == "*" {
		t.Error("wildcard origin emitted")
	}
	if !strings.Contains(rec.Header().Get("Vary"), "Origin") {
		t.Error("Vary: Origin missing; caches could serve one origin's response to another")
	}
}

func TestCORSPreflightShortCircuits(t *testing.T) {
	called := false
	req := httptest.NewRequest(http.MethodOptions, "/v1/payment_intents", nil)
	req.Header.Set("Origin", "https://shop.test")
	req.Header.Set("Access-Control-Request-Method", "POST")

	rec := serveThrough(cors([]string{"https://shop.test"}), func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}, req)

	if called {
		t.Error("preflight reached the handler instead of being answered by the middleware")
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("preflight status = %d, want 204", rec.Code)
	}
}

// --- integration ----------------------------------------------------------

// TestChainAppliesToRealServer confirms the middleware is actually wired into
// the server, not merely defined.
func TestChainAppliesToRealServer(t *testing.T) {
	srv := New(config.Config{
		APIKey: "sk_test_x", ActiveGateway: "stub",
		CORSOrigins: []string{"https://shop.test"},
	}, store.NewMemory(), stub.New())

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("Origin", "https://shop.test")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("healthz = %d", rec.Code)
	}
	if rec.Header().Get("X-Request-Id") == "" {
		t.Error("request id middleware not wired into the server")
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "https://shop.test" {
		t.Error("CORS middleware not wired into the server")
	}
}

// TestRateLimitDisabledWhenUnconfigured keeps the default construction path free
// of limiting so existing deployments and tests are unaffected until opted in.
func TestRateLimitDisabledWhenUnconfigured(t *testing.T) {
	srv := newTestServer(t) // RateLimitRPS is zero

	for i := range 50 {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d got %d; limiting should be off by default", i+1, rec.Code)
		}
	}
}
