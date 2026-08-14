# PayRouter End-to-End (E2E) Test Suite

Unified, cross-platform E2E verification suite for PayRouter. Runs black-box API scenarios and Playwright browser UI checkout tests, generating JUnit XML (for CI) and a self-contained HTML report with embedded visual screenshot artifacts.

---

## 1. Quick Start

### Run All Tests (API + UI) & Generate HTML/XML Report
```bash
# Windows PowerShell
go test -tags e2e_ui -json ./tests/e2e/... | go run ./tests/e2e/report -artifacts ./tests/e2e/ui/artifacts -out ./tests/e2e/report/out

# Linux / macOS
go test -tags e2e_ui -json ./tests/e2e/... | go run ./tests/e2e/report -artifacts ./tests/e2e/ui/artifacts -out ./tests/e2e/report/out
```

### Run API Layer Only
```bash
go test -v ./tests/e2e
```

### Run Browser UI Layer with Headless Chromium & Screenshots
```bash
# Windows PowerShell
$env:E2E_UI_SCREENSHOTS="1"; go test -tags e2e_ui -v ./tests/e2e/ui

# Linux / macOS
E2E_UI_SCREENSHOTS=1 go test -tags e2e_ui -v ./tests/e2e/ui
```

---

## 2. Directory Layout

```text
tests/e2e/
├── e2e_test.go           # Pure Go black-box HTTP scenario suite (Health, Auth, Catalog, Sessions, Failovers)
├── ui/                   # Playwright browser UI suite (Headless Chromium)
│   ├── checkout_test.go  # Browser tests (Picker, QRIS, Virtual Account, Timer, Polling)
│   ├── harness_test.go   # Playwright test server & snapshot harness
│   └── artifacts/        # Captured PNG step screenshots
├── report/               # Report Compiler
│   ├── main.go           # Consumes `go test -json` and generates HTML/XML reports
│   └── out/              # Output directory
│       ├── e2e-report.html # Human-readable dashboard with embedded screenshot gallery
│       └── e2e-report.xml  # Standard JUnit XML format for CI/CD pipelines
└── legacy_bash/          # Archived legacy bash/curl scripts (quarantined)
```

---

## 3. Report Output

The generated report in `tests/e2e/report/out/e2e-report.html` includes:
* **Executive Summary**: Pass / Fail / Skip counters and total runtime.
* **API Layer Matrix**: Detailed execution times for all 9 core API scenarios.
* **UI Layer & Screenshot Gallery**: High-resolution browser screenshots showing real checkout screens, QR codes, bank logos, and timer states.
