package agent

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pikopod/pikopod/internal/alert"
	"github.com/pikopod/pikopod/internal/config"
	"github.com/pikopod/pikopod/internal/importer"
	"github.com/pikopod/pikopod/internal/ir"
	"github.com/pikopod/pikopod/internal/sanitize"
	"github.com/pikopod/pikopod/internal/sanitize/specrules"
)

const uppercaseEnumSpec = `{"openapi":"3.1.0","info":{"title":"Pay","version":"1"},
"paths":{"/charges":{"post":{"responses":{"200":{"description":"ok","content":{"application/json":{"schema":{"type":"object","properties":{
  "id":{"type":"string"},
  "status":{"type":"string","enum":["ACTIVE","PENDING"]},
  "currency":{"type":"string","enum":["NGN","USD"]}
}}}}}}}}}}`

func driveEnumDrift(t *testing.T, rules map[string][]sanitize.Rule) string {
	t.Helper()
	var mutated atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		status := "ACTIVE"
		if mutated.Load() {
			status = "SUSPENDED"
		}
		fmt.Fprintf(w, `{"id":"ch_1","status":%q,"currency":"NGN","amount":5000}`, status)
	}))
	defer upstream.Close()
	cfg := &config.Config{
		Listen: "127.0.0.1", AgentPort: 0, DataDir: t.TempDir(),
		Upstreams: map[string]config.Upstream{"prov": {Listen: "/prov", Target: upstream.URL}},
		Warmup:    config.Warmup{MinSamples: 10, MinHours: new(int)},
	}
	sink := newCaptureSink()
	a, err := New(cfg, alert.Options{MinOccurrences: 2, Window: time.Minute}, sink)
	if err != nil {
		t.Fatal(err)
	}
	a.SetSpecRules(rules)
	startPipeline(t, a)
	front := httptest.NewServer(a.Proxy)
	defer front.Close()
	hit := func(n int) {
		for i := 0; i < n; i++ {
			resp, err := http.Post(front.URL+"/prov/charges", "application/json", strings.NewReader(`{"amount":5000}`))
			if err != nil {
				t.Fatal(err)
			}
			io.ReadAll(resp.Body)
			resp.Body.Close()
		}
	}
	hit(14)
	waitFor(t, func() bool { return a.Metrics.RecordingsWritten.Load() >= 14 })
	mutated.Store(true)
	hit(4)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && sink.n.Load() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	a.persistAll()
	return strings.Join(sink.texts, "\n")
}

func TestUppercaseEnumDriftIsReadableWithSpecRules(t *testing.T) {
	def, err := importer.NormalizeOpenAPI([]byte(uppercaseEnumSpec))
	if err != nil {
		t.Fatal(err)
	}
	text := driveEnumDrift(t, specrules.ForContracts(map[string]*ir.ApiDefinition{"prov": def}))
	for _, want := range []string{"`status`", "not in known set [ACTIVE]"} {
		if !strings.Contains(text, want) {
			t.Fatalf("alert must be readable, missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "SUSPENDED") {
		t.Fatalf("an undeclared value must not be written or alerted raw:\n%s", text)
	}
}

func TestUppercaseEnumDriftWithoutContractStaysAsToday(t *testing.T) {
	text := driveEnumDrift(t, nil)
	if strings.Contains(text, "ACTIVE") || strings.Contains(text, "PENDING") {
		t.Fatalf("without a contract the vocabulary must not reach the alert:\n%s", text)
	}
}
