package mayar

import "crypto/subtle"

// verifyToken performs a constant-time comparison of the token carried in the
// webhook URL query (?token=...) against the merchant's shared webhook secret.
//
// Mayar does not sign webhook payloads cryptographically. The documented pattern
// (matching Xendit's bare-token approach, but transported in the query string) is
// for the merchant to register a tokenized Notification URL
// (https://facade/v1/webhooks/mayar?token=<secret>) with Mayar; the facade then
// verifies the token on each callback. Constant-time comparison prevents timing
// leaks. An empty expected token never verifies.
func verifyToken(provided, expected string) bool {
	if expected == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}
