#!/usr/bin/env bash

scenario_catalog() {
  printf '\n== catalog (products & prices) ==\n'

  request POST /v1/products "name=Pro Plan" "description=Monthly AI usage plan"
  assert_eq "200" "$HTTP_STATUS" "create product"
  assert_jq "$HTTP_BODY" '.object == "product" and (.id | startswith("prod_")) and .active == true' "product has prod_ id and is active"
  local product_id
  product_id="$(jq_field "$HTTP_BODY" '.id')"

  request GET "/v1/products/${product_id}"
  assert_eq "200" "$HTTP_STATUS" "retrieve product"

  # One-time price against the existing product.
  request POST /v1/prices "product=${product_id}" "currency=idr" "unit_amount=15000"
  assert_eq "200" "$HTTP_STATUS" "create one-time price"
  assert_jq "$HTTP_BODY" '.object == "price" and .type == "one_time" and .unit_amount == 15000' "one-time price fields"
  local price_id
  price_id="$(jq_field "$HTTP_BODY" '.id')"

  request GET "/v1/prices/${price_id}"
  assert_eq "200" "$HTTP_STATUS" "retrieve price"

  # Recurring price with an inline product (Stripe's product_data[name] shortcut).
  request POST /v1/prices \
    "product_data[name]=Team Plan" \
    "currency=idr" \
    "unit_amount=250000" \
    "recurring[interval]=month"
  assert_eq "200" "$HTTP_STATUS" "create recurring price with inline product"
  assert_jq "$HTTP_BODY" '.type == "recurring" and .recurring.interval == "month" and (.product | startswith("prod_"))' "recurring price fields"

  request POST /v1/prices "currency=idr" "unit_amount=-5"
  assert_eq "400" "$HTTP_STATUS" "negative unit_amount is rejected"
  assert_stripe_error "$HTTP_BODY" "parameter_invalid" "negative unit_amount error code"

  PRODUCT_ID="$product_id"
  PRICE_ID="$price_id"
}
