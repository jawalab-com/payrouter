#!/usr/bin/env bash
#
# run-all.sh — run BOTH e2e layers and produce a combined report.
#
#   ./tests/e2e/run-all.sh                 # API + UI (headless), report under ./tests/e2e/report/out
#   ./tests/e2e/run-all.sh --record        # UI headed with screenshots/video
#   ./tests/e2e/run-all.sh --install       # first-time Chromium install
#   ./tests/e2e/run-all.sh --open          # open the HTML report in a browser when done
#   E2E_REPORT_DIR=... ./tests/e2e/run-all.sh
#
# Unlike run.sh and run-ui.sh, this does NOT fail-fast: it runs each layer to
# completion, captures both exit codes, then generates the report, and only then
# returns non-zero if either layer failed. That way a failure in one layer still
# gives you a full report covering both.
#
# Skipping a layer:
#   E2E_SKIP_UI=1    ./tests/e2e/run-all.sh   # API only, no Playwright/Chromium needed
#   E2E_SKIP_API=1   ./tests/e2e/run-all.sh   # UI only
set -uo pipefail # NB: no -e — we survive per-layer failures by design.

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$SCRIPT_DIR/../.."

# Default report output next to the generator package; overridable.
export E2E_REPORT_DIR="${E2E_REPORT_DIR:-$SCRIPT_DIR/report/out}"
ARTIFACTS_DIR="$SCRIPT_DIR/ui/artifacts"
OUT_DIR="$E2E_REPORT_DIR"

OPEN=0
PASSTHROUGH=() # flags forwarded to run-ui.sh (--install/--record)
for arg in "$@"; do
  case "$arg" in
    --open) OPEN=1 ;;
    --install|--record) PASSTHROUGH+=("$arg") ;;
    *) echo "unknown flag: $arg" >&2; exit 2 ;;
  esac
done

mkdir -p "$E2E_REPORT_DIR"
echo "== PayRouter e2e (both layers) — report → $E2E_REPORT_DIR =="

API_RC=0
UI_RC=0

# --- API layer ---------------------------------------------------------------
if [[ "${E2E_SKIP_API:-0}" != "1" ]]; then
  echo -e "\n== [1/2] API (bash + curl) =="
  # run.sh is fail-fast under set -e; its exit code tells us pass/fail.
  "$SCRIPT_DIR/run.sh" || API_RC=$?
else
  echo "== [1/2] API: skipped (E2E_SKIP_API=1) =="
fi

# --- UI layer ----------------------------------------------------------------
if [[ "${E2E_SKIP_UI:-0}" != "1" ]]; then
  echo -e "\n== [2/2] UI (Playwright) =="
  # run-ui.sh emits ui.json into E2E_REPORT_DIR in report mode and returns its rc.
  "$SCRIPT_DIR/ui/run-ui.sh" "${PASSTHROUGH[@]}" || UI_RC=$?
else
  echo "== [2/2] UI: skipped (E2E_SKIP_UI=1) =="
fi

# --- Report ------------------------------------------------------------------
echo -e "\n== generating report =="
REPORT_ARGS=(-api "$E2E_REPORT_DIR/api.jsonl" -ui "$E2E_REPORT_DIR/ui.json" -artifacts "$ARTIFACTS_DIR" -out "$OUT_DIR")
[[ "$OPEN" == "1" ]] && REPORT_ARGS+=(-open)

# The generator exits 1 if any case failed; capture so we can summarize first.
GEN_RC=0
( cd "$REPO_ROOT" && go run ./tests/e2e/report "${REPORT_ARGS[@]}" ) || GEN_RC=$?

echo
echo "================ RESULT ================"
if [[ "${E2E_SKIP_API:-0}" == "1" ]]; then
  echo "  API layer:  SKIP (E2E_SKIP_API=1)"
else
  echo "  API layer:  $([ "$API_RC" -eq 0 ] && echo PASS || echo "FAIL (rc=$API_RC)")"
fi
if [[ "${E2E_SKIP_UI:-0}" == "1" ]]; then
  echo "  UI  layer:  SKIP (E2E_SKIP_UI=1)"
else
  echo "  UI  layer:  $([ "$UI_RC" -eq 0 ] && echo PASS || echo "FAIL (rc=$UI_RC)")"
fi
echo "  Report:     $OUT_DIR/e2e-report.html"
echo "========================================"

# A layer failure is the signal callers should care about. A skipped layer is
# not a failure — it simply did not run, so it is absent from the report.
if [[ "${E2E_SKIP_API:-0}" != "1" && "$API_RC" -ne 0 ]]; then exit 1; fi
if [[ "${E2E_SKIP_UI:-0}"  != "1" && "$UI_RC"  -ne 0 ]]; then exit 1; fi
exit 0
