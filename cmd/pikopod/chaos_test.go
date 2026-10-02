package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestChaosArmsABodyOverTheControlPlane(t *testing.T) {
	cfg := testConfig(t, "https://example.invalid")
	if err := sandboxAdd(cfg, "widgets", widgetsSpecPath, "chaos-seed-2", "", "", false, io.Discard); err != nil {
		t.Fatalf("add: %v", err)
	}
	sbx, err := newSandboxServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer sbx.Close()
	srv := httptest.NewServer(sbx)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/_pikopod/sandboxes/widgets/faults", "application/json",
		strings.NewReader(`{"method":"POST","path":"/widgets","kind":"error","status":402,"body":{"error":{"code":"card_declined"}},"headers":{"x-request-id":"req_cp_1"}}`))
	if err != nil {
		t.Fatal(err)
	}
	armed, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 201 || !strings.Contains(string(armed), `"card_declined"`) {
		t.Fatalf("arming with a body should 201 and echo it, got %d %s", resp.StatusCode, armed)
	}

	hit, err := http.Post(srv.URL+"/widgets/widgets", "application/json", strings.NewReader(`{"name":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer hit.Body.Close()
	var body map[string]any
	json.NewDecoder(hit.Body).Decode(&body)
	if hit.StatusCode != 402 || body["error"].(map[string]any)["code"] != "card_declined" {
		t.Fatalf("armed body must reach the client: %d %v", hit.StatusCode, body)
	}
	if hit.Header.Get("content-type") != "application/json; charset=utf-8" || hit.Header.Get("x-request-id") != "req_cp_1" {
		t.Fatalf("armed headers must reach the client: %v", hit.Header)
	}
}

func TestChaosBodyFlagReadsInlineJSONOrAFile(t *testing.T) {
	inline, err := parseFaultBody(`{"error":{"code":"card_declined"}}`)
	if err != nil || string(inline) != `{"error":{"code":"card_declined"}}` {
		t.Fatalf("inline body: %s %v", inline, err)
	}
	path := t.TempDir() + "/decline.json"
	if err := os.WriteFile(path, []byte("{\"error\": {\"code\": \"expired_card\"}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fromFile, err := parseFaultBody("@" + path)
	if err != nil || !strings.Contains(string(fromFile), "expired_card") {
		t.Fatalf("file body: %s %v", fromFile, err)
	}
	if _, err := parseFaultBody("not json"); err == nil {
		t.Fatal("a body that is not JSON must be refused")
	}
	if _, err := parseFaultBody(`"declined"`); err == nil {
		t.Fatal("a body that is not an object or array must be refused")
	}
	if _, err := parseFaultBody("@" + t.TempDir() + "/missing.json"); err == nil {
		t.Fatal("a missing file must be refused")
	}
}

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

func TestChaosListRendersOneLinePerFault(t *testing.T) {
	admin := chaosTestAdmin(t, "chaos-list-1")
	armedFault(t, admin, `{"method":"POST","path":"/widgets","kind":"error","status":503,"times":2,"per":"idempotency-key"}`)
	got := chaosListOutput(t, admin)

	if got != "armed   error 503 on POST /widgets (first 2 per idempotency-key)\n" {
		t.Fatalf("list must render one line per fault, got %q", got)
	}
	if strings.ContainsAny(got, "{}") {
		t.Fatalf("list output must carry no JSON, got %q", got)
	}
}

func TestChaosListRendersWebhookFaultsByEvent(t *testing.T) {
	admin := chaosTestAdmin(t, "chaos-list-2")
	armedFault(t, admin, `{"kind":"drop_webhook","event":"widget.created"}`)
	got := chaosListOutput(t, admin)
	if got != "armed   drop_webhook on widget.created\n" {
		t.Fatalf("a webhook fault must show its event, got %q", got)
	}
}

func TestChaosListSaysWhenNothingIsArmed(t *testing.T) {
	admin := chaosTestAdmin(t, "chaos-list-3")
	if got := chaosListOutput(t, admin); strings.TrimSpace(got) != "no standing faults" {
		t.Fatalf("an empty list must say so, got %q", got)
	}
}

func chaosTestAdmin(t *testing.T, seed string) string {
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
	return srv.URL + "/_pikopod/sandboxes/widgets/faults"
}

func chaosListOutput(t *testing.T, admin string) string {
	t.Helper()
	resp, err := http.Get(admin)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	if err := chaosList(resp, &buf); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func armedFault(t *testing.T, admin, body string) {
	t.Helper()
	resp, err := http.Post(admin, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("arming should 201, got %d", resp.StatusCode)
	}
}
