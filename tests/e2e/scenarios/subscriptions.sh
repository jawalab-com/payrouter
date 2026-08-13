#!/usr/bin/env bash

scenario_subscriptions() {
  printf '\n== subscriptions ==\n'

  : "${CUSTOMER_ID:?subscriptions scenario requires CUSTOMER_ID from customers scenario}"

  request POST /v1/subscriptions \
    "customer=${CUSTOMER_ID}" \
    "items[0][price_data][currency]=idr" \
    "items[0][price_data][unit_amount]=99000" \
    "items[0][price_data][recurring][interval]=month" \
    "items[0][price_data][product_data][name]=Team Plan"

  if [[ "$HTTP_STATUS" == "400" ]] && jq -e '.error.message | test("not supported")' >/dev/null 2>&1 <<<"$HTTP_BODY"; then
    skip "active gateway does not support subscriptions (Midtrans-only today); create rejected as expected"
    return
  fi

  assert_eq "200" "$HTTP_STATUS" "create subscription"
  assert_jq "$HTTP_BODY" '.object == "subscription" and (.id | startswith("sub_")) and .status == "incomplete"' "subscription starts incomplete pending authorization"
  assert_jq "$HTTP_BODY" '.latest_invoice.payment_intent.next_action != null' "subscription surfaces the authorization redirect"
  local sub_id
  sub_id="$(jq_field "$HTTP_BODY" '.id')"

  request GET "/v1/subscriptions/${sub_id}"
  assert_eq "200" "$HTTP_STATUS" "retrieve subscription"
  assert_jq "$HTTP_BODY" ".id == \"${sub_id}\"" "retrieved subscription id matches"

  local invoice_id
  invoice_id="$(jq_field "$HTTP_BODY" '.latest_invoice.id // .latest_invoice')"
  if [[ -n "$invoice_id" && "$invoice_id" != "null" ]]; then
    request GET "/v1/invoices/${invoice_id}"
    assert_eq "200" "$HTTP_STATUS" "retrieve subscription's latest invoice"
  fi

  request DELETE "/v1/subscriptions/${sub_id}"
  assert_eq "200" "$HTTP_STATUS" "cancel subscription"
  assert_jq "$HTTP_BODY" '.status == "canceled"' "canceled subscription reports status canceled"
}
