// Command e2e-report turns the outputs of the e2e layers into a JUnit XML
// file (for CI) and a self-contained HTML file (for humans).
//
// Inputs:
//
//	-json <file>       Unified `go test -json` output (covering both API and UI layers).
//	-api <file>        Legacy JSONL emitted by bash scenarios (optional).
//	-ui  <file>        `go test -json` output from the Playwright UI layer (optional).
//	-artifacts <dir>   UI screenshots directory (linked/embedded in the HTML).
//	-out  <dir>        Where to write e2e-report.xml and e2e-report.html.
//
// If no input files are specified via flags, it reads `go test -json` directly from stdin.
package main

import (
	"bufio"
	"encoding/json"
	"encoding/xml"
	"flag"
	"fmt"
	"html/template"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// status values.
const (
	statusPass = "pass"
	statusFail = "fail"
	statusSkip = "skip"
)

// testCase is the unified result shape for both layers.
type testCase struct {
	Layer       string        // "API" or "UI"
	Suite       string        // scenario group / package
	Name        string        // scenario or test name
	Status      string        // pass | fail | skip
	Duration    time.Duration // rounded to ms for display
	Error       string        // failure detail (assertion message / test output)
	Screenshots []string      // relative paths (UI only)
}

// goTestEvent is the standard subset of `go test -json`.
type goTestEvent struct {
	Action  string  `json:"Action"`  // run | pass | fail | skip | output | cont
	Package string  `json:"Package"` // e.g. github.com/.../tests/e2e/ui
	Test    string  `json:"Test"`    // top-level Test... name
	Elapsed float64 `json:"Elapsed"` // seconds, set on terminal actions
	Output  string  `json:"Output"`  // log/fatal text, on "output" actions
}

func main() {
	var (
		jsonFile     = flag.String("json", "", "unified go test -json output file")
		apiFile      = flag.String("api", "", "api.jsonl from legacy bash runner")
		uiFile       = flag.String("ui", "", "go test -json output from the UI layer")
		artifactsDir = flag.String("artifacts", "", "UI screenshots directory")
		outDir       = flag.String("out", filepath.Join(".", "out"), "output directory for the report files")
		title        = flag.String("title", "PayRouter E2E Verification Report", "report title")
		openReport   = flag.Bool("open", false, "attempt to open the HTML report in the default browser")
	)
	flag.Parse()

	var cases []testCase

	if *jsonFile != "" {
		f, err := os.Open(*jsonFile)
		if err == nil {
			defer f.Close()
			cases = append(cases, parseGoTestJSON(f)...)
		}
	} else if *apiFile != "" || *uiFile != "" {
		cases = append(cases, parseLegacyAPI(*apiFile)...)
		if *uiFile != "" {
			f, err := os.Open(*uiFile)
			if err == nil {
				defer f.Close()
				cases = append(cases, parseGoTestJSON(f)...)
			}
		}
	} else {
		// Read from stdin if piped
		stat, err := os.Stdin.Stat()
		if err == nil && (stat.Mode()&os.ModeCharDevice) == 0 {
			cases = append(cases, parseGoTestJSON(os.Stdin)...)
		}
	}

	// Default artifacts dir if not explicitly given
	if *artifactsDir == "" {
		candidate := filepath.Join("tests", "e2e", "ui", "artifacts")
		if _, err := os.Stat(candidate); err == nil {
			*artifactsDir = candidate
		} else {
			candidateRel := filepath.Join("..", "ui", "artifacts")
			if _, err := os.Stat(candidateRel); err == nil {
				*artifactsDir = candidateRel
			}
		}
	}

	attachScreenshots(cases, *artifactsDir)

	sort.SliceStable(cases, func(i, j int) bool {
		if cases[i].Layer != cases[j].Layer {
			return cases[i].Layer == "API" // API first
		}
		if cases[i].Suite != cases[j].Suite {
			return cases[i].Suite < cases[j].Suite
		}
		return cases[i].Name < cases[j].Name
	})

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fatalf("create out dir: %v", err)
	}
	xmlPath := filepath.Join(*outDir, "e2e-report.xml")
	htmlPath := filepath.Join(*outDir, "e2e-report.html")

	if err := writeJUnit(xmlPath, cases, *title); err != nil {
		fatalf("write JUnit: %v", err)
	}
	if err := writeHTML(htmlPath, cases, *title, *artifactsDir, *outDir); err != nil {
		fatalf("write HTML: %v", err)
	}

	// Stdout summary
	var pass, fail, skip int
	var total time.Duration
	for _, c := range cases {
		total += c.Duration
		switch c.Status {
		case statusPass:
			pass++
		case statusFail:
			fail++
		case statusSkip:
			skip++
		}
	}
	fmt.Printf("e2e report: %d pass, %d fail, %d skip (%s)\n", pass, fail, skip, total.Round(time.Millisecond))
	fmt.Printf("  HTML: %s\n  XML:  %s\n", htmlPath, xmlPath)

	if *openReport {
		openURL(htmlPath)
	}
	if fail > 0 {
		os.Exit(1)
	}
}

// --- parsing -----------------------------------------------------------------

func parseGoTestJSON(r io.Reader) []testCase {
	if r == nil {
		return nil
	}

	type acc struct {
		pkg    string
		suite  string
		output strings.Builder
	}
	accs := map[string]*acc{}

	var out []testCase
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 2<<20), 2<<20)
	for sc.Scan() {
		var ev goTestEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			continue
		}
		if ev.Test == "" {
			continue
		}
		a := accs[ev.Test]
		if a == nil {
			suite := suiteName(ev.Package, ev.Test)
			a = &acc{pkg: ev.Package, suite: suite}
			accs[ev.Test] = a
		}
		switch ev.Action {
		case "output":
			a.output.WriteString(ev.Output)
		case statusPass, statusFail, statusSkip:
			layer := "API"
			if strings.HasSuffix(ev.Package, "/ui") || strings.Contains(strings.ToLower(ev.Test), "ui") || strings.Contains(strings.ToLower(ev.Test), "picker") || strings.Contains(strings.ToLower(ev.Test), "qris") || strings.Contains(strings.ToLower(ev.Test), "virtualaccount") {
				layer = "UI"
			}
			out = append(out, testCase{
				Layer:    layer,
				Suite:    a.suite,
				Name:     ev.Test,
				Status:   ev.Action,
				Duration: time.Duration(ev.Elapsed * float64(time.Second)),
				Error:    strings.TrimSpace(a.output.String()),
			})
		}
	}
	return out
}

func parseLegacyAPI(path string) []testCase {
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var out []testCase
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r struct {
			Layer      string `json:"layer"`
			Suite      string `json:"suite"`
			Name       string `json:"name"`
			Status     string `json:"status"`
			DurationMs int64  `json:"duration_ms"`
			Error      string `json:"error"`
		}
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			continue
		}
		out = append(out, testCase{
			Layer: "API", Suite: defaultStr(r.Suite, "api"), Name: r.Name,
			Status: normalize(r.Status), Duration: time.Duration(r.DurationMs) * time.Millisecond,
			Error: r.Error,
		})
	}
	return out
}

func suiteName(pkg, test string) string {
	if strings.HasSuffix(pkg, "/ui") {
		return "ui"
	}
	if strings.HasPrefix(test, "TestE2E_") {
		return strings.TrimPrefix(test, "TestE2E_")
	}
	if strings.HasPrefix(test, "TestCheckout_") {
		return strings.TrimPrefix(test, "TestCheckout_")
	}
	if i := strings.LastIndex(pkg, "/"); i >= 0 {
		return pkg[i+1:]
	}
	return defaultStr(pkg, "e2e")
}

// --- artifacts ---------------------------------------------------------------

func attachScreenshots(cases []testCase, artifactsDir string) {
	if artifactsDir == "" {
		return
	}
	pngs := globRel(artifactsDir, ".png")
	for i, c := range cases {
		if c.Layer != "UI" {
			continue
		}
		target := sanitize(c.Name)
		for _, p := range pngs {
			base := sanitize(filepath.Base(p))
			if strings.Contains(base, target) {
				cases[i].Screenshots = append(cases[i].Screenshots, p)
			}
		}
	}
}

func globRel(dir, ext string) []string {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), ext) {
			rel, err := filepath.Rel(dir, filepath.Join(dir, e.Name()))
			if err == nil {
				out = append(out, rel)
			}
		}
	}
	sort.Strings(out)
	return out
}

func sanitize(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, "_", "")
	s = strings.ReplaceAll(s, "-", "")
	s = strings.ReplaceAll(s, "/", "")
	return strings.ReplaceAll(s, " ", "")
}

// --- JUnit XML ---------------------------------------------------------------

type junitTestSuites struct {
	XMLName  xml.Name     `xml:"testsuites"`
	Name     string       `xml:"name,attr"`
	Tests    int          `xml:"tests,attr"`
	Failures int          `xml:"failures,attr"`
	Time     float64      `xml:"time,attr"`
	Suites   []junitSuite `xml:"testsuite"`
}

type junitSuite struct {
	Name     string      `xml:"name,attr"`
	Tests    int         `xml:"tests,attr"`
	Failures int         `xml:"failures,attr"`
	Time     float64     `xml:"time,attr"`
	Cases    []junitCase `xml:"testcase"`
}

type junitCase struct {
	Classname string        `xml:"classname,attr"`
	Name      string        `xml:"name,attr"`
	Time      float64       `xml:"time,attr"`
	Failure   *junitFailure `xml:"failure,omitempty"`
	Skipped   *struct{}     `xml:"skipped,omitempty"`
}

type junitFailure struct {
	Message string `xml:"message,attr"`
	Text    string `xml:",chardata"`
}

func writeJUnit(path string, cases []testCase, title string) error {
	suites := buildSuites(cases)
	doc := junitTestSuites{Name: title}
	for _, c := range cases {
		doc.Tests++
		if c.Status == statusFail {
			doc.Failures++
		}
		doc.Time += c.Duration.Seconds()
	}
	doc.Suites = suites
	w, err := os.Create(path)
	if err != nil {
		return err
	}
	defer w.Close()
	w.WriteString(xml.Header)
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	return enc.Encode(doc)
}

func buildSuites(cases []testCase) []junitSuite {
	byLayer := map[string][]testCase{}
	for _, c := range cases {
		byLayer[c.Layer] = append(byLayer[c.Layer], c)
	}
	var suites []junitSuite
	for _, layer := range []string{"API", "UI"} {
		cs := byLayer[layer]
		if len(cs) == 0 {
			continue
		}
		js := junitSuite{Name: layer}
		for _, c := range cs {
			js.Tests++
			if c.Status == statusFail {
				js.Failures++
			}
			js.Time += c.Duration.Seconds()
			jc := junitCase{Classname: layer + "." + c.Suite, Name: c.Name, Time: c.Duration.Seconds()}
			switch c.Status {
			case statusFail:
				jc.Failure = &junitFailure{Message: firstLine(c.Error), Text: c.Error}
			case statusSkip:
				jc.Skipped = &struct{}{}
			}
			js.Cases = append(js.Cases, jc)
		}
		suites = append(suites, js)
	}
	return suites
}

// --- HTML --------------------------------------------------------------------

func writeHTML(path string, cases []testCase, title, artifactsDir, outDir string) error {
	relArtifacts, err := filepath.Rel(outDir, artifactsDir)
	if err != nil || artifactsDir == "" {
		relArtifacts = ""
	}

	type shotView struct {
		Name string
		URL  string
	}

	type rowView struct {
		Layer       string
		Suite       string
		Name        string
		Status      string
		StatusClass string
		Duration    string
		Error       string
		Shots       []shotView
	}

	var rows []rowView
	var pass, fail, skip int
	var total time.Duration
	for _, c := range cases {
		total += c.Duration
		switch c.Status {
		case statusPass:
			pass++
		case statusFail:
			fail++
		case statusSkip:
			skip++
		}
		var shots []shotView
		for _, s := range c.Screenshots {
			url := filepath.ToSlash(filepath.Join(relArtifacts, s))
			shots = append(shots, shotView{Name: s, URL: url})
		}
		errDetail := ""
		if c.Status == statusFail {
			errDetail = c.Error
		}
		rows = append(rows, rowView{
			Layer:       c.Layer,
			Suite:       c.Suite,
			Name:        c.Name,
			Status:      strings.ToUpper(c.Status),
			StatusClass: c.Status,
			Duration:    formatDuration(c.Duration),
			Error:       errDetail,
			Shots:       shots,
		})
	}

	data := struct {
		Title      string
		Pass       int
		Fail       int
		Skip       int
		TotalCount int
		Duration   string
		Generated  string
		Rows       []rowView
	}{
		Title:      title,
		Pass:       pass,
		Fail:       fail,
		Skip:       skip,
		TotalCount: len(cases),
		Duration:   formatDuration(total),
		Generated:  time.Now().Format("2006-01-02 15:04:05 MST"),
		Rows:       rows,
	}

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return reportTmpl.Execute(f, data)
}

func formatDuration(d time.Duration) string {
	if d < time.Millisecond {
		return fmt.Sprintf("%.2fms", float64(d.Microseconds())/1000.0)
	}
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.2fs", d.Seconds())
}

func normalize(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "pass", "passed", "ok", "success":
		return statusPass
	case "skip", "skipped":
		return statusSkip
	default:
		return statusFail
	}
}

func defaultStr(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func openURL(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "e2e-report: "+format+"\n", args...)
	os.Exit(2)
}

var reportTmpl = template.Must(template.New("report").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>{{.Title}}</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>
:root {
  --bg: #0b0f17;
  --card: #151b26;
  --card-subtle: #1c2433;
  --fg: #f1f5f9;
  --muted: #94a3b8;
  --line: #232d3f;
  --ok: #22c55e;
  --ok-bg: rgba(34, 197, 94, 0.12);
  --fail: #ef4444;
  --fail-bg: rgba(239, 68, 68, 0.12);
  --skip: #eab308;
  --accent: #3b82f6;
  --font-mono: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
}
* { box-sizing: border-box; margin: 0; padding: 0; }
body {
  background: var(--bg);
  color: var(--fg);
  font-family: system-ui, -apple-system, "Segoe UI", Roboto, sans-serif;
  line-height: 1.5;
  padding: 2rem 1rem;
}
.container { max-width: 1100px; margin: 0 auto; }
header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 2rem;
  padding-bottom: 1rem;
  border-bottom: 1px solid var(--line);
}
h1 { font-size: 1.5rem; font-weight: 700; letter-spacing: -0.02em; }
.meta { font-size: 0.8125rem; color: var(--muted); }
.stats-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(160px, 1fr));
  gap: 1rem;
  margin-bottom: 2rem;
}
.stat-card {
  background: var(--card);
  border: 1px solid var(--line);
  border-radius: 10px;
  padding: 1.25rem;
}
.stat-val { font-size: 2rem; font-weight: 800; line-height: 1; }
.stat-label { font-size: 0.75rem; text-transform: uppercase; color: var(--muted); margin-top: 0.5rem; font-weight: 600; letter-spacing: 0.05em; }
.stat-card.pass .stat-val { color: var(--ok); }
.stat-card.fail .stat-val { color: var(--fail); }
.stat-card.skip .stat-val { color: var(--skip); }

.table-wrap {
  background: var(--card);
  border: 1px solid var(--line);
  border-radius: 12px;
  overflow: hidden;
}
table { width: 100%; border-collapse: collapse; text-align: left; font-size: 0.875rem; }
th {
  background: var(--card-subtle);
  padding: 0.75rem 1rem;
  font-weight: 600;
  color: var(--muted);
  font-size: 0.75rem;
  text-transform: uppercase;
  letter-spacing: 0.05em;
  border-bottom: 1px solid var(--line);
}
td { padding: 0.875rem 1rem; border-bottom: 1px solid var(--line); vertical-align: top; }
tr:last-child td { border-bottom: none; }
tr:hover td { background: rgba(255,255,255,0.015); }

.badge {
  display: inline-block;
  padding: 0.2rem 0.5rem;
  border-radius: 6px;
  font-size: 0.75rem;
  font-weight: 700;
  letter-spacing: 0.03em;
}
.badge.pass { background: var(--ok-bg); color: var(--ok); }
.badge.fail { background: var(--fail-bg); color: var(--fail); }
.badge.skip { background: var(--skip); color: #000; }
.badge.layer { background: var(--card-subtle); color: var(--accent); border: 1px solid var(--line); }

.test-name { font-weight: 600; font-family: var(--font-mono); font-size: 0.8125rem; }
.test-suite { font-size: 0.75rem; color: var(--muted); }
.duration { font-family: var(--font-mono); font-size: 0.8125rem; color: var(--muted); }

.shots-grid {
  display: flex;
  flex-wrap: wrap;
  gap: 0.75rem;
  margin-top: 0.75rem;
}
.shot-link {
  display: inline-block;
  border: 1px solid var(--line);
  border-radius: 6px;
  overflow: hidden;
  transition: transform 0.15s ease, border-color 0.15s ease;
}
.shot-link:hover { transform: scale(1.03); border-color: var(--accent); }
.shot-thumb { width: 140px; height: 90px; object-fit: cover; display: block; background: #000; }
.shot-name { font-size: 0.65rem; color: var(--muted); padding: 0.25rem 0.4rem; background: var(--card-subtle); display: block; max-width: 140px; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }

.error-box {
  margin-top: 0.5rem;
  padding: 0.5rem 0.75rem;
  background: var(--fail-bg);
  border: 1px solid rgba(239,68,68,0.3);
  border-radius: 6px;
  font-family: var(--font-mono);
  font-size: 0.75rem;
  color: #fca5a5;
  white-space: pre-wrap;
  word-break: break-all;
}
</style>
</head>
<body>
<div class="container">
  <header>
    <div>
      <h1>{{.Title}}</h1>
      <div class="meta">Generated {{.Generated}} • Total Elapsed: {{.Duration}}</div>
    </div>
  </header>

  <div class="stats-grid">
    <div class="stat-card">
      <div class="stat-val">{{.TotalCount}}</div>
      <div class="stat-label">Total Tests</div>
    </div>
    <div class="stat-card pass">
      <div class="stat-val">{{.Pass}}</div>
      <div class="stat-label">Passed</div>
    </div>
    <div class="stat-card fail">
      <div class="stat-val">{{.Fail}}</div>
      <div class="stat-label">Failed</div>
    </div>
    <div class="stat-card skip">
      <div class="stat-val">{{.Skip}}</div>
      <div class="stat-label">Skipped</div>
    </div>
  </div>

  <div class="table-wrap">
    <table>
      <thead>
        <tr>
          <th>Layer</th>
          <th>Suite</th>
          <th>Test Name & Artifacts</th>
          <th>Status</th>
          <th>Duration</th>
        </tr>
      </thead>
      <tbody>
        {{range .Rows}}
        <tr>
          <td><span class="badge layer">{{.Layer}}</span></td>
          <td><span class="test-suite">{{.Suite}}</span></td>
          <td>
            <div class="test-name">{{.Name}}</div>
            {{if .Error}}<div class="error-box">{{.Error}}</div>{{end}}
            {{if .Shots}}
            <div class="shots-grid">
              {{range .Shots}}
              <a class="shot-link" href="{{.URL}}" target="_blank" title="{{.Name}}">
                <img class="shot-thumb" src="{{.URL}}" alt="{{.Name}}" loading="lazy">
                <span class="shot-name">{{.Name}}</span>
              </a>
              {{end}}
            </div>
            {{end}}
          </td>
          <td><span class="badge {{.StatusClass}}">{{.Status}}</span></td>
          <td><span class="duration">{{.Duration}}</span></td>
        </tr>
        {{end}}
      </tbody>
    </table>
  </div>
</div>
</body>
</html>`))
