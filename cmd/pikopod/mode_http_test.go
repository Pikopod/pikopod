package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pikopod/pikopod/internal/mode"
)

func modeServer(t *testing.T, seed string) *httptest.Server {
	t.Helper()
	cfg := testConfig(t, "https://example.invalid")
	if err := sandboxAdd(cfg, "widgets", widgetsSpecPath, seed, "", "", false, io.Discard); err != nil {
		t.Fatalf("add: %v", err)
	}
	sbx, err := newSandboxServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sbx.Close() })
	srv := httptest.NewServer(sbx)
	t.Cleanup(srv.Close)
	return srv
}

func setMode(t *testing.T, srv *httptest.Server, body string) *http.Response {
	t.Helper()
	res, err := http.Post(srv.URL+"/_pikopod/sandboxes/widgets/mode", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func createWidget(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	res, err := http.Post(srv.URL+"/widgets/widgets", "application/json", strings.NewReader(`{"name":"g"}`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

func TestModeChangesWhatTheRunningSandboxServes(t *testing.T) {
	srv := modeServer(t, "mode-seed-1")

	if code := createWidget(t, srv); code != 201 {
		t.Fatalf("baseline create = %d, want 201", code)
	}

	res := setMode(t, srv, `{"name":"declines"}`)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("mode set = %d: %v", res.StatusCode, readJSON(t, res)["message"])
	}

	if code := createWidget(t, srv); code == 201 {
		t.Fatal("after `mode set declines` the same create must not succeed")
	}

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/_pikopod/sandboxes/widgets/mode", nil)
	if _, err := http.DefaultClient.Do(req); err != nil {
		t.Fatal(err)
	}
	if code := createWidget(t, srv); code != 201 {
		t.Fatalf("after clear, create = %d, want 201", code)
	}
}

func TestModeShowReportsWhatIsArmed(t *testing.T) {
	srv := modeServer(t, "mode-seed-2")

	res, err := http.Get(srv.URL + "/_pikopod/sandboxes/widgets/mode")
	if err != nil {
		t.Fatal(err)
	}
	if got := readJSON(t, res)["mode"]; got != nil {
		t.Fatalf("fresh sandbox reports a mode: %v", got)
	}

	if res := setMode(t, srv, `{"name":"declines"}`); res.StatusCode != http.StatusCreated {
		t.Fatalf("mode set = %d", res.StatusCode)
	}
	res, err = http.Get(srv.URL + "/_pikopod/sandboxes/widgets/mode")
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := readJSON(t, res)["mode"].(map[string]any)
	if spec == nil || spec["name"] != "declines" {
		t.Fatalf("mode show = %v, want declines", spec)
	}
}

func TestModeRefusesAnArchetypeWithNoStandingState(t *testing.T) {
	srv := modeServer(t, "mode-seed-3")
	res := setMode(t, srv, `{"name":"happy_path"}`)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("happy_path as a mode = %d, want 400", res.StatusCode)
	}
	msg, _ := readJSON(t, res)["message"].(string)
	if !strings.Contains(msg, "no standing state") {
		t.Fatalf("refusal must name the reason: %s", msg)
	}
}

func TestModeRefusesAnUnknownScenario(t *testing.T) {
	srv := modeServer(t, "mode-seed-4")
	res := setMode(t, srv, `{"name":"not_a_thing"}`)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown scenario = %d, want 400", res.StatusCode)
	}
}

func TestModeClearSaysHowManyInWords(t *testing.T) {
	srv := modeServer(t, "mode-clear-1")
	if res := setMode(t, srv, `{"name":"declines"}`); res.StatusCode != http.StatusCreated {
		t.Fatalf("mode set = %d", res.StatusCode)
	}
	listed, err := http.Get(srv.URL + "/_pikopod/sandboxes/widgets/faults")
	if err != nil {
		t.Fatal(err)
	}
	var standing struct {
		Faults []json.RawMessage `json:"faults"`
	}
	json.NewDecoder(listed.Body).Decode(&standing)
	listed.Body.Close()
	if len(standing.Faults) == 0 {
		t.Fatal("the mode must arm at least one fault for this test to mean anything")
	}

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/_pikopod/sandboxes/widgets/mode", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	if err := printCleared(resp, &buf); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("cleared %d fault(s)\n", len(standing.Faults))
	if got := buf.String(); got != want {
		t.Fatalf("mode clear must say how many in words, got %q want %q", got, want)
	}
}

func TestModeClearFailsWhenTheServerRefuses(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"refused":     func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no such sandbox", http.StatusNotFound) },
		"unreachable": func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("<html>proxy</html>")) },
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(h)
			defer srv.Close()
			out, err := clearCLI(t, srv)
			if err == nil {
				t.Fatalf("mode clear must fail, exited 0 with %q", out)
			}
			if strings.Contains(out, "fault(s)") {
				t.Fatalf("must not claim it cleared anything: %q", out)
			}
		})
	}
}

func clearCLI(t *testing.T, srv *httptest.Server) (string, error) {
	t.Helper()
	dir := cliDir(t)
	u, _ := url.Parse(srv.URL)
	yaml := fmt.Sprintf("listen: 127.0.0.1\nsandbox_port: %s\ndata_dir: %s\nupstreams:\n  widgets:\n    target: https://api.example.invalid\n", u.Port(), filepath.Join(dir, "data"))
	if err := os.WriteFile(filepath.Join(dir, "pikopod.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return runCLI(t, newModeCmd(), "clear", "widgets")
}

func showCLI(t *testing.T, srv *httptest.Server, args ...string) (string, error) {
	t.Helper()
	dir := cliDir(t)
	u, _ := url.Parse(srv.URL)
	yaml := fmt.Sprintf("listen: 127.0.0.1\nsandbox_port: %s\ndata_dir: %s\nupstreams:\n  widgets:\n    target: https://api.example.invalid\n", u.Port(), filepath.Join(dir, "data"))
	if err := os.WriteFile(filepath.Join(dir, "pikopod.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	allArgs := append([]string{"show", "widgets"}, args...)
	return runCLI(t, newModeCmd(), allArgs...)
}

func TestModeShowFormatJSON(t *testing.T) {
	srv := modeServer(t, "mode-show-json-1")

	freshText, err := showCLI(t, srv)
	if err != nil {
		t.Fatal(err)
	}
	if freshText != "no mode set\n" {
		t.Fatalf("fresh mode show = %q, want %q", freshText, "no mode set\n")
	}

	freshJSON, err := showCLI(t, srv, "--format", "json")
	if err != nil {
		t.Fatal(err)
	}
	if freshJSON != "null\n" {
		t.Fatalf("fresh mode show --format json = %q, want %q", freshJSON, "null\n")
	}

	if res := setMode(t, srv, `{"name":"declines"}`); res.StatusCode != http.StatusCreated {
		t.Fatalf("mode set = %d", res.StatusCode)
	}

	textOut, err := showCLI(t, srv)
	if err != nil {
		t.Fatal(err)
	}
	explicitTextOut, err := showCLI(t, srv, "--format", "text")
	if err != nil {
		t.Fatal(err)
	}
	if textOut != explicitTextOut {
		t.Fatalf("default format must match --format text: %q vs %q", textOut, explicitTextOut)
	}

	jsonOut, err := showCLI(t, srv, "--format", "json")
	if err != nil {
		t.Fatal(err)
	}
	var spec mode.Spec
	if err := json.Unmarshal([]byte(jsonOut), &spec); err != nil {
		t.Fatalf("mode show --format json is not valid json: %v\n%s", err, jsonOut)
	}
	if spec.Name != "declines" {
		t.Fatalf("spec name = %q, want declines", spec.Name)
	}
	if len(spec.Faults) == 0 {
		t.Fatal("declines spec must have faults")
	}

	if _, err := showCLI(t, srv, "--format", "yaml"); err == nil {
		t.Fatal("unknown format must error")
	}
}
