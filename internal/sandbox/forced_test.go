package sandbox

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestForcedStatus(t *testing.T) {
	e := newEngine(t, loadSpecDef(t, declaredErrorSpec), Config{ID: "sbx_f", Seed: "f-1"})

	got := do(t, e, "POST", "/charges", `{"amount":5}`, map[string]string{ForcedStatusHeader: "422"})
	if got.status != 422 || got.headers[forcedMarkerHeader] != "true" {
		t.Fatalf("declared status must force: %d %v", got.status, got.headers)
	}
	var body map[string]any
	json.Unmarshal([]byte(got.body), &body)
	if _, ok := body["error"]; !ok {
		t.Fatalf("forced body must be the declared shape: %s", got.body)
	}

	got2 := do(t, e, "POST", "/charges", `{"amount":5}`, map[string]string{ForcedStatusHeader: "422"})
	if got.body != got2.body {
		t.Fatal("forced synthesis must be seed-deterministic")
	}

	do(t, e, "POST", "/charges", `{"amount":5}`, map[string]string{ForcedStatusHeader: "201"})
	list := do(t, e, "GET", "/charges", "", nil)
	if strings.Contains(list.body, "charges_") {
		t.Fatalf("forced requests must never write state: %s", list.body)
	}
	if pending := len(e.Deliveries("")); pending != 0 {
		t.Fatalf("forced requests must never emit webhooks: %d queued", pending)
	}

	got = do(t, e, "POST", "/charges", `{"amount":5}`, map[string]string{ForcedStatusHeader: "418"})
	if got.status != 400 || got.headers[forcedMarkerHeader] != "refused" {
		t.Fatalf("undeclared status must refuse: %d %v", got.status, got.headers)
	}
	if !strings.Contains(got.body, "201") || !strings.Contains(got.body, "422") {
		t.Fatalf("refusal must name the declared codes: %s", got.body)
	}

	got = do(t, e, "POST", "/charges", `{"amount":5}`, map[string]string{ForcedStatusHeader: "teapot"})
	if got.status != 400 || got.headers[forcedMarkerHeader] != "refused" {
		t.Fatalf("garbage header must refuse: %d", got.status)
	}

	got = do(t, e, "POST", "/charges", `{"amount":5}`, nil)
	if got.status != 201 || got.headers[forcedMarkerHeader] != "" {
		t.Fatalf("organic path must be unaffected: %d %v", got.status, got.headers)
	}
}
