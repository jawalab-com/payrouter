package doku

import (
	"strings"

	stripe "github.com/stripe/stripe-go/v81"
)

// mapStatus translates a DOKU transaction.status into a canonical Stripe
// PaymentIntentStatus. DOKU payment-notification statuses (verified from DOKU's
// notification docs): SUCCESS, PENDING, FAILED. Input is matched
// case-insensitively.
func mapStatus(status string) stripe.PaymentIntentStatus {
	switch strings.ToUpper(status) {
	case "SUCCESS":
		return stripe.PaymentIntentStatusSucceeded
	case "PENDING":
		return stripe.PaymentIntentStatusRequiresAction
	case "FAILED":
		return stripe.PaymentIntentStatusRequiresPaymentMethod
	default:
		// Unknown status: safest non-terminal state is "customer action pending".
		return stripe.PaymentIntentStatusRequiresAction
	}
}

// stripeEventType maps a canonical status to the Stripe event type emitted on
// webhook translation (consumed by the M2 webhook-out layer).
func stripeEventType(status stripe.PaymentIntentStatus) string {
	switch status {
	case stripe.PaymentIntentStatusSucceeded:
		return "payment_intent.succeeded"
	case stripe.PaymentIntentStatusRequiresPaymentMethod:
		return "payment_intent.payment_failed"
	case stripe.PaymentIntentStatusCanceled:
		return "payment_intent.canceled"
	default:
		return "payment_intent.requires_action"
	}
}
