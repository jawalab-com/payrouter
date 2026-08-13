package xendit

import "crypto/subtle"

// verifyCallbackToken performs a constant-time comparison of the x-callback-token
// header value against the merchant's configured Webhook Verification Token.
//
// Unlike Stripe (HMAC-SHA256 over the body) or Midtrans (SHA-512 digest of fields
// plus the Server Key), Xendit authenticates callbacks with a bare shared token
// sent verbatim in the x-callback-token header. Constant-time comparison prevents
// timing leaks.
//
// An empty expected token always rejects: callbacks are never accepted when the
// merchant has not configured a verification token (defence-in-depth; config also
// requires XENDIT_WEBHOOK_TOKEN when the gateway is xendit).
func verifyCallbackToken(provided, expected string) bool {
	if expected == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}
