#!/usr/bin/env bash
set -eu

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/assertions.sh
source "$SCRIPT_DIR/lib/assertions.sh"
# shellcheck source=lib/api.sh
source "$SCRIPT_DIR/lib/api.sh"

require_command curl
require_command jq

BASE_URL="${PAYMENT_BASE_URL:-http://localhost:8787}"
API_KEY="${PAYMENT_API_KEY:-sk_test_local_vamios}"
GATEWAY="${PAYMENT_GATEWAY:-stub}"

# Optional result capture for the e2e report (lib/report.sh). No-op unless
# E2E_REPORT_DIR is set.
# shellcheck source=lib/report.sh
source "$SCRIPT_DIR/lib/report.sh"
report_init

printf 'payrouter E2E\n'
printf 'Target: %s (gateway=%s)\n' "$BASE_URL" "$GATEWAY"

source "$SCRIPT_DIR/scenarios/health.sh"
source "$SCRIPT_DIR/scenarios/auth.sh"
source "$SCRIPT_DIR/scenarios/customers.sh"
source "$SCRIPT_DIR/scenarios/catalog.sh"
source "$SCRIPT_DIR/scenarios/payment_intents.sh"
source "$SCRIPT_DIR/scenarios/refunds.sh"
source "$SCRIPT_DIR/scenarios/checkout.sh"
source "$SCRIPT_DIR/scenarios/subscriptions.sh"
source "$SCRIPT_DIR/scenarios/webhooks.sh"

report_scenario health    health         scenario_health
report_scenario auth      auth           scenario_auth
report_scenario customers customers     scenario_customers
report_scenario catalog   catalog        scenario_catalog
report_scenario payments  payment_intents scenario_payment_intents
report_scenario refunds   refunds        scenario_refunds
report_scenario checkout  checkout       scenario_checkout
report_scenario subscriptions subscriptions scenario_subscriptions
report_scenario webhooks  webhooks       scenario_webhooks

printf '\nAll facade E2E scenarios passed.\n'
