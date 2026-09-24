package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/pikopod/pikopod/internal/config"
)

func callTool(t *testing.T, cfg *config.Config, name string, args any) (map[string]any, string) {
	t.Helper()
	argsRaw, _ := json.Marshal(args)
	line := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":%s}}`+"\n", name, argsRaw)
	var out bytes.Buffer
	if err := mcpServer(cfg).Serve(context.Background(), strings.NewReader(line), &out); err != nil {
		t.Fatal(err)
	}
	var res struct {
		Result struct {
			StructuredContent map[string]any `json:"structuredContent"`
			IsError           bool           `json:"isError"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("bad response: %v\n%s", err, out.String())
	}
	if res.Error != nil {
		t.Fatalf("protocol error for %s: %s", name, res.Error.Message)
	}
	return res.Result.StructuredContent, out.String()
}

func listTools(t *testing.T, cfg *config.Config) []string {
	t.Helper()
	var out bytes.Buffer
	if err := mcpServer(cfg).Serve(context.Background(), strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	var res struct {
		Result struct {
			Tools []struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, tl := range res.Result.Tools {
		if tl.Description == "" {
			t.Errorf("tool %s has no description", tl.Name)
		}
		names = append(names, tl.Name)
	}
	return names
}

func TestMCPToolListIsReadersAndFakeControlsOnly(t *testing.T) {
	names := listTools(t, testConfig(t, "https://example.invalid"))
	want := []string{"arm_fault", "clear_faults", "clear_mode", "conformance", "drift_events", "emit_webhook", "get_requests", "replay_ci", "reproduce", "scenario_list", "scenario_run", "set_mode", "spec_diff"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("tools:\n got %v\nwant %v", names, want)
	}
	for _, forbidden := range []string{"fix", "import", "chaos", "ack", "accept", "baseline_reset", "sandbox_reset", "up"} {
		for _, n := range names {
			if n == forbidden {
				t.Fatalf("%s must not be exposed over MCP", forbidden)
			}
		}
	}
}

func TestDriftEventsBeforeWarmupIsNotClean(t *testing.T) {
	cfg := testConfig(t, "https://example.invalid")
	res, wire := callTool(t, cfg, "drift_events", map[string]any{"upstream": "fake"})
	if res["verdict"] != VerdictUnverifiable || !strings.Contains(wire, `"verdict":"UNVERIFIABLE"`) {
		t.Fatalf("no baselines must be UNVERIFIABLE on the wire, got %v", res["verdict"])
	}
	if reason, _ := res["reason"].(string); !strings.Contains(reason, "50 samples") || !strings.Contains(reason, "48h") {
		t.Fatalf("the reason must state the gate: %q", reason)
	}
	warm, _ := res["warmup"].(map[string]any)
	if warm == nil || warm["no_baselines_yet"] != true {
		t.Fatalf("warmup block missing: %v", res)
	}

	baselines := filepath.Join(cfg.DataDir, "baselines")
	os.MkdirAll(baselines, 0o700)
	write := func(frozen bool) {
		fam := fmt.Sprintf(`{"POST /charges 2xx":{"method":"POST","template":"/charges","status_class":"2xx","samples":12,"frozen":%t,"fields":{}}}`, frozen)
		if err := os.WriteFile(filepath.Join(baselines, "fake.json"), []byte(fam), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(false)
	res, _ = callTool(t, cfg, "drift_events", map[string]any{"upstream": "fake"})
	if res["verdict"] != VerdictUnverifiable || !strings.Contains(res["reason"].(string), "warmup incomplete") {
		t.Fatalf("a family still warming must be UNVERIFIABLE: %v", res)
	}
	write(true)
	res, _ = callTool(t, cfg, "drift_events", map[string]any{"upstream": "fake"})
	if res["verdict"] != VerdictClean {
		t.Fatalf("a warmed-up family with no events is CLEAN: %v", res)
	}
	res, _ = callTool(t, cfg, "drift_events", map[string]any{"upstream": "ghost"})
	if res["verdict"] != VerdictError {
		t.Fatalf("an unknown upstream is ERROR: %v", res)
	}
}

func TestReplayCIWithoutRecordingsIsUnverifiable(t *testing.T) {
	cfg := testConfig(t, "https://example.invalid")
	res, _ := callTool(t, cfg, "replay_ci", map[string]any{})
	if res["verdict"] != VerdictUnverifiable || !strings.Contains(res["reason"].(string), "no baselines") {
		t.Fatalf("no baselines is not a pass: %v", res)
	}
}

func TestConformanceVerdicts(t *testing.T) {
	cfg := testConfig(t, "https://example.invalid")
	res, _ := callTool(t, cfg, "conformance", map[string]any{"upstream": "fake"})
	if res["verdict"] != VerdictError {
		t.Fatalf("no imported spec is ERROR: %v", res)
	}
	if err := sandboxAdd(cfg, "fake", widgetsSpecPath, "s", "", "", false, io.Discard); err != nil {
		t.Fatal(err)
	}
	res, _ = callTool(t, cfg, "conformance", map[string]any{"upstream": "fake"})
	if res["verdict"] != VerdictUnverifiable || !strings.Contains(res["reason"].(string), "no recordings") {
		t.Fatalf("no recordings is UNVERIFIABLE with a reason: %v", res)
	}
}

func TestSpecDiffVerdicts(t *testing.T) {
	cfg := testConfig(t, "https://example.invalid")
	dir := t.TempDir()
	base := `{"openapi":"3.1.0","info":{"title":"W","version":"1"},"paths":{"/widgets":{"get":{"responses":{"200":{"description":"ok"}}}},"/gadgets":{"get":{"responses":{"200":{"description":"ok"}}}}}}`
	trimmed := `{"openapi":"3.1.0","info":{"title":"W","version":"1"},"paths":{"/widgets":{"get":{"responses":{"200":{"description":"ok"}}}}}}`
	old, newer := filepath.Join(dir, "old.json"), filepath.Join(dir, "new.json")
	os.WriteFile(old, []byte(base), 0o600)
	os.WriteFile(newer, []byte(trimmed), 0o600)
	res, _ := callTool(t, cfg, "spec_diff", map[string]any{"old": old, "new": old})
	if res["verdict"] != VerdictClean {
		t.Fatalf("identical specs are CLEAN: %v", res)
	}
	res, _ = callTool(t, cfg, "spec_diff", map[string]any{"old": old, "new": newer})
	if res["verdict"] != VerdictFindings {
		t.Fatalf("a removed endpoint is FINDINGS: %v", res)
	}
	res, _ = callTool(t, cfg, "spec_diff", map[string]any{"old": old, "new": filepath.Join(dir, "missing.json")})
	if res["verdict"] != VerdictError || res["error"] == nil {
		t.Fatalf("a missing document is ERROR with what/why/fix: %v", res)
	}
}

func TestScenarioListAndRunOverMCP(t *testing.T) {
	cfg := testConfig(t, "https://example.invalid")
	if err := sandboxAdd(cfg, "widgets", widgetsSpecPath, "s", "", "", false, io.Discard); err != nil {
		t.Fatal(err)
	}
	res, _ := callTool(t, cfg, "scenario_list", map[string]any{"sandbox": "widgets"})
	if res["verdict"] != VerdictClean {
		t.Fatalf("widgets binds archetypes: %v", res)
	}
	arch, _ := res["data"].(map[string]any)["archetypes"].([]any)
	if len(arch) == 0 {
		t.Fatal("archetypes missing")
	}
	sawReason := false
	for _, a := range arch {
		m := a.(map[string]any)
		if m["applicable"] == false && m["reason"] != "" {
			sawReason = true
		}
	}
	if !sawReason {
		t.Fatal("a non-binding archetype must carry its reason")
	}
	res, _ = callTool(t, cfg, "scenario_run", map[string]any{"sandbox": "widgets", "names": []string{"declines"}})
	if res["verdict"] != VerdictClean {
		t.Fatalf("declines passes on widgets: %v", res)
	}
	res, _ = callTool(t, cfg, "scenario_run", map[string]any{"sandbox": "widgets", "names": []string{"not_a_thing"}})
	if res["verdict"] != VerdictError {
		t.Fatalf("an unknown scenario is ERROR: %v", res)
	}
	res, _ = callTool(t, cfg, "scenario_list", map[string]any{"sandbox": "ghost"})
	if res["verdict"] != VerdictError {
		t.Fatalf("an unknown sandbox is ERROR: %v", res)
	}
}

func TestControlToolsNeedARunningSandbox(t *testing.T) {
	cfg := testConfig(t, "https://example.invalid")
	cfg.SandboxPort = 1
	res, _ := callTool(t, cfg, "set_mode", map[string]any{"sandbox": "widgets", "name": "declines"})
	if res["verdict"] != VerdictError || !strings.Contains(res["error"].(map[string]any)["what"].(string), "not running") {
		t.Fatalf("without pikopod up the control tools are ERROR naming the fix: %v", res)
	}
}

func TestControlToolsDriveTheRunningSandbox(t *testing.T) {
	cfg := testConfig(t, "https://example.invalid")
	if err := sandboxAdd(cfg, "widgets", widgetsSpecPath, "mcp-seed", "", "", false, io.Discard); err != nil {
		t.Fatal(err)
	}
	sbx, err := newSandboxServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sbx.Close() })
	srv := httptest.NewServer(sbx)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	cfg.SandboxPort, _ = strconv.Atoi(u.Port())

	res, _ := callTool(t, cfg, "set_mode", map[string]any{"sandbox": "widgets", "name": "declines"})
	if res["verdict"] != VerdictClean {
		t.Fatalf("set_mode: %v", res)
	}
	create, err := http.Post(srv.URL+"/widgets/widgets", "application/json", strings.NewReader(`{"name":"g"}`))
	if err != nil {
		t.Fatal(err)
	}
	create.Body.Close()
	if create.StatusCode != 400 {
		t.Fatalf("with declines standing, create = %d", create.StatusCode)
	}
	res, _ = callTool(t, cfg, "get_requests", map[string]any{"sandbox": "widgets", "last": 5})
	reqs, _ := res["data"].(map[string]any)["requests"].([]any)
	if res["verdict"] != VerdictClean || len(reqs) != 1 {
		t.Fatalf("get_requests must show what the app sent: %v", res)
	}
	res, _ = callTool(t, cfg, "clear_mode", map[string]any{"sandbox": "widgets"})
	if res["verdict"] != VerdictClean {
		t.Fatalf("clear_mode: %v", res)
	}
	res, _ = callTool(t, cfg, "arm_fault", map[string]any{"sandbox": "widgets", "kind": "error", "status": 503, "method": "POST", "path": "/widgets"})
	if res["verdict"] != VerdictClean {
		t.Fatalf("arm_fault: %v", res)
	}
	create, _ = http.Post(srv.URL+"/widgets/widgets", "application/json", strings.NewReader(`{"name":"g"}`))
	create.Body.Close()
	if create.StatusCode != 503 {
		t.Fatalf("armed fault must show on plain HTTP: %d", create.StatusCode)
	}
	res, _ = callTool(t, cfg, "clear_faults", map[string]any{"sandbox": "widgets"})
	if res["verdict"] != VerdictClean {
		t.Fatalf("clear_faults: %v", res)
	}
	res, _ = callTool(t, cfg, "arm_fault", map[string]any{"sandbox": "widgets", "kind": "error"})
	if res["verdict"] != VerdictError {
		t.Fatalf("an HTTP fault without a target is ERROR: %v", res)
	}
	res, _ = callTool(t, cfg, "emit_webhook", map[string]any{"sandbox": "widgets", "event": "nope.event"})
	if res["verdict"] != VerdictError {
		t.Fatalf("an undeclared event is refused: %v", res)
	}
}

func TestMCPCommandRefusesBadConfig(t *testing.T) {
	cmd := newMCPCmd()
	cmd.Flags().String("config", filepath.Join(t.TempDir(), "missing.yaml"), "")
	cmd.SetIn(strings.NewReader(""))
	cmd.SetOut(io.Discard)
	if err := cmd.RunE(cmd, nil); err == nil {
		t.Fatal("a missing config must refuse (exit 2 through main)")
	}
}
