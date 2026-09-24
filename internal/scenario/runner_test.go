package scenario

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pikopod/pikopod/internal/importer"
	"github.com/pikopod/pikopod/internal/ir"
	"github.com/pikopod/pikopod/internal/sandbox"
)

const runnerSpec = `{
  "openapi": "3.0.0",
  "info": {"title": "Widgets", "version": "1.0.0"},
  "security": [{"bearerAuth": []}],
  "paths": {
    "/widgets": {
      "get": {"responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {
        "type": "object",
        "properties": {"items": {"type": "array", "items": {"$ref": "#/components/schemas/Widget"}}, "total": {"type": "integer"}}
      }}}}}},
      "post": {
        "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Widget"}}}},
        "responses": {"201": {"description": "created", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Widget"}}}}}
      }
    },
    "/widgets/{widgetId}": {
      "parameters": [{"name": "widgetId", "in": "path", "required": true, "schema": {"type": "string"}}],
      "get": {"responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Widget"}}}}}}
    }
  },
  "components": {
    "securitySchemes": {"bearerAuth": {"type": "http", "scheme": "bearer"}},
    "schemas": {"Widget": {
      "type": "object",
      "properties": {"id": {"type": "string"}, "name": {"type": "string"}, "createdAt": {"type": "string", "format": "date-time"}}
    }}
  }
}`

func runnerIR(t *testing.T) *ir.ApiDefinition {
	t.Helper()
	def, err := importer.NormalizeOpenAPI([]byte(runnerSpec))
	if err != nil {
		t.Fatalf("normalize runner spec: %v", err)
	}
	return def
}

func runnerEngine(t *testing.T, seed string) *sandbox.Engine {
	t.Helper()
	store, err := sandbox.OpenMemoryStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	eng, err := sandbox.NewEngine(runnerIR(t), sandbox.Config{ID: "sbx_runner", Seed: seed}, store)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	return eng
}

func parseDef(t *testing.T, raw string) *ScenarioDefinition {
	t.Helper()
	def, errs := ParseDefinitionJSON([]byte(raw))
	if len(errs) > 0 {
		t.Fatalf("definition did not parse: %+v", errs)
	}
	return def
}

const happyPathDef = `{
  "inputs": [{"name": "widgetName", "type": "string", "required": false, "default": "gizmo"}],
  "defaults": {},
  "steps": [
    {"key": "seed", "type": "SEED_STATE",
     "config": {"resources": [{"type": "/widgets", "resourceKey": "widgets_seeded", "attributes": {"name": "seeded"}}]}},
    {"key": "create", "type": "REQUEST",
     "capture": {"wid": "response.body$.id"},
     "assertions": [{"target": "response.status", "op": "equals", "expected": 201}],
     "config": {"method": "POST", "path": "/widgets", "body": {"name": "{{widgetName}}"}}},
    {"key": "read", "type": "REQUEST",
     "assertions": [
       {"target": "response.status", "op": "equals", "expected": 200},
       {"target": "response.body", "path": "$.name", "op": "equals", "expected": "gizmo"}
     ],
     "config": {"method": "GET", "path": "/widgets/{{wid}}"}},
    {"key": "state", "type": "ASSERT_STATE",
     "assertions": [{"target": "state.resource", "path": "$.name", "op": "equals", "expected": "seeded"}],
     "config": {"resourceType": "/widgets", "resourceId": "widgets_seeded"}}
  ]
}`

func TestRunHappyPathDeterministic(t *testing.T) {
	def := parseDef(t, happyPathDef)

	run := func() *RunResult {
		eng := runnerEngine(t, "seed-1")

		rec := httptest.NewRecorder()
		eng.ServeHTTP(rec, httptest.NewRequest("GET", "/widgets", nil))
		if rec.Code != 401 {
			t.Fatalf("bare request should 401, got %d", rec.Code)
		}

		res, err := Run(eng, def, nil, "seed-1")
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	a, b := run(), run()
	if a.Status != RunPassed {
		t.Fatalf("run should pass, got %s (%s)", a.Status, a.Summary)
	}
	if len(a.Steps) != 4 {
		t.Fatalf("expected 4 step results, got %d", len(a.Steps))
	}
	if a.ResultHash == "" || a.ResultHash != b.ResultHash {
		t.Fatalf("same seed must reproduce the same result hash: %q vs %q", a.ResultHash, b.ResultHash)
	}

	readDetail := a.Steps[2].Detail["request"].(map[string]any)
	if !strings.HasPrefix(readDetail["path"].(string), "/widgets/widgets_") {
		t.Fatalf("capture did not flow into the path: %v", readDetail["path"])
	}

	engC := runnerEngine(t, "seed-2")
	c, err := Run(engC, def, nil, "seed-2")
	if err != nil {
		t.Fatal(err)
	}
	if c.ResultHash == a.ResultHash {
		t.Fatal("a different seed must change the result hash")
	}
}

func TestRunHardFailureSkipsRemaining(t *testing.T) {
	def := parseDef(t, `{
	  "steps": [
	    {"key": "create", "type": "REQUEST",
	     "assertions": [{"target": "response.status", "op": "equals", "expected": 599}],
	     "config": {"method": "POST", "path": "/widgets", "body": {"name": "x"}}},
	    {"key": "after", "type": "NOTE", "config": {"text": "never runs"}}
	  ]
	}`)
	res, err := Run(runnerEngine(t, "s"), def, nil, "s")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != RunFailed {
		t.Fatalf("expected FAILED, got %s", res.Status)
	}
	if res.Steps[0].Status != StatusFailed || res.Steps[1].Status != StatusSkipped {
		t.Fatalf("expected FAILED then SKIPPED, got %s then %s", res.Steps[0].Status, res.Steps[1].Status)
	}
}

func TestRunContinueOnFailureKeepsGoing(t *testing.T) {
	def := parseDef(t, `{
	  "steps": [
	    {"key": "flaky", "type": "REQUEST", "continueOnFailure": true,
	     "assertions": [{"target": "response.status", "op": "equals", "expected": 599}],
	     "config": {"method": "POST", "path": "/widgets", "body": {"name": "x"}}},
	    {"key": "after", "type": "REQUEST",
	     "assertions": [{"target": "response.status", "op": "equals", "expected": 200}],
	     "config": {"method": "GET", "path": "/widgets"}}
	  ]
	}`)
	res, err := Run(runnerEngine(t, "s"), def, nil, "s")
	if err != nil {
		t.Fatal(err)
	}
	if res.Steps[1].Status != StatusPassed {
		t.Fatalf("step after a continueOnFailure failure must still run, got %s", res.Steps[1].Status)
	}
}

func TestRunWaitIsVirtual(t *testing.T) {
	eng := runnerEngine(t, "s")
	start := eng.VirtualClockMs()
	def := parseDef(t, `{
	  "steps": [{"key": "wait", "type": "WAIT", "config": {"durationMs": 7776000000}}]
	}`)
	res, err := Run(eng, def, nil, "s")
	if err != nil {
		t.Fatal(err)
	}
	if res.Steps[0].VirtualEndMs != start+7776000000 {
		t.Fatalf("virtual end wrong: %d", res.Steps[0].VirtualEndMs)
	}
	if eng.VirtualClockMs() != start+7776000000 {
		t.Fatalf("engine clock not advanced: %d", eng.VirtualClockMs())
	}
	if res.Status != RunConditionGenerated {
		t.Fatalf("a run with nothing evaluable is CONDITION_GENERATED, got %s", res.Status)
	}
}

func TestRunFaultInjectionAndClear(t *testing.T) {
	def := parseDef(t, `{
	  "steps": [
	    {"key": "arm", "type": "INJECT_FAULT",
	     "config": {"method": "POST", "path": "/widgets", "kind": "error", "status": 503}},
	    {"key": "hit", "type": "REQUEST",
	     "assertions": [
	       {"target": "response.status", "op": "equals", "expected": 503},
	       {"target": "execution.faultApplied", "op": "equals", "expected": true}
	     ],
	     "config": {"method": "POST", "path": "/widgets", "body": {"name": "x"}}},
	    {"key": "clear", "type": "CLEAR_FAULT", "config": {"method": "POST", "path": "/widgets"}},
	    {"key": "ok", "type": "REQUEST",
	     "assertions": [{"target": "response.status", "op": "equals", "expected": 201}],
	     "config": {"method": "POST", "path": "/widgets", "body": {"name": "x"}}}
	  ]
	}`)
	res, err := Run(runnerEngine(t, "s"), def, nil, "s")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != RunPassed {
		t.Fatalf("fault lifecycle should pass, got %s (%s)", res.Status, res.Summary)
	}
}

func TestRunLatencyFaultVirtualized(t *testing.T) {
	def := parseDef(t, `{
	  "steps": [
	    {"key": "arm", "type": "INJECT_FAULT",
	     "config": {"method": "GET", "path": "/widgets", "kind": "latency", "delayMs": 250}},
	    {"key": "slow", "type": "REQUEST",
	     "assertions": [
	       {"target": "response.latencyMs", "op": "equals", "expected": 250},
	       {"target": "response.headers", "key": "x-pikopod-fault-delay-ms", "op": "absent"},
	       {"target": "response.status", "op": "equals", "expected": 200}
	     ],
	     "config": {"method": "GET", "path": "/widgets"}}
	  ]
	}`)
	res, err := Run(runnerEngine(t, "s"), def, nil, "s")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != RunPassed {
		t.Fatalf("latency fault run should pass, got %s (%s)", res.Status, res.Summary)
	}
}

const runnerWebhookSpecExtra = `"webhooks": {
    "widgets.created": {"post": {"requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Widget"}}}}, "responses": {"200": {"description": "ack"}}}}
  },`

func webhookRunnerEngine(t *testing.T, seed string) *sandbox.Engine {
	t.Helper()
	spec := strings.Replace(runnerSpec, `"paths": {`, runnerWebhookSpecExtra+`
  "paths": {`, 1)
	def, err := importer.NormalizeOpenAPI([]byte(spec))
	if err != nil {
		t.Fatalf("normalize webhook runner spec: %v", err)
	}
	if len(def.Webhooks) != 1 {
		t.Fatalf("spec must declare 1 webhook, IR has %d", len(def.Webhooks))
	}
	store, err := sandbox.OpenMemoryStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	eng, err := sandbox.NewEngine(def, sandbox.Config{ID: "sbx_wh_runner", Seed: seed}, store)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	return eng
}

func TestRunExpectWebhookPass(t *testing.T) {
	def := parseDef(t, `{
	  "steps": [
	    {"key": "create", "type": "REQUEST",
	     "assertions": [{"target": "response.status", "op": "equals", "expected": 201}],
	     "config": {"method": "POST", "path": "/widgets", "body": {"name": "x"}}},
	    {"key": "hook", "type": "EXPECT_WEBHOOK",
	     "capture": {"whid": "webhook.delivery$.id"},
	     "assertions": [
	       {"target": "webhook.count", "op": "equals", "expected": 1},
	       {"target": "webhook.delivery", "op": "exists"}
	     ],
	     "config": {"match": {"eventType": "widgets.created"}, "timeoutMs": 30000}},
	    {"key": "read", "type": "REQUEST",
	     "assertions": [{"target": "response.status", "op": "equals", "expected": 200}],
	     "config": {"method": "GET", "path": "/widgets/{{whid}}"}}
	  ]
	}`)
	run := func() *RunResult {
		res, err := Run(webhookRunnerEngine(t, "s"), def, nil, "s")
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	a, b := run(), run()
	if a.Status != RunPassed {
		t.Fatalf("expected PASSED, got %s (%s)", a.Status, a.Summary)
	}

	if a.Steps[1].VirtualEndMs != a.Steps[1].VirtualStartMs {
		t.Fatalf("satisfied expect must not advance the clock: %d → %d", a.Steps[1].VirtualStartMs, a.Steps[1].VirtualEndMs)
	}

	readDetail := a.Steps[2].Detail["request"].(map[string]any)
	if !strings.HasPrefix(readDetail["path"].(string), "/widgets/widgets_") {
		t.Fatalf("webhook capture did not flow into the path: %v", readDetail["path"])
	}
	if a.ResultHash != b.ResultHash {
		t.Fatalf("webhook runs must be deterministic: %s vs %s", a.ResultHash, b.ResultHash)
	}
}

func TestRunExpectWebhookTimeout(t *testing.T) {
	quiet := parseDef(t, `{
	  "steps": [
	    {"key": "hook", "type": "EXPECT_WEBHOOK",
	     "assertions": [{"target": "webhook.count", "op": "equals", "expected": 0}],
	     "config": {"match": {"eventType": "widgets.created"}, "timeoutMs": 30000}}
	  ]
	}`)
	eng := webhookRunnerEngine(t, "s")
	start := eng.VirtualClockMs()
	res, err := Run(eng, quiet, nil, "s")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != RunPassed {
		t.Fatalf("count-0 on a quiet outbox must pass, got %s (%s)", res.Status, res.Summary)
	}
	if res.Steps[0].VirtualEndMs != start+30000 {
		t.Fatalf("an unmet expect must advance by timeoutMs, got %d", res.Steps[0].VirtualEndMs)
	}

	expecting := parseDef(t, `{
	  "steps": [
	    {"key": "hook", "type": "EXPECT_WEBHOOK",
	     "assertions": [{"target": "webhook.count", "op": "gte", "expected": 1}],
	     "config": {"match": {"eventType": "widgets.created"}, "timeoutMs": 30000}}
	  ]
	}`)
	res, err = Run(webhookRunnerEngine(t, "s"), expecting, nil, "s")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != RunFailed {
		t.Fatalf("expecting a delivery that never came must FAIL, got %s", res.Status)
	}
}

func TestRunWebhookFaultKinds(t *testing.T) {
	dup := parseDef(t, `{
	  "steps": [
	    {"key": "arm", "type": "INJECT_FAULT", "config": {"kind": "duplicate_webhook", "target": "widgets.created"}},
	    {"key": "create", "type": "REQUEST",
	     "config": {"method": "POST", "path": "/widgets", "body": {"name": "a"}}},
	    {"key": "hook", "type": "EXPECT_WEBHOOK",
	     "assertions": [{"target": "webhook.count", "op": "equals", "expected": 2}],
	     "config": {"match": {"eventType": "widgets.created"}, "timeoutMs": 1000}}
	  ]
	}`)
	res, err := Run(webhookRunnerEngine(t, "s"), dup, nil, "s")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != RunPassed {
		t.Fatalf("duplicate: expected PASSED, got %s (%s)", res.Status, res.Summary)
	}

	drop := parseDef(t, `{
	  "steps": [
	    {"key": "arm", "type": "INJECT_FAULT", "config": {"kind": "drop_webhook", "target": "widgets.created"}},
	    {"key": "create", "type": "REQUEST",
	     "config": {"method": "POST", "path": "/widgets", "body": {"name": "a"}}},
	    {"key": "hook", "type": "EXPECT_WEBHOOK",
	     "assertions": [{"target": "webhook.count", "op": "equals", "expected": 0}],
	     "config": {"match": {"eventType": "widgets.created"}, "timeoutMs": 1000}}
	  ]
	}`)
	res, err = Run(webhookRunnerEngine(t, "s"), drop, nil, "s")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != RunPassed {
		t.Fatalf("drop: expected PASSED, got %s (%s)", res.Status, res.Summary)
	}

	reorder := parseDef(t, `{
	  "steps": [
	    {"key": "arm", "type": "INJECT_FAULT", "config": {"kind": "reorder_webhook", "target": "widgets.created"}},
	    {"key": "a", "type": "REQUEST", "config": {"method": "POST", "path": "/widgets", "body": {"name": "a"}}},
	    {"key": "b", "type": "REQUEST", "config": {"method": "POST", "path": "/widgets", "body": {"name": "b"}}},
	    {"key": "hook", "type": "EXPECT_WEBHOOK",
	     "capture": {"headId": "webhook.delivery$.id"},
	     "assertions": [{"target": "webhook.count", "op": "equals", "expected": 2}],
	     "config": {"match": {"eventType": "widgets.created"}, "timeoutMs": 1000}},
	    {"key": "head", "type": "REQUEST",
	     "assertions": [{"target": "response.body", "path": "$.name", "op": "equals", "expected": "b"}],
	     "config": {"method": "GET", "path": "/widgets/{{headId}}"}}
	  ]
	}`)
	res, err = Run(webhookRunnerEngine(t, "s"), reorder, nil, "s")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != RunPassed {
		t.Fatalf("reorder: head delivery must be the LATER create, got %s (%s)", res.Status, res.Summary)
	}
}

func TestRunInputResolution(t *testing.T) {
	def := parseDef(t, `{
	  "inputs": [{"name": "amount", "type": "number", "required": true}],
	  "steps": [{"key": "n", "type": "NOTE", "config": {"text": "hi"}}]
	}`)
	eng := runnerEngine(t, "s")
	if _, err := Run(eng, def, nil, "s"); err == nil {
		t.Fatal("missing required input must fail the run up front")
	}
	if _, err := Run(eng, def, map[string]any{"amount": "not-a-number"}, "s"); err == nil {
		t.Fatal("type mismatch must fail")
	}
	if _, err := Run(eng, def, map[string]any{"amount": 5.0, "typo": 1.0}, "s"); err == nil {
		t.Fatal("unknown input must fail")
	}
	if _, err := Run(eng, def, map[string]any{"amount": 5.0}, "s"); err != nil {
		t.Fatalf("valid inputs must run: %v", err)
	}
}

func TestRunDelayedWebhook(t *testing.T) {
	run := func(timeoutMs int64) *RunResult {
		def := parseDef(t, fmt.Sprintf(`{
		  "steps": [
		    {"key": "arm", "type": "INJECT_FAULT",
		     "config": {"kind": "delay_webhook", "target": "widgets.created", "delayMs": 60000}},
		    {"key": "create", "type": "REQUEST",
		     "config": {"method": "POST", "path": "/widgets", "body": {"name": "x"}}},
		    {"key": "hook", "type": "EXPECT_WEBHOOK",
		     "assertions": [{"target": "webhook.count", "op": "equals", "expected": 1}],
		     "config": {"match": {"eventType": "widgets.created"}, "timeoutMs": %d}}
		  ]
		}`, timeoutMs))
		res, err := Run(webhookRunnerEngine(t, "s"), def, nil, "s")
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	hit := run(90000)
	if hit.Status != RunPassed {
		t.Fatalf("delayed delivery within the window must pass, got %s (%s)", hit.Status, hit.Summary)
	}
	hookStep := hit.Steps[2]
	if hookStep.VirtualEndMs-hookStep.VirtualStartMs != 60000 {
		t.Fatalf("clock must advance to the ARRIVAL (60s), not the timeout: %d", hookStep.VirtualEndMs-hookStep.VirtualStartMs)
	}

	miss := run(30000)
	if miss.Status != RunFailed {
		t.Fatalf("delay beyond the timeout must fail the expectation, got %s", miss.Status)
	}
}
