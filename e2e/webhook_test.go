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
	"sync/atomic"
	"testing"
	"time"
)

func TestWebhookEmitAndListEndToEnd(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var sinkCalls atomic.Int32
	received := make(chan []byte, 8)
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "expected POST", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil || !json.Valid(body) {
			http.Error(w, "invalid JSON delivery", http.StatusBadRequest)
			return
		}
		sinkCalls.Add(1)
		received <- body
		w.WriteHeader(http.StatusNoContent)
	}))
	defer sink.Close()

	ports := freePorts(t, 2)
	cfg := fmt.Sprintf("listen: 127.0.0.1\nagent_port: %d\nsandbox_port: %d\ndata_dir: data\nupstreams:\n  pay:\n    target: https://api.example.invalid\n", ports[0], ports[1])
	if err := os.WriteFile(filepath.Join(dir, "pikopod.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := `{
  "openapi": "3.0.0",
  "info": {"title": "Payments", "version": "1"},
  "paths": {},
  "webhooks": {
    "invoice.paid": {
      "post": {
        "requestBody": {"content": {"application/json": {"schema": {"type": "object", "properties": {"invoice_id": {"type": "string"}}}}}},
        "responses": {"200": {"description": "ack"}},
        "x-pikopod-emit-only": true
      }
    }
  }
}`
	if err := os.WriteFile(filepath.Join(dir, "spec.json"), []byte(spec), 0o600); err != nil {
		t.Fatal(err)
	}

	out, code := run(t, dir, "import", "pay", "--spec", "spec.json", "--webhook-url", sink.URL, "--config", "pikopod.yaml")
	if code != 0 {
		t.Fatalf("import failed (%d): %s", code, out)
	}
	up := startUp(t, dir, ports[0])

	out, code = run(t, dir, "webhook", "emit", "pay", "invoice.paid", "--config", "pikopod.yaml")
	if code != 0 || !strings.Contains(out, "emitted invoice.paid") {
		t.Fatalf("webhook emit failed (%d): %s", code, out)
	}
	select {
	case <-received:
	case <-time.After(5 * time.Second):
		t.Fatal("webhook sink did not receive the emitted event")
	}

	var list string
	waitUntil(t, "webhook list to record the sink delivery", 5*time.Second, func() bool {
		var listCode int
		list, listCode = run(t, dir, "webhook", "list", "pay", "--config", "pikopod.yaml")
		return listCode == 0 && strings.Contains(list, "1 delivered, sink: 1 delivered, 0 failed, 0 dropped")
	})
	if strings.Count(list, "invoice.paid") != 1 {
		t.Fatalf("webhook list should show one invoice.paid delivery, got:\n%s", list)
	}
	if sinkCalls.Load() != 1 {
		t.Fatalf("webhook sink received %d deliveries, want 1", sinkCalls.Load())
	}
	up.stop(t)
}
