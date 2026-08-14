#!/usr/bin/env bash

scenario_auth() {
  printf '\n== auth ==\n'

  unauth_request POST /v1/customers
  assert_eq "401" "$HTTP_STATUS" "missing Authorization header is rejected"
  assert_stripe_error "$HTTP_BODY" "authentication_required" "missing key error code"

  unauth_request POST /v1/customers --header "Authorization: Bearer sk_test_definitely_not_a_real_key"
  assert_eq "401" "$HTTP_STATUS" "unknown API key is rejected"
  assert_stripe_error "$HTTP_BODY" "authentication_required" "unknown key error code"

  request POST /v1/customers "email=auth-check@example.test"
  assert_eq "200" "$HTTP_STATUS" "valid API key is accepted"
}
