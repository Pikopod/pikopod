package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

// The whole point: a mode changes what the RUNNING sandbox answers, so a
// developer's own client meets the failure without running a pack.
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

// GET reports the standing state so cross-talk between clients is diagnosable
// rather than mysterious.
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

// An archetype that asserts correct behaviour has no standing state, and
// saying so beats arming nothing and reporting success.
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
