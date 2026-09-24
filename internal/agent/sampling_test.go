package agent

import (
	"encoding/json"
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

func TestSamplingDoesNotDistortPresenceRates(t *testing.T) {
	var n atomic.Int64
	var drifted atomic.Bool
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body := map[string]any{"id": "th_0000000001", "status": "success"}
		if n.Add(1)%2 == 0 {
			body["opt"] = "present"
		}
		if drifted.Load() {
			body["fee_bearer"] = "merchant"
		}
		json.NewEncoder(w).Encode(body)
	}))
	defer provider.Close()

	dir := t.TempDir()
	zero := 0.0
	cfg := &config.Config{
		Listen: "127.0.0.1", DataDir: dir,
		Upstreams: map[string]config.Upstream{"prov": {Listen: "/prov", Target: provider.URL}},
		Warmup:    config.Warmup{MinSamples: 8, MinHours: new(int)},
		Sampling:  config.Sampling{Rate: &zero},
	}
	a, err := New(cfg, alert.Options{MinOccurrences: 1, Window: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	startPipeline(t, a)
	front := httptest.NewServer(a.Proxy)
	defer front.Close()

	hit := func(count int) {
		for i := 0; i < count; i++ {
			resp, err := http.Get(front.URL + "/prov/things/th_0000000001")
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}
	hit(8)
	hit(8)
	drifted.Store(true)
	hit(2)

	waitProcessed := func(want int64) {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if a.Metrics.RecordingsWritten.Load()+a.Metrics.RecordingsSampledOut.Load() >= want {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("pipeline stalled: written=%d sampled_out=%d",
			a.Metrics.RecordingsWritten.Load(), a.Metrics.RecordingsSampledOut.Load())
	}
	waitProcessed(18)

	fams := a.Learners()["prov"].Families()
	if len(fams) != 1 || !fams[0].Frozen {
		t.Fatalf("expected one frozen family: %+v", fams)
	}
	if r := fams[0].PresenceRatio("opt"); r != 0.5 {
		t.Fatalf("presence must reflect the FULL stream (want 0.5), got %v — sampling distorted the math", r)
	}

	if fams[0].Samples != 18 {
		t.Fatalf("the learner must see every record: %d of 18", fams[0].Samples)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "recordings", "prov.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Count(string(raw), "\n")
	if lines != 10 {
		t.Fatalf("disk must hold warmup+drift only (10), got %d", lines)
	}
	if !strings.Contains(string(raw), "fee_bearer") {
		t.Fatal("drift evidence must reach disk regardless of rate")
	}
	if got := a.Metrics.RecordingsSampledOut.Load(); got != 8 {
		t.Fatalf("expected the 8 routine records sampled out, got %d", got)
	}
	a.Alerter.Flush()
	if a.Alerter.Sent() == 0 {
		t.Fatal("drift on sampled-out-class traffic must still alert")
	}
}
