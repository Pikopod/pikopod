package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pikopod/pikopod/internal/alert"
	"github.com/pikopod/pikopod/internal/config"
	"github.com/pikopod/pikopod/internal/importer"
	"github.com/pikopod/pikopod/internal/ir"
)

// End-to-end reproduction of issue #28's acceptance check: a spec declares
// status enum [ACTIVE, PENDING]; traffic carries ACTIVE, then the upstream
// starts returning an undeclared SUSPENDED. Both must reach the recording in
// clear text — a tok_… there means the rules never reached the recorder, and
// their absence means the value is still being dropped. An undeclared field
// (currency) with no spec rule must still fail closed, proving the fix does
// not loosen redaction generally.
func TestEnumFieldSurvivesRedactionEndToEnd(t *testing.T) {
	def, err := importer.NormalizeOpenAPI([]byte(`{
	  "openapi": "3.0.0", "info": {"title": "p", "version": "1"},
	  "paths": {"/tx": {"get": {"responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {
	    "type": "object",
	    "properties": {
	      "id": {"type": "string"},
	      "status": {"type": "string", "enum": ["ACTIVE", "PENDING"]}
	    }
	  }}}}}}}}
	}`))
	if err != nil {
		t.Fatal(err)
	}

	var suspended atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		status := "ACTIVE"
		if suspended.Load() {
			status = "SUSPENDED" // undeclared: the whole point of drift detection
		}
		fmt.Fprintf(w, `{"id":"tx_1","status":%q,"currency":"NGN"}`, status)
	}))
	defer upstream.Close()

	dir := t.TempDir()
	cfg := &config.Config{
		Listen: "127.0.0.1", AgentPort: 0, DataDir: dir,
		Upstreams: map[string]config.Upstream{"prov": {Listen: "/prov", Target: upstream.URL}},
		Warmup:    config.Warmup{MinSamples: 8, MinHours: new(int)},
	}
	sink := newCaptureSink()
	a, err := New(cfg, alert.Options{MinOccurrences: 2, Window: time.Minute}, sink)
	if err != nil {
		t.Fatal(err)
	}
	a.SetContracts(map[string]*ir.ApiDefinition{"prov": def})
	go a.Recorder.Run(a.Proxy.Captures())
	front := httptest.NewServer(a.Proxy)
	defer front.Close()

	hit := func(n int) {
		for i := 0; i < n; i++ {
			resp, err := http.Get(front.URL + "/prov/tx")
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
		}
	}
	hit(10) // through warmup on the declared member
	suspended.Store(true)
	hit(2) // the undeclared member drift exists to catch
	waitFor(t, func() bool { return a.Metrics.RecordingsWritten.Load() >= 12 })

	raw, err := os.ReadFile(filepath.Join(dir, "recordings", "prov.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	var sawActive, sawSuspended bool
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		body, _ := rec["resp_body"].(map[string]any)
		if body == nil {
			continue
		}
		switch body["status"] {
		case "ACTIVE":
			sawActive = true
		case "SUSPENDED":
			sawSuspended = true
		}
		if s, ok := body["status"].(string); ok && strings.HasPrefix(s, "tok_") {
			t.Fatalf("status was tokenized despite a spec-derived rule: %v", body)
		}
		if _, present := body["currency"]; present {
			t.Fatalf("currency has no spec rule and must still fail closed, got %v", body)
		}
	}
	if !sawActive {
		t.Fatal("declared member ACTIVE never reached the recording in clear text")
	}
	if !sawSuspended {
		t.Fatal("undeclared but enum-shaped member SUSPENDED never reached the recording in clear text")
	}
}
