package docimport

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pikopod/pikopod/internal/scenario/nl"
)

type sized struct {
	n   int
	url string
}

func (s sized) size() int    { return s.n }
func (s sized) name() string { return s.url }

// The observed failure: a large reference section filled the budget before
// the webhook pages, which carry the only event facts.
func TestPlanCorpusKeepsWebhookPagesUnderReferencePressure(t *testing.T) {
	hooks := []sized{{8000, "hooks/overview"}, {40000, "hooks/event-types"}}
	var api []sized
	for i := 0; i < 20; i++ {
		api = append(api, sized{40000, "api/" + string(rune('a'+i))})
	}
	rest := []sized{{5000, "guide/x"}, {5000, "guide/y"}}
	chosen, skipped := planCorpus([][]sized{hooks, api, rest}, 240<<10, corpusShares)
	names := map[string]bool{}
	total := 0
	for _, c := range chosen {
		names[c.url] = true
		total += c.n
	}
	if !names["hooks/overview"] || !names["hooks/event-types"] {
		t.Fatalf("webhook pages must survive: %v", names)
	}
	if !names["guide/x"] {
		t.Fatalf("the guides share must be honoured: %v", names)
	}
	if total > 240<<10 {
		t.Fatalf("budget exceeded: %d", total)
	}
	if len(skipped) == 0 || !strings.HasPrefix(skipped[0], "api/") {
		t.Fatalf("the pages that did not fit must be reported, reference first: %v", skipped)
	}
	if len(chosen)+len(skipped) != len(hooks)+len(api)+len(rest) {
		t.Fatal("every page is either chosen or reported")
	}
}

func TestPlanCorpusSpendsLeftoverInGroupOrder(t *testing.T) {
	hooks := []sized{{10, "h1"}}
	api := []sized{{10, "a1"}, {10, "a2"}, {10, "a3"}}
	chosen, skipped := planCorpus([][]sized{hooks, api, nil}, 100, corpusShares)
	if len(chosen) != 4 || len(skipped) != 0 {
		t.Fatalf("everything fits, all must be chosen: %v %v", chosen, skipped)
	}
}

// Rung 3b: a platform that publishes its spec at a well-known path needs no
// model at all.
func TestLadderWellKnownSpecPath(t *testing.T) {
	prose := []byte(`<html><body><p>just words</p></body></html>`)
	spec := []byte(`{"openapi":"3.0.0","info":{"title":"api","version":"1"},"paths":{}}`)
	res, err := FromDocsURL("https://docs.x.test/intro", prose,
		fetcherFor(map[string][]byte{"https://docs.x.test/openapi.json": spec}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Method != "well-known-spec" || res.Source != "https://docs.x.test/openapi.json" {
		t.Fatalf("wrong rung: %s %s", res.Method, res.Source)
	}
	html := fetcherFor(map[string][]byte{"https://docs.x.test/openapi.json": []byte("<!doctype html><html>not a spec</html>")})
	if _, err := FromDocsURL("https://docs.x.test/intro", prose, html, nil); err == nil {
		t.Fatal("an HTML page at the well-known path must not be taken for a spec")
	}
}

func TestLadderWellKnownSpecFromLlmsTxt(t *testing.T) {
	prose := []byte(`<html><body><p>words</p></body></html>`)
	pages := map[string][]byte{
		"https://docs.x.test/llms.txt":            []byte("# X\n- [Spec](https://docs.x.test/static/openapi.yaml): the spec\n"),
		"https://docs.x.test/static/openapi.yaml": []byte("openapi: 3.0.0\ninfo:\n  title: x\n  version: '1'\npaths: {}\n"),
	}
	res, err := FromDocsURL("https://docs.x.test/intro", prose, fetcherFor(pages), nil)
	if err != nil || res.Method != "well-known-spec" {
		t.Fatalf("llms.txt spec link must be used: %v %+v", err, res)
	}
}

// Skipped pages reach the result so the CLI can say the spec is partial.
func TestLLMExtractionReportsSkippedPages(t *testing.T) {
	big := strings.Repeat("reference text ", 20000) // ~300 KB, over the whole budget
	pages := map[string][]byte{
		"https://docs.x.test/llms.txt":          []byte("- [A](https://docs.x.test/api-reference/a)\n- [B](https://docs.x.test/api-reference/b)\n- [W](https://docs.x.test/webhooks/overview)\n"),
		"https://docs.x.test/api-reference/a":   []byte(big),
		"https://docs.x.test/api-reference/b":   []byte("small reference"),
		"https://docs.x.test/webhooks/overview": []byte("signature scheme"),
	}
	spec := `{"openapi":"3.0.0","info":{"title":"api","version":"1"},"paths":{}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": spec}}}})
	}))
	defer srv.Close()
	llm := nl.NewClient("test-key", "test-model")
	llm.BaseURL = srv.URL
	res, err := FromDocsURL("https://docs.x.test/intro", []byte(`<html><body>x</body></html>`), fetcherFor(pages), llm)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Skipped) != 1 || res.Skipped[0] != "https://docs.x.test/api-reference/a" {
		t.Fatalf("the oversized page must be reported as skipped: %v", res.Skipped)
	}
}
