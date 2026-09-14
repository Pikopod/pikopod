// Package demo is the zero-config first-run story: an in-process fake provider
// serves traffic through a real agent, then silently ships a drifting change.
package demo

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"time"

	"github.com/pikopod/pikopod/internal/agent"
	"github.com/pikopod/pikopod/internal/alert"
	"github.com/pikopod/pikopod/internal/config"
)

const warmupSamples = 30

func Run(out io.Writer) error {
	if out == nil {
		out = os.Stdout
	}
	say := func(format string, args ...any) { fmt.Fprintf(out, format+"\n", args...) }

	say("pikopod demo — a fake payment provider, a real agent, and a silent change.")
	say("")

	var mutated atomic.Bool
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		status, extra := "success", ""
		if mutated.Load() {
			status, extra = "succeeded", `,"fee_bearer":"merchant"`
		}
		fmt.Fprintf(w, `{"id":"tx_4f9a2b7c1d3e","status":%q,"amount":245000,"currency":"NGN","customer_email":"demo.user@example-shop.com"%s}`, status, extra)
	}))
	defer provider.Close()

	dir, err := os.MkdirTemp("", "pikopod-demo-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	cfg := &config.Config{
		Listen: "127.0.0.1", DataDir: dir,
		Upstreams: map[string]config.Upstream{"fakepay": {Listen: "/fakepay", Target: provider.URL}},
		Warmup:    config.Warmup{MinSamples: warmupSamples, MinHours: new(int)}, // eval thresholds — demo only
	}
	a, err := agent.New(cfg, alert.Options{MinOccurrences: 3, Window: time.Minute}, alert.StdoutSink{})
	if err != nil {
		return err
	}
	go a.Recorder.Run(a.Proxy.Captures())
	front := httptest.NewServer(a.Proxy)
	defer front.Close()

	say("① Your app's base URL points at the agent; the agent forwards to the provider.")
	say("   app → agent %s/fakepay → provider %s", front.URL, provider.URL)
	say("")
	say("② Sending %d normal payments while pikopod learns what 'normal' looks like…", warmupSamples+10)

	hit := func(n int) {
		for i := 0; i < n; i++ {
			resp, err := http.Get(fmt.Sprintf("%s/fakepay/transaction/tx_%012d", front.URL, i))
			if err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}
	}
	hit(warmupSamples + 10)
	waitFor(func() bool { return a.Metrics.RecordingsWritten.Load() >= int64(warmupSamples+10) }, 10*time.Second)
	say("   baselines learned and frozen: GET /transaction/tx_{id} (2xx) — %d samples, redacted at write.", warmupSamples+10)
	say("")
	say("③ The provider now silently ships a change (renames an enum, adds a field) —")
	say("   exactly the kind of change that costs real money to find in production:")

	mutated.Store(true)
	hit(10)
	waitFor(func() bool { return a.Alerter.Sent() >= 2 }, 10*time.Second)
	time.Sleep(100 * time.Millisecond) // let alert prints flush

	say("")
	say("④ ONE alert per change, deduped across %d drifted requests, with the exact", 10)
	say("   field diff and a fingerprint.")
	say("")
	say("⑤ Detecting it is the easy part. The fingerprint above is a handle: feed it")
	say("   back and the change becomes a scenario that runs against a sandbox built")
	say("   from your provider's own spec, so the break happens on your laptop.")
	say("   That loop — rehearse, ship, observe, reproduce, keep it fixed — is the")
	say("   product. This demo showed one lap of it.")
	say("")
	say("Try the other end of the loop, no proxy and no production needed:")
	say("  pikopod init                        # scaffold pikopod.yaml")
	say("  pikopod import <name> --spec …      # their spec → a stateful sandbox")
	say("  pikopod scenario list <name>        # which failure modes YOUR integration has")
	say("")
	say("Then, when you are ready to watch real traffic:")
	say("  pikopod up                          # staging first, then production")
	say("  pikopod incidents                   # what failed; reproduce any of it")
	return nil
}

func waitFor(cond func() bool, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}
