package sandbox

import (
	"strings"
	"testing"
)

// Every matched response names its operation; unmatched responses name the
// closest declared operations — "why didn't this match" answered on the
// wire (headers only: response bodies are untouched).
func TestDiagnosticHeaders(t *testing.T) {
	e := newEngine(t, loadWidgets(t), Config{ID: "sbx_diag", Seed: "diag-1"})

	got := do(t, e, "POST", "/widgets", `{"name":"g"}`, nil)
	if got.headers[OperationHeader] == "" {
		t.Fatalf("matched responses must name their operation: %v", got.headers)
	}

	// A typo'd path: the 404 suggests the real routes, body stays neutral.
	miss := do(t, e, "GET", "/widgets_typo", "", nil)
	if miss.status != 404 {
		t.Fatalf("expected 404, got %d", miss.status)
	}
	closest := miss.headers[ClosestHeader]
	if !strings.Contains(closest, "/widgets") {
		t.Fatalf("near-miss header must suggest the declared routes: %q", closest)
	}
	if !strings.Contains(miss.body, "Not Found") || strings.Contains(miss.body, "closest") {
		t.Fatalf("the 404 BODY must stay parity-neutral: %s", miss.body)
	}

	// Faulted responses carry the operation too — a 500 you armed is still
	// attributable.
	e.ArmFault(FaultRule{Method: "POST", Path: "/widgets", Kind: "error", Probability: 1})
	faulted := do(t, e, "POST", "/widgets", `{"name":"g"}`, nil)
	if faulted.status != 500 || faulted.headers[OperationHeader] == "" {
		t.Fatalf("faulted responses must stay attributable: %d %v", faulted.status, faulted.headers)
	}
}

// The trace narrates the pipeline in order and is exact under replay: two
// traced replays of the same request produce the same narration.
func TestTraceNarratesPipeline(t *testing.T) {
	run := func() []string {
		e := newEngine(t, loadWidgets(t), Config{ID: "sbx_trace", Seed: "trace-1"})
		var lines []string
		e.SetTrace(func(stage, msg string) { lines = append(lines, stage+": "+msg) })
		do(t, e, "POST", "/widgets", `{"name":"g"}`, nil)
		return lines
	}
	first := run()
	joined := strings.Join(first, "\n")
	for _, want := range []string{"route:", "auth:", "operation:", "faults:"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("trace missing %q stage:\n%s", want, joined)
		}
	}
	if second := strings.Join(run(), "\n"); second != joined {
		t.Fatalf("deterministic replay must produce an identical trace:\n%s\nvs\n%s", joined, second)
	}
}

// Correlation echo: request ids reflect onto responses — success and
// error paths alike — without clobbering anything the engine already set.
func TestCorrelationEcho(t *testing.T) {
	e := newEngine(t, loadWidgets(t), Config{ID: "sbx_echo", Seed: "echo-1"})
	h := map[string]string{"X-Request-Id": "req-42", "Idempotency-Key": "ik-7"}
	got := do(t, e, "POST", "/widgets", `{"name":"g"}`, h)
	if got.headers["x-request-id"] != "req-42" || got.headers["idempotency-key"] != "ik-7" {
		t.Fatalf("correlation headers must echo: %v", got.headers)
	}
	miss := do(t, e, "GET", "/nope", "", map[string]string{"X-Request-Id": "req-43"})
	if miss.headers["x-request-id"] != "req-43" {
		t.Fatalf("errors must echo too: %v", miss.headers)
	}
	bare := do(t, e, "GET", "/widgets", "", nil)
	if _, present := bare.headers["x-request-id"]; present {
		t.Fatal("nothing to echo when the client sent nothing")
	}
}
