#!/usr/bin/env bash

scenario_health() {
  printf '\n== health ==\n'

  unauth_request GET /healthz
  assert_eq "200" "$HTTP_STATUS" "GET /healthz returns 200"
  assert_jq "$HTTP_BODY" '.status == "ok"' "healthz body reports status ok"

  unauth_request GET /health/ready
  assert_eq "200" "$HTTP_STATUS" "GET /health/ready returns 200"
}
