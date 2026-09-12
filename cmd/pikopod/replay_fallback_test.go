package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pikopod/pikopod/internal/proxy"
)

// A --recordings-fallback sandbox serves the linked upstream's recordings for
// requests neither the spec nor admitted traffic can answer.
func TestRecordingsFallbackThroughServer(t *testing.T) {
	cfg := testConfig(t, "https://example.invalid")
	if err := sandboxAdd(cfg, "widgets", widgetsSpecPath, "rec-seed-1", "", "", true, io.Discard); err != nil {
		t.Fatalf("add: %v", err)
	}
	// Recordings for the auto-linked upstream (name == "widgets").
	recDir := filepath.Join(cfg.DataDir, "recordings")
	if err := os.MkdirAll(recDir, 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(recDir, "widgets.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	json.NewEncoder(f).Encode(proxy.Record{Method: "GET", Path: "/balance", Status: 200, RespKind: "json",
		RespBody: map[string]any{"available": float64(5000)}})
	f.Close()

	sbx, err := newSandboxServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer sbx.Close()
	srv := httptest.NewServer(sbx)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/widgets/balance")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || resp.Header.Get("X-Pikopod-Replay-Tier") == "" {
		t.Fatalf("recordings fallback must serve with its tier named: %d %v %s", resp.StatusCode, resp.Header, raw)
	}
	var body map[string]any
	json.Unmarshal(raw, &body)
	if body["available"] != float64(5000) {
		t.Fatalf("recorded body wrong: %s", raw)
	}
}

// Without the opt-in the same request 404s: the tier never turns itself on.
func TestRecordingsFallbackIsOptIn(t *testing.T) {
	cfg := testConfig(t, "https://example.invalid")
	if err := sandboxAdd(cfg, "widgets", widgetsSpecPath, "rec-seed-2", "", "", false, io.Discard); err != nil {
		t.Fatalf("add: %v", err)
	}
	recDir := filepath.Join(cfg.DataDir, "recordings")
	os.MkdirAll(recDir, 0o700)
	f, _ := os.Create(filepath.Join(recDir, "widgets.ndjson"))
	json.NewEncoder(f).Encode(proxy.Record{Method: "GET", Path: "/balance", Status: 200, RespKind: "json",
		RespBody: map[string]any{"available": float64(5000)}})
	f.Close()

	sbx, err := newSandboxServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer sbx.Close()
	srv := httptest.NewServer(sbx)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/widgets/balance")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 || resp.Header.Get("X-Pikopod-Replay-Tier") != "" {
		t.Fatalf("recordings must stay off without the opt-in: %d %v", resp.StatusCode, resp.Header)
	}
}

// The admin journal surface: what the client sent is inspectable over
// /_pikopod/sandboxes/<name>/requests, and DELETE resets it.
func TestAdminRequestJournal(t *testing.T) {
	cfg := testConfig(t, "https://example.invalid")
	if err := sandboxAdd(cfg, "widgets", widgetsSpecPath, "adm-seed-1", "", "", false, io.Discard); err != nil {
		t.Fatalf("add: %v", err)
	}
	sbx, err := newSandboxServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer sbx.Close()
	srv := httptest.NewServer(sbx)
	defer srv.Close()

	if _, err := http.Post(srv.URL+"/widgets/widgets", "application/json", strings.NewReader(`{"name":"j"}`)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(srv.URL + "/_pikopod/sandboxes/widgets/requests")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var payload struct {
		Requests []map[string]any `json:"requests"`
		Evicted  int64            `json:"evicted"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Requests) != 1 || payload.Requests[0]["method"] != "POST" {
		t.Fatalf("journal must show the client's request: %+v", payload.Requests)
	}

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/_pikopod/sandboxes/widgets/requests", nil)
	if _, err := http.DefaultClient.Do(req); err != nil {
		t.Fatal(err)
	}
	resp2, err := http.Get(srv.URL + "/_pikopod/sandboxes/widgets/requests")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	payload.Requests = nil
	json.NewDecoder(resp2.Body).Decode(&payload)
	if len(payload.Requests) != 0 {
		t.Fatalf("DELETE must reset the journal: %+v", payload.Requests)
	}
}
