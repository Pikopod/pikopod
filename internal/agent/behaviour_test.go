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

func runStatefulTraffic(t *testing.T, enabled bool) (*Agent, string) {
	t.Helper()
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		status := "pending"
		if calls.Add(1) > 1 {
			status = "succeeded"
		}
		fmt.Fprintf(w, `{"id":"ch_CANARYQ7Z9","status":%q,"amount":100}`, status)
	}))
	defer upstream.Close()
	dir := t.TempDir()
	cfg := &config.Config{
		Listen: "127.0.0.1", AgentPort: 0, DataDir: dir,
		Upstreams: map[string]config.Upstream{"prov": {Listen: "/prov", Target: upstream.URL, VolatileFields: []string{"amount"}}},
		Warmup:    config.Warmup{MinSamples: 2, MinHours: new(int)},
		Behaviour: config.Behaviour{Enabled: enabled},
	}
	a, err := New(cfg, alert.Options{MinOccurrences: 2, Window: time.Minute}, newCaptureSink())
	if err != nil {
		t.Fatal(err)
	}
	go a.Recorder.Run(a.Proxy.Captures())
	front := httptest.NewServer(a.Proxy)
	defer front.Close()
	for i := 0; i < 2; i++ {
		resp, err := http.Get(front.URL + "/prov/charges/ch_CANARYQ7Z9")
		if err != nil {
			t.Fatal(err)
		}
		io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	waitFor(t, func() bool { return a.Metrics.RecordingsWritten.Load() >= 2 })
	a.persistAll()
	return a, dir
}

func TestBehaviourOffMeansNoWork(t *testing.T) {
	a, dir := runStatefulTraffic(t, false)
	if a.Behaviour("prov") != nil || len(a.behaviours) != 0 {
		t.Fatal("disabled: no tracker may exist")
	}
	if _, err := os.Stat(filepath.Join(dir, "apis", "prov.behaviour.json")); !os.IsNotExist(err) {
		t.Fatal("disabled: nothing may be written")
	}
}

func TestBehaviourOnLearnsFromTheObserveStream(t *testing.T) {
	a, dir := runStatefulTraffic(t, true)
	tr := a.Behaviour("prov")
	if tr == nil {
		t.Fatal("enabled: the tracker must exist")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "apis", "prov.behaviour.json"))
	if err != nil {
		t.Fatal(err)
	}
	var g struct {
		Endpoints map[string]struct {
			Fields map[string]struct {
				Edges map[string]struct{ Count int64 } `json:"edges"`
			} `json:"fields"`
		} `json:"endpoints"`
	}
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	ep := g.Endpoints["GET|/charges/ch_{id}"]
	if ep.Fields["status"].Edges["pending→succeeded"].Count != 1 {
		t.Fatalf("the transition the upstream made must be learned: %s", raw)
	}
	if _, tracked := ep.Fields["amount"]; tracked {
		t.Fatal("volatile_fields applies here too")
	}
	if strings.Contains(strings.ToLower(string(raw)), "canaryq7z9") {
		t.Fatalf("CANARY LEAK: resource identifier in the persisted graph:\n%s", raw)
	}
}
