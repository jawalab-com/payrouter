#!/usr/bin/env bash
#
# run-ui.sh — OPTIONAL browser e2e for the hosted checkout UI.
#
# This is deliberately separate from ../run.sh, which stays bash+curl-only and
# runs in every CI environment. This layer needs Chromium, so it is opt-in.
#
# Usage:
#   ./tests/e2e/ui/run-ui.sh               # run headless, fast (default)
#   ./tests/e2e/ui/run-ui.sh --install     # first time: download Chromium, then run
#   ./tests/e2e/ui/run-ui.sh --screenshots # headless, but capture a PNG per step
#   ./tests/e2e/ui/run-ui.sh --record      # headed + screenshots + cursor (live supervision)
#   ./tests/e2e/ui/run-ui.sh --install --record
#
# Environment:
#   E2E_UI_INSTALL=1     same as --install
#   E2E_UI_SCREENSHOTS=1 same as --screenshots (silent, headless PNG capture)
#   E2E_UI_RECORD=1      same as --record (headed; implies screenshots)
#   E2E_UI_SLOWMO=N      milliseconds to slow each step (default 120 in record mode)
#
# Artifacts (when --screenshots or --record): tests/e2e/ui/artifacts/*.png
#
# Install Chromium only (no test run), e.g. to warm a CI image:
#   E2E_UI_INSTALL=1 go test -tags e2e_ui -run '^$' ./tests/e2e/ui/...
set -eu

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR/../../.." # repo root (ui/ is tests/e2e/ui/, three levels deep)

INSTALL=0
RECORD=0
SCREENSHOTS=0
for arg in "$@"; do
  case "$arg" in
    --install)     INSTALL=1 ;;
    --record)      RECORD=1 ;;
    --screenshots) SCREENSHOTS=1 ;;
    *) echo "unknown flag: $arg" >&2; exit 2 ;;
  esac
done
[[ "${E2E_UI_INSTALL:-0}" == "1" ]] && INSTALL=1
[[ "${E2E_UI_RECORD:-0}" == "1" ]] && RECORD=1
[[ "${E2E_UI_SCREENSHOTS:-0}" == "1" ]] && SCREENSHOTS=1

# --install hands off to the test binary's TestMain, which calls
# playwright.Install({chromium}) before anything else. With -run '^$' that is the
# ONLY thing it does (zero tests match), so it is a clean, self-contained
# installer using the same code path the tests use.
if [[ "$INSTALL" == "1" ]]; then
  echo ">> installing Chromium for playwright-go (one-time, ~150MB)"
  E2E_UI_INSTALL=1 go test -tags e2e_ui -run '^$' ./tests/e2e/ui/...
  echo ">> Chromium installed"
fi

# A normal (non-install) run must never re-download: leave E2E_UI_INSTALL unset.
if [[ "$INSTALL" == "1" ]]; then export E2E_UI_INSTALL=1; else unset E2E_UI_INSTALL || true; fi

# --screenshots captures a PNG per step silently (headless); --record is headed
# supervision with the cursor overlay and implies screenshots. They compose.
if [[ "$RECORD" == "1" ]]; then
  export E2E_UI_RECORD=1
  echo ">> running UI e2e (HEADED + screenshots + cursor)"
elif [[ "$SCREENSHOTS" == "1" ]]; then
  export E2E_UI_SCREENSHOTS=1
  echo ">> running UI e2e (headless + screenshots)"
else
  echo ">> running UI e2e (headless)"
fi

# Report mode: emit go-test JSON for the report generator and capture the exit
# code (a failing test must not abort before run-all.sh records it). Verbose
# human output is dropped here — the HTML report is the readable artifact.
if [[ -n "${E2E_REPORT_DIR:-}" ]]; then
  echo ">> capturing JSON report to $E2E_REPORT_DIR/ui.json"
  set +e
  go test -tags e2e_ui -count=1 -json ./tests/e2e/ui/... > "$E2E_REPORT_DIR/ui.json"
  UI_RC=$?
  set -e
  if [[ "$UI_RC" -ne 0 ]]; then
    echo ">> UI e2e reported failures (rc=$UI_RC); see the HTML report" >&2
  fi
  exit "$UI_RC"
fi

go test -tags e2e_ui -count=1 -v ./tests/e2e/ui/...
