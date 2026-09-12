package scenario_test

import (
	"encoding/json"
	"testing"

	"github.com/pikopod/pikopod/internal/importer"
	"github.com/pikopod/pikopod/internal/ir"
	"github.com/pikopod/pikopod/internal/sandbox"
	"github.com/pikopod/pikopod/internal/scenario"
	"github.com/pikopod/pikopod/internal/scenario/archetype"
)

// A minimal spec that DECLARES webhooks (OpenAPI 3.1 top-level `webhooks`) —
// the catalogue's duplicate_delivery archetype requires a webhookEvent role,
// so it only binds against an IR like this one.
const webhookArchetypeSpec = `{
  "openapi": "3.1.0",
  "info": {"title": "Orders", "version": "1.0.0"},
  "webhooks": {
    "orders.created": {"post": {"requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Order"}}}}, "responses": {"200": {"description": "ack"}}}}
  },
  "paths": {
    "/orders": {
      "post": {
        "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Order"}}}},
        "responses": {"201": {"description": "created", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Order"}}}}}
      },
      "get": {"responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {
        "type": "object",
        "properties": {"items": {"type": "array", "items": {"$ref": "#/components/schemas/Order"}}}
      }}}}}}
    }
  },
  "components": {"schemas": {"Order": {
    "type": "object",
    "properties": {"id": {"type": "string"}, "name": {"type": "string"}}
  }}}
}`

func webhookArchetypeIR(t *testing.T) *ir.ApiDefinition {
	t.Helper()
	def, err := importer.NormalizeOpenAPI([]byte(webhookArchetypeSpec))
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if len(def.Webhooks) != 1 || def.Webhooks[0].Event.Value != "orders.created" {
		t.Fatalf("importer must surface the declared webhook, got %+v", def.Webhooks)
	}
	return def
}

// The catalogue's duplicate_delivery archetype — dormant until the outbox
// landed because no IR role could satisfy `bind: webhookEvent` at runtime —
// now binds, expands, and RUNS against a webhook-declaring spec. The emitted
// event (`orders.created` = typeSlug.action) matches the declared one, so
// EXPECT_WEBHOOK observes the create's delivery, and the run is
// deterministic.
func TestDuplicateDeliveryArchetypeEndToEnd(t *testing.T) {
	apiDef := webhookArchetypeIR(t)
	a := archetype.Find("duplicate_delivery")
	if a == nil {
		t.Fatal("duplicate_delivery must exist in the catalogue")
	}

	binding := archetype.Bind(a, apiDef)
	if !binding.Applicable {
		t.Fatalf("must bind against a webhook-declaring IR: %s", binding.Reason)
	}
	if got := binding.Candidates[0].Bindings["emittedEvent"]; got != "orders.created" {
		t.Fatalf("webhookEvent role must bind the declared event, got %q", got)
	}

	exp, err := archetype.Expand(a, binding.Candidates[0].Bindings, apiDef)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	def, errs := scenario.ParseDefinition(exp.Definition)
	if len(errs) > 0 {
		t.Fatalf("expansion does not parse: %+v", errs)
	}
	vr, _ := scenario.ValidateScenario(exp.Definition, apiDef)
	if !vr.Valid {
		t.Fatalf("expansion does not ground: %+v", vr.Errors)
	}

	run := func() *scenario.RunResult {
		store, err := sandbox.OpenMemoryStore()
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
		defer store.Close()
		eng, err := sandbox.NewEngine(apiDef, sandbox.Config{ID: "sbx_dupdel", Seed: "seed-dup"}, store)
		if err != nil {
			t.Fatalf("new engine: %v", err)
		}
		res, err := scenario.Run(eng, def, nil, "seed-dup")
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		return res
	}
	r1, r2 := run(), run()
	if r1.Status != scenario.RunPassed {
		steps, _ := json.MarshalIndent(r1.Steps, "", " ")
		t.Fatalf("duplicate_delivery must PASS: %s (%s)\nsteps: %s", r1.Status, r1.Summary, steps)
	}
	if r1.ResultHash != r2.ResultHash {
		t.Fatalf("run must be deterministic: %s vs %s", r1.ResultHash, r2.ResultHash)
	}
	// The first EXPECT_WEBHOOK observed the create's delivery (matched 1), so
	// it consumed no timeout; the CLIENT-subject count assertion in the last
	// step stays NOT_EVALUATED locally (no client binding), never a failure.
	await1 := r1.Steps[1]
	if await1.Type != "EXPECT_WEBHOOK" || await1.Detail["matched"].(int) != 1 {
		t.Fatalf("await1 must observe the create's delivery: %+v", await1)
	}
	if await1.VirtualEndMs != await1.VirtualStartMs {
		t.Fatal("an observed delivery must not burn the timeout")
	}
}
