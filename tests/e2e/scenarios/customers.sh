#!/usr/bin/env bash

scenario_customers() {
  printf '\n== customers ==\n'

  request POST /v1/customers \
    "email=ada@example.test" \
    "name=Ada Lovelace" \
    "phone=+62 811 000 000" \
    "metadata[plan]=pro"
  assert_eq "200" "$HTTP_STATUS" "create customer"
  assert_jq "$HTTP_BODY" '.object == "customer" and (.id | startswith("cus_"))' "customer has cus_ id"
  assert_jq "$HTTP_BODY" '.email == "ada@example.test" and .metadata.plan == "pro"' "customer echoes email and metadata"
  local customer_id
  customer_id="$(jq_field "$HTTP_BODY" '.id')"

  request GET "/v1/customers/${customer_id}"
  assert_eq "200" "$HTTP_STATUS" "retrieve customer"
  assert_jq "$HTTP_BODY" ".id == \"${customer_id}\"" "retrieved customer id matches"

  request GET "/v1/customers/cus_does_not_exist"
  assert_eq "404" "$HTTP_STATUS" "retrieve unknown customer is 404"
  assert_stripe_error "$HTTP_BODY" "resource_missing" "unknown customer error code"

  CUSTOMER_ID="$customer_id"
}
