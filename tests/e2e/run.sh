#!/usr/bin/env bash
set -eu

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/assertions.sh
source "$SCRIPT_DIR/lib/assertions.sh"
# shellcheck source=lib/api.sh
source "$SCRIPT_DIR/lib/api.sh"

require_command curl
require_command jq

BASE_URL="${FACADE_BASE_URL:-http://localhost:8787}"
API_KEY="${FACADE_API_KEY:-sk_test_local_vamios}"
GATEWAY="${FACADE_GATEWAY:-stub}"

printf 'stripe-compatible-facade E2E\n'
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

scenario_health
scenario_auth
scenario_customers
scenario_catalog
scenario_payment_intents
scenario_refunds
scenario_checkout
scenario_subscriptions
scenario_webhooks

printf '\nAll facade E2E scenarios passed.\n'
