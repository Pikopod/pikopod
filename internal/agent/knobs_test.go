package agent

import (
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

	"github.com/pikopod/pikopod/internal/alert"
	"github.com/pikopod/pikopod/internal/config"
	"github.com/pikopod/pikopod/internal/importer"
	"github.com/pikopod/pikopod/internal/ir"
)

func knobAgent(t *testing.T, upstream *httptest.Server, up config.Upstream) (*captureSink, *Agent, string) {
	t.Helper()
	dir := t.TempDir()
	up.Target = upstream.URL
	up.Listen = "/prov"
	cfg := &config.Config{
		Listen: "127.0.0.1", AgentPort: 0, DataDir: dir,
		Upstreams: map[string]config.Upstream{"prov": up},
		Warmup:    config.Warmup{MinSamples: 8, MinHours: new(int)},
	}
	sink := newCaptureSink()
	a, err := New(cfg, alert.Options{MinOccurrences: 2, Window: time.Minute}, sink)
	if err != nil {
		t.Fatal(err)
	}
	startPipeline(t, a)
	return sink, a, dir
}

func driftingUpstream(mutated *atomic.Bool) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		status, extra := "success", ""
		if mutated.Load() {
			status = "succeeded"
			extra = `,"fee_bearer":"merchant"`
		}
		fmt.Fprintf(w, `{"id":"tx_1a2b3c4d5e","status":%q,"amount":100,"request_ref":"nonce-%d"%s}`, status, time.Now().UnixNano(), extra)
	}))
}

func drive(t *testing.T, a *Agent, n int) {
	t.Helper()
	front := httptest.NewServer(a.Proxy)
	defer front.Close()
	for i := 0; i < n; i++ {
		resp, err := http.Get(front.URL + "/prov/tx")
		if err != nil {
			t.Fatal(err)
		}
		io.ReadAll(resp.Body)
		resp.Body.Close()
	}
}

func TestMuteSuppressesAlerts(t *testing.T) {
	var mutated atomic.Bool
	upstream := driftingUpstream(&mutated)
	defer upstream.Close()

	sink, a, dir := knobAgent(t, upstream, config.Upstream{
		Mute:           []string{"/tx"},
		VolatileFields: []string{"request_ref"},
	})
	drive(t, a, 10)
	waitFor(t, func() bool { return a.Metrics.RecordingsWritten.Load() >= 10 })
	mutated.Store(true)
	drive(t, a, 6)
	waitFor(t, func() bool { return a.Metrics.RecordingsWritten.Load() >= 16 })
	a.persistAll()

	if got := sink.n.Load(); got != 0 {
		t.Fatalf("muted template must never alert, got %d: %v", got, sink.snapshot())
	}
	if _, err := os.Stat(filepath.Join(dir, "events.ndjson")); err == nil {
		raw, _ := os.ReadFile(filepath.Join(dir, "events.ndjson"))
		if strings.TrimSpace(string(raw)) != "" {
			t.Fatalf("muted template must not log events: %s", raw)
		}
	}

	if _, err := os.Stat(filepath.Join(dir, "baselines", "prov.json")); err != nil {
		t.Fatal("muted endpoints must still learn baselines")
	}
}

func TestVolatileFieldsExcludedFromDrift(t *testing.T) {
	var mutated atomic.Bool
	upstream := driftingUpstream(&mutated)
	defer upstream.Close()

	sink, a, _ := knobAgent(t, upstream, config.Upstream{
		VolatileFields: []string{"request_ref"},
	})
	drive(t, a, 10)
	waitFor(t, func() bool { return a.Metrics.RecordingsWritten.Load() >= 10 })
	if got := sink.n.Load(); got != 0 {
		t.Fatalf("volatile churn alone must not alert, got %d: %v", got, sink.snapshot())
	}
	mutated.Store(true)
	drive(t, a, 6)

	waitFor(t, func() bool { return a.Metrics.RecordingsWritten.Load() >= 16 && sink.n.Load() >= 1 })
	joined := strings.Join(sink.snapshot(), "\n")
	if strings.Contains(joined, "request_ref") {
		t.Fatalf("volatile field must never appear in an alert: %s", joined)
	}
	if !strings.Contains(joined, "fee_bearer") && !strings.Contains(joined, "succeeded") {
		t.Fatalf("real drift on the same endpoint must still alert: %s", joined)
	}
}

func TestAckEndpoint(t *testing.T) {
	var mutated atomic.Bool
	upstream := driftingUpstream(&mutated)
	defer upstream.Close()

	sink, a, _ := knobAgent(t, upstream, config.Upstream{VolatileFields: []string{"request_ref"}})
	drive(t, a, 10)
	waitFor(t, func() bool { return a.Metrics.RecordingsWritten.Load() >= 10 })
	mutated.Store(true)
	drive(t, a, 6)

	waitFor(t, func() bool { return a.Metrics.RecordingsWritten.Load() >= 16 && sink.n.Load() >= 1 })

	active := a.Alerter.Active()
	if len(active) == 0 {
		t.Fatal("expected active alerts")
	}
	front := httptest.NewServer(a.Proxy)
	defer front.Close()

	if resp, _ := http.Get(front.URL + "/ack?fp=" + active[0].Fingerprint); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /ack must 405, got %d", resp.StatusCode)
	}
	if resp, _ := http.Post(front.URL+"/ack?fp=fp_nope", "", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown fp must 404, got %d", resp.StatusCode)
	}

	for attempt := 0; attempt < 5; attempt++ {
		for _, ev := range a.Alerter.Active() {
			resp, err := http.Post(front.URL+"/ack?fp="+ev.Fingerprint, "", nil)
			if err != nil || resp.StatusCode != http.StatusOK {
				t.Fatalf("ack failed: %v %v", err, resp)
			}
		}
		if len(a.Alerter.Active()) == 0 {
			break
		}
	}
	if left := a.Alerter.Active(); len(left) != 0 {
		t.Fatalf("acked fingerprints must leave the active list, %d remain", len(left))
	}
}

func TestAcceptRefreezesBaseline(t *testing.T) {
	var mutated atomic.Bool
	upstream := driftingUpstream(&mutated)
	defer upstream.Close()

	sink, a, _ := knobAgent(t, upstream, config.Upstream{VolatileFields: []string{"request_ref"}})
	drive(t, a, 10)
	waitFor(t, func() bool { return a.Metrics.RecordingsWritten.Load() >= 10 })
	mutated.Store(true)
	drive(t, a, 6)
	waitFor(t, func() bool { return a.Metrics.RecordingsWritten.Load() >= 16 && sink.n.Load() >= 1 })

	front := httptest.NewServer(a.Proxy)
	defer front.Close()
	for _, ev := range a.Alerter.Active() {
		resp, err := http.Post(front.URL+"/accept?fp="+ev.Fingerprint, "", nil)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("accept failed: %v %v", err, resp)
		}
	}
	if left := a.Alerter.Active(); len(left) != 0 {
		t.Fatalf("accept must ack, %d remain", len(left))
	}

	emittedBefore := a.EventsEmitted.Load()
	drive(t, a, 8)
	waitFor(t, func() bool { return a.Metrics.RecordingsWritten.Load() >= 24 })
	if a.EventsEmitted.Load() != emittedBefore {
		t.Fatalf("accepted drift must generate ZERO new findings, got %d more", a.EventsEmitted.Load()-emittedBefore)
	}
}

func TestRefinementLearnsUndeclaredField(t *testing.T) {
	var mutated atomic.Bool
	upstream := driftingUpstream(&mutated)
	defer upstream.Close()

	dir := t.TempDir()
	cfg := &config.Config{
		Listen: "127.0.0.1", AgentPort: 0, DataDir: dir,
		Upstreams: map[string]config.Upstream{"prov": {Listen: "/prov", Target: upstream.URL}},
		Warmup:    config.Warmup{MinSamples: 8, MinHours: new(int)},
		Refine:    config.Refine{Enabled: true},
	}
	sink := newCaptureSink()
	a, err := New(cfg, alert.Options{MinOccurrences: 2, Window: time.Minute}, sink)
	if err != nil {
		t.Fatal(err)
	}

	def, err := importer.NormalizeOpenAPI([]byte(`{
	  "openapi": "3.0.0", "info": {"title": "p", "version": "1"},
	  "paths": {"/tx": {"get": {"responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {
	    "type": "object", "properties": {"id": {"type": "string"}, "status": {"type": "string"}, "amount": {"type": "integer"}}
	  }}}}}}}}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	a.SetContracts(map[string]*ir.ApiDefinition{"prov": def})
	startPipeline(t, a)

	mutated.Store(true)
	drive(t, a, 12)
	waitFor(t, func() bool { return a.Metrics.RecordingsWritten.Load() >= 12 })
	a.persistAll()

	raw, err := os.ReadFile(filepath.Join(dir, "apis", "prov.observed.json"))
	if err != nil {
		t.Fatalf("overlay must persist: %v", err)
	}
	text := string(raw)
	if !strings.Contains(text, `"fee_bearer"`) || !strings.Contains(text, `"kind": "field"`) {
		t.Fatalf("undeclared field must be journaled as an admission:\n%s", text)
	}
	if !strings.Contains(text, `"version": 1`) {
		t.Fatalf("admissions must carry monotonic versions:\n%s", text)
	}
}
