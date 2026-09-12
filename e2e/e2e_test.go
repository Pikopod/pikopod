// Package e2e drives the REAL pikopod binary as a subprocess through the
// product's full loop — import → up → traffic → warmup → silent provider
// change → one deduped Slack alert → CI gate exit codes → from-drift pin —
// asserting the things in-process tests structurally cannot: CLI wiring,
// config loading from disk, process lifecycle and SIGTERM persistence,
// cross-restart state, real HTTP egress to the (fake) Slack webhook, and
// the documented exit-code contract (0 clean / 1 drift / 2 error).
package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

var binPath string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "pikopod-e2e-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	binPath = filepath.Join(dir, "pikopod")
	build := exec.Command("go", "build", "-o", binPath, "github.com/pikopod/pikopod/cmd/pikopod")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "building pikopod:", err)
		os.Exit(2)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// run executes the binary to completion in dir and returns combined output
// and the exit code.
func run(t *testing.T, dir string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(binPath, args...)
	cmd.Dir = dir
	out, _ := cmd.CombinedOutput()
	return string(out), cmd.ProcessState.ExitCode()
}

// freePorts reserves n DISTINCT ephemeral ports. Every listener is held open
// until all n are chosen — taking them one at a time lets the kernel hand back
// the port it just reclaimed, which would silently collide the agent and
// sandbox listeners.
func freePorts(t *testing.T, n int) []int {
	t.Helper()
	ls := make([]net.Listener, 0, n)
	ports := make([]int, 0, n)
	for i := 0; i < n; i++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ls = append(ls, l)
		ports = append(ports, l.Addr().(*net.TCPAddr).Port)
	}
	for _, l := range ls {
		l.Close()
	}
	return ports
}

func waitUntil(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

const thingsSpec = `{
  "openapi": "3.0.0", "info": {"title": "Things", "version": "1"},
  "paths": {
    "/things": {"post": {
      "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Thing"}}}},
      "responses": {"201": {"description": "created", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Thing"}}}}}
    }},
    "/things/{id}": {"get": {
      "parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "string"}}],
      "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Thing"}}}}}
    }}
  },
  "components": {"schemas": {"Thing": {"type": "object", "properties": {
    "id": {"type": "string"}, "status": {"type": "string"}, "amount": {"type": "number"}
  }}}}
}`

// upProcess manages one `pikopod up` subprocess.
type upProcess struct {
	cmd *exec.Cmd
	out *lockedBuffer
}

type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func startUp(t *testing.T, dir string, agentPort int) *upProcess {
	t.Helper()
	buf := &lockedBuffer{}
	cmd := exec.Command(binPath, "up", "--config", "pikopod.yaml")
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = buf, buf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	up := &upProcess{cmd: cmd, out: buf}
	t.Cleanup(func() {
		if cmd.Process != nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	})
	waitUntil(t, "agent /healthz", 15*time.Second, func() bool {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", agentPort))
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == 200
	})
	return up
}

// stop SIGTERMs and asserts the documented clean-shutdown contract: exit 0,
// state persisted.
func (u *upProcess) stop(t *testing.T) {
	t.Helper()
	u.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- u.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("up did not shut down on SIGTERM\n%s", u.out.String())
	}
	if code := u.cmd.ProcessState.ExitCode(); code != 0 {
		t.Fatalf("graceful shutdown must exit 0, got %d\n%s", code, u.out.String())
	}
}

func healthz(t *testing.T, agentPort int) map[string]any {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", agentPort))
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var m map[string]any
	json.NewDecoder(resp.Body).Decode(&m)
	return m
}

func TestFullLoop(t *testing.T) {
	dir := t.TempDir()

	// The "provider": normal until mutated, then it silently ships fee_bearer.
	var mutated atomic.Bool
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		extra := ""
		if mutated.Load() {
			extra = `,"fee_bearer":"merchant"`
		}
		fmt.Fprintf(w, `{"id":"th_0000000001","status":"success","amount":100%s}`, extra)
	}))
	defer provider.Close()

	// The "Slack channel": captures every webhook post.
	var slackMu sync.Mutex
	var slackTexts []string
	slack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var m map[string]string
		json.Unmarshal(raw, &m)
		slackMu.Lock()
		slackTexts = append(slackTexts, m["text"])
		slackMu.Unlock()
	}))
	defer slack.Close()
	slackPosts := func(substr string) int {
		slackMu.Lock()
		defer slackMu.Unlock()
		n := 0
		for _, txt := range slackTexts {
			if strings.Contains(txt, substr) {
				n++
			}
		}
		return n
	}

	ports := freePorts(t, 2)
	agentPort, sbxPort := ports[0], ports[1]
	if err := os.WriteFile(filepath.Join(dir, "spec.json"), []byte(thingsSpec), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf(`listen: 127.0.0.1
agent_port: %d
sandbox_port: %d
data_dir: data
upstreams:
  prov:
    target: %s
warmup:
  min_samples: 12
  min_hours: 0
slack:
  webhook_url: %s
`, agentPort, sbxPort, provider.URL, slack.URL)
	if err := os.WriteFile(filepath.Join(dir, "pikopod.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	// ① import: spec → registered sandbox, auto-linked to the upstream.
	out, code := run(t, dir, "import", "prov", "--spec", "spec.json", "--config", "pikopod.yaml")
	if code != 0 || !strings.Contains(out, "sandbox prov registered") {
		t.Fatalf("import failed (%d): %s", code, out)
	}

	// ② up + warm traffic until the baseline freezes; SIGTERM persists it.
	up := startUp(t, dir, agentPort)
	client := &http.Client{Timeout: 5 * time.Second}
	hit := func(n int) {
		for i := 0; i < n; i++ {
			resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/prov/things/th_0000000001", agentPort))
			if err != nil {
				t.Fatalf("proxy request: %v", err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("pass-through must serve the provider's 200, got %d", resp.StatusCode)
			}
		}
	}
	hit(15)
	waitUntil(t, "15 recordings", 15*time.Second, func() bool {
		h := healthz(t, agentPort)
		if h == nil {
			return false
		}
		n, _ := h["recordings_written"].(float64)
		return n >= 15
	})

	// The sandbox serves the SAME process: stateful create → read-your-write.
	resp, err := client.Post(fmt.Sprintf("http://127.0.0.1:%d/prov/things", sbxPort), "application/json", strings.NewReader(`{"status":"pending","amount":5}`))
	if err != nil || resp.StatusCode != 201 {
		t.Fatalf("sandbox create: %v %v", err, resp)
	}
	var created map[string]any
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatalf("sandbox create returned no id: %v", created)
	}
	resp, err = client.Get(fmt.Sprintf("http://127.0.0.1:%d/prov/things/%s", sbxPort, id))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("sandbox read-back: %v %v", err, resp)
	}
	var got map[string]any
	json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if got["status"] != "pending" {
		t.Fatalf("read-your-write broke across the HTTP surface: %v", got)
	}

	up.stop(t)

	// ③ CI gate on clean recordings: exit 0.
	out, code = run(t, dir, "replay", "--ci", "prov")
	if code != 0 || !strings.Contains(out, "clean") {
		t.Fatalf("clean gate must exit 0 (got %d): %s", code, out)
	}

	// ④ restart (baselines load from disk), the provider silently ships a
	// change, and EXACTLY ONE alert reaches Slack across 5 drifted requests.
	up = startUp(t, dir, agentPort)
	mutated.Store(true)
	hit(5)
	waitUntil(t, "the Slack alert", 15*time.Second, func() bool { return slackPosts("fee_bearer") >= 1 })
	up.stop(t)
	if n := slackPosts("fee_bearer"); n != 1 {
		t.Fatalf("dedupe must hold across drifted requests: %d Slack posts", n)
	}

	// The alert's fingerprint is in the persisted event log — the handle the
	// whole from-drift loop keys on.
	evRaw, err := os.ReadFile(filepath.Join(dir, "data", "events.ndjson"))
	if err != nil {
		t.Fatalf("event log missing: %v", err)
	}
	var fp string
	for _, line := range strings.Split(strings.TrimSpace(string(evRaw)), "\n") {
		var ev map[string]any
		if json.Unmarshal([]byte(line), &ev) == nil && ev["kind"] == "field_added" && ev["field"] == "fee_bearer" {
			fp, _ = ev["fingerprint"].(string)
		}
	}
	if !regexp.MustCompile(`^fp_[0-9a-f]{12}$`).MatchString(fp) {
		t.Fatalf("no field_added event persisted: %q in\n%s", fp, evRaw)
	}
	if slackMu.Lock(); !strings.Contains(strings.Join(slackTexts, "\n"), fp) {
		slackMu.Unlock()
		t.Fatalf("the Slack alert must carry the replayable fingerprint %s", fp)
	} else {
		slackMu.Unlock()
	}

	// ⑤ CI gate now fails the build: exit 1, naming the drift.
	out, code = run(t, dir, "replay", "--ci", "prov")
	if code != 1 || !strings.Contains(out, "field_added") || !strings.Contains(out, "fee_bearer") {
		t.Fatalf("drifted gate must exit 1 naming the drift (got %d): %s", code, out)
	}

	// ⑥ from-drift: the fingerprint becomes a pinned scenario, replayed
	// against the sandbox — green now, breaking the moment a re-import
	// adopts the change.
	out, code = run(t, dir, "scenario", "from-drift", fp)
	if code != 0 || !strings.Contains(out, "baseline pinned and green") {
		t.Fatalf("from-drift must pin and pass on the baseline sandbox (got %d): %s", code, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "data", "scenarios", "drift-"+strings.TrimPrefix(fp, "fp_")+".yaml")); err != nil {
		t.Fatalf("pinned pack must persist for CI reuse: %v", err)
	}
}

// The exit-code contract's error leg: a tool/config problem is exit 2,
// never conflated with drift's exit 1.
func TestConfigErrorExitsTwo(t *testing.T) {
	dir := t.TempDir()
	out, code := run(t, dir, "up", "--config", "does-not-exist.yaml")
	if code != 2 {
		t.Fatalf("config errors must exit 2, got %d: %s", code, out)
	}
	// Unknown config keys are startup errors too (dead-knob guarantee),
	// through the real CLI surface.
	os.WriteFile(filepath.Join(dir, "pikopod.yaml"), []byte("upstreams: {}\nnot_a_real_key: true\n"), 0o600)
	out, code = run(t, dir, "up", "--config", "pikopod.yaml")
	if code != 2 || !strings.Contains(out, "not_a_real_key") {
		t.Fatalf("unknown keys must be loud startup errors (got %d): %s", code, out)
	}
}
