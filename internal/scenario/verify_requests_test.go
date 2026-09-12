package scenario

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
)

// VERIFY_REQUESTS: client-behavior verification against the sandbox's
// request journal — count by template (param names structural), and body
// claims on the last matching request.
func TestVerifyRequestsAssertions(t *testing.T) {
	def := parseDef(t, `{
	  "steps": [
	    {"key": "c1", "type": "REQUEST",
	     "config": {"method": "POST", "path": "/widgets", "body": {"name": "a", "amount": 500}}},
	    {"key": "c2", "type": "REQUEST",
	     "config": {"method": "POST", "path": "/widgets", "body": {"name": "b", "amount": 900}}},
	    {"key": "verify", "type": "VERIFY_REQUESTS",
	     "config": {"method": "POST", "path": "/widgets"},
	     "assertions": [
	       {"target": "sandbox.requestCount", "op": "equals", "expected": 2},
	       {"target": "sandbox.request", "path": "$.amount", "op": "equals", "expected": 900}
	     ]}
	  ]
	}`)
	res, err := Run(runnerEngine(t, "vr-1"), def, nil, "vr-1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != RunPassed {
		t.Fatalf("verification must pass: %s (%s)", res.Status, res.Summary)
	}

	// The negative direction: a wrong count FAILS the run — this is the
	// "did the app double-charge?" assertion doing its job.
	defWrong := parseDef(t, `{
	  "steps": [
	    {"key": "c1", "type": "REQUEST",
	     "config": {"method": "POST", "path": "/widgets", "body": {"name": "a"}}},
	    {"key": "verify", "type": "VERIFY_REQUESTS",
	     "config": {"method": "POST", "path": "/widgets"},
	     "assertions": [{"target": "sandbox.requestCount", "op": "equals", "expected": 2}]}
	  ]
	}`)
	res, err = Run(runnerEngine(t, "vr-2"), defWrong, nil, "vr-2")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != RunFailed {
		t.Fatalf("a wrong count must fail: %s", res.Status)
	}
}

// Fail-closed: once the journal has evicted, upper-bound count assertions
// FAIL with an honest message — never a silent false green; lower bounds
// (gte) remain provable and pass.
func TestVerifyRequestsFailsClosedOnEviction(t *testing.T) {
	eng := runnerEngine(t, "vr-evict")
	srv := httptest.NewServer(eng)
	defer srv.Close()
	// Overflow the journal ring directly through the serving path.
	for i := 0; i < 1030; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", fmt.Sprintf("/widgets/w_%04d", i), nil)
		eng.ServeHTTP(rec, req)
	}
	if _, evicted := eng.JournalCount("GET", "/widgets/{id}"); !evicted {
		t.Fatal("setup: the journal must have evicted")
	}

	upper := parseDef(t, `{
	  "steps": [{"key": "verify", "type": "VERIFY_REQUESTS",
	    "config": {"method": "GET", "path": "/widgets/{id}"},
	    "assertions": [{"target": "sandbox.requestCount", "op": "equals", "expected": 1024}]}]
	}`)
	res, err := Run(eng, upper, nil, "vr-evict")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != RunFailed {
		t.Fatalf("evicted journal must fail upper-bound claims closed: %s", res.Status)
	}
	if !strings.Contains(res.Summary, "unprovable") {
		t.Fatalf("the failure must say WHY: %s", res.Summary)
	}

	lower := parseDef(t, `{
	  "steps": [{"key": "verify", "type": "VERIFY_REQUESTS",
	    "config": {"method": "GET", "path": "/widgets/{id}"},
	    "assertions": [{"target": "sandbox.requestCount", "op": "gte", "expected": 1000}]}]
	}`)
	res, err = Run(eng, lower, nil, "vr-evict")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != RunPassed {
		t.Fatalf("lower bounds stay provable under eviction: %s (%s)", res.Status, res.Summary)
	}
}
