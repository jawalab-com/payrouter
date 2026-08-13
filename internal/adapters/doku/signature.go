package doku

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
)

// digest computes the DOKU Digest component: base64(SHA-256(body)). It both
// authenticates the body (it is signed) and binds the body into the signature.
func digest(body []byte) string {
	sum := sha256.Sum256(body)
	return base64.StdEncoding.EncodeToString(sum[:])
}

// canonical builds the newline-joined component string DOKU signs. Template
// (no trailing newline), verified against DOKU's official signature docs:
//
//	Client-Id:{cid}
//	Request-Id:{rid}
//	Request-Timestamp:{ts}
//	Request-Target:{target}
//	Digest:{digest}
func canonical(clientID, reqID, ts, target, dgst string) string {
	return fmt.Sprintf("Client-Id:%s\nRequest-Id:%s\nRequest-Timestamp:%s\nRequest-Target:%s\nDigest:%s",
		clientID, reqID, ts, target, dgst)
}

// sign computes the DOKU Signature header value:
//
//	"HMACSHA256=" + base64(HMAC-SHA256(canonical, secretKey))
func sign(canonicalStr, secretKey string) string {
	mac := hmac.New(sha256.New, []byte(secretKey))
	mac.Write([]byte(canonicalStr))
	return "HMACSHA256=" + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// verify checks a received Signature header against the locally recomputed
// canonical string in constant time. The digest and Request-Target must match
// what the sender used (the digest is recomputed from the received body; the
// target is the merchant's notification URL path).
func verify(provided, clientID, reqID, ts, target, dgst, secretKey string) bool {
	if provided == "" {
		return false
	}
	want := sign(canonical(clientID, reqID, ts, target, dgst), secretKey)
	return subtle.ConstantTimeCompare([]byte(provided), []byte(want)) == 1
}

// uuidv4 returns a random RFC 4122 version-4 UUID for the Request-Id header.
func uuidv4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is catastrophic and effectively never happens; the
		// fallback keeps the request well-formed rather than aborting a payment.
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
