package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// One triggered event, one declared with neither trigger nor emit-only.
const hookSpecUntriggered = `{"openapi":"3.1.0","info":{"title":"Bank","version":"1"},
"paths":{"/virtual-accounts":{"post":{"responses":{"201":{"description":"created"}}}}},
"webhooks":{
  "virtualaccount.approved":{"post":{"x-pikopod-trigger":{"method":"post","path":"/virtual-accounts"},"responses":{"200":{"description":"ack"}}}},
  "settlement.report":{"post":{"responses":{"200":{"description":"ack"}}}}
}}`

// Same, with the orphan marked emit-only.
const hookSpecEmitOnly = `{"openapi":"3.1.0","info":{"title":"Bank","version":"1"},
"paths":{"/virtual-accounts":{"post":{"responses":{"201":{"description":"created"}}}}},
"webhooks":{
  "virtualaccount.approved":{"post":{"x-pikopod-trigger":{"method":"post","path":"/virtual-accounts"},"responses":{"200":{"description":"ack"}}}},
  "settlement.report":{"post":{"x-pikopod-emit-only":true,"responses":{"200":{"description":"ack"}}}}
}}`

func writeSpec(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "bank.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// A declared event that can never fire is a fact worth stating at import,
// with both ways to fix it.
func TestImportWarnsOnUntriggerableWebhook(t *testing.T) {
	cfg := testConfig(t, "https://example.invalid")
	var out strings.Builder
	if err := sandboxAdd(cfg, "bank", writeSpec(t, hookSpecUntriggered), "seed-w1", "", "", false, &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"1 declared event(s) have no trigger", "x-pikopod-trigger", "x-pikopod-emit-only"} {
		if !strings.Contains(got, want) {
			t.Errorf("import output missing %q:\n%s", want, got)
		}
	}
}

func TestEmitOnlySilencesTheWarningAndShowsInList(t *testing.T) {
	cfg := testConfig(t, "https://example.invalid")
	var out strings.Builder
	if err := sandboxAdd(cfg, "bank", writeSpec(t, hookSpecEmitOnly), "seed-w2", "", "", false, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "never fire") {
		t.Fatalf("an emit-only event must not be reported as untriggerable:\n%s", out.String())
	}
	var list strings.Builder
	if err := sandboxList(cfg, &list); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(list.String(), "webhooks: 2 declared, 1 triggered, 1 emit-only") {
		t.Fatalf("list must show the counts:\n%s", list.String())
	}
}

func emitServer(t *testing.T) *httptest.Server {
	t.Helper()
	cfg := testConfig(t, "https://example.invalid")
	if err := sandboxAdd(cfg, "bank", writeSpec(t, hookSpecEmitOnly), "seed-w3", "", "", false, io.Discard); err != nil {
		t.Fatal(err)
	}
	sbx, err := newSandboxServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sbx.Close() })
	srv := httptest.NewServer(sbx)
	t.Cleanup(srv.Close)
	return srv
}

func TestControlPlaneEmitRefusesUndeclaredEvent(t *testing.T) {
	srv := emitServer(t)
	res, err := http.Post(srv.URL+"/_pikopod/sandboxes/bank/webhooks/emit", "application/json",
		strings.NewReader(`{"event":"nope.event"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("undeclared emit = %d, want 400", res.StatusCode)
	}
	if msg, _ := readJSON(t, res)["message"].(string); !strings.Contains(msg, "nope.event") {
		t.Fatalf("refusal must name the event: %s", msg)
	}
}

func TestControlPlaneEmitFiresDeclaredEvent(t *testing.T) {
	srv := emitServer(t)
	res, err := http.Post(srv.URL+"/_pikopod/sandboxes/bank/webhooks/emit", "application/json",
		strings.NewReader(`{"event":"settlement.report","data":{"total":12}}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("declared emit = %d: %v", res.StatusCode, readJSON(t, res)["message"])
	}
}

func TestControlPlaneListsDeliveriesAndSinkOutcome(t *testing.T) {
	srv := emitServer(t)
	if _, err := http.Post(srv.URL+"/_pikopod/sandboxes/bank/webhooks/emit", "application/json", strings.NewReader(`{"event":"settlement.report"}`)); err != nil {
		t.Fatal(err)
	}
	res, err := http.Get(srv.URL + "/_pikopod/sandboxes/bank/webhooks")
	if err != nil {
		t.Fatal(err)
	}
	body := readJSON(t, res)
	deliveries, _ := body["deliveries"].([]any)
	if res.StatusCode != http.StatusOK || len(deliveries) != 1 {
		t.Fatalf("want one listed delivery, got %d (status %d): %v", len(deliveries), res.StatusCode, body)
	}
	if first, _ := deliveries[0].(map[string]any); first["event"] != "settlement.report" {
		t.Fatalf("listed delivery must name the event: %v", deliveries[0])
	}
	if _, ok := body["sink"].(map[string]any); !ok {
		t.Fatalf("sink stats missing: %v", body)
	}
}
