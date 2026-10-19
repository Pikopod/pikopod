package main

import (
	"encoding/json"
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pikopod/pikopod/internal/alert"
	"github.com/pikopod/pikopod/internal/drift"
	"gopkg.in/yaml.v3"
)

type junitSuites struct {
	XMLName xml.Name     `xml:"testsuites"`
	Suites  []junitSuite `xml:"testsuite"`
}

type junitSuite struct {
	Name     string      `xml:"name,attr"`
	Tests    int         `xml:"tests,attr"`
	Failures int         `xml:"failures,attr"`
	Errors   int         `xml:"errors,attr"`
	Cases    []junitCase `xml:"testcase"`
}

type junitCase struct {
	Name    string `xml:"name,attr"`
	Class   string `xml:"classname,attr"`
	Failure *struct {
		Message string `xml:"message,attr"`
		Body    string `xml:",chardata"`
	} `xml:"failure"`
	Skipped *struct{} `xml:"skipped"`
}

func captureExit(t *testing.T) *int {
	t.Helper()
	code := 0
	was := exit
	exit = func(c int) { code = c }
	t.Cleanup(func() { exit = was })
	return &code
}

func writeWrongStatusPack(t *testing.T, dir string) string {
	t.Helper()
	pack := map[string]any{"name": "wrong-status", "provider": "widgets", "definition": map[string]any{"steps": []map[string]any{
		{"key": "create", "type": "REQUEST", "config": map[string]any{"method": "POST", "path": "/widgets", "body": map[string]any{"name": "gear"}},
			"assertions": []map[string]any{{"target": "response.status", "op": "equals", "expected": 201}}},
		{"key": "read-back", "type": "REQUEST", "config": map[string]any{"method": "GET", "path": "/widgets/widgets_1"},
			"assertions": []map[string]any{{"target": "response.status", "op": "equals", "expected": 503}}},
	}}}
	raw, _ := yaml.Marshal(pack)
	os.MkdirAll(filepath.Join(dir, "data", "scenarios"), 0o700)
	path := filepath.Join(dir, "data", "scenarios", "wrong-status.yaml")
	os.WriteFile(path, raw, 0o600)
	return path
}

func TestScenarioCheckJUnitNamesTheFailedStep(t *testing.T) {
	spec, _ := filepath.Abs(widgetsSpecPath)
	dir := cliDir(t)
	if _, err := runCLI(t, newImportCmd(), "widgets", "--spec", spec, "--seed", "junit-1"); err != nil {
		t.Fatalf("import: %v", err)
	}
	writeWrongStatusPack(t, dir)
	code := captureExit(t)
	out, err := runCLI(t, newScenarioCmd(), "check", "widgets", "wrong-status", "--format", "junit")
	if err != nil || *code != 1 {
		t.Fatalf("the run fails with exit 1: err=%v code=%d\n%s", err, *code, out)
	}
	var suites junitSuites
	if err := xml.Unmarshal([]byte(out), &suites); err != nil {
		t.Fatalf("junit output must parse: %v\n%s", err, out)
	}
	if len(suites.Suites) != 1 || suites.Suites[0].Name != "wrong-status" || suites.Suites[0].Tests != 2 || suites.Suites[0].Failures != 1 {
		t.Fatalf("one suite per scenario, one case per step: %+v", suites.Suites)
	}
	var failed *junitCase
	for i := range suites.Suites[0].Cases {
		if suites.Suites[0].Cases[i].Failure != nil {
			failed = &suites.Suites[0].Cases[i]
		}
	}
	if failed == nil || failed.Name != "read-back" || !strings.Contains(failed.Failure.Message, "expected response.status equals 503, got 200") {
		t.Fatalf("the failed case is the step that failed, with the summary as the message: %+v", suites.Suites[0].Cases)
	}
	if strings.Contains(out, "✗") || strings.Contains(out, "FAILED         read-back") {
		t.Fatalf("the artifact carries nothing but the report:\n%s", out)
	}
}

func TestScenarioCheckGitHubActionsAnnotatesThePackLine(t *testing.T) {
	spec, _ := filepath.Abs(widgetsSpecPath)
	dir := cliDir(t)
	if _, err := runCLI(t, newImportCmd(), "widgets", "--spec", spec, "--seed", "gha-1"); err != nil {
		t.Fatalf("import: %v", err)
	}
	path := writeWrongStatusPack(t, dir)
	raw, _ := os.ReadFile(path)
	line := 0
	for i, l := range strings.Split(string(raw), "\n") {
		if strings.Contains(l, "key: read-back") {
			line = i + 1
		}
	}
	captureExit(t)
	out, _ := runCLI(t, newScenarioCmd(), "check", "widgets", "wrong-status", "--format", "githubactions")
	want := "::error file=" + path + ",line=" + itoa(line) + ",title=wrong-status / read-back::expected response.status equals 503, got 200"
	if !strings.Contains(out, want) {
		t.Fatalf("missing %q in:\n%s", want, out)
	}
	if strings.Count(out, "::error") != 1 {
		t.Fatalf("one annotation per failed step:\n%s", out)
	}
}

func itoa(n int) string {
	raw, _ := json.Marshal(n)
	return string(raw)
}

func TestScenarioCheckSaysWhichOperationsWereExercised(t *testing.T) {
	spec, _ := filepath.Abs("../../docs/demo/examplepay.spec.json")
	cliDir(t)
	if _, err := runCLI(t, newImportCmd(), "examplepay", "--spec", spec, "--seed", "ex-1"); err != nil {
		t.Fatalf("import: %v", err)
	}
	out, err := runCLI(t, newScenarioCmd(), "check", "examplepay", "declines")
	if err != nil {
		t.Fatalf("check: %v\n%s", err, out)
	}
	if !strings.Contains(out, "exercised 1 of 4 declared operations (unexercised: GET /charges, GET /charges/{id}, PATCH /charges/{id})") {
		t.Fatalf("the run ends with the exercised line:\n%s", out)
	}
	out, err = runCLI(t, newScenarioCmd(), "check", "examplepay", "declines", "--format", "json")
	if err != nil {
		t.Fatalf("check json: %v\n%s", err, out)
	}
	var doc struct {
		Runs        []map[string]any `json:"runs"`
		Exercised   []string         `json:"exercised"`
		Unexercised []string         `json:"unexercised"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("json output must parse: %v\n%s", err, out)
	}
	if len(doc.Runs) != 1 || len(doc.Exercised) != 1 || doc.Exercised[0] != "POST /charges" || len(doc.Unexercised) != 3 {
		t.Fatalf("json carries exercised and unexercised: %+v", doc)
	}
}

func TestAgentReportEndsWithTheOutcomeLine(t *testing.T) {
	dir := cliDir(t)
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	events := []alert.DriftEvent{
		{SchemaVersion: alert.SchemaVersion, Fingerprint: "fp_aaaaaaaaaaaa", Upstream: "widgets", Method: "POST", Endpoint: "/widgets", StatusClass: "5xx", Kind: drift.UpstreamError, After: "503", Level: "ERR", FirstSeen: now.Add(-72 * time.Hour), LastSeen: now, Occurrences: 4},
		{SchemaVersion: alert.SchemaVersion, Fingerprint: "fp_bbbbbbbbbbbb", Upstream: "widgets", Method: "GET", Endpoint: "/widgets/{id}", StatusClass: "5xx", Kind: drift.UpstreamUnreachable, After: "502", Level: "ERR", FirstSeen: now.Add(-48 * time.Hour), LastSeen: now.Add(-48 * time.Hour), Occurrences: 1},
		{SchemaVersion: alert.SchemaVersion, Fingerprint: "fp_cccccccccccc", Upstream: "widgets", Method: "POST", Endpoint: "/widgets", StatusClass: "4xx", Kind: drift.RateLimited, After: "429", Level: "WARN", FirstSeen: now.Add(-24 * time.Hour), LastSeen: now.Add(-24 * time.Hour), Occurrences: 2},
		{SchemaVersion: alert.SchemaVersion, Fingerprint: "fp_dddddddddddd", Upstream: "widgets", Method: "GET", Endpoint: "/widgets", StatusClass: "2xx", Kind: drift.FieldAdded, Field: "colour", Level: "INFO", FirstSeen: now, LastSeen: now, Occurrences: 1},
	}
	os.MkdirAll(filepath.Join(dir, "data"), 0o700)
	f, err := os.Create(filepath.Join(dir, "data", "events.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		json.NewEncoder(f).Encode(ev)
	}
	f.Close()
	os.MkdirAll(filepath.Join(dir, "data", "scenarios"), 0o700)
	os.MkdirAll(filepath.Join(dir, "scenarios"), 0o700)
	for _, name := range []string{"incident-aaaaaaaaaaaa", "incident-bbbbbbbbbbbb"} {
		os.WriteFile(filepath.Join(dir, "data", "scenarios", name+".yaml"), []byte("name: "+name+"\n"), 0o600)
	}
	committed := filepath.Join(dir, "scenarios", "incident-aaaaaaaaaaaa.yaml")
	os.WriteFile(committed, []byte("name: incident-aaaaaaaaaaaa\n"), 0o600)
	os.Chtimes(committed, now.Add(-time.Hour), now.Add(-time.Hour))
	out, err := runCLI(t, newReportCmd())
	if err != nil {
		t.Fatalf("report: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	last := lines[len(lines)-1]
	if last != "3 provider failures recorded · 2 reproduced locally · 1 committed as scenarios · 1 recurred after being committed" {
		t.Fatalf("the report ends with the outcome line, got %q in:\n%s", last, out)
	}
}
