package midtrans

import (
	"crypto/sha512"
	"crypto/subtle"
	"encoding/hex"
)

// computeSignature reproduces Midtrans' signature_key.
//
// Algorithm (verified against the Midtrans "HTTP(S) Notification / Webhooks"
// docs): concatenate the raw string values of order_id, status_code,
// gross_amount and the ServerKey, then SHA-512 the result and hex-encode it.
// This is a plain digest, NOT an HMAC.
//
// IMPORTANT: gross_amount must be passed exactly as it appears in the
// notification (e.g. "10000.00"), because it is part of the signed payload —
// do not normalize it to an integer before signing.
func computeSignature(orderID, statusCode, grossAmount, serverKey string) string {
	sum := sha512.Sum512([]byte(orderID + statusCode + grossAmount + serverKey))
	return hex.EncodeToString(sum[:])
}

// verifySignature reports whether the supplied signature_key matches the
// expected digest for the given fields, compared in constant time.
func verifySignature(want, orderID, statusCode, grossAmount, serverKey string) bool {
	if want == "" {
		return false
	}
	expected := computeSignature(orderID, statusCode, grossAmount, serverKey)
	return subtle.ConstantTimeCompare([]byte(expected), []byte(want)) == 1
}
