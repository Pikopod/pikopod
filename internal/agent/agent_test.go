package agent

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

	"github.com/pikopod/pikopod/internal/alert"
	"github.com/pikopod/pikopod/internal/config"
)

// captureSink records delivered alerts for assertions.
type captureSink struct {
	mu    chan struct{}
	texts []string
	n     atomic.Int32
}

func newCaptureSink() *captureSink  { return &captureSink{mu: make(chan struct{}, 1)} }
func (c *captureSink) Name() string { return "capture" }
func (c *captureSink) Deliver(text string) error {
	c.mu <- struct{}{}
	c.texts = append(c.texts, text)
	<-c.mu
	c.n.Add(1)
	return nil
}

// snapshot returns a copy of delivered texts under the sink's lock — tests
// must never read c.texts directly while the pipeline is live.
func (c *captureSink) snapshot() []string {
	c.mu <- struct{}{}
	out := append([]string(nil), c.texts...)
	<-c.mu
	return out
}

// The scripted story as a test: record → warmup → provider mutates →
// EXACTLY ONE alert per fingerprint → canary sentinels never egress.
func TestScriptedStory_DriftDetectedOncePerFingerprint(t *testing.T) {
	const canaryEmail = "story.canary@example.com"
	var mutated atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		status := "success"
		extra := ""
		if mutated.Load() {
			status = "succeeded"               // enum drift
			extra = `,"fee_bearer":"merchant"` // new-field drift
		}
		fmt.Fprintf(w, `{"id":"tx_9a8b7c6d5e","status":%q,"amount":5000,"customer_email":%q%s}`, status, canaryEmail, extra)
	}))
	defer upstream.Close()

	dir := t.TempDir()
	cfg := &config.Config{
		Listen: "127.0.0.1", AgentPort: 0, DataDir: dir,
		Upstreams: map[string]config.Upstream{"demoprov": {Listen: "/demoprov", Target: upstream.URL}},
		Warmup:    config.Warmup{MinSamples: 15, MinHours: new(int)},
	}
	sink := newCaptureSink()
	a, err := New(cfg, alert.Options{MinOccurrences: 2, Window: time.Minute}, sink)
	if err != nil {
		t.Fatal(err)
	}
	go a.Recorder.Run(a.Proxy.Captures())
	front := httptest.NewServer(a.Proxy)
	defer front.Close()

	hit := func(n int) {
		for i := 0; i < n; i++ {
			resp, err := http.Get(front.URL + "/demoprov/transaction/tx_" + fmt.Sprintf("%012d", i))
			if err != nil {
				t.Fatal(err)
			}
			io.ReadAll(resp.Body)
			resp.Body.Close()
		}
	}

	hit(20) // warmup (MinSamples 15) + a few frozen-reference clean samples
	waitFor(t, func() bool { return a.Metrics.RecordingsWritten.Load() >= 20 })
	if got := sink.n.Load(); got != 0 {
		t.Fatalf("no alerts expected during clean traffic, got %d: %v", got, sink.texts)
	}

	mutated.Store(true)
	hit(8) // several occurrences of both drifts → threshold crossed once each
	waitFor(t, func() bool { return sink.n.Load() >= 2 })
	hit(8) // MORE drifted traffic must NOT re-alert (dedupe per fingerprint)
	waitFor(t, func() bool { return a.Metrics.RecordingsWritten.Load() >= 36 })

	if got := sink.n.Load(); got != 2 {
		t.Fatalf("want exactly 2 alerts (enum + new field), got %d:\n%s", got, strings.Join(sink.texts, "\n---\n"))
	}
	joined := strings.Join(sink.texts, "\n")
	for _, want := range []string{"new value", `"succeeded"`, "new field", "fee_bearer", "from-drift fp_"} {
		if !strings.Contains(joined, want) {
			t.Errorf("alert text missing %q:\n%s", want, joined)
		}
	}
	// Egress enforcement: templated endpoint, no concrete tx id, no canary.
	if !strings.Contains(joined, "tx_{id}") {
		t.Errorf("alerts must carry the path TEMPLATE, got:\n%s", joined)
	}
	for _, leak := range []string{canaryEmail, "tx_000000000000"} {
		if strings.Contains(joined, leak) {
			t.Fatalf("CANARY LEAK in alert text: %q", leak)
		}
	}
	// ...and the local event log carries valid schema-shaped events, also leak-free.
	raw, err := os.ReadFile(filepath.Join(dir, "events.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), canaryEmail) {
		t.Fatal("CANARY LEAK in events.ndjson")
	}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var ev alert.DriftEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("event log line not schema-shaped: %v", err)
		}
		if ev.SchemaVersion != alert.SchemaVersion || !strings.HasPrefix(ev.Fingerprint, "fp_") {
			t.Fatalf("bad event: %+v", ev)
		}
	}
}

// Flapping provider: brief wobble under the occurrence threshold must stay silent.
func TestFlappingProviderNoStorm(t *testing.T) {
	var flap atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if flap.Load() {
			fmt.Fprint(w, `{"id":"tx_1a2b3c4d5e","status":"success","blip":"x"}`)
			flap.Store(false) // single-occurrence blip
			return
		}
		fmt.Fprint(w, `{"id":"tx_1a2b3c4d5e","status":"success"}`)
	}))
	defer upstream.Close()

	dir := t.TempDir()
	cfg := &config.Config{
		Listen: "127.0.0.1", DataDir: dir,
		Upstreams: map[string]config.Upstream{"p": {Listen: "/p", Target: upstream.URL}},
		Warmup:    config.Warmup{MinSamples: 10, MinHours: new(int)},
	}
	sink := newCaptureSink()
	a, err := New(cfg, alert.Options{MinOccurrences: 3, Window: time.Minute}, sink)
	if err != nil {
		t.Fatal(err)
	}
	go a.Recorder.Run(a.Proxy.Captures())
	front := httptest.NewServer(a.Proxy)
	defer front.Close()

	for i := 0; i < 15; i++ { // warmup
		resp, _ := http.Get(front.URL + "/p/tx/tx_00000000000" + fmt.Sprint(i%10))
		io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	flap.Store(true)
	for i := 0; i < 5; i++ { // one blip inside otherwise-clean traffic
		resp, _ := http.Get(front.URL + "/p/tx/tx_000000000001")
		io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	waitFor(t, func() bool { return a.Metrics.RecordingsWritten.Load() >= 20 })
	if got := sink.n.Load(); got != 0 {
		t.Fatalf("single-occurrence blip must not alert (threshold 3), got %d: %v", got, sink.texts)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatal("condition not reached in time")
}
