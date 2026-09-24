package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChaosAdminSurface(t *testing.T) {
	cfg := testConfig(t, "https://example.invalid")
	if err := sandboxAdd(cfg, "widgets", widgetsSpecPath, "chaos-seed-1", "", "", false, io.Discard); err != nil {
		t.Fatalf("add: %v", err)
	}
	sbx, err := newSandboxServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer sbx.Close()
	srv := httptest.NewServer(sbx)
	defer srv.Close()
	admin := srv.URL + "/_pikopod/sandboxes/widgets/faults"

	post := func(url, body string) *http.Response {
		t.Helper()
		resp, err := http.Post(url, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}

	if resp := post(admin, `{"method":"POST","path":"/widgets","kind":"error","status":503}`); resp.StatusCode != 201 {
		t.Fatalf("arming should 201, got %d", resp.StatusCode)
	}
	hit, err := http.Post(srv.URL+"/widgets/widgets", "application/json", strings.NewReader(`{"name":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	hit.Body.Close()
	if hit.StatusCode != 503 {
		t.Fatalf("armed fault must trip live traffic, got %d", hit.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodDelete, admin+"?method=POST&path=/widgets", nil)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 200 {
		t.Fatalf("clear failed: %v %v", err, resp)
	}
	ok, err := http.Post(srv.URL+"/widgets/widgets", "application/json", strings.NewReader(`{"name":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer ok.Body.Close()
	if ok.StatusCode != 201 {
		t.Fatalf("traffic must recover after clear, got %d", ok.StatusCode)
	}
	var created map[string]any
	json.NewDecoder(ok.Body).Decode(&created)
	if created["name"] != "x" {
		t.Fatalf("recovered create should echo: %v", created)
	}

	if resp := post(admin, `{"kind":"duplicate_webhook","probability":1}`); resp.StatusCode != 201 {
		t.Fatalf("webhook fault kind must arm, got %d", resp.StatusCode)
	}

	if resp := post(admin, `{"method":"POST","path":"/widgets","kind":"not_a_kind"}`); resp.StatusCode != 400 {
		t.Fatalf("unknown fault kind must be refused, got %d", resp.StatusCode)
	}
	if resp := post(admin, `{"kind":"error"}`); resp.StatusCode != 400 {
		t.Fatalf("fault without method/path must be refused, got %d", resp.StatusCode)
	}
	if resp := post(admin, `not json`); resp.StatusCode != 400 {
		t.Fatalf("malformed body must be refused, got %d", resp.StatusCode)
	}
	if resp := post(srv.URL+"/_pikopod/sandboxes/production/faults", `{"method":"GET","path":"/x","kind":"error"}`); resp.StatusCode != 404 {
		t.Fatalf("unknown sandbox must 404, got %d", resp.StatusCode)
	}
	if resp := post(srv.URL+"/_pikopod/other", `{}`); resp.StatusCode != 404 {
		t.Fatalf("unknown admin route must 404, got %d", resp.StatusCode)
	}
}
