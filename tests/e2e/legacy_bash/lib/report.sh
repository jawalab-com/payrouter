#!/usr/bin/env bash
#
# report.sh — opt-in result capture for the bash e2e harness.
#
# When E2E_REPORT_DIR is set, each scenario's pass/fail (and the assertion that
# failed) is appended as one JSON line to $E2E_REPORT_DIR/api.jsonl, which the
# Go report generator (tests/e2e/report) turns into JUnit XML + HTML. When
# E2E_REPORT_DIR is unset every function here is a no-op, so run.sh's behavior is
# byte-for-byte unchanged.
#
# Scenarios still run in the SAME shell (not subshells), because later scenarios
# depend on globals earlier ones set (CUSTOMER_ID, PRICE_ID). So the run stays
# fail-fast: the first failing assertion aborts, and scenarios after it are simply
# never recorded. The report reflects that honestly (pass up to the failure).

# epoch_ms prints milliseconds since the epoch via GNU date, or 0 if unavailable.
epoch_ms() {
  local t
  t=$(date +%s%3N 2>/dev/null)
  case "$t" in
    '' | *[^0-9]*) echo 0 ;;
    *) echo "$t" ;;
  esac
}

# report_init prepares the report directory and a fresh api.jsonl. No-op when
# reporting is off.
report_init() {
  [[ -n "${E2E_REPORT_DIR:-}" ]] || return 0
  mkdir -p "$E2E_REPORT_DIR"
  : > "$E2E_REPORT_DIR/api.jsonl"
}

# report_record LAYER SUITE NAME STATUS DURATION_MS ERROR appends one JSON object.
# No-op when reporting is off. duration is coerced to a number defensively.
report_record() {
  [[ -n "${E2E_REPORT_DIR:-}" ]] || return 0
  local layer="$1" suite="$2" name="$3" status="$4" dur="$5" err="$6"
  jq -nc \
    --arg layer "$layer" --arg suite "$suite" --arg name "$name" \
    --arg status "$status" --arg duration "$dur" --arg error "$err" \
    '{layer:$layer, suite:$suite, name:$name, status:$status,
      duration_ms:($duration|tonumber? // 0), error:$error}' \
    >> "$E2E_REPORT_DIR/api.jsonl"
}

# report_scenario SUITE NAME FUNC... runs FUNC in the current shell (so the globals
# it sets propagate to later scenarios), recording pass on return. A failure calls
# fail() (which records the failure itself) and exits, so this only records passes.
CURRENT_SUITE=""
CURRENT_SCENARIO=""

report_scenario() {
  local suite="$1" name="$2"
  shift 2
  CURRENT_SUITE="$suite"
  CURRENT_SCENARIO="$name"
  local start
  start=$(epoch_ms)
  "$@"
  report_record api "$suite" "$name" pass "$(( $(epoch_ms) - start ))" ""
  CURRENT_SCENARIO=""
  CURRENT_SUITE=""
}
