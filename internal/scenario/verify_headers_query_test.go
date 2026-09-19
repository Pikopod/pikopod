package scenario

import (
	"strings"
	"testing"
)

func TestVerifyRequestsSeesTheIdempotencyKey(t *testing.T) {
	def := parseDef(t, `{
	  "steps": [
	    {"key": "c1", "type": "REQUEST",
	     "config": {"method": "POST", "path": "/widgets",
	                "headers": {"Idempotency-Key": "abc123"}, "body": {"name": "a"}}},
	    {"key": "verify", "type": "VERIFY_REQUESTS",
	     "config": {"method": "POST", "path": "/widgets"},
	     "assertions": [
	       {"target": "sandbox.request.headers", "path": "$.idempotency-key", "op": "exists"},
	       {"target": "sandbox.request.headers", "path": "$.idempotency-key", "op": "notContains", "expected": "abc123"}
	     ]}
	  ]
	}`)
	res, err := Run(runnerEngine(t, "vh-1"), def, nil, "vh-1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != RunPassed {
		t.Fatalf("the key must be journaled and redacted: %s (%s)", res.Status, res.Summary)
	}
}

// A header assertion that should not hold must fail the run.
func TestVerifyRequestsHeaderMismatchFails(t *testing.T) {
	def := parseDef(t, `{
	  "steps": [
	    {"key": "c1", "type": "REQUEST",
	     "config": {"method": "POST", "path": "/widgets",
	                "headers": {"X-Client": "checkout"}, "body": {"name": "a"}}},
	    {"key": "verify", "type": "VERIFY_REQUESTS",
	     "config": {"method": "POST", "path": "/widgets"},
	     "assertions": [
	       {"target": "sandbox.request.headers", "path": "$.x-client", "op": "equals", "expected": "something-else"}
	     ]}
	  ]
	}`)
	res, err := Run(runnerEngine(t, "vh-2"), def, nil, "vh-2")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status == RunPassed {
		t.Fatal("a mismatched header must fail the run")
	}
}

// Query values the sanitizer allows survive verbatim and are assertable.
func TestVerifyRequestsAssertsOnRequestQuery(t *testing.T) {
	def := parseDef(t, `{
	  "steps": [
	    {"key": "c1", "type": "REQUEST",
	     "config": {"method": "GET", "path": "/widgets", "query": {"cursor": "abc"}}},
	    {"key": "verify", "type": "VERIFY_REQUESTS",
	     "config": {"method": "GET", "path": "/widgets"},
	     "assertions": [
	       {"target": "sandbox.request.query", "path": "$.cursor[0]", "op": "equals", "expected": "abc"}
	     ]}
	  ]
	}`)
	res, err := Run(runnerEngine(t, "vq-1"), def, nil, "vq-1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != RunPassed {
		t.Fatalf("query assertion must pass: %s (%s)", res.Status, res.Summary)
	}
}

// A credential must never reach an assertion verbatim: redaction happens
// before the journal, so the assertion sees the substituted value.
func TestJournaledAuthorizationIsRedactedBeforeAssertions(t *testing.T) {
	def := parseDef(t, `{
	  "steps": [
	    {"key": "c1", "type": "REQUEST",
	     "config": {"method": "POST", "path": "/widgets",
	                "headers": {"Authorization": "Bearer SUPERSECRETVALUE0123456789"},
	                "body": {"name": "a"}}},
	    {"key": "verify", "type": "VERIFY_REQUESTS",
	     "config": {"method": "POST", "path": "/widgets"},
	     "assertions": [
	       {"target": "sandbox.request.headers", "path": "$.authorization", "op": "notContains", "expected": "SUPERSECRETVALUE"}
	     ]}
	  ]
	}`)
	res, err := Run(runnerEngine(t, "vh-3"), def, nil, "vh-3")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != RunPassed {
		t.Fatalf("the journal must not expose the credential: %s (%s)", res.Status, res.Summary)
	}
	if strings.Contains(res.Summary, "SUPERSECRETVALUE") {
		t.Fatal("the run summary leaked the credential")
	}
}
