#!/usr/bin/env bash

fail() {
  printf '\nFAIL: %s\n' "$*" >&2
  exit 1
}

pass() {
  printf '  PASS  %s\n' "$*"
}

skip() {
  printf '  SKIP  %s\n' "$*"
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "required command not found: $1"
}

assert_eq() {
  local expected="$1"
  local actual="$2"
  local message="$3"
  [[ "$actual" == "$expected" ]] || fail "$message (expected '$expected', got '$actual')"
  pass "$message"
}

assert_jq() {
  local json="$1"
  local expression="$2"
  local message="$3"
  jq -e "$expression" >/dev/null 2>&1 <<<"$json" || {
    printf 'Response was: %s\n' "$json" >&2
    fail "$message (jq expression: $expression)"
  }
  pass "$message"
}

# jq_field JSON EXPR — convenience for scenarios that need to thread an id/value
# from one response into the next request.
jq_field() {
  jq -r "$2" <<<"$1"
}
