package scenario

import (
	"encoding/json"
	"testing"

	"github.com/pikopod/pikopod/internal/importer"
	"github.com/pikopod/pikopod/internal/sandbox"
)

const emitHookSpec = `{"openapi":"3.1.0","info":{"title":"Pay","version":"1"},
"paths":{"/x":{"get":{"responses":{"200":{"description":"ok"}}}}},
"webhooks":{"transaction.created":{"post":{
  "x-pikopod-emit-only":true,
  "requestBody":{"content":{"application/json":{"schema":{"type":"object","properties":{
    "event":{"type":"string"},
    "transaction":{"type":"object","properties":{"amount":{"type":"number"},"currency":{"type":"string"}}}
  }}}}},
  "responses":{"200":{"description":"ack"}}}}}}`

func emitEngine(t *testing.T, id string) *sandbox.Engine {
	t.Helper()
	def, err := importer.NormalizeOpenAPI([]byte(emitHookSpec))
	if err != nil {
		t.Fatal(err)
	}
	st, err := sandbox.OpenMemoryStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	eng, err := sandbox.NewEngine(def, sandbox.Config{ID: id, Seed: "emit-seed"}, st)
	if err != nil {
		t.Fatal(err)
	}
	return eng
}

func TestEmitWebhookStepFiresAndIsObservable(t *testing.T) {
	def := parseDef(t, `{
	  "steps": [
	    {"key": "fire", "type": "EMIT_WEBHOOK",
	     "config": {"event": "transaction.created", "data": {"amount": 71717, "currency": "TZS"}}},
	    {"key": "got", "type": "EXPECT_WEBHOOK",
	     "config": {"match": {"eventType": "transaction.created"}, "timeoutMs": 1000},
	     "assertions": [
	       {"target": "webhook.delivery", "path": "$.transaction.amount", "op": "equals", "expected": 71717},
	       {"target": "webhook.delivery", "path": "$.transaction.currency", "op": "equals", "expected": "TZS"}
	     ]}
	  ]
	}`)
	res, err := Run(emitEngine(t, "sbx_emit_1"), def, nil, "emit-1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != RunPassed {
		t.Fatalf("emit then expect must pass: %s (%s)", res.Status, res.Summary)
	}
}

func TestEmitWebhookStepRefusesUndeclaredEvent(t *testing.T) {
	def := parseDef(t, `{
	  "steps": [
	    {"key": "fire", "type": "EMIT_WEBHOOK", "config": {"event": "nope.event"}}
	  ]
	}`)
	res, err := Run(emitEngine(t, "sbx_emit_2"), def, nil, "emit-2")
	if err == nil && res != nil && res.Status == RunPassed {
		t.Fatal("an undeclared event must not produce a passing run")
	}
}

func TestEmitWebhookDataMustBeAnObject(t *testing.T) {
	var raw any
	if err := json.Unmarshal([]byte(`{"steps":[{"key":"fire","type":"EMIT_WEBHOOK","config":{"event":"transaction.created","data":"not-an-object"}}]}`), &raw); err != nil {
		t.Fatal(err)
	}
	if _, errs := ParseDefinition(raw); len(errs) == 0 {
		t.Fatal("a non-object data value must fail validation")
	}
}
