package webhook

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/jawalab-com/payrouter/internal/store"
)

const fixedNow int64 = 1700000000

func newDeliverer(st *store.Memory, url string) *Deliverer {
	d := New(st, url, testSecret, nil)
	d.now = func() int64 { return fixedNow }
	d.backoff = func(attempts, now int64) int64 { return now } // immediate retry (deterministic)
	d.logger = nil
	return d
}

func enqueueEvent(t *testing.T, st *store.Memory) string {
	t.Helper()
	id := "evt_test1"
	st.EnqueueWebhook(&store.WebhookDelivery{
		ID: id, Type: "payment_intent.succeeded", Reference: "pi_1",
		Payload: []byte(`{"id":"evt_test1","type":"payment_intent.succeeded"}`),
		Status:  store.DeliveryPending, Created: fixedNow,
	})
	return id
}

func TestDeliverOnceSuccess(t *testing.T) {
	st := store.NewMemory()
	id := enqueueEvent(t, st)

	var gotSig, gotBody string
	merchant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h := r.Header.Get("Stripe-Signature"); h != "" {
			gotSig = h
		}
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer merchant.Close()

	d := newDeliverer(st, merchant.URL)
	if n := d.DeliverOnce(); n != 1 {
		t.Fatalf("attempted %d; want 1", n)
	}

	// Merchant received a verifiable Stripe-Signature over the exact payload.
	if err := Verify([]byte(gotBody), gotSig, testSecret, fixedNow, 0); err != nil {
		t.Errorf("merchant signature did not verify: %v", err)
	}
	dlv, ok := st.GetWebhook(id)
	if !ok || dlv.Status != store.DeliveryDelivered {
		t.Fatalf("delivery status = %q; want delivered", dlv.Status)
	}
	if dlv.Attempts != 1 {
		t.Errorf("attempts = %d; want 1", dlv.Attempts)
	}
}

func TestDeliverRetryThenSuccess(t *testing.T) {
	st := store.NewMemory()
	id := enqueueEvent(t, st)

	var (
		mu        sync.Mutex
		failCount int
	)
	merchant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		failCount++
		if failCount < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer merchant.Close()

	d := newDeliverer(st, merchant.URL)
	d.DeliverOnce() // attempt 1 -> 500
	d.DeliverOnce() // attempt 2 -> 500
	d.DeliverOnce() // attempt 3 -> 200

	dlv, ok := st.GetWebhook(id)
	if !ok || dlv.Status != store.DeliveryDelivered {
		t.Fatalf("status = %q; want delivered after retry", dlv.Status)
	}
	if dlv.Attempts != 3 {
		t.Errorf("attempts = %d; want 3", dlv.Attempts)
	}
}

func TestDeliverDeadLetter(t *testing.T) {
	st := store.NewMemory()
	id := enqueueEvent(t, st)

	merchant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer merchant.Close()

	d := newDeliverer(st, merchant.URL)
	d.maxAttempts = 2
	d.DeliverOnce() // attempt 1 -> retry
	d.DeliverOnce() // attempt 2 -> dead-letter

	dlv, ok := st.GetWebhook(id)
	if !ok || dlv.Status != store.DeliveryFailed {
		t.Fatalf("status = %q; want failed (dead-lettered)", dlv.Status)
	}
	if dlv.Attempts != 2 {
		t.Errorf("attempts = %d; want 2", dlv.Attempts)
	}
	if dlv.LastError == "" {
		t.Error("expected a recorded error on the failed delivery")
	}
}

func TestDeliverNoURLIsNoOp(t *testing.T) {
	st := store.NewMemory()
	id := enqueueEvent(t, st)
	d := newDeliverer(st, "") // delivery disabled
	if n := d.DeliverOnce(); n != 0 {
		t.Fatalf("attempted %d; want 0 when URL unset", n)
	}
	dlv, _ := st.GetWebhook(id)
	if dlv.Status != store.DeliveryPending {
		t.Errorf("status = %q; want pending (untouched)", dlv.Status)
	}
}
