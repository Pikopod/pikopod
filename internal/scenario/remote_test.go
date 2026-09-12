package scenario

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A remote target runs REQUEST steps against a real HTTP endpoint with the
// same assertion machinery: headers injected on every request, real wall
// latency mapped into response.latencyMs, transport failure surfaced as an
// assertable 599 rather than an aborted run.
func TestRemoteTargetRunsRequestSteps(t *testing.T) {
	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		time.Sleep(15 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"id":"live_1","status":"success"}`))
	}))
	defer upstream.Close()

	target, err := NewRemoteTarget(upstream.URL, map[string]string{"Authorization": "Bearer live-token"})
	if err != nil {
		t.Fatal(err)
	}
	def := parseDef(t, `{
	  "steps": [{"key": "probe", "type": "REQUEST",
	    "config": {"method": "GET", "path": "/charges/live_1"},
	    "assertions": [
	      {"target": "response.status", "op": "equals", "expected": 200},
	      {"target": "response.body", "path": "$.status", "op": "equals", "expected": "success"},
	      {"target": "response.latencyMs", "op": "gte", "expected": 10}
	    ]}]
	}`)
	res, err := Run(target, def, nil, "remote-1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != RunPassed {
		raw, _ := json.MarshalIndent(res.Steps, "", " ")
		t.Fatalf("remote run must pass: %s (%s)\n%s", res.Status, res.Summary, raw)
	}
	if gotAuth != "Bearer live-token" {
		t.Fatalf("target headers must reach the endpoint: %q", gotAuth)
	}

	// A dead endpoint: the failure is a RESULT (599), not an abort.
	upstream.Close()
	res, err = Run(target, def, nil, "remote-2")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != RunFailed {
		t.Fatalf("transport failure must fail assertions, not error the run: %s", res.Status)
	}
}

// The honest subset: conditioning/state/webhook/journal/WAIT steps are
// refused up front with a reason, never silently skipped.
func TestRemoteRefusesUnsupportedSteps(t *testing.T) {
	def := parseDef(t, `{
	  "steps": [
	    {"key": "arm", "type": "INJECT_FAULT",
	     "config": {"method": "POST", "path": "/x", "kind": "error"}},
	    {"key": "go", "type": "REQUEST", "config": {"method": "POST", "path": "/x"}}
	  ]
	}`)
	err := RefuseUnsupportedSteps(def)
	if err == nil || !strings.Contains(err.Error(), "INJECT_FAULT") {
		t.Fatalf("conditioning must be refused by name: %v", err)
	}
	ok := parseDef(t, `{
	  "steps": [{"key": "go", "type": "REQUEST", "config": {"method": "GET", "path": "/x"}}]
	}`)
	if err := RefuseUnsupportedSteps(ok); err != nil {
		t.Fatalf("REQUEST-only definitions must pass: %v", err)
	}
}
