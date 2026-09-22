package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pikopod/pikopod/internal/alert"
	"github.com/pikopod/pikopod/internal/drift"
	"github.com/pikopod/pikopod/internal/proxy"
)

const bundleSecret = "xpay_secret_BUNDLECANARY000000000000"

func incidentDir(t *testing.T) string {
	t.Helper()
	dir := cliDir(t)
	ev := alert.DriftEvent{SchemaVersion: "1", Fingerprint: "fp_bundlecli01", Upstream: "widgets", Method: "GET", Endpoint: "/tx/{id}",
		StatusClass: "5xx", Kind: drift.UpstreamError, After: "503", FirstSeen: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), LastSeen: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), Occurrences: 4}
	writeFixEvent(t, dir, ev)
	writeCLIRecordings(t, dir, []proxy.Record{{TS: ev.LastSeen, Upstream: "widgets", Method: "GET", Path: "/tx/tx_ojausaumavlbxe", Status: 503, RespKind: "json", RespBody: map[string]any{"message": "down"}}})
	os.WriteFile(filepath.Join(dir, "data", ".salt"), []byte(bundleSecret), 0o600)
	os.WriteFile(filepath.Join(dir, "token"), []byte(bundleSecret), 0o600)
	os.WriteFile(filepath.Join(dir, "pikopod.yaml"), []byte("listen: 127.0.0.1\ndata_dir: "+filepath.Join(dir, "data")+"\ntoken_file: "+filepath.Join(dir, "token")+"\nretention:\n  max_age_hours: 48\nupstreams:\n  widgets:\n    target: https://api.example.invalid\n"), 0o600)
	return dir
}

func TestIncidentsExportWritesABundleWithoutSecrets(t *testing.T) {
	incidentDir(t)
	out, err := runCLI(t, newIncidentsCmd(), "export", "fp_bundlecli01")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	var b map[string]any
	if err := json.Unmarshal([]byte(out), &b); err != nil {
		t.Fatalf("bundle must be JSON: %v\n%s", err, out)
	}
	if b["schema_version"] != 1.0 || b["expires_at"] != "2026-01-04T00:00:00Z" || b["event"].(map[string]any)["fingerprint"] != "fp_bundlecli01" {
		t.Fatalf("bundle header: %s", out)
	}
	if strings.Contains(out, bundleSecret) {
		t.Fatalf("CANARY LEAK: the salt or token reached the bundle:\n%s", out)
	}
	rows, err := runCLI(t, newIncidentsCmd())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rows, "reproducible until 2026-01-04T00:00:00Z") || !strings.Contains(rows, "export: pikopod incidents export fp_bundlecli01") {
		t.Fatalf("rows must carry the deadline and the export hint:\n%s", rows)
	}
	arr, err := runCLI(t, newIncidentsCmd(), "export", "--since", "24h")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(strings.TrimSpace(arr), "[") {
		t.Fatalf("--since must emit an array:\n%s", arr)
	}
}

func TestReproduceFromBundleAgainstEmptyDataDir(t *testing.T) {
	dir := incidentDir(t)
	bundleOut, err := runCLI(t, newIncidentsCmd(), "export", "fp_bundlecli01")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI(t, newReproduceCmd(), "fp_bundlecli01"); err != nil {
		t.Fatalf("reproduce from data_dir: %v", err)
	}
	fromDataDir, err := os.ReadFile(filepath.Join(dir, "data", "scenarios", "incident-bundlecli01.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	empty := cliDir(t)
	bundle := filepath.Join(empty, "incident.json")
	os.WriteFile(bundle, []byte(bundleOut), 0o600)
	if _, err := os.Stat(filepath.Join(empty, "data", "events.ndjson")); !os.IsNotExist(err) {
		t.Fatal("the second data_dir must be empty")
	}
	out, err := runCLI(t, newReproduceCmd(), bundle)
	if err != nil {
		t.Fatalf("reproduce from a bundle must not need data_dir: %v\n%s", err, out)
	}
	fromBundle, err := os.ReadFile(filepath.Join(empty, "data", "scenarios", "incident-bundlecli01.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(fromBundle) != string(fromDataDir) {
		t.Fatalf("the bundle is lossy:\n--- data_dir\n%s\n--- bundle\n%s", fromDataDir, fromBundle)
	}
}

func TestFixReadsTheEventFromABundle(t *testing.T) {
	incidentDir(t)
	bundleOut, err := runCLI(t, newIncidentsCmd(), "export", "fp_bundlecli01")
	if err != nil {
		t.Fatal(err)
	}
	empty := cliDir(t)
	bundle := filepath.Join(empty, "incident.json")
	os.WriteFile(bundle, []byte(bundleOut), 0o600)
	_, err = runCLI(t, newFixCmd(), bundle, "--dir", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "UNVERIFIABLE") {
		t.Fatalf("fix must read the event from the bundle and keep its UNVERIFIABLE guarantee on an empty scan: %v", err)
	}
}
