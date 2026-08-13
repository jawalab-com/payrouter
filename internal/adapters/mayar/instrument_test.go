package mayar

import (
	"testing"

	"github.com/jawalab-com/payrouter/internal/gateway"
)

// TestMayarDeliberatelyDoesNotIssueInstruments pins a decision that is easy to
// mistake for an oversight.
//
// Mayar has a dynamic QR endpoint in both API versions (POST
// /hl/v1/qrcode/create and POST /hl/v2/qr-codes/create), so implementing
// gateway.InstrumentGateway here looks like a small gap to close. It is not:
// both take only an amount and return only {url, amount}, with no id and no
// reference field anywhere in the exchange. V2 standardizes the response
// envelope but adds no identifiers.
//
// This adapter correlates payments solely through Mayar's own id — CreatePayment
// stores resp.Data.ID as the GatewayReference and ParseWebhook resolves
// payment.received back through that reverse index. A QR issued with no id
// produces a webhook referencing a value that was never stored, so the customer
// pays and the order is never marked paid.
//
// If this assertion ever fails, someone has added instrument issuance. Confirm
// first that Mayar now returns a correlatable id; otherwise the implementation
// loses payments silently.
func TestMayarDeliberatelyDoesNotIssueInstruments(t *testing.T) {
	var a any = New("key", "token", true)

	if _, ok := a.(gateway.InstrumentGateway); ok {
		t.Fatal("Mayar now implements InstrumentGateway — verify the QR endpoint returns " +
			"an id that ParseWebhook can correlate, or payments will be received without " +
			"their intents being marked paid")
	}
}

// TestMayarStillSatisfiesGateway guards the capability that does exist: the
// hosted payment link remains the supported path.
func TestMayarStillSatisfiesGateway(t *testing.T) {
	var _ gateway.Gateway = (*Adapter)(nil)
}
