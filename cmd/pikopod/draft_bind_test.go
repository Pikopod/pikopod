package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pikopod/pikopod/internal/config"
	"github.com/pikopod/pikopod/internal/importer"
	"github.com/pikopod/pikopod/internal/store"
)

const draftPaySpec = `{"openapi":"3.1.0","info":{"title":"Pay","version":"1"},
"paths":{
  "/payment-intents":{"post":{"operationId":"createIntent","responses":{"201":{"description":"created"}}}},
  "/payment-intents/{id}":{"get":{"operationId":"getIntent","responses":{"200":{"description":"ok"}}}}
}}`

// draftSandbox registers "pay" the way a docs import does: every fact extracted.
func draftSandbox(t *testing.T) *config.Config {
	t.Helper()
	cfg := testConfig(t, "https://example.invalid")
	if err := sandboxAdd(cfg, "pay", writeSpec(t, draftPaySpec), "draft-seed", "", "", false, io.Discard); err != nil {
		t.Fatal(err)
	}
	def, err := importer.NormalizeLLMExtracted([]byte(draftPaySpec))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(def)
	entry, _, err := loadSandboxDef(cfg, "pay")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteFileAtomic(filepath.Join(cfg.DataDir, entry.IRFile), raw); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestScenarioListOffersTheBindLineOnADraft(t *testing.T) {
	cfg := draftSandbox(t)
	var out strings.Builder
	if err := scenarioList(cfg, "pay", false, &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"extracted facts", "assert it: pikopod scenario run pay declines --bind op=createIntent"} {
		if !strings.Contains(got, want) {
			t.Errorf("list must show %q:\n%s", want, got)
		}
	}
}

func TestModeSetWithBindGroundsADraftAndSaysSo(t *testing.T) {
	cfg := draftSandbox(t)
	sbx, err := newSandboxServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sbx.Close() })
	srv := httptest.NewServer(sbx)
	t.Cleanup(srv.Close)

	res, err := http.Post(srv.URL+"/_pikopod/sandboxes/pay/mode", "application/json", strings.NewReader(`{"name":"declines"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("unasserted mode on a draft = %d, want 400", res.StatusCode)
	}
	res, err = http.Post(srv.URL+"/_pikopod/sandboxes/pay/mode", "application/json", strings.NewReader(`{"name":"declines","bind":{"op":"createIntent"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("asserted mode = %d: %v", res.StatusCode, readJSON(t, res)["message"])
	}
	mode, _ := readJSON(t, res)["mode"].(map[string]any)
	if src, _ := mode["source"].(string); !strings.Contains(src, "you asserted op") {
		t.Fatalf("the mode must say it rests on an asserted fact: %v", mode)
	}
	create, err := http.Post(srv.URL+"/pay/payment-intents", "application/json", strings.NewReader(`{"amount":1}`))
	if err != nil {
		t.Fatal(err)
	}
	create.Body.Close()
	if create.StatusCode != 400 {
		t.Fatalf("with declines standing, create = %d, want 400", create.StatusCode)
	}
}
