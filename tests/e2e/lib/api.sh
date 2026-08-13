#!/usr/bin/env bash

# request METHOD PATH [FIELD=VALUE ...]
#
# Talks to the facade as a Stripe client would: Bearer auth, form-urlencoded
# body. Fields are passed as separate "key=value" arguments (brackets in the
# key, e.g. "metadata[plan]=pro" or "items[0][price_data][unit_amount]=1000",
# are left literal; only the value is percent-encoded). An optional
# IDEMPOTENCY_KEY variable, if set before the call, is sent as the
# Idempotency-Key header and cleared after.
#
# Sets HTTP_STATUS, HTTP_BODY, HTTP_HEADERS as globals, same convention as the
# Rust platform's scripts/e2e harness.
request() {
  local method="$1"
  local path="$2"
  shift 2

  local curl_args=(--silent --show-error --request "$method")
  curl_args+=(--header "Authorization: Bearer ${API_KEY}")
  if [[ -n "${IDEMPOTENCY_KEY:-}" ]]; then
    curl_args+=(--header "Idempotency-Key: ${IDEMPOTENCY_KEY}")
  fi
  for field in "$@"; do
    curl_args+=(--data-urlencode "$field")
  done

  local output headers
  output="$(mktemp "${TMPDIR:-/tmp}/facade-e2e-response.XXXXXX")"
  headers="$(mktemp "${TMPDIR:-/tmp}/facade-e2e-headers.XXXXXX")"
  HTTP_STATUS="$(curl "${curl_args[@]}" --output "$output" --write-out '%{http_code}' \
    --dump-header "$headers" "${BASE_URL}${path}")" || {
    local curl_status=$?
    rm -f "$output" "$headers"
    fail "curl failed for $method $path with exit code $curl_status"
  }
  HTTP_BODY="$(cat "$output")"
  HTTP_HEADERS="$(tr -d '\r' <"$headers")"
  rm -f "$output" "$headers"
  unset IDEMPOTENCY_KEY
}

# unauth_request METHOD PATH — like request, but with no Authorization header
# and no form encoding, for probing auth failures and raw webhook posts.
unauth_request() {
  local method="$1"
  local path="$2"
  shift 2

  local output headers
  output="$(mktemp "${TMPDIR:-/tmp}/facade-e2e-response.XXXXXX")"
  headers="$(mktemp "${TMPDIR:-/tmp}/facade-e2e-headers.XXXXXX")"
  HTTP_STATUS="$(curl --silent --show-error --request "$method" --output "$output" \
    --write-out '%{http_code}' --dump-header "$headers" "${BASE_URL}${path}" "$@")" || {
    local curl_status=$?
    rm -f "$output" "$headers"
    fail "curl failed for $method $path with exit code $curl_status"
  }
  HTTP_BODY="$(cat "$output")"
  HTTP_HEADERS="$(tr -d '\r' <"$headers")"
  rm -f "$output" "$headers"
}

assert_stripe_error() {
  local body="$1"
  local expected_code="$2"
  local message="$3"
  assert_jq "$body" ".error.code == \"${expected_code}\"" "$message"
}
