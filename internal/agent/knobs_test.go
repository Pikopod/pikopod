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

// knobAgent runs a full agent against a mutating upstream and returns the
// sink + data dir. mute/volatile come from the config under test.
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

// mute: a muted endpoint template still LEARNS but never alerts.
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
	// Learning still happened: baselines persisted despite the mute.
	if _, err := os.Stat(filepath.Join(dir, "baselines", "prov.json")); err != nil {
		t.Fatal("muted endpoints must still learn baselines")
	}
}

// volatile_fields: a churning field named volatile causes no alerts, while a
// real drift on the same endpoint still fires.
func TestVolatileFieldsExcludedFromDrift(t *testing.T) {
	var mutated atomic.Bool
	upstream := driftingUpstream(&mutated)
	defer upstream.Close()

	sink, a, _ := knobAgent(t, upstream, config.Upstream{
		VolatileFields: []string{"request_ref"}, // churns every response pre-mutation
	})
	drive(t, a, 10) // request_ref changes every hit; must not prevent freeze or alert
	waitFor(t, func() bool { return a.Metrics.RecordingsWritten.Load() >= 10 })
	if got := sink.n.Load(); got != 0 {
		t.Fatalf("volatile churn alone must not alert, got %d: %v", got, sink.snapshot())
	}
	mutated.Store(true)
	drive(t, a, 6)
	// Settle the WHOLE pipeline before reading deliveries: late observers
	// both race the slice and can add a second fingerprint.
	waitFor(t, func() bool { return a.Metrics.RecordingsWritten.Load() >= 16 && sink.n.Load() >= 1 })
	joined := strings.Join(sink.snapshot(), "\n")
	if strings.Contains(joined, "request_ref") {
		t.Fatalf("volatile field must never appear in an alert: %s", joined)
	}
	if !strings.Contains(joined, "fee_bearer") && !strings.Contains(joined, "succeeded") {
		t.Fatalf("real drift on the same endpoint must still alert: %s", joined)
	}
}

// ack: the /ack endpoint acknowledges a live fingerprint; Active() empties.
func TestAckEndpoint(t *testing.T) {
	var mutated atomic.Bool
	upstream := driftingUpstream(&mutated)
	defer upstream.Close()

	sink, a, _ := knobAgent(t, upstream, config.Upstream{VolatileFields: []string{"request_ref"}})
	drive(t, a, 10)
	waitFor(t, func() bool { return a.Metrics.RecordingsWritten.Load() >= 10 })
	mutated.Store(true)
	drive(t, a, 6)
	// Let the WHOLE pipeline settle: every capture recorded and observed —
	// otherwise a second fingerprint can alert after the Active() snapshot.
	waitFor(t, func() bool { return a.Metrics.RecordingsWritten.Load() >= 16 && sink.n.Load() >= 1 })

	active := a.Alerter.Active()
	if len(active) == 0 {
		t.Fatal("expected active alerts")
	}
	front := httptest.NewServer(a.Proxy)
	defer front.Close()

	// GET refused; unknown fp 404s; real fp acks.
	if resp, _ := http.Get(front.URL + "/ack?fp=" + active[0].Fingerprint); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /ack must 405, got %d", resp.StatusCode)
	}
	if resp, _ := http.Post(front.URL+"/ack?fp=fp_nope", "", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown fp must 404, got %d", resp.StatusCode)
	}
	// Ack until drained (bounded): late observer work may mint one more
	// fingerprint between snapshots.
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

// accept: the drifted behavior becomes the new baseline — further drifted
// traffic generates ZERO findings (not merely deduped alerts), closing the
// permanent overlay/baseline divergence.
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
	drive(t, a, 8) // still drifted — but drifted IS the baseline now
	waitFor(t, func() bool { return a.Metrics.RecordingsWritten.Load() >= 24 })
	if a.EventsEmitted.Load() != emittedBefore {
		t.Fatalf("accepted drift must generate ZERO new findings, got %d more", a.EventsEmitted.Load()-emittedBefore)
	}
}

// refine.enabled: the full pipeline grows a traffic overlay with journaled
// admissions for fields the spec never declared — the refiner's acceptance.
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
	// Spec declares id/status/amount only — fee_bearer arrives post-mutation
	// and request_ref was never declared either.
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

	mutated.Store(true) // fee_bearer present from the start
	drive(t, a, 12)
	waitFor(t, func() bool { return a.Metrics.RecordingsWritten.Load() >= 12 })
	a.persistAll() // admission pass + overlay persist

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
