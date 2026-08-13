#!/usr/bin/env bash

scenario_refunds() {
  printf '\n== refunds ==\n'

  : "${PAYMENT_INTENT_ID:?refunds scenario requires PAYMENT_INTENT_ID from payment_intents scenario}"

  request POST /v1/refunds "payment_intent=${PAYMENT_INTENT_ID}" "amount=25000" "reason=requested_by_customer"
  assert_eq "200" "$HTTP_STATUS" "create partial refund"
  assert_jq "$HTTP_BODY" ".object == \"refund\" and (.id | startswith(\"re_\")) and .payment_intent == \"${PAYMENT_INTENT_ID}\" and .amount == 25000" "refund fields"
  local refund_id
  refund_id="$(jq_field "$HTTP_BODY" '.id')"

  request GET "/v1/refunds/${refund_id}"
  assert_eq "200" "$HTTP_STATUS" "retrieve refund"

  request POST /v1/refunds "payment_intent=pi_does_not_exist" "amount=1000"
  assert_eq "404" "$HTTP_STATUS" "refund against unknown payment_intent is 404"

  request POST /v1/refunds "payment_intent=${PAYMENT_INTENT_ID}" "reason=not_a_real_reason"
  assert_eq "400" "$HTTP_STATUS" "invalid refund reason is rejected"
  assert_stripe_error "$HTTP_BODY" "parameter_invalid" "invalid reason error code"
}
