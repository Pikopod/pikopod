package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pikopod/pikopod/internal/config"
	"github.com/pikopod/pikopod/internal/proxy"
	"github.com/spf13/cobra"
)

func cliDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	yaml := "listen: 127.0.0.1\ndata_dir: " + filepath.Join(dir, "data") + "\nupstreams:\n  widgets:\n    target: https://api.example.invalid\n"
	if err := os.WriteFile(filepath.Join(dir, "pikopod.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	return dir
}

func runCLI(t *testing.T, c *cobra.Command, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs(args)
	err := c.Execute()
	return out.String(), err
}

func writeCLIRecordings(t *testing.T, dir string, recs []proxy.Record) {
	t.Helper()
	rdir := filepath.Join(dir, "data", "recordings")
	os.MkdirAll(rdir, 0o700)
	f, err := os.Create(filepath.Join(rdir, "widgets.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	enc := json.NewEncoder(f)
	for _, r := range recs {
		enc.Encode(r)
	}
	f.Close()
}

func cliRecord(method, path string, status int, body map[string]any) proxy.Record {
	return proxy.Record{TS: time.Now(), Upstream: "widgets", Method: method, Path: path,
		Status: status, RespKind: "json", RespBody: body}
}

func TestSandboxUpdateRefusesUnknownName(t *testing.T) {
	cliDir(t)
	_, err := runCLI(t, newImportCmd(), "ghost", "--update")
	if err == nil || !strings.Contains(err.Error(), "no such sandbox") {
		t.Fatalf("unknown sandbox must refuse: %v", err)
	}
}

func TestImportUpdateRefreshesPin(t *testing.T) {

	spec, err := filepath.Abs("../../testdata/parity/sandbox/widgets.spec.json")
	if err != nil {
		t.Fatal(err)
	}
	cliDir(t)
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	if err := sandboxAdd(cfg, "widgets", spec, "s1", "", "", false, io.Discard); err != nil {
		t.Fatalf("add: %v", err)
	}
	out, err := runCLI(t, newImportCmd(), "widgets", "--update")
	if err != nil {
		t.Fatalf("update with recorded source: %v", err)
	}
	if !strings.Contains(out, "no declared changes") || !strings.Contains(out, "pin refreshed") {
		t.Fatalf("update output: %s", out)
	}
}

func TestVolatileSuggestRefusesWithoutRecordings(t *testing.T) {
	cliDir(t)
	_, err := runCLI(t, newVolatileCmd(), "suggest", "widgets")
	if err == nil || !strings.Contains(err.Error(), "no recordings") {
		t.Fatalf("no recordings must refuse: %v", err)
	}
}

func TestVolatileSuggestFindsChurn(t *testing.T) {
	dir := cliDir(t)
	var recs []proxy.Record
	for i := 0; i < 20; i++ {
		recs = append(recs, cliRecord("GET", "/tx/1", 200, map[string]any{
			"session_ref": "ref_" + strings.Repeat("x", i+1),
			"currency":    "NGN",
		}))
	}
	writeCLIRecordings(t, dir, recs)
	out, err := runCLI(t, newVolatileCmd(), "suggest", "widgets")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "session_ref") || !strings.Contains(out, "suggestion") {
		t.Fatalf("churn must be suggested: %s", out)
	}
}

func TestFromRecordingsRefusesWithoutRecordings(t *testing.T) {
	cliDir(t)
	_, err := runCLI(t, newFromRecordingsCmd(), "widgets")
	if err == nil || !strings.Contains(err.Error(), "no recordings") {
		t.Fatalf("%v", err)
	}
}

func TestFromRecordingsGeneratesAndSavesPack(t *testing.T) {
	dir := cliDir(t)
	writeCLIRecordings(t, dir, []proxy.Record{
		cliRecord("POST", "/charges", 201, map[string]any{"id": "ch_a1b2c3d4e5"}),
		cliRecord("GET", "/charges/ch_a1b2c3d4e5", 200, map[string]any{"id": "ch_a1b2c3d4e5"}),
	})
	out, err := runCLI(t, newFromRecordingsCmd(), "widgets")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "generated traffic-widgets") {
		t.Fatalf("output: %s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "data", "scenarios", "traffic-widgets.yaml")); err != nil {
		t.Fatalf("pack not saved: %v", err)
	}
}

func TestFromDriftRefusesUnknownFingerprint(t *testing.T) {
	cliDir(t)
	_, err := runCLI(t, newFromDriftCmd(), "fp_nonexistent99")
	if err == nil || !strings.Contains(err.Error(), "drift events") {
		t.Fatalf("%v", err)
	}
}

func TestWhyRefusesUnknownSandbox(t *testing.T) {
	cliDir(t)
	_, err := runCLI(t, newWhyCmd(), "ghost", "GET", "/x")
	if err == nil {
		t.Fatal("unknown sandbox must refuse")
	}
}

func TestAcceptRefusesWhenAgentDown(t *testing.T) {
	cliDir(t)
	_, err := runCLI(t, newAcceptCmd(), "fp_x")
	if err == nil || !strings.Contains(err.Error(), "agent is not running") {
		t.Fatalf("%v", err)
	}
}

func TestSpecUpdateRefusesWithoutSandbox(t *testing.T) {
	cliDir(t)
	_, err := runCLI(t, newSpecUpdateCmd(), "widgets")
	if err == nil || !strings.Contains(err.Error(), "no sandbox linked") {
		t.Fatalf("%v", err)
	}
}

func TestScenarioRunRefusesUnknownSandbox(t *testing.T) {
	cliDir(t)
	c := newScenarioCmd()
	_, err := runCLI(t, c, "run", "ghost", "declines")
	if err == nil {
		t.Fatal("unknown sandbox must refuse")
	}
}
