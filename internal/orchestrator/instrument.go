package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jawalab-com/payrouter/internal/gateway"
)

// Instrument-aware routing.
//
// This is where least-cost routing finally works properly. CreatePayment routes a
// hosted checkout, where the customer picks a method on the gateway's page AFTER
// a gateway has been chosen — so no single fee applies and selection falls back
// to priority order. Issuing an instrument inverts that: the method is known
// first, so the router can price a real channel and compare candidates on it.
//
// Selection runs in two stages, mirroring the eligibility check a payment
// orchestrator needs:
//
//  1. Eligibility — keep only adapters that can actually issue this instrument.
//     A gateway missing credentials, or one that simply has no API for the
//     method, must not win on price and then fail at charge time.
//  2. Cost — compare the surviving candidates on the resolved fee channel.
//
// When nothing is eligible the caller gets ErrInstrumentUnsupported and can fall
// back to a hosted redirect, which is always available.

// SupportsInstrument reports whether ANY configured adapter can issue the method
// directly. The server uses this to decide whether to offer the method on its
// own checkout page or send the customer to a hosted one.
func (og *OrchestratedGateway) SupportsInstrument(method gateway.IDPaymentMethodType) bool {
	return len(og.instrumentCandidates(method)) > 0
}

// IssueInstrument routes an instrument request to the cheapest gateway that can
// actually issue it.
func (og *OrchestratedGateway) IssueInstrument(ctx context.Context, in *gateway.CreatePaymentInput) (*gateway.PaymentResult, error) {
	if in == nil || in.Reference == "" {
		return nil, errors.New("orchestrator: IssueInstrument requires a reference")
	}
	if len(og.adapters) == 0 {
		return nil, errors.New("orchestrator: no gateway credentials configured in environment")
	}

	eligible := og.instrumentCandidates(in.PaymentMethodType)
	if len(eligible) == 0 {
		return nil, fmt.Errorf("orchestrator: %w: no configured gateway can issue %s",
			gateway.ErrInstrumentUnsupported, in.PaymentMethodType)
	}

	// The method is concrete here, so the channel resolves and a real cost
	// comparison is possible — unlike the hosted path.
	channel, resolved := ResolveChannel(string(in.PaymentMethodType), in.MethodParams)
	if !resolved {
		channel = string(in.PaymentMethodType)
	}

	names := make([]string, 0, len(eligible))
	for name := range eligible {
		names = append(names, name)
	}
	ranked := og.router.RankCandidates(names, channel, float64(in.AmountMinor))
	if len(ranked) == 0 {
		return nil, fmt.Errorf("orchestrator: %w: no eligible gateways available for %s",
			gateway.ErrInstrumentUnsupported, in.PaymentMethodType)
	}

	var lastErr error
	for attemptIdx, info := range ranked {
		issuer, ok := eligible[info.Gateway]
		if !ok {
			continue
		}

		if attemptIdx == 0 {
			if info.Basis == gateway.RoutingLeastCost {
				slog.Debug("orchestrator: least-cost instrument selection",
					"method", in.PaymentMethodType,
					"channel", info.Channel,
					"selected_gateway", info.Gateway,
					"fee_minor", info.FeeMinor,
					"eligible", names)
			} else {
				slog.Warn("orchestrator: no fee data for instrument channel, falling back to priority order",
					"method", in.PaymentMethodType,
					"resolved_channel", info.Channel,
					"selected_gateway", info.Gateway,
					"eligible", names,
					"hint", "add a fee entry for this channel in config.yaml to enable least-cost routing")
			}
		} else {
			slog.Warn("orchestrator: failover attempt to next instrument issuer",
				"method", in.PaymentMethodType,
				"failed_attempt_index", attemptIdx-1,
				"failover_gateway", info.Gateway)
		}

		res, err := issuer.IssueInstrument(ctx, in)
		if err == nil {
			if og.router.breaker != nil {
				og.router.breaker.RecordSuccess(info.Gateway)
			}
			if res != nil {
				if res.Reference != "" {
					res.Reference = FormatOrderID(info.Gateway, res.Reference)
				}
				routing := info
				res.Routing = &routing
			}
			return res, nil
		}

		lastErr = err
		if og.router.breaker != nil {
			og.router.breaker.RecordFailure(info.Gateway, err)
		}
		slog.Warn("orchestrator: gateway issue instrument failed",
			"gateway", info.Gateway,
			"method", in.PaymentMethodType,
			"error", err)
	}

	return nil, lastErr
}

// instrumentCandidates returns the adapters that both implement the optional
// InstrumentGateway capability and report support for this specific method.
//
// Both checks matter: an adapter may implement the interface yet be unable to
// issue a given method because the required credentials were never configured —
// DOKU without a partner service id can do QRIS but not virtual accounts.
func (og *OrchestratedGateway) instrumentCandidates(method gateway.IDPaymentMethodType) map[string]gateway.InstrumentGateway {
	out := make(map[string]gateway.InstrumentGateway, len(og.adapters))
	for name, adapter := range og.adapters {
		issuer, ok := adapter.(gateway.InstrumentGateway)
		if !ok || !issuer.SupportsInstrument(method) {
			continue
		}
		out[name] = issuer
	}
	return out
}
