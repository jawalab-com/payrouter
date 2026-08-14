package stub

import (
	"context"
	"fmt"
	"time"

	"github.com/jawalab-com/payrouter/internal/gateway"
	stripe "github.com/stripe/stripe-go/v81"
)

// Direct instrument issuance for the stub gateway.
//
// This exists so the hosted checkout UI can be developed and demonstrated
// end-to-end without live gateway credentials: PAYMENT_GATEWAY=stub renders a
// real method picker, a real (scannable but non-payable) QR, and a virtual
// account number.
//
// The QRIS payload is a well-formed EMVCo string so QR encoding and scanning can
// be verified, but it carries the stub's own merchant identifiers and is not
// payable anywhere.
const stubInstrumentTTL = 24 * time.Hour

// SupportsInstrument reports the methods the stub can issue: the same two the
// real adapters support, so the picker exercises both paths.
func (g *Gateway) SupportsInstrument(method gateway.IDPaymentMethodType) bool {
	switch method {
	case gateway.IDQRIS, gateway.IDVirtualAccount:
		return true
	default:
		return false
	}
}

// IssueInstrument returns a canned instrument shaped like a real one.
func (g *Gateway) IssueInstrument(_ context.Context, in *gateway.CreatePaymentInput) (*gateway.PaymentResult, error) {
	if in == nil || in.Reference == "" {
		return nil, fmt.Errorf("stub: IssueInstrument requires a reference")
	}
	expires := time.Now().Add(stubInstrumentTTL).Unix()
	ref := "stub_" + in.Reference

	display := &gateway.DisplayInstructions{ExpiresAt: expires}
	switch in.PaymentMethodType {
	case gateway.IDQRIS:
		display.Kind = gateway.InstrumentQRIS
		display.QRIS = &gateway.QRISInstruction{Payload: stubQRPayload(in.AmountMinor)}
	case gateway.IDVirtualAccount:
		bank, _ := in.MethodParams["bank"].(string)
		if bank == "" {
			bank = "bca"
		}
		display.Kind = gateway.InstrumentVirtualAccount
		display.VirtualAccount = &gateway.VirtualAccountInstruction{
			Bank:          bank,
			AccountNumber: fmt.Sprintf("8808%011d", in.AmountMinor%100000000000),
			AccountName:   "STUB MERCHANT",
		}
	default:
		return nil, fmt.Errorf("stub: %w: %s", gateway.ErrInstrumentUnsupported, in.PaymentMethodType)
	}

	return &gateway.PaymentResult{
		Reference:        in.Reference,
		GatewayReference: ref,
		Status:           stripe.PaymentIntentStatusRequiresAction,
		ExpiresAt:        expires,
		Display:          display,
		Raw:              map[string]any{"stub": true},
	}, nil
}

// stubQRPayload builds a structurally valid EMVCo QR string so the rendered code
// scans as QRIS-shaped data rather than arbitrary text. The CRC is a fixed
// placeholder — this is not a payable code.
func stubQRPayload(amountMinor int64) string {
	return fmt.Sprintf(
		"00020101021226590013ID.CO.STUB.WWW0118936000000000000000215ID.CO.STUB.WWW0303UMI"+
			"51440014ID.CO.QRIS.WWW0215ID20232000000005204581453033605802ID"+
			"5910STUBSTORE6007JAKARTA61051234062070703A01540%d6304ABCD",
		amountMinor)
}
