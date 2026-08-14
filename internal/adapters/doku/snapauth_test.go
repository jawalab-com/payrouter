package doku

import (
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jawalab-com/payrouter/internal/gateway"
)

// testKey generates an RSA keypair for signing assertions. Generated rather than
// hardcoded so no usable private key is ever committed to the repository.
func testKey(t *testing.T) (*rsa.PrivateKey, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	return key, pemBytes
}

// hmacSHA512Base64 and hexEncode recompute the expected signature independently
// of the implementation. Reusing the production helpers would make the test
// tautological — it would pass even if the whole scheme were wrong.
func hmacSHA512Base64(secret, message string) string {
	mac := hmac.New(sha512.New, []byte(secret))
	mac.Write([]byte(message))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func hexEncode(b []byte) string { return hex.EncodeToString(b) }

func fixedTime() time.Time {
	return time.Date(2026, 8, 13, 10, 30, 0, 0, time.FixedZone("WIB", 7*60*60))
}

// snapServer stands in for DOKU: it issues tokens and answers QR requests,
// recording what it received.
type snapServer struct {
	*httptest.Server
	tokenCalls atomic.Int32
	qrHeaders  http.Header
	qrBody     []byte
	qrPath     string
	tokenHdrs  http.Header
	qrResponse string
}

func newSNAPServer(t *testing.T) *snapServer {
	t.Helper()
	s := &snapServer{qrResponse: `{
		"responseCode": "2004700",
		"responseMessage": "Successful",
		"referenceNo": "ref-123",
		"partnerReferenceNo": "pi_abc",
		"qrContent": "00020101021226620014COM.DOKU.WWW",
		"terminalId": "term-1",
		"additionalInfo": {"validityPeriod": "2026-08-14T10:30:00+07:00"}
	}`}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case tokenPath:
			s.tokenCalls.Add(1)
			s.tokenHdrs = r.Header.Clone()
			_, _ = io.WriteString(w, `{"responseCode":"2007300","accessToken":"tok-abc","tokenType":"Bearer","expiresIn":"900"}`)
		case qrGeneratePath:
			s.qrPath = r.URL.Path
			s.qrHeaders = r.Header.Clone()
			s.qrBody, _ = io.ReadAll(r.Body)
			_, _ = io.WriteString(w, s.qrResponse)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func newSNAPAdapter(t *testing.T, srv *snapServer, keyPEM []byte) *Adapter {
	t.Helper()
	a := newForTest("client-1", "secret-1", srv.URL, srv.Client())
	a.nowFn = fixedTime
	if err := a.EnableSNAP(SNAPConfig{
		PrivateKeyPEM: keyPEM, MerchantID: "merchant-1", TerminalID: "terminal-1",
		PartnerServiceID: "88881",
	}); err != nil {
		t.Fatalf("EnableSNAP: %v", err)
	}
	return a
}

// --- signing --------------------------------------------------------------

// TestAsymmetricSignatureVerifies checks the token signature against the public
// key. A wrong construction here fails only at the gateway, with an opaque
// rejection and nothing local to debug.
func TestAsymmetricSignatureVerifies(t *testing.T) {
	key, _ := testKey(t)
	const clientID, timestamp = "client-1", "2026-08-13T10:30:00+07:00"

	sig, err := signAsymmetric(key, clientID, timestamp)
	if err != nil {
		t.Fatalf("signAsymmetric: %v", err)
	}
	rawSig, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		t.Fatalf("signature is not base64: %v", err)
	}

	// SNAP defines stringToSign as clientID + "|" + timestamp.
	digest := sha256.Sum256([]byte(clientID + "|" + timestamp))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], rawSig); err != nil {
		t.Errorf("signature does not verify against the public key: %v", err)
	}

	// A different string must not verify, or the signature proves nothing.
	other := sha256.Sum256([]byte(clientID + "|2026-01-01T00:00:00+07:00"))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, other[:], rawSig); err == nil {
		t.Error("signature verified against a different timestamp")
	}
}

// TestSymmetricSignatureMatchesSpec pins the exact stringToSign layout:
// method:path:token:lowerhex(sha256(minified body)):timestamp, HMAC-SHA512 with
// the client secret, base64 encoded.
func TestSymmetricSignatureMatchesSpec(t *testing.T) {
	const (
		secret    = "secret-1"
		method    = http.MethodPost
		path      = qrGeneratePath
		token     = "tok-abc"
		timestamp = "2026-08-13T10:30:00+07:00"
	)
	body := []byte(`{"partnerReferenceNo":"pi_abc"}`)

	got := signSymmetric(secret, method, path, token, body, timestamp)

	sum := sha256.Sum256(body)
	want := hmacSHA512Base64(secret, strings.Join([]string{
		method, path, token, strings.ToLower(hexEncode(sum[:])), timestamp,
	}, ":"))
	if got != want {
		t.Errorf("signature = %q, want %q", got, want)
	}

	// Any component change must change the signature, or the binding is broken.
	for _, alt := range []struct {
		name string
		sig  string
	}{
		{"different body", signSymmetric(secret, method, path, token, []byte(`{"a":1}`), timestamp)},
		{"different token", signSymmetric(secret, method, path, "other-token", body, timestamp)},
		{"different timestamp", signSymmetric(secret, method, path, token, body, "2026-01-01T00:00:00+07:00")},
		{"different path", signSymmetric(secret, method, "/other", token, body, timestamp)},
		{"different secret", signSymmetric("other-secret", method, path, token, body, timestamp)},
	} {
		if alt.sig == got {
			t.Errorf("%s produced an identical signature; that component is not bound", alt.name)
		}
	}
}

// TestMinifyIsAppliedBeforeHashing: SNAP hashes the minified body, so a pretty
// printed and a compact form of the same JSON must sign identically.
func TestMinifyIsAppliedBeforeHashing(t *testing.T) {
	const args = "secret"
	compact := []byte(`{"a":1,"b":2}`)
	pretty := []byte("{\n  \"a\": 1,\n  \"b\": 2\n}")

	if signSymmetric(args, "POST", "/p", "t", compact, "ts") != signSymmetric(args, "POST", "/p", "t", pretty, "ts") {
		t.Error("minification not applied; equivalent JSON produced different signatures")
	}
}

func TestParsePrivateKeyAcceptsPKCS1AndPKCS8(t *testing.T) {
	key, pkcs1 := testKey(t)
	if _, err := ParsePrivateKey(pkcs1); err != nil {
		t.Errorf("PKCS#1: %v", err)
	}

	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal pkcs8: %v", err)
	}
	pkcs8 := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if _, err := ParsePrivateKey(pkcs8); err != nil {
		t.Errorf("PKCS#8: %v", err)
	}

	if _, err := ParsePrivateKey([]byte("not a key")); err == nil {
		t.Error("expected an error for non-PEM input")
	}
}

// --- token lifecycle ------------------------------------------------------

// TestTokenIsCachedAcrossCalls: minting a token per payment would add a round
// trip to every charge and burn DOKU's rate limit for no benefit.
func TestTokenIsCachedAcrossCalls(t *testing.T) {
	srv := newSNAPServer(t)
	_, keyPEM := testKey(t)
	a := newSNAPAdapter(t, srv, keyPEM)

	for range 3 {
		if _, err := a.accessToken(context.Background()); err != nil {
			t.Fatalf("accessToken: %v", err)
		}
	}
	if got := srv.tokenCalls.Load(); got != 1 {
		t.Errorf("token endpoint called %d times, want 1 (token should be cached)", got)
	}
}

// TestTokenRefreshesBeforeExpiry guards against a token expiring mid-payment.
func TestTokenRefreshesBeforeExpiry(t *testing.T) {
	srv := newSNAPServer(t)
	_, keyPEM := testKey(t)
	a := newSNAPAdapter(t, srv, keyPEM)

	if _, err := a.accessToken(context.Background()); err != nil {
		t.Fatalf("first token: %v", err)
	}
	// Advance to inside the refresh skew of the 900s lifetime.
	a.nowFn = func() time.Time { return fixedTime().Add(880 * time.Second) }
	if _, err := a.accessToken(context.Background()); err != nil {
		t.Fatalf("second token: %v", err)
	}
	if got := srv.tokenCalls.Load(); got != 2 {
		t.Errorf("token endpoint called %d times, want 2 (should renew inside the skew)", got)
	}
}

func TestTokenRequestSendsRequiredHeaders(t *testing.T) {
	srv := newSNAPServer(t)
	_, keyPEM := testKey(t)
	a := newSNAPAdapter(t, srv, keyPEM)

	if _, err := a.accessToken(context.Background()); err != nil {
		t.Fatalf("accessToken: %v", err)
	}
	for _, h := range []string{"X-CLIENT-KEY", "X-TIMESTAMP", "X-SIGNATURE"} {
		if srv.tokenHdrs.Get(h) == "" {
			t.Errorf("token request missing %s", h)
		}
	}
	if got := srv.tokenHdrs.Get("X-CLIENT-KEY"); got != "client-1" {
		t.Errorf("X-CLIENT-KEY = %q, want client-1", got)
	}
}

// --- QRIS issuance --------------------------------------------------------

func TestIssueQRISReturnsPayload(t *testing.T) {
	srv := newSNAPServer(t)
	_, keyPEM := testKey(t)
	a := newSNAPAdapter(t, srv, keyPEM)

	res, err := a.IssueInstrument(context.Background(), &gateway.CreatePaymentInput{
		Reference: "pi_abc", AmountMinor: 100000, Currency: "idr",
		PaymentMethodType: gateway.IDQRIS,
	})
	if err != nil {
		t.Fatalf("IssueInstrument: %v", err)
	}

	if res.Display == nil || res.Display.QRIS == nil {
		t.Fatal("no QRIS instruction returned")
	}
	if res.Display.QRIS.Payload != "00020101021226620014COM.DOKU.WWW" {
		t.Errorf("payload = %q", res.Display.QRIS.Payload)
	}
	if res.GatewayReference != "ref-123" {
		t.Errorf("GatewayReference = %q, want DOKU's referenceNo", res.GatewayReference)
	}

	for _, h := range []string{"Authorization", "X-TIMESTAMP", "X-SIGNATURE", "X-PARTNER-ID", "X-EXTERNAL-ID", "CHANNEL-ID"} {
		if srv.qrHeaders.Get(h) == "" {
			t.Errorf("QR request missing %s", h)
		}
	}
	if got := srv.qrHeaders.Get("Authorization"); got != "Bearer tok-abc" {
		t.Errorf("Authorization = %q, want the B2B token", got)
	}

	var body map[string]any
	if err := json.Unmarshal(srv.qrBody, &body); err != nil {
		t.Fatalf("request body is not JSON: %v", err)
	}
	if body["partnerReferenceNo"] != "pi_abc" {
		t.Errorf("partnerReferenceNo = %v; correlation depends on it", body["partnerReferenceNo"])
	}
	// SNAP carries amounts as decimal strings even for zero-decimal IDR.
	amount, _ := body["amount"].(map[string]any)
	if amount["value"] != "100000.00" {
		t.Errorf("amount.value = %v, want \"100000.00\"", amount["value"])
	}
	if amount["currency"] != "IDR" {
		t.Errorf("amount.currency = %v, want IDR", amount["currency"])
	}
}

// TestSignatureCoversExactBytesSent: the signature includes a hash of the body,
// so the bytes signed must be the bytes transmitted. Re-encoding between the two
// would reorder keys and the gateway would reject every request.
func TestSignatureCoversExactBytesSent(t *testing.T) {
	srv := newSNAPServer(t)
	_, keyPEM := testKey(t)
	a := newSNAPAdapter(t, srv, keyPEM)

	if _, err := a.IssueInstrument(context.Background(), &gateway.CreatePaymentInput{
		Reference: "pi_abc", AmountMinor: 100000, PaymentMethodType: gateway.IDQRIS,
	}); err != nil {
		t.Fatalf("IssueInstrument: %v", err)
	}

	want := signSymmetric("secret-1", http.MethodPost, qrGeneratePath, "tok-abc",
		srv.qrBody, srv.qrHeaders.Get("X-TIMESTAMP"))
	if got := srv.qrHeaders.Get("X-SIGNATURE"); got != want {
		t.Errorf("X-SIGNATURE does not match a signature recomputed over the received body")
	}
}

// TestSNAPFailureCodeOnHTTP200IsDetected: SNAP returns rejections with a
// non-2xxxxxx responseCode, sometimes still under HTTP 200.
func TestSNAPFailureCodeOnHTTP200IsDetected(t *testing.T) {
	srv := newSNAPServer(t)
	srv.qrResponse = `{"responseCode":"4004701","responseMessage":"Invalid merchant"}`
	_, keyPEM := testKey(t)
	a := newSNAPAdapter(t, srv, keyPEM)

	_, err := a.IssueInstrument(context.Background(), &gateway.CreatePaymentInput{
		Reference: "pi_abc", AmountMinor: 100000, PaymentMethodType: gateway.IDQRIS,
	})
	if err == nil {
		t.Fatal("failing responseCode treated as success")
	}
	if !strings.Contains(err.Error(), "4004701") || !strings.Contains(err.Error(), "Invalid merchant") {
		t.Errorf("err = %v, want the responseCode and message surfaced", err)
	}
}

// --- capability gating ----------------------------------------------------

// TestNoSNAPMeansNoInstrumentSupport: an adapter without SNAP credentials must
// fall back to the hosted Checkout page rather than fail when a customer pays.
func TestNoSNAPMeansNoInstrumentSupport(t *testing.T) {
	a := newForTest("client-1", "secret-1", "http://unused", nil)

	if a.SupportsInstrument(gateway.IDQRIS) {
		t.Error("SupportsInstrument(QRIS) = true without SNAP configured")
	}
	_, err := a.IssueInstrument(context.Background(), &gateway.CreatePaymentInput{
		Reference: "pi_x", AmountMinor: 1000, PaymentMethodType: gateway.IDQRIS,
	})
	if err == nil {
		t.Fatal("expected an error without SNAP configured")
	}
}

func TestDokuSatisfiesInstrumentGateway(t *testing.T) {
	var _ gateway.InstrumentGateway = (*Adapter)(nil)
}
