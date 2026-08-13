package webhook

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/jawalab-com/payrouter/internal/store"
)

// Deliverer drains the outbound webhook queue and POSTs each Stripe-shaped event
// to the merchant's endpoint, re-signing the body with the Stripe-Signature
// scheme on every attempt. It retries with exponential backoff and dead-letters
// after maxAttempts. A single in-process goroutine does delivery (no concurrent
// sends), so the store's lease/complete is enough to stay consistent.
type Deliverer struct {
	store        store.Store
	outbound     store.OutboundStore // durable outbox; nil => drain the in-memory queue
	url          string              // merchant endpoint; "" => delivery disabled (events still queued)
	secret       string
	client       *http.Client
	pollInterval time.Duration
	maxAttempts  int64
	backoff      func(attempts, now int64) int64 // next-attempt unix time after a failure
	now          func() int64                    // injectable clock (tests)
	logger       *log.Logger
}

// New creates a Deliverer with production defaults (5s poll, 5 attempts, 10s HTTP
// timeout, ~minute-to-half-hour backoff). If the store implements OutboundStore
// (durable), the deliverer drains the Postgres outbox; otherwise the in-memory
// queue.
func New(st store.Store, url, secret string, client *http.Client) *Deliverer {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	d := &Deliverer{
		store:        st,
		url:          url,
		secret:       secret,
		client:       client,
		pollInterval: 5 * time.Second,
		maxAttempts:  5,
		backoff:      defaultBackoff,
		now:          func() int64 { return time.Now().Unix() },
		logger:       log.Default(),
	}
	d.outbound, _ = st.(store.OutboundStore)
	return d
}

// Start runs the delivery loop until ctx is canceled.
func (d *Deliverer) Start(ctx context.Context) {
	if d.url == "" {
		d.logger.Printf("webhook delivery disabled (no PAYMENT_WEBHOOK_URL); events are received but not forwarded")
		return
	}
	ticker := time.NewTicker(d.pollInterval)
	defer ticker.Stop()
	d.logger.Printf("webhook deliverer started -> %s (poll=%s, maxAttempts=%d)", d.url, d.pollInterval, d.maxAttempts)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.DeliverOnce()
		}
	}
}

// DeliverOnce performs a single drain cycle: lease every due delivery and attempt
// it synchronously. Returns the number of deliveries attempted. Tests call this
// directly to avoid real timers.
func (d *Deliverer) DeliverOnce() int {
	if d.url == "" {
		return 0
	}
	if d.outbound != nil {
		return d.deliverOnceDurable()
	}
	now := d.now()
	var n int
	for {
		dlv := d.store.LeaseDueWebhook(now)
		if dlv == nil {
			break
		}
		n++
		ok, errStr := d.deliverPayload(dlv.Payload)
		d.store.CompleteWebhook(dlv.ID, ok, errStr, d.backoff(int64(dlv.Attempts), now), d.maxAttempts)
		if !ok && d.logger != nil {
			d.logger.Printf("webhook %s (%s) attempt %d failed: %s", dlv.ID, dlv.Type, dlv.Attempts, errStr)
		}
	}
	return n
}

// deliverOnceDurable drains the Postgres outbox: lease via FOR UPDATE SKIP LOCKED,
// POST, and complete under the lease's ownership token (a stale worker's complete
// is rejected). Crash safety comes from the lease: an in-flight row whose complete
// never arrives is re-leased after the lease expires.
func (d *Deliverer) deliverOnceDurable() int {
	ctx := context.Background()
	var n int
	for {
		ev, err := d.outbound.LeaseDueOutbound(ctx, 30)
		if err != nil {
			if d.logger != nil {
				d.logger.Printf("outbound lease failed: %v", err)
			}
			return n
		}
		if ev == nil {
			return n
		}
		n++
		ok, errStr := d.deliverPayload(ev.Payload)
		maxAttempts := int(d.maxAttempts)
		if ev.MaxAttempts > 0 {
			maxAttempts = ev.MaxAttempts
		}
		nextAttempt := time.Unix(d.backoff(int64(ev.Attempts), d.now()), 0)
		if cerr := d.outbound.CompleteOutbound(ctx, ev.ID, ev.ClaimToken, ok, errStr, nextAttempt, maxAttempts); cerr != nil && cerr != store.ErrOutboundClaimLost && d.logger != nil {
			d.logger.Printf("outbound complete failed (%s): %v", ev.ID, cerr)
		}
		if !ok && d.logger != nil {
			d.logger.Printf("outbound %s (%s) attempt %d failed: %s", ev.ID, ev.Type, ev.Attempts, errStr)
		}
	}
}

// deliverPayload POSTs the payload to the merchant endpoint with a fresh Stripe-Signature.
func (d *Deliverer) deliverPayload(payload []byte) (bool, string) {
	req, err := http.NewRequest(http.MethodPost, d.url, bytes.NewReader(payload))
	if err != nil {
		return false, "bad endpoint url: " + err.Error()
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Stripe-Signature", Sign(payload, d.secret, d.now()))
	req.Header.Set("User-Agent", "payrouter/1.0")
	resp, err := d.client.Do(req)
	if err != nil {
		return false, err.Error()
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return true, ""
	}
	return false, fmt.Sprintf("merchant returned HTTP %d", resp.StatusCode)
}

// defaultBackoff returns the next-attempt unix time after the given (1-based)
// attempt count fails: immediate, 30s, 2m, 10m, 30m. Caps at the last tier.
func defaultBackoff(attempts, now int64) int64 {
	delays := []int64{0, 30, 120, 600, 1800}
	i := int(attempts)
	if i < 0 {
		i = 0
	}
	if i >= len(delays) {
		i = len(delays) - 1
	}
	return now + delays[i]
}
