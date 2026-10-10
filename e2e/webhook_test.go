package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const webhookSpec = `{
  "openapi": "3.1.0",
  "info": {"title": "examplepay", "version": "1"},
  "paths": {},
  "webhooks": {
    "settlement.report": {"post": {
      "x-pikopod-emit-only": true,
      "responses": {"200": {"description": "acknowledged"}}
    }}
  }
}`

func TestWebhookEndToEnd(t *testing.T) {
	dir := t.TempDir()
	ports := freePorts(t, 2)
	agentPort, sbxPort := ports[0], ports[1]
	var mu sync.Mutex
	type sinkRequest struct {
		method string
		header http.Header
		body   []byte
		err    error
	}
	var requests []sinkRequest
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		mu.Lock()
		requests = append(requests, sinkRequest{r.Method, r.Header.Clone(), body, err})
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer sink.Close()

	if err := os.WriteFile(filepath.Join(dir, "spec.json"), []byte(webhookSpec), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf("listen: 127.0.0.1\nagent_port: %d\nsandbox_port: %d\ndata_dir: data\nupstreams:\n  examplepay:\n    target: https://api.examplepay.invalid\n", agentPort, sbxPort)
	if err := os.WriteFile(filepath.Join(dir, "pikopod.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, code := run(t, dir, "import", "examplepay", "--spec", "spec.json", "--webhook-url", sink.URL, "--config", "pikopod.yaml"); code != 0 {
		t.Fatalf("import failed (%d): %s", code, out)
	}
	up := startUp(t, dir, agentPort)
	if out, code := run(t, dir, "webhook", "emit", "examplepay", "settlement.report", "--config", "pikopod.yaml"); code != 0 || !strings.Contains(out, "emitted settlement.report on examplepay") {
		t.Fatalf("webhook emit failed (%d): %s", code, out)
	}

	waitUntil(t, "webhook sink request", 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(requests) > 0
	})
	var out string
	waitUntil(t, "successful webhook delivery in webhook list", 5*time.Second, func() bool {
		var code int
		out, code = run(t, dir, "webhook", "list", "examplepay", "--config", "pikopod.yaml")
		if code != 0 {
			t.Fatalf("webhook list failed (%d): %s", code, out)
		}
		return strings.Contains(out, "1 delivered, sink: 1 delivered, 0 failed, 0 dropped")
	})
	up.stop(t)

	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 1 {
		t.Fatalf("want one sink request, got %d", len(requests))
	}
	req := requests[0]
	if req.err != nil {
		t.Fatal(req.err)
	}
	if req.method != http.MethodPost || req.header.Get("Content-Type") != "application/json" || req.header.Get("X-Pikopod-Webhook-Event") != "settlement.report" {
		t.Fatalf("unexpected sink request: %s %v", req.method, req.header)
	}
	var payload struct {
		ID    string `json:"id"`
		Event string `json:"event"`
	}
	if err := json.Unmarshal(req.body, &payload); err != nil {
		t.Fatalf("decode webhook payload: %v; body: %s", err, req.body)
	}
	if payload.Event != "settlement.report" || payload.ID == "" || payload.ID != req.header.Get("X-Pikopod-Webhook-ID") {
		t.Fatalf("unexpected webhook payload: %s", req.body)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("want one delivery row and one summary, got:\n%s", out)
	}
	fields := strings.Fields(lines[0])
	if len(fields) != 4 || fields[0] != "1" || fields[1] != payload.Event || fields[2] != payload.ID {
		t.Fatalf("listed delivery must match the sink payload, got:\n%s", out)
	}
}
