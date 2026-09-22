package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pikopod/pikopod/internal/config"
)

func TestScenarioRunDrainsTheSinkBeforeReturning(t *testing.T) {
	dir := cliDir(t)
	var got atomic.Int32
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Add(1)
		w.WriteHeader(200)
	}))
	t.Cleanup(sink.Close)
	cfg, err := config.Load(filepath.Join(dir, "pikopod.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := sandboxAdd(cfg, "bank", writeSpec(t, hookSpecEmitOnly), "sink-seed", sink.URL, "", false, io.Discard); err != nil {
		t.Fatal(err)
	}
	packs := filepath.Join(cfg.DataDir, "scenarios")
	os.MkdirAll(packs, 0o700)
	pack := "name: settle\nprovider: bank\ndefinition:\n  steps:\n    - key: fire\n      type: EMIT_WEBHOOK\n      config: {event: settlement.report, data: {total: 12}}\n    - key: got\n      type: EXPECT_WEBHOOK\n      config: {match: {eventType: settlement.report}, timeoutMs: 1000}\n"
	if err := os.WriteFile(filepath.Join(packs, "settle.yaml"), []byte(pack), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runCLI(t, newScenarioCmd(), "run", "bank", "settle")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got.Load() != 1 {
		t.Fatalf("the sink must have received the delivery before scenario run returned (got %d)\n%s", got.Load(), out)
	}
	if !strings.Contains(out, "sink: 1 delivered, 0 failed to "+sink.URL) {
		t.Fatalf("the summary must report the sink outcome:\n%s", out)
	}
}
