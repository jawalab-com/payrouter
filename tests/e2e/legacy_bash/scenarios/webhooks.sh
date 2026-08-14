#!/usr/bin/env bash

scenario_webhooks() {
  printf '\n== inbound webhooks ==\n'

  # The route only accepts the path matching the facade's active gateway
  # (PAYMENT_GATEWAY on the server). A mismatched gateway name is a 404 without
  # even attempting signature verification.
  unauth_request POST "/v1/webhooks/not-a-real-gateway" --data 'anything=1'
  assert_eq "404" "$HTTP_STATUS" "webhook path for an inactive gateway is 404"
  assert_stripe_error "$HTTP_BODY" "resource_missing" "inactive gateway webhook error code"

  if [[ "$GATEWAY" == "stub" ]]; then
    skip "stub gateway's ParseWebhook is a no-op fixture; no signature scheme to exercise"
    return
  fi

  printf '  NOTE  live signature verification for %s webhooks is not exercised here — see xendit.sh\n' "$GATEWAY"
}
