package docimport

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pikopod/pikopod/internal/importer"
	"github.com/pikopod/pikopod/internal/scenario/nl"
)

func fetcherFor(pages map[string][]byte) Fetcher {
	return func(url string) ([]byte, error) {
		if raw, ok := pages[url]; ok {
			return raw, nil
		}
		return nil, fmt.Errorf("no such page: %s", url)
	}
}

func TestLadderDocumenterLink(t *testing.T) {
	page := []byte(`<html><body><a href="https://docs.x.test/api/collections/1/AbC?versionTag=latest">collection</a></body></html>`)
	collection := []byte(`{"info":{"name":"X","schema":"https://schema.getpostman.com/json/collection/v2.0.0/collection.json"},"item":[]}`)
	res, err := FromDocsURL("https://docs.x.test", page,
		fetcherFor(map[string][]byte{"https://docs.x.test/api/collections/1/AbC?versionTag=latest": collection}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Method != "postman-documenter" || string(res.Spec) != string(collection) {
		t.Fatalf("wrong result: %s", res.Method)
	}
}

func TestLadderReadmeEmbedded(t *testing.T) {
	embedded := `{"openapi":"3.1.0","info":{"title":"t","version":"1"},"paths":{"/wallets?businessID={businessId}":{"get":{"responses":{"200":{"description":"ok"}}}},"/charges/":{"post":{"responses":{"201":{"description":"c"}}}}}}`
	refPage := []byte(`<html><body>x "schema":` + embedded + ` y</body></html>`)
	prosePage := []byte(`<html><body><a href="/reference">API Reference</a></body></html>`)
	res, err := FromDocsURL("https://docs.x.test/docs/start", prosePage,
		fetcherFor(map[string][]byte{"https://docs.x.test/reference": refPage}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Method != "readme-embedded" {
		t.Fatalf("method: %s", res.Method)
	}
	var doc map[string]any
	json.Unmarshal(res.Spec, &doc)
	paths := doc["paths"].(map[string]any)
	if _, ok := paths["/wallets"]; !ok {
		t.Fatalf("path key not cleaned: %v", keysOf(paths))
	}
	if _, ok := paths["/charges"]; !ok {
		t.Fatalf("trailing slash not stripped: %v", keysOf(paths))
	}
	get := paths["/wallets"].(map[string]any)["get"].(map[string]any)
	params, _ := get["parameters"].([]any)
	if len(params) != 1 || params[0].(map[string]any)["name"] != "businessID" {
		t.Fatalf("query template not promoted to parameter: %v", params)
	}

	if _, err := importer.NormalizeOpenAPI(res.Spec); err != nil {
		t.Fatalf("extracted spec does not normalize: %v", err)
	}
}

func TestLadderLLMExtraction(t *testing.T) {
	prose := []byte(`<html><body><h1>API</h1><p>POST /v1/things requires name.</p></body></html>`)

	spec := `{"openapi":"3.0.0","info":{"title":"api","version":"1"},"paths":{"/v1/things":{"post":{"requestBody":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/Thing"}}}},"responses":{"201":{"description":"c"}}}}},"components":{"schemas":{"Thing":{"type":"object","required":["name"],"properties":{"name":{"type":"string"}}}}}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": spec}}}})
	}))
	defer srv.Close()
	llm := nl.NewClient("test-key", "test-model")
	llm.BaseURL = srv.URL

	res, err := FromDocsURL("https://docs.x.test/guide", prose, fetcherFor(nil), llm)
	if err != nil {
		t.Fatal(err)
	}
	if res.Method != "llm-extracted" {
		t.Fatalf("method: %s", res.Method)
	}
	var doc map[string]any
	json.Unmarshal(res.Spec, &doc)
	schema := dig(doc, "paths", "/v1/things", "post", "requestBody", "content", "application/json", "schema")
	if schema["$ref"] != nil {
		t.Fatal("request-body $ref must be inlined")
	}
	req, _ := schema["required"].([]any)
	if len(req) != 1 || req[0] != "name" {
		t.Fatalf("inlined schema lost required: %v", schema)
	}
	if _, err := importer.NormalizeOpenAPI(res.Spec); err != nil {
		t.Fatalf("LLM spec does not normalize: %v", err)
	}
}

func TestLadderNoLLMKeyFailsLoudly(t *testing.T) {
	prose := []byte(`<html><body><p>just words</p></body></html>`)
	_, err := FromDocsURL("https://docs.x.test/guide", prose, fetcherFor(nil), nil)
	if err == nil || !strings.Contains(err.Error(), "model-assisted") {
		t.Fatalf("want the BYOK guidance error, got: %v", err)
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func dig(m map[string]any, path ...string) map[string]any {
	cur := m
	for _, p := range path {
		next, _ := cur[p].(map[string]any)
		if next == nil {
			return map[string]any{}
		}
		cur = next
	}
	return cur
}
