package importer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testCollection = `{
  "info": {"name": "Test Payments", "schema": "https://schema.getpostman.com/json/collection/v2.0.0/collection.json"},
  "auth": {"type": "bearer", "bearer": [{"key": "token", "value": "{{secret}}"}]},
  "item": [
    {"name": "Charges", "item": [
      {"name": "Create charge", "request": {
        "method": "POST",
        "url": "https://api.test.example/v1/charges?expand=customer",
        "body": {"mode": "raw", "raw": "{\"amount\": 5000, \"currency\": \"NGN\", \"paid\": false, \"customer\": {\"email\": \"a@b.test\"}}"}
      }, "response": [
        {"name": "created", "code": 200, "body": "{\"id\": \"chg_1\", \"status\": \"pending\", \"amount\": 5000}"},
        {"name": "bad request", "code": 400, "body": "{\"error\": \"invalid amount\"}"}
      ]},
      {"name": "Fetch charge", "request": {
        "method": "GET",
        "url": {"raw": "https://api.test.example/v1/charges/:chargeReference", "query": [{"key": "expand", "value": "customer"}]}
      }, "response": [
        {"name": "ok", "code": 200, "body": "{\"id\": \"chg_1\", \"status\": \"paid\"}"}
      ]},
      {"name": "Broken example", "request": {
        "method": "POST",
        "url": "{{baseUrl}}/v1/charges/{{chargeId}}/refund",
        "body": {"mode": "raw", "raw": "{invalid json"}
      }, "response": [
        {"name": "accepted", "code": 202, "body": "also not json {"}
      ]}
    ]}
  ]
}`

func TestPostmanNativeConversion(t *testing.T) {
	def, err := NormalizePostman([]byte(testCollection))
	if err != nil {
		t.Fatal(err)
	}
	if len(def.Endpoints) != 3 {
		t.Fatalf("want 3 endpoints, got %d", len(def.Endpoints))
	}

	byKey := map[string]int{}
	for i, e := range def.Endpoints {
		byKey[strings.ToUpper(e.Method.Value)+" "+e.PathTemplate.Value] = i
	}
	create, ok := byKey["POST /v1/charges"]
	if !ok {
		t.Fatalf("POST /v1/charges missing: %v", keys(byKey))
	}
	fetch, ok := byKey["GET /v1/charges/{chargeReference}"]
	if !ok {
		t.Fatalf(":param must become {chargeReference}: %v", keys(byKey))
	}
	if _, ok := byKey["POST /v1/charges/{chargeId}/refund"]; !ok {
		t.Fatalf("{{var}} segment must become {chargeId}: %v", keys(byKey))
	}

	if len(def.AuthSchemes) != 1 || def.AuthSchemes[0].Kind.Value != "http" || def.AuthSchemes[0].Scheme.Value != "bearer" {
		t.Fatalf("bearer auth not carried: %+v", def.AuthSchemes)
	}
	if len(def.Endpoints[create].Security) == 0 {
		t.Fatal("global security requirement not applied to operations")
	}

	codes := map[string]bool{}
	for _, r := range def.Endpoints[create].Responses {
		codes[r.StatusCode] = true
	}
	if !codes["200"] || !codes["400"] {
		t.Fatalf("create must document 200 and 400, got %v", codes)
	}

	found := false
	for _, r := range def.Endpoints[fetch].Responses {
		if r.StatusCode != "200" {
			continue
		}
		for _, c := range r.Content {
			for _, p := range c.Schema.Properties {
				if p.Name == "status" {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatal("response schema not inferred from the JSON example")
	}

	broken := byKey["POST /v1/charges/{chargeId}/refund"]
	has202 := false
	for _, r := range def.Endpoints[broken].Responses {
		if r.StatusCode == "202" {
			has202 = true
			if len(r.Content) != 0 {
				t.Fatal("malformed example body must not produce a schema")
			}
		}
	}
	if !has202 {
		t.Fatal("documented 202 must survive a malformed body")
	}

	rb := def.Endpoints[create].RequestBody
	if rb == nil || len(rb.Content) == 0 {
		t.Fatal("request body schema not inferred")
	}
	types := map[string]string{}
	for _, p := range rb.Content[0].Schema.Properties {
		types[p.Name] = p.Schema.Type.Value
	}
	if types["amount"] != "integer" || types["paid"] != "boolean" || types["customer"] != "object" {
		t.Fatalf("inferred types wrong: %v", types)
	}
}

func keys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestPostmanProductionScaleCollection(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "parity", "importer", "specs", "synthetic-payments.postman.json"))
	if err != nil {
		t.Fatalf("synthetic collection fixture missing (run tools/gen-synthetic-fixtures.py): %v", err)
	}
	kind, err := Detect(raw)
	if err != nil || kind != KindPostman {
		t.Fatalf("detect: %v %v", kind, err)
	}
	def, err := NormalizeOpenAPI(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(def.Endpoints) < 50 {
		t.Fatalf("expected a substantial API, got %d endpoints", len(def.Endpoints))
	}
	if len(def.AuthSchemes) == 0 {
		t.Fatal("the collection's bearer auth must be carried")
	}
	has4xx := false
	for _, e := range def.Endpoints {
		for _, r := range e.Responses {
			if len(r.StatusCode) == 3 && r.StatusCode[0] == '4' {
				has4xx = true
			}
		}
	}
	if !has4xx {
		t.Fatal("documented 4xx responses must survive conversion")
	}
}
