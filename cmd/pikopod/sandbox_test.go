package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pikopod/pikopod/internal/agent"
	"github.com/pikopod/pikopod/internal/alert"
	"github.com/pikopod/pikopod/internal/config"
)

const widgetsSpecPath = "../../testdata/parity/sandbox/widgets.spec.json"

func testConfig(t *testing.T, upstreamTarget string) *config.Config {
	t.Helper()
	dir := t.TempDir()
	yaml := "listen: 127.0.0.1\ndata_dir: " + filepath.Join(dir, "data") + "\nupstreams:\n  fake:\n    target: " + upstreamTarget + "\n"
	cfgPath := filepath.Join(dir, "pikopod.yaml")
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestSandboxPanicRecovery_AgentUnaffected(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := testConfig(t, upstream.URL)

	if err := sandboxAdd(cfg, "widgets", widgetsSpecPath, "panic-seed-1", "", "", false, io.Discard); err != nil {
		t.Fatalf("sandbox add: %v", err)
	}

	sbx, err := newSandboxServer(cfg)
	if err != nil {
		t.Fatalf("sandbox server: %v", err)
	}
	defer sbx.Close()

	sbx.handlers["boom"] = http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("injected sandbox fault")
	})

	sandboxSrv := httptest.NewServer(sbx)
	defer sandboxSrv.Close()

	a, err := agent.New(cfg, alert.Options{})
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	agentSrv := httptest.NewServer(a.Proxy)
	defer agentSrv.Close()

	res, err := http.Post(sandboxSrv.URL+"/widgets/widgets", "application/json", strings.NewReader(`{"name":"gear","size":3}`))
	if err != nil {
		t.Fatal(err)
	}
	created := readJSON(t, res)
	if res.StatusCode != 201 || created["id"] != "widgets_1" {
		t.Fatalf("create = %d %v, want 201 widgets_1", res.StatusCode, created)
	}

	res, err = http.Get(sandboxSrv.URL + "/boom/anything")
	if err != nil {
		t.Fatalf("panic must not kill the connection: %v", err)
	}
	body := readJSON(t, res)
	if res.StatusCode != 500 || body["message"] != "Internal Server Error" {
		t.Fatalf("panic answer = %d %v, want mirrored 500", res.StatusCode, body)
	}

	res, err = http.Get(agentSrv.URL + "/fake/ping")
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 200 {
		t.Fatalf("agent proxy = %d, want 200", res.StatusCode)
	}
	res.Body.Close()

	res, err = http.Get(sandboxSrv.URL + "/widgets/widgets/widgets_1")
	if err != nil {
		t.Fatal(err)
	}
	got := readJSON(t, res)
	if res.StatusCode != 200 || got["name"] != "gear" {
		t.Fatalf("post-panic read = %d %v, want the committed resource", res.StatusCode, got)
	}

	res, err = http.Get(sandboxSrv.URL + "/ghost/x")
	if err != nil {
		t.Fatal(err)
	}
	if body := readJSON(t, res); res.StatusCode != 404 || body["message"] != "Not Found" {
		t.Fatalf("unknown sandbox = %d %v, want mirrored 404", res.StatusCode, body)
	}
}

func TestSandboxRegistryCLI(t *testing.T) {
	cfg := testConfig(t, "https://example.invalid")

	if err := sandboxAdd(cfg, "widgets", widgetsSpecPath, "cli-seed-1", "", "", false, io.Discard); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := sandboxAdd(cfg, "widgets", widgetsSpecPath, "", "", "", false, io.Discard); err == nil {
		t.Fatal("duplicate add must fail")
	}

	var list strings.Builder
	if err := sandboxList(cfg, &list); err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(list.String(), "widgets") || !strings.Contains(list.String(), "sbx_") || !strings.Contains(list.String(), "seed=cli-seed-1") {
		t.Fatalf("list output missing fields: %q", list.String())
	}

	entries, err := loadRegistry(cfg.DataDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("registry: %v %v", entries, err)
	}
	if _, err := os.Stat(filepath.Join(cfg.DataDir, entries[0].IRFile)); err != nil {
		t.Fatalf("persisted IR missing: %v", err)
	}
	if entries[0].Mode != "deterministic" || !strings.HasPrefix(entries[0].ID, "sbx_") {
		t.Fatalf("entry not deterministic/sbx_: %+v", entries[0])
	}

	sbx, err := newSandboxServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(sbx)
	res, err := http.Post(srv.URL+"/widgets/widgets", "application/json", strings.NewReader(`{"name":"tmp"}`))
	if err != nil || res.StatusCode != 201 {
		t.Fatalf("create: %v %d", err, res.StatusCode)
	}
	res.Body.Close()
	srv.Close()
	if err := sbx.Close(); err != nil {
		t.Fatal(err)
	}

	if err := sandboxReset(cfg, "widgets", io.Discard); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if err := sandboxReset(cfg, "ghost", io.Discard); err == nil {
		t.Fatal("reset of unknown sandbox must fail")
	}

	sbx2, err := newSandboxServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer sbx2.Close()
	srv2 := httptest.NewServer(sbx2)
	defer srv2.Close()
	res, err = http.Get(srv2.URL + "/widgets/widgets")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var listBody []any
	if err := json.NewDecoder(res.Body).Decode(&listBody); err != nil || len(listBody) != 0 {
		t.Fatalf("after reset list = %v (err %v), want empty", listBody, err)
	}

	if err := sandboxAdd(cfg, "hooked", widgetsSpecPath, "cli-seed-2", "ftp://nope", "", false, io.Discard); err == nil {
		t.Fatal("non-http(s) webhook url must be refused")
	}
	if err := sandboxAdd(cfg, "hooked", widgetsSpecPath, "cli-seed-2", "http://127.0.0.1:9/hooks", "", false, io.Discard); err != nil {
		t.Fatalf("add with webhook url: %v", err)
	}
	entries, err = loadRegistry(cfg.DataDir)
	if err != nil {
		t.Fatalf("registry reload: %v", err)
	}
	hooked := findEntry(entries, "hooked")
	if hooked == nil || hooked.WebhookURL != "http://127.0.0.1:9/hooks" {
		t.Fatalf("webhook url not persisted: %+v", hooked)
	}
}

func readJSON(t *testing.T, res *http.Response) map[string]any {
	t.Helper()
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("non-JSON body %q: %v", raw, err)
		}
	}
	return m
}
