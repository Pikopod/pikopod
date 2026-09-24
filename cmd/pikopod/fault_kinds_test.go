package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pikopod/pikopod/internal/sandbox"
)

func faultServer(t *testing.T, seed string) *httptest.Server {
	t.Helper()
	cfg := testConfig(t, "https://example.invalid")
	if err := sandboxAdd(cfg, "widgets", widgetsSpecPath, seed, "", "", false, io.Discard); err != nil {
		t.Fatalf("add: %v", err)
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

func armFault(t *testing.T, srv *httptest.Server, body string) *http.Response {
	t.Helper()
	res, err := http.Post(srv.URL+"/_pikopod/sandboxes/widgets/faults", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestAdminArmsEveryFaultKind(t *testing.T) {
	srv := faultServer(t, "kinds-seed")
	for _, kind := range sandbox.FaultKinds() {
		body := `{"kind":"` + kind + `","probability":1`
		if !sandbox.IsWebhookFaultKind(kind) {

			body += `,"method":"POST","path":"/widgets"`
		}
		body += `}`
		res := armFault(t, srv, body)
		got := readJSON(t, res)
		if res.StatusCode != http.StatusCreated {
			t.Errorf("kind %q = %d, want 201: %v", kind, res.StatusCode, got["message"])
		}
	}
}

func TestAdminUnknownKindNamesEveryValidKind(t *testing.T) {
	srv := faultServer(t, "kinds-seed-2")
	res := armFault(t, srv, `{"method":"POST","path":"/widgets","kind":"not_a_kind"}`)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown kind = %d, want 400", res.StatusCode)
	}
	msg, _ := readJSON(t, res)["message"].(string)
	for _, kind := range sandbox.FaultKinds() {
		if !strings.Contains(msg, kind) {
			t.Errorf("rejection does not name %q: %s", kind, msg)
		}
	}
}

func TestAdminRateLimitArmsA429Error(t *testing.T) {
	srv := faultServer(t, "kinds-seed-3")
	if res := armFault(t, srv, `{"method":"POST","path":"/widgets","kind":"rate_limit","probability":1}`); res.StatusCode != http.StatusCreated {
		t.Fatalf("arm rate_limit = %d, want 201", res.StatusCode)
	}
	res, err := http.Get(srv.URL + "/_pikopod/sandboxes/widgets/faults")
	if err != nil {
		t.Fatal(err)
	}
	faults, _ := readJSON(t, res)["faults"].([]any)
	if len(faults) != 1 {
		t.Fatalf("want one armed rule, got %d", len(faults))
	}
	rule, _ := faults[0].(map[string]any)
	if rule["kind"] != "error" {
		t.Errorf("armed kind = %v, want error (rate_limit is sugar)", rule["kind"])
	}
	if status, _ := rule["status"].(float64); status != 429 {
		t.Errorf("armed status = %v, want 429", rule["status"])
	}
}

func TestAdminWebhookFaultNeedsNoMethodOrPath(t *testing.T) {
	srv := faultServer(t, "kinds-seed-4")
	if res := armFault(t, srv, `{"kind":"duplicate_webhook","probability":1}`); res.StatusCode != http.StatusCreated {
		t.Fatalf("arm duplicate_webhook with no method/path = %d, want 201", res.StatusCode)
	}
}

func TestAdminResponseFaultStillNeedsMethodAndPath(t *testing.T) {
	srv := faultServer(t, "kinds-seed-5")
	res := armFault(t, srv, `{"kind":"error","status":503}`)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("error fault with no method/path = %d, want 400", res.StatusCode)
	}
}
