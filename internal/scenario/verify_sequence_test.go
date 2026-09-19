package scenario

import (
	"fmt"
	"strings"
	"testing"
)

func TestVerifySequenceProvesIdempotencyKeyReuse(t *testing.T) {
	def := parseDef(t, `{
	  "steps": [
	    {"key": "a1", "type": "REQUEST",
	     "config": {"method": "POST", "path": "/widgets",
	                "headers": {"Idempotency-Key": "same-key"}, "body": {"name": "a"}}},
	    {"key": "a2", "type": "REQUEST",
	     "config": {"method": "POST", "path": "/widgets",
	                "headers": {"Idempotency-Key": "same-key"}, "body": {"name": "a"}}},
	    {"key": "seq", "type": "VERIFY_SEQUENCE",
	     "config": {"requests": [
	       {"method": "POST", "path": "/widgets"},
	       {"method": "POST", "path": "/widgets"}
	     ]}}
	  ]
	}`)
	res, err := Run(runnerEngine(t, "vs-1"), def, nil, "vs-1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != RunPassed {
		t.Fatalf("two ordered POSTs must match: %s (%s)", res.Status, res.Summary)
	}
}

func TestVerifySequenceEnforcesOrder(t *testing.T) {
	def := parseDef(t, `{
	  "steps": [
	    {"key": "get", "type": "REQUEST", "config": {"method": "GET", "path": "/widgets"}},
	    {"key": "post", "type": "REQUEST",
	     "config": {"method": "POST", "path": "/widgets", "body": {"name": "a"}}},
	    {"key": "seq", "type": "VERIFY_SEQUENCE",
	     "config": {"requests": [
	       {"method": "POST", "path": "/widgets"},
	       {"method": "GET", "path": "/widgets"}
	     ]}}
	  ]
	}`)
	res, err := Run(runnerEngine(t, "vs-2"), def, nil, "vs-2")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status == RunPassed {
		t.Fatal("GET happened before POST, so POST-then-GET must not match")
	}
	if !strings.Contains(res.Summary, "never matched") {
		t.Fatalf("the failure should name the unsatisfied matcher: %s", res.Summary)
	}
}

func TestVerifySequenceAllowsUnrelatedRequestsBetween(t *testing.T) {
	def := parseDef(t, `{
	  "steps": [
	    {"key": "p1", "type": "REQUEST", "config": {"method": "POST", "path": "/widgets", "body": {"name": "a"}}},
	    {"key": "noise", "type": "REQUEST", "config": {"method": "GET", "path": "/widgets"}},
	    {"key": "p2", "type": "REQUEST", "config": {"method": "POST", "path": "/widgets", "body": {"name": "b"}}},
	    {"key": "seq", "type": "VERIFY_SEQUENCE",
	     "config": {"requests": [
	       {"method": "POST", "path": "/widgets"},
	       {"method": "POST", "path": "/widgets"}
	     ]}}
	  ]
	}`)
	res, err := Run(runnerEngine(t, "vs-3"), def, nil, "vs-3")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != RunPassed {
		t.Fatalf("an intervening GET must not break the subsequence: %s (%s)", res.Status, res.Summary)
	}
}

func TestVerifySequenceEnforcesMinGap(t *testing.T) {
	withWait := `{
	  "steps": [
	    {"key": "a1", "type": "REQUEST", "config": {"method": "POST", "path": "/widgets", "body": {"name": "a"}}},
	    {"key": "wait", "type": "WAIT", "config": {"durationMs": %d}},
	    {"key": "a2", "type": "REQUEST", "config": {"method": "POST", "path": "/widgets", "body": {"name": "b"}}},
	    {"key": "seq", "type": "VERIFY_SEQUENCE",
	     "config": {"requests": [
	       {"method": "POST", "path": "/widgets"},
	       {"method": "POST", "path": "/widgets", "minGapMs": 1000}
	     ]}}
	  ]
	}`
	res, err := Run(runnerEngine(t, "vs-4"), parseDef(t, fmt.Sprintf(withWait, 2000)), nil, "vs-4")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != RunPassed {
		t.Fatalf("a 2000ms gap satisfies minGapMs 1000: %s (%s)", res.Status, res.Summary)
	}

	res, err = Run(runnerEngine(t, "vs-5"), parseDef(t, fmt.Sprintf(withWait, 100)), nil, "vs-5")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status == RunPassed {
		t.Fatal("a 100ms gap must fail minGapMs 1000 — that is the backoff claim")
	}
	if !strings.Contains(res.Summary, "minGapMs") {
		t.Fatalf("the failure should name the gap: %s", res.Summary)
	}
}

// A header matcher narrows which entries count.
func TestVerifySequenceMatchesOnHeaders(t *testing.T) {
	def := parseDef(t, `{
	  "steps": [
	    {"key": "a1", "type": "REQUEST",
	     "config": {"method": "POST", "path": "/widgets",
	                "headers": {"X-Client": "checkout"}, "body": {"name": "a"}}},
	    {"key": "seq", "type": "VERIFY_SEQUENCE",
	     "config": {"requests": [
	       {"method": "POST", "path": "/widgets", "headers": {"x-client": "checkout"}}
	     ]}}
	  ]
	}`)
	res, err := Run(runnerEngine(t, "vs-6"), def, nil, "vs-6")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != RunPassed {
		t.Fatalf("header matcher must match: %s (%s)", res.Status, res.Summary)
	}

	wrong := parseDef(t, `{
	  "steps": [
	    {"key": "a1", "type": "REQUEST",
	     "config": {"method": "POST", "path": "/widgets",
	                "headers": {"X-Client": "checkout"}, "body": {"name": "a"}}},
	    {"key": "seq", "type": "VERIFY_SEQUENCE",
	     "config": {"requests": [
	       {"method": "POST", "path": "/widgets", "headers": {"x-client": "batch"}}
	     ]}}
	  ]
	}`)
	res, err = Run(runnerEngine(t, "vs-7"), wrong, nil, "vs-7")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status == RunPassed {
		t.Fatal("a header that does not match must fail")
	}
}
