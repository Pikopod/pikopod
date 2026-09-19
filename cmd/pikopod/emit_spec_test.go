package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pikopod/pikopod/internal/ir"
)

const docsSiteSpec = `{"openapi":"3.1.0","info":{"title":"Pay","version":"1"},
"paths":{"/intents":{"post":{"operationId":"createIntent","responses":{"201":{"description":"created"}}}}},
"webhooks":{
  "intent.completed":{"post":{"responses":{"200":{"description":"ack"}}}},
  "intent.failed":{"post":{"responses":{"200":{"description":"ack"}}}}
}}`

// A docs site with nothing but prose on the page and the spec at a
// well-known path: the platform rung, no model.
func docsSite(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/openapi.json":
			w.Write([]byte(docsSiteSpec))
		case "/intro":
			w.Write([]byte(`<!doctype html><html><body><p>Welcome to the API.</p></body></html>`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestEmitSpecWritesTheFetchedSpecAndReimportIsDeterministic(t *testing.T) {
	cfg := testConfig(t, "https://example.invalid")
	site := docsSite(t)
	emitted := filepath.Join(t.TempDir(), "pay.openapi.json")
	var out strings.Builder
	if err := sandboxAddOpts(cfg, "pay", addOptions{SpecSource: site.URL + "/intro", Seed: "s1", EmitSpec: emitted}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "well-known-spec") || !strings.Contains(out.String(), "spec written to "+emitted) {
		t.Fatalf("import must say which rung answered and where the spec went:\n%s", out.String())
	}
	raw, err := os.ReadFile(emitted)
	if err != nil || string(raw) != docsSiteSpec {
		t.Fatalf("a fetched spec is written verbatim: %v %s", err, raw)
	}
	_, first, err := loadSandboxDef(cfg, "pay")
	if err != nil {
		t.Fatal(err)
	}
	if err := sandboxUpdate(cfg, "pay", emitted, io.Discard); err != nil {
		t.Fatal(err)
	}
	_, second, _ := loadSandboxDef(cfg, "pay")
	h1, _ := ir.NormalizedHash(first)
	h2, _ := ir.NormalizedHash(second)
	if h1 != h2 {
		t.Fatalf("re-importing the emitted file must reproduce the contract: %s vs %s", h1, h2)
	}
}

// An emitted extraction carries a marker; it stays DRAFT until a person
// removes it.
func TestEmittedExtractionReimportsAsDraftUntilMarkerRemoved(t *testing.T) {
	cfg := testConfig(t, "https://example.invalid")
	marked := strings.Replace(docsSiteSpec, `"openapi":"3.1.0",`, `"openapi":"3.1.0","x-pikopod-origin":"llm-extracted",`, 1)
	var out strings.Builder
	if err := sandboxAdd(cfg, "pay", writeSpec(t, marked), "s2", "", "", false, &out); err != nil {
		t.Fatal(err)
	}
	_, def, _ := loadSandboxDef(cfg, "pay")
	if !def.Endpoints[0].Method.IsUncertain() {
		t.Fatal("a marked spec must import with extracted provenance")
	}
	var list strings.Builder
	sandboxList(cfg, &list)
	if !strings.Contains(list.String(), "DRAFT") {
		t.Fatalf("list must show the DRAFT marking:\n%s", list.String())
	}
	if err := sandboxUpdate(cfg, "pay", writeSpec(t, docsSiteSpec), io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, def, _ = loadSandboxDef(cfg, "pay"); def.Endpoints[0].Method.IsUncertain() {
		t.Fatal("without the marker the facts are the reviewer's: explicit")
	}
}

const eventsSidecar = `events:
  intent.completed: { trigger: { method: post, path: /intents } }
  intent.failed: { emitOnly: true }
`

func TestSidecarEventsBindWithoutReimport(t *testing.T) {
	cfg := testConfig(t, "https://example.invalid")
	if err := sandboxAdd(cfg, "pay", writeSpec(t, docsSiteSpec), "s3", "", "", false, io.Discard); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := applyWebhookSidecar(cfg, "pay", writeSidecar(t, eventsSidecar), &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"intent.completed fires on POST /intents", "intent.failed is emit-only"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q:\n%s", want, out.String())
		}
	}
	var list strings.Builder
	sandboxList(cfg, &list)
	if !strings.Contains(list.String(), "webhooks: 2 declared, 1 triggered, 1 emit-only") {
		t.Fatalf("bindings must show in list:\n%s", list.String())
	}
	if err := sandboxUpdate(cfg, "pay", writeSpec(t, docsSiteSpec), io.Discard); err != nil {
		t.Fatal(err)
	}
	_, def, _ := loadSandboxDef(cfg, "pay")
	bound := 0
	for _, w := range def.Webhooks {
		if w.Trigger != nil || w.EmitOnly {
			bound++
		}
	}
	if bound != 2 {
		t.Fatalf("a re-import must keep the person's bindings, got %d", bound)
	}
}

func TestSidecarEventsRefuseTheUndeclaredAndTheUnknownOperation(t *testing.T) {
	cfg := testConfig(t, "https://example.invalid")
	if err := sandboxAdd(cfg, "pay", writeSpec(t, docsSiteSpec), "s4", "", "", false, io.Discard); err != nil {
		t.Fatal(err)
	}
	err := applyWebhookSidecar(cfg, "pay", writeSidecar(t, "events:\n  nope.event: { emitOnly: true }\n"), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "nope.event") {
		t.Fatalf("an undeclared event must be refused by name: %v", err)
	}
	err = applyWebhookSidecar(cfg, "pay", writeSidecar(t, "events:\n  intent.completed: { trigger: { method: post, path: /nothing } }\n"), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "POST /nothing") {
		t.Fatalf("an unknown operation must be refused by name: %v", err)
	}
	if _, def, _ := loadSandboxDef(cfg, "pay"); def.Webhooks[0].Trigger != nil || def.Webhooks[0].EmitOnly {
		t.Fatal("a refused sidecar must not be persisted")
	}
}
