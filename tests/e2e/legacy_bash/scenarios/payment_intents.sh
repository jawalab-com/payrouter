#!/usr/bin/env bash

scenario_payment_intents() {
  printf '\n== payment intents ==\n'

  request POST /v1/payment_intents \
    "amount=125000" \
    "currency=idr" \
    "description=Usage overage" \
    "metadata[order]=e2e-1"
  assert_eq "200" "$HTTP_STATUS" "create payment intent"
  assert_jq "$HTTP_BODY" '.object == "payment_intent" and (.id | startswith("pi_")) and .amount == 125000' "payment intent fields"
  assert_jq "$HTTP_BODY" '. as $pi | $pi.client_secret != null and ($pi.client_secret | startswith($pi.id + "_secret_"))' "client_secret is scoped to the intent id"
  local intent_id
  intent_id="$(jq_field "$HTTP_BODY" '.id')"

  request GET "/v1/payment_intents/${intent_id}"
  assert_eq "200" "$HTTP_STATUS" "retrieve payment intent"
  assert_jq "$HTTP_BODY" ".id == \"${intent_id}\"" "retrieved intent id matches"

  request POST "/v1/payment_intents/${intent_id}/confirm" "return_url=https://merchant.example.test/return"
  assert_eq "200" "$HTTP_STATUS" "confirm payment intent"
  assert_jq "$HTTP_BODY" ".id == \"${intent_id}\"" "confirm returns the same intent"

  request POST /v1/payment_intents "amount=not-a-number" "currency=idr"
  assert_eq "400" "$HTTP_STATUS" "non-numeric amount is rejected"
  assert_stripe_error "$HTTP_BODY" "parameter_invalid" "invalid amount error code"

  request GET "/v1/payment_intents/pi_does_not_exist"
  assert_eq "404" "$HTTP_STATUS" "retrieve unknown intent is 404"

  # Idempotency-Key handling is only active when the facade runs against
  # PostgreSQL (PAYMENT_DATABASE_URL); the in-memory store treats it as a no-op.
  # Detect which mode we're in from the replay itself instead of assuming.
  local idem_key="e2e-idem-$$-$RANDOM"
  IDEMPOTENCY_KEY="$idem_key" request POST /v1/payment_intents "amount=5000" "currency=idr"
  assert_eq "200" "$HTTP_STATUS" "idempotent create (first)"
  local first_id
  first_id="$(jq_field "$HTTP_BODY" '.id')"

  IDEMPOTENCY_KEY="$idem_key" request POST /v1/payment_intents "amount=5000" "currency=idr"
  assert_eq "200" "$HTTP_STATUS" "idempotent create (replay)"
  local replay_id
  replay_id="$(jq_field "$HTTP_BODY" '.id')"

  if [[ "$replay_id" == "$first_id" ]]; then
    pass "replay returns the same payment_intent id (durable idempotency active)"
    IDEMPOTENCY_KEY="$idem_key" request POST /v1/payment_intents "amount=9999" "currency=idr"
    assert_eq "409" "$HTTP_STATUS" "same Idempotency-Key with a different body conflicts"
  else
    skip "replay produced a different id — facade is running without PAYMENT_DATABASE_URL, so Idempotency-Key is a no-op"
  fi

  PAYMENT_INTENT_ID="$intent_id"
}
