package midtrans

import (
	stripe "github.com/stripe/stripe-go/v81"
)

// mapStatus translates a Midtrans transaction_status (with its optional
// fraud_status) into a canonical Stripe PaymentIntentStatus. See plans.md §4.1.
//
// Midtrans success rule (per their webhook docs): a transaction is successful
// when transaction_status is "settlement", or "capture" with fraud_status
// "accept". For non-card methods fraud_status is often absent and may be ignored.
//
// Note on "failure"/"deny": Stripe has no "failed" PaymentIntent status — a
// failed payment resolves to requires_payment_method. For our one-shot facade
// that is the most wire-faithful value (and one Stripe apps already handle as a
// failed-payment UI state).
func mapStatus(transactionStatus, fraudStatus string) stripe.PaymentIntentStatus {
	switch transactionStatus {
	case "settlement":
		return stripe.PaymentIntentStatusSucceeded
	case "capture":
		if fraudStatus == "deny" {
			// Card captured but then rejected by Midtrans FDS -> treat as failed.
			return stripe.PaymentIntentStatusRequiresPaymentMethod
		}
		return stripe.PaymentIntentStatusSucceeded
	case "pending":
		return stripe.PaymentIntentStatusRequiresAction
	case "authorize":
		// Pre-authorize only (card). Funds are reserved; capture happens later.
		return stripe.PaymentIntentStatusRequiresCapture
	case "deny", "failure":
		return stripe.PaymentIntentStatusRequiresPaymentMethod
	case "cancel", "expire":
		return stripe.PaymentIntentStatusCanceled
	case "refund", "partial_refund":
		// Refunds do not change a PaymentIntent's status in Stripe: the PI stays
		// succeeded and the refund is reflected at the Charge level. So we keep
		// the intent succeeded; the Refund object carries the refund detail.
		return stripe.PaymentIntentStatusSucceeded
	default:
		// Unknown status: safest non-terminal state is "customer action pending".
		return stripe.PaymentIntentStatusRequiresAction
	}
}

// stripeEventType maps a canonical status to the Stripe event type emitted on
// webhook translation (consumed by the M2 webhook-out layer). These are the
// facade-level event names callers see in the re-signed Stripe-Signature payload.
func stripeEventType(status stripe.PaymentIntentStatus) string {
	switch status {
	case stripe.PaymentIntentStatusSucceeded:
		return "payment_intent.succeeded"
	case stripe.PaymentIntentStatusRequiresPaymentMethod:
		return "payment_intent.payment_failed"
	case stripe.PaymentIntentStatusCanceled:
		return "payment_intent.canceled"
	case stripe.PaymentIntentStatusRequiresCapture:
		return "payment_intent.requires_action" // closest canonical event; capture is an M+ concern
	default:
		return "payment_intent.requires_action"
	}
}
