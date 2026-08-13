package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

// This file holds the cross-cutting HTTP concerns that sit in front of every
// handler: request identity, access logging, panic recovery, CORS, and rate
// limiting. They are plain func(http.Handler) http.Handler decorators so they
// compose with anything in the stdlib and stay testable in isolation — the same
// shape as the existing auth and idempotency middleware.

// ctxKey is the private context key type for values this package stashes.
type ctxKey string

const requestIDKey ctxKey = "request_id"

// maxInboundRequestIDLen bounds an inbound X-Request-Id before it is echoed back
// or written to logs. Untrusted header values must not be able to bloat a log
// line or smuggle control characters into it.
const maxInboundRequestIDLen = 64

// RequestID returns the correlation id for r, or "" when the middleware is not
// installed. It is the value to quote when tracing a payment through the logs.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// withRequestID assigns each request a correlation id, echoes it in the response
// so a caller can quote it in a support request, and puts it in the context for
// downstream logs.
//
// An inbound X-Request-Id is honored so a request can be traced across a proxy or
// a calling service, but it is sanitized first: it is attacker-controlled input
// that lands in log output.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := sanitizeRequestID(r.Header.Get("X-Request-Id"))
		if id == "" {
			id = "req_" + randID()
		}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

// sanitizeRequestID keeps only printable ASCII that cannot break a log line, and
// truncates to a sane length. Returns "" if nothing usable survives.
func sanitizeRequestID(raw string) string {
	if len(raw) > maxInboundRequestIDLen {
		raw = raw[:maxInboundRequestIDLen]
	}
	var b strings.Builder
	for _, c := range raw {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			b.WriteRune(c)
		case c == '-' || c == '_' || c == '.':
			b.WriteRune(c)
		}
	}
	return b.String()
}

// recoverPanics turns a panicking handler into a Stripe-shaped 500 instead of a
// dropped connection.
//
// net/http already stops a panic from killing the process, but it closes the
// connection without a response and writes an unstructured trace to stderr —
// outside the JSON log pipeline, and invisible to a client mid-payment. Here the
// panic is logged with its request id and stack, and the caller gets a real
// error envelope it can parse.
func recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			// http.ErrAbortHandler is the documented way to abort a response on
			// purpose; it is not an error condition and must not be swallowed.
			if rec == http.ErrAbortHandler {
				panic(rec)
			}
			slog.Error("panic recovered",
				"request_id", RequestID(r.Context()),
				"method", r.Method,
				"path", r.URL.Path,
				"panic", rec,
				"stack", string(debug.Stack()))

			// Only write an envelope if the handler had not already started a
			// response; otherwise the bytes would be appended to a partial body.
			if cw, ok := w.(*countingWriter); ok && cw.wroteHeader {
				return
			}
			writeStripeError(w, http.StatusInternalServerError, "api_error",
				"An unexpected error occurred while processing the request.")
		}()
		next.ServeHTTP(w, r)
	})
}

// accessLog emits one structured line per request. It deliberately records only
// method, path, status, size and duration: request bodies and the Authorization
// header carry secret keys and card-adjacent data and must never reach the logs.
func accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		cw := &countingWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(cw, r)

		level := slog.LevelInfo
		if cw.status >= 500 {
			level = slog.LevelError
		} else if cw.status >= 400 {
			level = slog.LevelWarn
		}
		slog.Log(r.Context(), level, "http request",
			"request_id", RequestID(r.Context()),
			"method", r.Method,
			"path", r.URL.Path,
			"status", cw.status,
			"bytes", cw.bytes,
			"duration_ms", time.Since(start).Milliseconds(),
			"remote_ip", clientIP(r, false))
	})
}

// countingWriter records the status and byte count of a response so the access
// log can report them and the panic handler can tell whether a response has
// already begun.
type countingWriter struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool
}

func (c *countingWriter) WriteHeader(status int) {
	if c.wroteHeader {
		return
	}
	c.status = status
	c.wroteHeader = true
	c.ResponseWriter.WriteHeader(status)
}

func (c *countingWriter) Write(b []byte) (int, error) {
	if !c.wroteHeader {
		c.WriteHeader(http.StatusOK)
	}
	n, err := c.ResponseWriter.Write(b)
	c.bytes += n
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer for flushing
// and deadline control.
func (c *countingWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

// cors answers browser preflights and sets the response headers, for exactly the
// origins the operator listed.
//
// It is OFF unless origins are configured, and it never reflects an arbitrary
// Origin or emits "*". The v1 API is authenticated with secret (sk_) keys, which
// have no business in a browser; this exists for the public, client-secret-scoped
// endpoints a hosted checkout page will need. Turning it on for the whole API
// would invite putting secret keys in front-end code.
func cors(allowed []string) func(http.Handler) http.Handler {
	set := make(map[string]bool, len(allowed))
	for _, o := range allowed {
		if o = strings.TrimSpace(o); o != "" {
			set[o] = true
		}
	}
	return func(next http.Handler) http.Handler {
		if len(set) == 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin != "" && set[origin] {
				h := w.Header()
				h.Set("Access-Control-Allow-Origin", origin)
				// The response varies by Origin, so caches must key on it.
				h.Add("Vary", "Origin")
				h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key, X-Request-Id")
				h.Set("Access-Control-Max-Age", "600")
			}
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				// Preflight: answer here regardless of match. An unlisted origin
				// simply gets no allow headers and the browser blocks it.
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// rateLimiter is a fixed-rate token bucket keyed by caller, kept in-process and
// dependency-free.
//
// Scope note: this bounds a single instance. Running several replicas multiplies
// the effective limit, so treat it as protection against runaway clients and
// credential-stuffing, not as a quota system.
type rateLimiter struct {
	rps   float64
	burst float64

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

// staleBucketTTL is how long an idle caller's bucket is retained. Without
// eviction the map would grow without bound, which is itself a memory-exhaustion
// vector on a public endpoint.
const staleBucketTTL = 10 * time.Minute

func newRateLimiter(rps, burst float64) *rateLimiter {
	return &rateLimiter{rps: rps, burst: burst, buckets: make(map[string]*bucket)}
}

// allow consumes a token for key, reporting whether the request may proceed.
func (l *rateLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[key]
	if !ok {
		// Sweep opportunistically on first sight of a new caller: no background
		// goroutine to manage, and the cost lands on growth rather than steady state.
		l.evictStaleLocked(now)
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}

	b.tokens += now.Sub(b.last).Seconds() * l.rps
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (l *rateLimiter) evictStaleLocked(now time.Time) {
	for k, b := range l.buckets {
		if now.Sub(b.last) > staleBucketTTL {
			delete(l.buckets, k)
		}
	}
}

// middleware limits by API key when one is presented and by client IP otherwise,
// so one noisy merchant cannot starve another and an unauthenticated flood is
// still bounded.
func (l *rateLimiter) middleware(trustProxy bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !l.allow(rateLimitKey(r, trustProxy), time.Now()) {
				w.Header().Set("Retry-After", "1")
				writeStripeError(w, http.StatusTooManyRequests, "rate_limit_error",
					"Too many requests; please retry shortly.")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// rateLimitKey identifies the caller. The API key is hashed rather than used
// directly so the limiter's map never holds raw secrets in memory.
func rateLimitKey(r *http.Request, trustProxy bool) string {
	const prefix = "Bearer "
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, prefix) {
		if token := h[len(prefix):]; token != "" {
			return "key:" + hashToken(token)
		}
	}
	return "ip:" + clientIP(r, trustProxy)
}

// hashToken reduces a secret to a short, non-reversible map key.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:8])
}

// clientIP resolves the caller's address. X-Forwarded-For is honored only when
// the operator has declared that PayRouter sits behind a trusted proxy —
// otherwise any client could spoof the header and evade IP-based limiting.
func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// Left-most entry is the original client.
			if first, _, found := strings.Cut(xff, ","); found || first != "" {
				if ip := strings.TrimSpace(first); ip != "" {
					return ip
				}
			}
		}
		if xr := strings.TrimSpace(r.Header.Get("X-Real-Ip")); xr != "" {
			return xr
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
