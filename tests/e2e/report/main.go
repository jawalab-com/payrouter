// Command e2e-report turns the outputs of the two e2e layers into a JUnit XML
// file (for CI) and a self-contained HTML file (for humans). It is invoked by
// tests/e2e/run-all.sh.
//
// Inputs:
//
//	-api <file>        JSONL emitted by lib/report.sh (one {layer,suite,name,status,duration_ms,error} per line).
//	-ui  <file>        Raw `go test -json` output from the Playwright UI layer.
//	-artifacts <dir>   UI screenshots/video dir (linked from the HTML when present).
//	-out  <dir>        Where to write e2e-report.xml and e2e-report.html.
//
// Either -api or -ui may be omitted; the report then covers whichever layers have
// input. Missing files are treated as "that layer did not run".
package main

import (
	"bufio"
	"encoding/json"
	"encoding/xml"
	"flag"
	"fmt"
	"html/template"
	"os"
	"os/exec"
	"path/filepath"
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

// goTestEvent is the subset of `go test -json` we consume.
type goTestEvent struct {
	Action  string  `json:"Action"`  // run | pass | fail | skip | output | cont
	Package string  `json:"Package"` // e.g. github.com/.../tests/e2e/ui
	Test    string  `json:"Test"`    // top-level Test... name
	Elapsed float64 `json:"Elapsed"` // seconds, set on terminal actions
	Output  string  `json:"Output"`  // log/fatal text, on "output" actions
}

func main() {
	var (
		apiFile      = flag.String("api", "", "api.jsonl from lib/report.sh")
		uiFile       = flag.String("ui", "", "go test -json output from the UI layer")
		artifactsDir = flag.String("artifacts", "", "UI screenshots/video directory")
		outDir       = flag.String("out", ".", "output directory for the report files")
		title        = flag.String("title", "PayRouter e2e", "report title")
		openReport   = flag.Bool("open", false, "attempt to open the HTML report in the default browser")
	)
	flag.Parse()

	var cases []testCase
	cases = append(cases, parseAPI(*apiFile)...)
	cases = append(cases, parseUI(*uiFile)...)

	videos := collectVideos(*artifactsDir)
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
	if err := writeHTML(htmlPath, cases, videos, *title, *artifactsDir, *outDir); err != nil {
		fatalf("write HTML: %v", err)
	}

	// Stdout summary so CI logs show the headline without opening the file.
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
		os.Exit(1) // CI: a failing layer should fail the reporting step too.
	}
}

// --- parsing -----------------------------------------------------------------

func parseAPI(path string) []testCase {
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil // missing file => layer did not run
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
			continue // skip malformed lines rather than aborting the report
		}
		out = append(out, testCase{
			Layer: "API", Suite: defaultStr(r.Suite, "api"), Name: r.Name,
			Status: normalize(r.Status), Duration: time.Duration(r.DurationMs) * time.Millisecond,
			Error: r.Error,
		})
	}
	return out
}

func parseUI(path string) []testCase {
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	// Accumulate output per test so a failure carries its assertion text.
	type acc struct {
		suite  string
		output strings.Builder
	}
	accs := map[string]*acc{}

	var out []testCase
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var ev goTestEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			continue
		}
		if ev.Test == "" {
			continue // package-level event, not an individual test
		}
		a := accs[ev.Test]
		if a == nil {
			a = &acc{suite: uiSuite(ev.Package)}
			accs[ev.Test] = a
		}
		switch ev.Action {
		case "output":
			a.output.WriteString(ev.Output)
		case statusPass, statusFail, statusSkip:
			out = append(out, testCase{
				Layer: "UI", Suite: a.suite, Name: ev.Test, Status: ev.Action,
				Duration: time.Duration(ev.Elapsed * float64(time.Second)),
				Error:    strings.TrimSpace(a.output.String()),
			})
		}
	}
	return out
}

// uiSuite shortens a Go package path to its last segment for display.
func uiSuite(pkg string) string {
	if pkg == "" {
		return "ui"
	}
	if i := strings.LastIndex(pkg, "/"); i >= 0 {
		return pkg[i+1:]
	}
	return pkg
}

// --- artifacts ---------------------------------------------------------------

// collectVideos returns relative .webm paths in the artifacts dir, newest first.
func collectVideos(artifactsDir string) []string {
	return globRel(artifactsDir, ".webm")
}

// attachScreenshots links each UI test to its step screenshots, matched by the
// sanitized test name prefix the test binary writes (see snap() in the UI tests).
func attachScreenshots(cases []testCase, artifactsDir string) {
	pngs := globRel(artifactsDir, ".png")
	for i, c := range cases {
		if c.Layer != "UI" {
			continue
		}
		prefix := sanitize(c.Name)
		for _, p := range pngs {
			if strings.HasPrefix(filepath.Base(p), prefix) {
				cases[i].Screenshots = append(cases[i].Screenshots, p)
			}
		}
	}
}

// globRel lists files ending in ext under dir as paths relative to dir.
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

// sanitize mirrors the UI test binary's file-naming: lowercase, / and space → _.
func sanitize(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, "/", "_")
	return strings.ReplaceAll(s, " ", "_")
}

// --- JUnit XML ---------------------------------------------------------------

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

func writeHTML(path string, cases []testCase, videos []string, title, artifactsDir, outDir string) error {
	relArtifacts, err := filepath.Rel(outDir, artifactsDir)
	if err != nil || artifactsDir == "" {
		relArtifacts = "" // no screenshot/video links when the dir is unknown
	}

	type rowView struct {
		Layer       string
		Suite       string
		Name        string
		Status      string
		StatusClass string
		Duration    string
		Error       string
		Shots       []string
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
		rv := rowView{
			Layer: c.Layer, Suite: c.Suite, Name: c.Name, Status: c.Status,
			StatusClass: c.Status, Duration: c.Duration.Round(time.Millisecond).String(),
			Error: c.Error,
		}
		for _, s := range c.Screenshots {
			rv.Shots = append(rv.Shots, filepath.ToSlash(filepath.Join(relArtifacts, s)))
		}
		rows = append(rows, rv)
	}

	vids := make([]string, len(videos))
	for i, v := range videos {
		vids[i] = filepath.ToSlash(filepath.Join(relArtifacts, v))
	}

	funcMap := template.FuncMap{"firstLine": firstLine}
	tmpl := template.Must(template.New("report").Funcs(funcMap).Parse(reportTmpl))

	w, err := os.Create(path)
	if err != nil {
		return err
	}
	defer w.Close()
	return tmpl.Execute(w, map[string]any{
		"Title": title, "Generated": time.Now().Format("2 Jan 2006 15:04:05 MST"),
		"Total": len(rows), "Pass": pass, "Fail": fail, "Skip": skip,
		"Duration": total.Round(time.Millisecond).String(),
		"Rows": rows, "Videos": vids, "HasArtifacts": relArtifacts != "",
	})
}

// --- helpers -----------------------------------------------------------------

func normalize(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "pass", "ok", "success":
		return statusPass
	case "fail", "error", "failed":
		return statusFail
	case "skip", "skipped", "ignored":
		return statusSkip
	default:
		return s
	}
}

func defaultStr(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "e2e-report: "+format+"\n", args...)
	os.Exit(2)
}

func openURL(path string) {
	abs, _ := filepath.Abs(path)
	// Best-effort, cross-platform. Errors are non-fatal.
	for _, cmd := range [][]string{
		{"cmd", "/c", "start", "", abs}, // Windows
		{"open", abs},                   // macOS
		{"xdg-open", abs},               // Linux
	} {
		if _, err := exec.LookPath(cmd[0]); err == nil {
			_ = exec.Command(cmd[0], cmd[1:]...).Run()
			return
		}
	}
}

// JUnit XML model -------------------------------------------------------------
type junitTestSuites struct {
	XMLName  xml.Name `xml:"testsuites"`
	Name     string   `xml:"name,attr"`
	Tests    int      `xml:"tests,attr"`
	Failures int      `xml:"failures,attr"`
	Time     float64  `xml:"time,attr"`
	Suites   []junitSuite `xml:"testsuite"`
}
type junitSuite struct {
	Name     string       `xml:"name,attr"`
	Tests    int          `xml:"tests,attr"`
	Failures int          `xml:"failures,attr"`
	Time     float64      `xml:"time,attr"`
	Cases    []junitCase  `xml:"testcase"`
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

// reportTmpl is the self-contained HTML report. html/template auto-escapes the
// dynamic values (error text, test names), so only the screenshot/video hrefs —
// which the generator builds from a controlled artifacts directory — are emitted
// as raw URLs via the template.URL-typed .Shots/.Videos fields below.
const reportTmpl = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<title>{{.Title}} — e2e report</title>
<style>
  :root { color-scheme: light dark; }
  body { font: 14px/1.5 -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
         margin: 0; background: #f6f7f9; color: #1b1f23; }
  header { background: #1f2937; color: #fff; padding: 20px 28px; }
  header h1 { margin: 0 0 4px; font-size: 20px; }
  header .meta { opacity: .8; font-size: 12px; }
  main { max-width: 1100px; margin: 0 auto; padding: 24px 16px 64px; }
  .cards { display: flex; gap: 12px; flex-wrap: wrap; margin-bottom: 24px; }
  .card { background: #fff; border: 1px solid #e3e6ea; border-radius: 8px;
          padding: 14px 18px; min-width: 110px; box-shadow: 0 1px 2px rgba(0,0,0,.04); }
  .card .n { font-size: 26px; font-weight: 700; }
  .card .l { font-size: 12px; text-transform: uppercase; letter-spacing: .04em; opacity: .65; }
  .card.pass .n { color: #1a7f37; }
  .card.fail .n { color: #cf222e; }
  .card.skip .n { color: #9a6700; }
  .layer { margin-bottom: 32px; }
  .layer h2 { font-size: 16px; border-bottom: 2px solid #e3e6ea; padding-bottom: 6px; }
  table { width: 100%; border-collapse: collapse; background: #fff;
          border: 1px solid #e3e6ea; border-radius: 8px; overflow: hidden; }
  th, td { text-align: left; padding: 9px 12px; border-bottom: 1px solid #eef0f2; vertical-align: top; }
  th { background: #f0f2f4; font-size: 12px; text-transform: uppercase; letter-spacing: .03em; }
  tr:last-child td { border-bottom: none; }
  .badge { display: inline-block; padding: 2px 8px; border-radius: 10px;
           font-size: 11px; font-weight: 600; text-transform: uppercase; }
  .badge.pass { background: #dcfce7; color: #166534; }
  .badge.fail { background: #fee2e2; color: #991b1b; }
  .badge.skip { background: #fef9c3; color: #854d0e; }
  .err { color: #991b1b; white-space: pre-wrap; }
  .shots { display: flex; flex-wrap: wrap; gap: 8px; margin-top: 6px; }
  .shots a img { width: 160px; border: 1px solid #e3e6ea; border-radius: 4px; display: block; }
  .shots .webm a { display: inline-block; padding: 4px 8px; background:#1f2937; color:#fff;
                   border-radius: 4px; text-decoration: none; font-size: 12px; }
</style>
</head>
<body>
<header>
  <h1>{{.Title}}</h1>
  <div class="meta">Generated {{.Generated}} · {{.Total}} tests · {{.Duration}}</div>
</header>
<main>
  <div class="cards">
    <div class="card pass"><div class="n">{{.Pass}}</div><div class="l">Passed</div></div>
    <div class="card fail"><div class="n">{{.Fail}}</div><div class="l">Failed</div></div>
    <div class="card skip"><div class="n">{{.Skip}}</div><div class="l">Skipped</div></div>
  </div>

  {{$layer := ""}}
  {{range .Rows}}
    {{if ne .Layer $layer}}
      {{if $layer}}</table></div>{{end}}
      <div class="layer">
      <h2>{{.Layer}} layer</h2>
      <table>
      <tr><th>Status</th><th>Suite</th><th>Test</th><th>Duration</th><th>Detail</th></tr>
      {{$layer = .Layer}}
    {{end}}
    <tr>
      <td><span class="badge {{.StatusClass}}">{{.Status}}</span></td>
      <td>{{.Suite}}</td>
      <td>{{.Name}}</td>
      <td>{{.Duration}}</td>
      <td>
        {{if .Error}}<div class="err">{{.Error}}</div>{{end}}
        {{if .Shots}}<div class="shots">
          {{range .Shots}}<a href="{{.}}"><img src="{{.}}" alt="screenshot"></a>{{end}}
        </div>{{end}}
      </td>
    </tr>
  {{end}}
  {{if $layer}}</table></div>{{end}}

  {{if and .HasArtifacts .Videos}}
  <div class="layer">
    <h2>UI recordings</h2>
    <div class="shots webm">
      {{range .Videos}}<a href="{{.}}">▶ {{.}}</a>{{end}}
    </div>
  </div>
  {{end}}
</main>
</body>
</html>`
