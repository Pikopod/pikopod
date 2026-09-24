package main

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pikopod/pikopod/internal/contract"
	"github.com/pikopod/pikopod/internal/proxy"
)

func TestContractReport(t *testing.T) {
	cfg := testConfig(t, "https://example.invalid")

	spec := `{
	  "openapi": "3.0.0", "info": {"title": "W", "version": "1"},
	  "paths": {"/widgets/{id}": {"get": {
	    "parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "string"}}],
	    "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {
	      "type": "object", "properties": {"id": {"type": "string"}, "name": {"type": "string"}}
	    }}}}}
	  }}}
	}`
	specPath := filepath.Join(t.TempDir(), "widgets.json")
	if err := os.WriteFile(specPath, []byte(spec), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := sandboxAdd(cfg, "widgets", specPath, "contract-seed-1", "", "", false, io.Discard); err != nil {
		t.Fatalf("add: %v", err)
	}

	var empty strings.Builder
	if err := contractReport(cfg, "widgets", &empty); err != nil {
		t.Fatalf("report without overlay: %v", err)
	}
	if !strings.Contains(empty.String(), "spec contract only") {
		t.Fatalf("missing spec-only message: %q", empty.String())
	}

	_, def, err := loadSandboxDef(cfg, "widgets")
	if err != nil {
		t.Fatal(err)
	}
	r := contract.NewRefiner("widgets", cfg.DataDir, 10, 0)
	r.SetClock(func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) })
	for i := 0; i < 12; i++ {
		r.Observe(&proxy.Record{Method: "GET", Path: "/widgets/tok_x", Status: 200, RespKind: "json",
			RespBody: map[string]any{"id": "tok_x", "name": float64(7), "fee_bearer": "merchant"}})
	}
	if r.Admit(def, false) == 0 {
		t.Fatal("expected admissions")
	}
	if err := r.Persist(); err != nil {
		t.Fatal(err)
	}
	ov := r.Snapshot()

	packYAML := "name: drift-cafe01\nprovider: widgets\ncontractVersion: 1\ndefinition:\n  steps:\n    - key: context\n      type: NOTE\n      config:\n        text: pinned baseline\n"
	dir := filepath.Join(cfg.DataDir, "scenarios")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "drift-cafe01.yaml"), []byte(packYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	if err := contractReport(cfg, "widgets", &out); err != nil {
		t.Fatalf("report: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "overlay version: "+strconv.Itoa(ov.Version)) {
		t.Fatalf("missing live version: %q", got)
	}
	if !strings.Contains(got, "fee_bearer") || !strings.Contains(got, "presence 1.00") {
		t.Fatalf("OBSERVED field row missing: %q", got)
	}

	if !strings.Contains(got, "spec says string, traffic says number") || !strings.Contains(got, "winner: traffic") {
		t.Fatalf("contradiction row missing both sides: %q", got)
	}
	if ov.Version < 2 {
		t.Fatalf("test premise: expected ≥2 admissions, got version %d", ov.Version)
	}
	wantBehind := strconv.Itoa(ov.Version-1) + " version(s) behind"
	if !strings.Contains(got, "drift-cafe01") || !strings.Contains(got, "pinned at v1") || !strings.Contains(got, wantBehind) {
		t.Fatalf("pin staleness missing (want %q): %q", wantBehind, got)
	}
}
