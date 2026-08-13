package xendit

import (
	"strings"

	stripe "github.com/stripe/stripe-go/v81"
)

// mapStatus translates a Xendit Invoice status into a canonical Stripe
// PaymentIntentStatus. See plans.md §4.2.
//
// Xendit Invoice statuses (verified from Xendit's InvoiceStatus enum):
//   - PENDING: created, awaiting payment
//   - PAID:    customer paid; funds not yet reflected on the balance
//   - SETTLED: funds settled to the merchant balance
//   - EXPIRED: expired before payment was completed
//
// Both PAID and SETTLED are success from a PaymentIntent perspective (money
// collected); EXPIRED maps to canceled (Stripe has no "expired" PI status, mirroring
// how Midtrans' "expire" is mapped). Input is matched case-insensitively.
func mapStatus(status string) stripe.PaymentIntentStatus {
	switch strings.ToUpper(status) {
	case "PENDING":
		return stripe.PaymentIntentStatusRequiresAction
	case "PAID", "SETTLED":
		return stripe.PaymentIntentStatusSucceeded
	case "EXPIRED":
		return stripe.PaymentIntentStatusCanceled
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
