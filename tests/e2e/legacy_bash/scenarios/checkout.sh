#!/usr/bin/env bash

scenario_checkout() {
  printf '\n== checkout sessions ==\n'

  : "${CUSTOMER_ID:?checkout scenario requires CUSTOMER_ID from customers scenario}"
  : "${PRICE_ID:?checkout scenario requires PRICE_ID from catalog scenario}"

  request POST /v1/checkout/sessions \
    "mode=payment" \
    "success_url=https://merchant.example.test/success" \
    "cancel_url=https://merchant.example.test/cancel" \
    "customer=${CUSTOMER_ID}" \
    "line_items[0][price]=${PRICE_ID}" \
    "line_items[0][quantity]=2"
  assert_eq "200" "$HTTP_STATUS" "create checkout session"
  assert_jq "$HTTP_BODY" '.object == "checkout.session" and (.id | startswith("cs_")) and .mode == "payment"' "checkout session fields"
  assert_jq "$HTTP_BODY" '.url != null and .payment_intent != null' "checkout session carries a hosted URL and payment_intent"
  local session_id
  session_id="$(jq_field "$HTTP_BODY" '.id')"

  request GET "/v1/checkout/sessions/${session_id}"
  assert_eq "200" "$HTTP_STATUS" "retrieve checkout session"
  assert_jq "$HTTP_BODY" ".id == \"${session_id}\"" "retrieved session id matches"

  request POST /v1/checkout/sessions "mode=payment" "success_url=https://merchant.example.test/success"
  assert_eq "400" "$HTTP_STATUS" "checkout session without line_items is rejected"
  assert_stripe_error "$HTTP_BODY" "parameter_missing" "missing line_items error code"
}
