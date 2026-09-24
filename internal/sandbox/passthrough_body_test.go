package sandbox

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/pikopod/pikopod/internal/importer"
)

const passthroughSpec = `{"openapi":"3.1.0","info":{"title":"P","version":"1"},"paths":{
"/v1/charges/{id}":{"post":{"responses":{"200":{"description":"ok","content":{"application/json":{
  "schema":{"type":"object","required":["id","captured"],"properties":{"id":{"type":"string"},"captured":{"type":"boolean"}}},
  "example":{"id":"ch_1","captured":true,"amount_captured":1250}}}}}}},
"/v1/charges/bulk":{"put":{"responses":{"200":{"description":"ok","content":{"application/json":{
  "schema":{"type":"object","required":["accepted","batch"],"properties":{"accepted":{"type":"integer"},"batch":{"type":"string"}}}}}}}}},
"/v1/charges/purge":{"delete":{"responses":{"200":{"description":"ok"}}}},
"/v1/charges/reindex":{"patch":{"responses":{"200":{"description":"ok","content":{"application/json":{"schema":{"type":"array","items":{"type":"string"}}}}}}}},
"/v1/charges/flush":{"patch":{"responses":{"204":{"description":"gone"}}}}
}}`

func passthroughEngine(t *testing.T, id, seed string) *Engine {
	t.Helper()
	def, err := importer.NormalizeOpenAPI([]byte(passthroughSpec))
	if err != nil {
		t.Fatal(err)
	}
	return newEngine(t, def, Config{ID: id, Seed: seed})
}

func TestPassthroughUsesDeclaredExample(t *testing.T) {
	r := do(t, passthroughEngine(t, "pt_ex", "s"), "POST", "/v1/charges/ch_1", `{}`, nil)
	var got, want any
	json.Unmarshal([]byte(r.body), &got)
	json.Unmarshal([]byte(`{"id":"ch_1","captured":true,"amount_captured":1250}`), &want)
	if r.status != 200 || !reflect.DeepEqual(got, want) {
		t.Fatalf("want the declared example, got %d %s", r.status, r.body)
	}
}

func TestPassthroughSynthesisesFromSchema(t *testing.T) {
	r := do(t, passthroughEngine(t, "pt_schema", "s"), "PUT", "/v1/charges/bulk", `{}`, nil)
	var body map[string]any
	if err := json.Unmarshal([]byte(r.body), &body); err != nil || r.body == "{}" {
		t.Fatalf("want a synthesized body, got %d %s", r.status, r.body)
	}
	if _, ok := body["accepted"].(float64); !ok {
		t.Fatalf("accepted must be an integer: %s", r.body)
	}
	if _, ok := body["batch"].(string); !ok {
		t.Fatalf("batch must be a string: %s", r.body)
	}
}

func TestPassthroughFallsBackToEmptyObject(t *testing.T) {
	e := passthroughEngine(t, "pt_empty", "s")
	if r := do(t, e, "DELETE", "/v1/charges/purge", ``, nil); r.body != "{}" {
		t.Fatalf("no content declared must still answer {}: %s", r.body)
	}
	if r := do(t, e, "PATCH", "/v1/charges/reindex", `{}`, nil); !strings.HasPrefix(r.body, "[") {
		t.Fatalf("an array schema must answer an array: %s", r.body)
	}
}

func TestPassthrough204Unchanged(t *testing.T) {
	r := do(t, passthroughEngine(t, "pt_204", "s"), "PATCH", "/v1/charges/flush", `{}`, nil)
	if r.status != 204 || r.body != "" || r.headers["content-type"] != "" {
		t.Fatalf("204 must stay bodiless: %d %q %v", r.status, r.body, r.headers)
	}
}

func TestPassthroughDeterministic(t *testing.T) {
	a := do(t, passthroughEngine(t, "pt_d1", "seed-a"), "PUT", "/v1/charges/bulk", `{}`, nil)
	b := do(t, passthroughEngine(t, "pt_d2", "seed-a"), "PUT", "/v1/charges/bulk", `{}`, nil)
	c := do(t, passthroughEngine(t, "pt_d3", "seed-b"), "PUT", "/v1/charges/bulk", `{}`, nil)
	if a.body != b.body {
		t.Fatalf("same seed must replay byte for byte:\n%s\n%s", a.body, b.body)
	}
	if a.body == c.body {
		t.Fatalf("a different seed must synthesize differently: %s", a.body)
	}
	x := do(t, passthroughEngine(t, "pt_d4", "seed-a"), "POST", "/v1/charges/ch_1", `{}`, nil)
	y := do(t, passthroughEngine(t, "pt_d5", "seed-b"), "POST", "/v1/charges/ch_1", `{}`, nil)
	if x.body != y.body {
		t.Fatal("a declared example does not depend on the seed")
	}
}

const wrappedCreateSpec = `{"openapi":"3.1.0","info":{"title":"P","version":"1"},"paths":{
"/payment-intents":{"post":{
  "requestBody":{"content":{"application/json":{"schema":{"type":"object","properties":{"amount":{"type":"integer"},"currency":{"type":"string"}}}}}},
  "responses":{"200":{"description":"ok","content":{"application/json":{"schema":{"type":"object","properties":{
    "data":{"type":"object","properties":{"id":{"type":"string"},"amount":{"type":"integer"},"currency":{"type":"string"},"status":{"type":"string"}}},
    "message":{"type":"string"}}}}}}}}},
"/payment-intents/{id}":{"get":{"responses":{"200":{"description":"ok","content":{"application/json":{"schema":{"type":"object","properties":{
    "data":{"type":"object","properties":{"id":{"type":"string"},"amount":{"type":"integer"},"currency":{"type":"string"},"status":{"type":"string"}}},
    "message":{"type":"string"}}}}}}}}}
}}`

func TestCreateLandsInsideTheDeclaredEnvelope(t *testing.T) {
	def, err := importer.NormalizeOpenAPI([]byte(wrappedCreateSpec))
	if err != nil {
		t.Fatal(err)
	}
	e := newEngine(t, def, Config{ID: "env_create", Seed: "s"})
	r := do(t, e, "POST", "/payment-intents", `{"amount":1000,"currency":"USD"}`, nil)
	var body map[string]any
	if err := json.Unmarshal([]byte(r.body), &body); err != nil {
		t.Fatal(err)
	}
	data, _ := body["data"].(map[string]any)
	if data == nil || data["amount"] != 1000.0 || data["currency"] != "USD" {
		t.Fatalf("the resource must sit inside data: %s", r.body)
	}
	if _, top := body["amount"]; top {
		t.Fatalf("the resource must not also be merged beside the envelope: %s", r.body)
	}
	id, _ := data["id"].(string)
	if id == "" {
		t.Fatalf("data.id must be the stored key: %s", r.body)
	}
	if _, ok := body["message"].(string); !ok {
		t.Fatalf("envelope fields outside the resource are still synthesized: %s", r.body)
	}
	back := do(t, e, "GET", "/payment-intents/"+id, ``, nil)
	var read map[string]any
	if err := json.Unmarshal([]byte(back.body), &read); err != nil {
		t.Fatal(err)
	}
	rd, _ := read["data"].(map[string]any)
	if back.status != 200 || rd == nil || rd["amount"] != 1000.0 || rd["id"] != id {
		t.Fatalf("read-back must show the same resource in the same envelope: %d %s", back.status, back.body)
	}
}
