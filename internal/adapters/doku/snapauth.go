package doku

import (
	"bytes"
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
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Bank Indonesia SNAP authentication for DOKU.
//
// This is a different scheme from the Checkout API used elsewhere in this
// adapter, which signs each call with a single HMAC over the client secret. SNAP
// (Standar Nasional Open API Pembayaran) is a two-stage protocol:
//
//  1. Exchange an RSA signature for a short-lived B2B access token.
//     X-SIGNATURE = base64(SHA256withRSA(privateKey, clientID + "|" + timestamp))
//
//  2. Sign each transactional call symmetrically, binding the token, the exact
//     request body, and the timestamp together.
//     stringToSign = method:path:accessToken:lowerhex(sha256(body)):timestamp
//     X-SIGNATURE  = base64(HMAC-SHA512(clientSecret, stringToSign))
//
// Consequences worth knowing: the merchant must generate an RSA keypair and
// register the public half in the DOKU dashboard, and because the body hash is
// part of the signature, the bytes signed must be the exact bytes sent — the
// request is marshalled once and reused rather than re-encoded.
const (
	tokenPath = "/authorization/v1/access-token/b2b"

	// snapTimeFormat is ISO8601 with an explicit offset, as SNAP requires.
	snapTimeFormat = "2006-01-02T15:04:05-07:00"

	// tokenRefreshSkew renews the token early. DOKU issues 900s tokens; renewing
	// slightly ahead avoids racing an expiry mid-payment.
	tokenRefreshSkew = 60 * time.Second
)

// snapCredentials holds the merchant's SNAP identity. It is separate from the
// Checkout credentials because SNAP additionally needs an RSA private key.
type snapCredentials struct {
	clientID   string          // X-CLIENT-KEY / X-PARTNER-ID
	secretKey  string          // HMAC key for transactional signatures
	privateKey *rsa.PrivateKey // signs the token request only
	merchantID string          // merchant identifier echoed on QR requests
	terminalID string          // terminal identifier echoed on QR requests
}

// snapSession caches the B2B access token across calls. SNAP tokens last 15
// minutes; re-minting one per request would triple the latency of every payment
// and burn rate limit for no benefit.
type snapSession struct {
	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

// ParsePrivateKey decodes a PEM-encoded RSA private key in either PKCS#1 or
// PKCS#8 form, since key generation tooling emits both.
func ParsePrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("doku: private key is not valid PEM")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("doku: parse private key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("doku: private key is %T, want an RSA key", parsed)
	}
	return key, nil
}

// signAsymmetric produces the token-request signature:
// base64(SHA256withRSA(privateKey, clientID + "|" + timestamp)).
func signAsymmetric(key *rsa.PrivateKey, clientID, timestamp string) (string, error) {
	digest := sha256.Sum256([]byte(clientID + "|" + timestamp))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("doku: sign token request: %w", err)
	}
	return base64.StdEncoding.EncodeToString(sig), nil
}

// signSymmetric produces a transactional signature over the exact body bytes.
//
// body must be the bytes actually transmitted. Re-marshalling before sending
// could reorder keys or change spacing, changing the hash and yielding a
// signature the gateway rejects.
func signSymmetric(secretKey, method, path, accessToken string, body []byte, timestamp string) string {
	sum := sha256.Sum256(minifyJSON(body))
	stringToSign := strings.Join([]string{
		method,
		path,
		accessToken,
		strings.ToLower(hex.EncodeToString(sum[:])),
		timestamp,
	}, ":")

	mac := hmac.New(sha512.New, []byte(secretKey))
	mac.Write([]byte(stringToSign))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// minifyJSON re-encodes JSON with insignificant whitespace removed, as SNAP's
// "minify(RequestBody)" requires. Input that is not valid JSON is hashed as-is,
// so a malformed body fails signature verification rather than being silently
// altered.
func minifyJSON(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	var compacted bytes.Buffer
	if err := json.Compact(&compacted, body); err != nil {
		return body
	}
	return compacted.Bytes()
}

// externalID returns a value for X-EXTERNAL-ID, which SNAP requires to be unique
// per day — DOKU rejects a repeat as a duplicate request.
func externalID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// snapTimestamp formats the current time as SNAP expects.
func snapTimestamp(now time.Time) string { return now.Format(snapTimeFormat) }

// accessToken returns a cached B2B token, minting a new one when absent or close
// to expiry.
func (a *Adapter) accessToken(ctx context.Context) (string, error) {
	if a.snap == nil || a.snap.privateKey == nil {
		return "", errors.New("doku: SNAP requires an RSA private key (set DOKU_PRIVATE_KEY)")
	}
	a.session.mu.Lock()
	defer a.session.mu.Unlock()

	if a.session.token != "" && a.now().Add(tokenRefreshSkew).Before(a.session.expiresAt) {
		return a.session.token, nil
	}

	timestamp := snapTimestamp(a.now())
	signature, err := signAsymmetric(a.snap.privateKey, a.snap.clientID, timestamp)
	if err != nil {
		return "", err
	}

	body, err := json.Marshal(map[string]any{"grantType": "client_credentials"})
	if err != nil {
		return "", fmt.Errorf("doku: marshal token request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+tokenPath, strings.NewReader(string(body)))
	if err != nil {
		return "", fmt.Errorf("doku: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CLIENT-KEY", a.snap.clientID)
	req.Header.Set("X-TIMESTAMP", timestamp)
	req.Header.Set("X-SIGNATURE", signature)

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("doku: request B2B token: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("doku: read token response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("doku: B2B token request returned HTTP %d: %s", resp.StatusCode, snippet(raw))
	}

	var tr tokenResponse
	if err := json.Unmarshal(raw, &tr); err != nil {
		return "", fmt.Errorf("doku: decode token response: %w", err)
	}
	if tr.AccessToken == "" {
		return "", fmt.Errorf("doku: B2B token response carried no accessToken (responseCode %s: %s)",
			tr.ResponseCode, tr.ResponseMessage)
	}

	a.session.token = tr.AccessToken
	a.session.expiresAt = a.now().Add(time.Duration(tr.expiresInSeconds()) * time.Second)
	return a.session.token, nil
}

// tokenResponse is the subset of the B2B token response we act on.
type tokenResponse struct {
	ResponseCode    string `json:"responseCode"`
	ResponseMessage string `json:"responseMessage"`
	AccessToken     string `json:"accessToken"`
	TokenType       string `json:"tokenType"`
	// expiresIn is documented as a string ("900") but tolerate a number too.
	ExpiresIn json.RawMessage `json:"expiresIn"`
}

// expiresInSeconds reads the token lifetime, defaulting to DOKU's documented 900
// seconds when absent or unparseable.
func (t tokenResponse) expiresInSeconds() int64 {
	const defaultLifetime = 900
	raw := strings.Trim(string(t.ExpiresIn), `"`)
	if raw == "" {
		return defaultLifetime
	}
	var secs int64
	if _, err := fmt.Sscanf(raw, "%d", &secs); err != nil || secs <= 0 {
		return defaultLifetime
	}
	return secs
}

// snippet truncates a response body for inclusion in an error message, so a
// failure is diagnosable without dumping an entire payload into the logs.
func snippet(raw []byte) string {
	const max = 200
	s := strings.TrimSpace(string(raw))
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}
