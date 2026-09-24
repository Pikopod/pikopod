package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pikopod/pikopod/internal/alert"
	"github.com/pikopod/pikopod/internal/drift"
)

func writeFixEvent(t *testing.T, dir string, ev alert.DriftEvent) {
	t.Helper()
	os.MkdirAll(filepath.Join(dir, "data"), 0o700)
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "data", "events.ndjson"), append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func fixEvent() alert.DriftEvent {
	return alert.DriftEvent{
		SchemaVersion: "1", Fingerprint: "fp_fixtest01", Upstream: "widgets",
		Method: "GET", Endpoint: "/tx/{id}", StatusClass: "2xx",
		Kind: drift.FieldRemoved, Field: "data/account_number",
		Before: "string", FirstSeen: time.Now(), LastSeen: time.Now(), Occurrences: 5,
	}
}

func fakeOpenRouter(t *testing.T, proposal string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{"choices": []any{
			map[string]any{"message": map[string]any{"content": proposal}},
		}}
		json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func writeFixApp(t *testing.T) string {
	t.Helper()
	app := t.TempDir()
	src := "package pay\n\nfunc Read(m map[string]any) any {\n\treturn m[\"account_number\"]\n}\n"
	if err := os.WriteFile(filepath.Join(app, "client.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return app
}

func TestFixRefusesUnknownFingerprint(t *testing.T) {
	cliDir(t)
	_, err := runCLI(t, newFixCmd(), "fp_ghost")
	if err == nil || !strings.Contains(err.Error(), "drift events") {
		t.Fatalf("%v", err)
	}
}

func TestFixNoImpactsIsUnverifiableNotClean(t *testing.T) {
	dir := cliDir(t)
	writeFixEvent(t, dir, fixEvent())
	empty := t.TempDir()
	_, err := runCLI(t, newFixCmd(), "fp_fixtest01", "--dir", empty)
	if err == nil {
		t.Fatal("an empty scan exited clean — CI would read a silent miss as a pass")
	}
	if !strings.Contains(err.Error(), "UNVERIFIABLE") {
		t.Fatalf("the refusal must name itself UNVERIFIABLE, got: %v", err)
	}

	if !strings.Contains(err.Error(), "accountNumber") {
		t.Fatalf("the refusal must explain the renaming blind spot, got: %v", err)
	}
}

func TestFixCamelCaseClientIsNotReportedClean(t *testing.T) {
	dir := cliDir(t)
	writeFixEvent(t, dir, fixEvent())
	app := t.TempDir()
	ts := "export function read(r: any) { return r.accountNumber }\n"
	if err := os.WriteFile(filepath.Join(app, "client.ts"), []byte(ts), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := runCLI(t, newFixCmd(), "fp_fixtest01", "--dir", app)
	if err == nil {
		t.Fatal("a TypeScript client reading the drifted field was reported as no-impact, cleanly")
	}
	if !strings.Contains(err.Error(), "UNVERIFIABLE") {
		t.Fatalf("got: %v", err)
	}
}

func TestFixRefusesWithoutKeyAfterScan(t *testing.T) {
	dir := cliDir(t)
	writeFixEvent(t, dir, fixEvent())
	app := writeFixApp(t)
	t.Setenv("PIKOPOD_OPENROUTER_KEY", "")
	t.Setenv("OPENROUTER_API_KEY", "")
	out, err := runCLI(t, newFixCmd(), "fp_fixtest01", "--dir", app)
	if err == nil || !strings.Contains(err.Error(), "LLM key") {
		t.Fatalf("%v", err)
	}

	if !strings.Contains(out, "client.go") {
		t.Fatalf("impact scan output missing: %s", out)
	}
}

func TestFixAppliesEditAndPassesCheck(t *testing.T) {
	dir := cliDir(t)
	writeFixEvent(t, dir, fixEvent())
	app := writeFixApp(t)
	proposal := `{"edits":[{"file":"client.go","find":"m[\"account_number\"]","replace":"m[\"accountNumber\"]"}],"summary":"provider renamed account_number"}`
	srv := fakeOpenRouter(t, proposal)
	t.Setenv("PIKOPOD_OPENROUTER_KEY", "test-key")
	t.Setenv("PIKOPOD_OPENROUTER_BASE", srv.URL)

	out, err := runCLI(t, newFixCmd(), "fp_fixtest01", "--dir", app, "--check", "true")
	if err != nil {
		t.Fatalf("fix: %v\n%s", err, out)
	}
	if !strings.Contains(out, "applied 1 edit") || !strings.Contains(out, "check passed") {
		t.Fatalf("output: %s", out)
	}
	after, _ := os.ReadFile(filepath.Join(app, "client.go"))
	if !strings.Contains(string(after), `m["accountNumber"]`) {
		t.Fatalf("edit not applied: %s", after)
	}
}

func TestFixFailingCheckRevertsEverything(t *testing.T) {
	dir := cliDir(t)
	writeFixEvent(t, dir, fixEvent())
	app := writeFixApp(t)
	before, _ := os.ReadFile(filepath.Join(app, "client.go"))
	proposal := `{"edits":[{"file":"client.go","find":"m[\"account_number\"]","replace":"m[\"broken\"]"}],"summary":"bad fix"}`
	srv := fakeOpenRouter(t, proposal)
	t.Setenv("PIKOPOD_OPENROUTER_KEY", "test-key")
	t.Setenv("PIKOPOD_OPENROUTER_BASE", srv.URL)

	_, err := runCLI(t, newFixCmd(), "fp_fixtest01", "--dir", app, "--check", "false")
	if err == nil || !strings.Contains(err.Error(), "reverted") {
		t.Fatalf("failing check must revert: %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(app, "client.go"))
	if string(after) != string(before) {
		t.Fatalf("file not reverted:\n%s", after)
	}
}

func TestFixDryRunWritesNothing(t *testing.T) {
	dir := cliDir(t)
	writeFixEvent(t, dir, fixEvent())
	app := writeFixApp(t)
	before, _ := os.ReadFile(filepath.Join(app, "client.go"))
	proposal := `{"edits":[{"file":"client.go","find":"m[\"account_number\"]","replace":"m[\"accountNumber\"]"}],"summary":"rename"}`
	srv := fakeOpenRouter(t, proposal)
	t.Setenv("PIKOPOD_OPENROUTER_KEY", "test-key")
	t.Setenv("PIKOPOD_OPENROUTER_BASE", srv.URL)

	out, err := runCLI(t, newFixCmd(), "fp_fixtest01", "--dir", app, "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "dry run") || !strings.Contains(out, "accountNumber") {
		t.Fatalf("output: %s", out)
	}
	after, _ := os.ReadFile(filepath.Join(app, "client.go"))
	if string(after) != string(before) {
		t.Fatal("dry run must not write")
	}
}

func TestFixRefusesEditOutsideImpactSet(t *testing.T) {
	dir := cliDir(t)
	writeFixEvent(t, dir, fixEvent())
	app := writeFixApp(t)
	proposal := `{"edits":[{"file":"../evil.go","find":"a","replace":"b"}],"summary":"escape"}`
	srv := fakeOpenRouter(t, proposal)
	t.Setenv("PIKOPOD_OPENROUTER_KEY", "test-key")
	t.Setenv("PIKOPOD_OPENROUTER_BASE", srv.URL)

	_, err := runCLI(t, newFixCmd(), "fp_fixtest01", "--dir", app)
	if err == nil || !strings.Contains(err.Error(), "impact set") {
		t.Fatalf("out-of-set edit must be refused: %v", err)
	}
}
