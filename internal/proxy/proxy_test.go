package proxy

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/pikopod/pikopod/internal/config"
	"github.com/pikopod/pikopod/internal/sanitize"
)

func testServer(tb testing.TB, upstreamURL string, captureDepth int) *Server {
	tb.Helper()
	cfg := &config.Config{Upstreams: map[string]config.Upstream{
		"examplepay": {Listen: "/examplepay", Target: upstreamURL},
	}}
	s, err := New(cfg, &Metrics{}, captureDepth)
	if err != nil {
		tb.Fatal(err)
	}
	return s
}

// CRITICAL (fail-open): the proxied response must be byte-identical to the
// upstream's even when observation is fully wedged (nobody drains the
// channel and it overflows) — internal state can never touch the hot path.
func TestCriticalFailOpen_ByteIdenticalUnderObserverPressure(t *testing.T) {
	payload := make([]byte, 256<<10)
	rand.Read(payload)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Echo-Len", fmt.Sprint(len(body)))
		w.WriteHeader(201)
		w.Write(payload)
	}))
	defer up.Close()

	s := testServer(t, up.URL, 1) // capture depth 1: overflows immediately, nobody drains
	front := httptest.NewServer(s)
	defer front.Close()

	for i := 0; i < 25; i++ {
		resp, err := http.Post(front.URL+"/examplepay/transaction", "application/json", bytes.NewReader([]byte(`{"amount":1}`)))
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 201 || !bytes.Equal(got, payload) {
			t.Fatalf("iteration %d: response altered under observer pressure (status=%d, len=%d)", i, resp.StatusCode, len(got))
		}
	}
	if s.Metrics.CapturesDropped.Load() == 0 {
		t.Fatal("test invalid: expected drops to have occurred")
	}
}

// Upstream unreachable → honest 502 with the marker, and NEVER a retry
// (retrying a non-idempotent POST can double-charge).
func TestUpstreamUnreachable502NoRetry(t *testing.T) {
	var hits int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	up.Close() // dead upstream

	s := testServer(t, up.URL, 8)
	front := httptest.NewServer(s)
	defer front.Close()

	resp, err := http.Post(front.URL+"/examplepay/charge", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || resp.Header.Get("X-Pikopod-Error") != "upstream-unreachable" {
		t.Fatalf("want marked 502, got %d %q", resp.StatusCode, resp.Header.Get("X-Pikopod-Error"))
	}
	if hits != 0 {
		t.Fatalf("proxy must not retry, upstream saw %d hits", hits)
	}
}

func TestTokenGateAndHeaderStripped(t *testing.T) {
	var sawToken string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawToken = r.Header.Get("X-Pikopod-Token")
	}))
	defer up.Close()

	t.Setenv("PIKOPOD_TOKEN", "tok123")
	cfg := &config.Config{Upstreams: map[string]config.Upstream{"p": {Listen: "/p", Target: up.URL}}}
	// finish() is unexported; emulate via Load-equivalent: set token by env through a scratch config file instead.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "pikopod.yaml")
	os.WriteFile(cfgPath, []byte("listen: 0.0.0.0\nupstreams:\n  p:\n    target: "+up.URL+"\n"), 0o644)
	loaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = cfg
	s, err := New(loaded, &Metrics{}, 8)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(s)
	defer front.Close()

	resp, _ := http.Get(front.URL + "/p/x")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token must 401, got %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("GET", front.URL+"/p/x", nil)
	req.Header.Set("X-Pikopod-Token", "tok123")
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid token must pass, got %d", resp.StatusCode)
	}
	if sawToken != "" {
		t.Fatal("X-Pikopod-Token must never be forwarded upstream")
	}
}

// Recorder end-to-end: recordings are sanitized at write — canary sentinels
// (secret key, email, card number) must NOT survive to disk; enum strings must.
func TestRecorderRedactsAtWrite(t *testing.T) {
	const canarySecret = "xpay_secret_CANARY0000000000000000"
	const canaryEmail = "canary.person@realmail.com"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"tx_9CANARY9","status":"success","customer_email":%q,"api_key":%q,"amount":5000}`, canaryEmail, canarySecret)
	}))
	defer up.Close()

	s := testServer(t, up.URL, 64)
	dir := t.TempDir()
	tok := sanitize.NewTokenizer("test-master", "local", 1)
	rec := NewRecorder(dir, tok, s.Metrics)
	done := make(chan struct{})
	go func() { rec.Run(s.captures); close(done) }()

	front := httptest.NewServer(s)
	req, _ := http.NewRequest("GET", front.URL+"/examplepay/transaction/tx_8f3a91b2c4?customer=cus_9s6XKzkNRiz8i3", nil)
	req.Header.Set("Authorization", "Bearer "+canarySecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()
	front.Close()

	deadline := time.Now().Add(2 * time.Second)
	var raw []byte
	for time.Now().Before(deadline) {
		raw, _ = os.ReadFile(filepath.Join(dir, "recordings", "examplepay.ndjson"))
		if len(raw) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	close(s.captures)
	<-done
	if len(raw) == 0 {
		t.Fatal("no recording written")
	}
	text := string(raw)
	for _, sentinel := range []string{canarySecret, canaryEmail, "tx_8f3a91b2c4", "cus_9s6XKzkNRiz8i3"} {
		if strings.Contains(text, sentinel) {
			t.Fatalf("CANARY LEAK: %q reached disk:\n%s", sentinel, text)
		}
	}
	var record Record
	if err := json.Unmarshal(bytes.TrimSpace(raw), &record); err != nil {
		t.Fatalf("recording is not valid ndjson: %v", err)
	}
	body, _ := record.RespBody.(map[string]any)
	if body["status"] != "success" {
		t.Fatalf("enum values must survive sanitization, got %v", body["status"])
	}
	if body["amount"] != float64(5000) {
		t.Fatalf("numeric scalars must survive, got %v", body["amount"])
	}
	if record.Redactions == 0 {
		t.Fatal("expected redactions to be counted")
	}
}

// Latency budget: p50 added latency ≤ 5ms. Loopback benchmark —
// generous margin in CI, tight signal locally.
func TestLatencyBudgetP50(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()

	s := testServer(t, up.URL, 1024)
	go NewRecorder(t.TempDir(), sanitize.NewTokenizer("k", "local", 1), s.Metrics).Run(s.captures)
	front := httptest.NewServer(s)
	defer front.Close()

	direct := timeRequests(t, up.URL, 60)
	proxied := timeRequests(t, front.URL+"/examplepay/x", 60)
	added := proxied - direct
	if added > 5*time.Millisecond {
		t.Fatalf("p50 added latency %v exceeds 5ms budget (direct %v, proxied %v)", added, direct, proxied)
	}
}

func timeRequests(t *testing.T, url string, n int) time.Duration {
	t.Helper()
	durs := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		start := time.Now()
		resp, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		io.ReadAll(resp.Body)
		resp.Body.Close()
		durs = append(durs, time.Since(start))
	}
	sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
	return durs[n/2]
}

// Tokenless listeners refuse foreign Host headers (DNS-rebinding/CSRF
// guard) while loopback spellings pass.
func TestTokenlessListenerRefusesForeignHost(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer up.Close()
	s := testServer(t, up.URL, 8)
	front := httptest.NewServer(s)
	defer front.Close()

	// httptest clients send Host: 127.0.0.1:<port> — allowed.
	resp, err := http.Get(front.URL + "/examplepay/x")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("loopback host must pass: %v %d", err, resp.StatusCode)
	}
	resp.Body.Close()

	// A rebound page's request arrives with the attacker's Host.
	req, _ := http.NewRequest("GET", front.URL+"/examplepay/x", nil)
	req.Host = "attacker.example.com"
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign Host on a tokenless listener must be refused: %d", resp.StatusCode)
	}

	// localhost spelling passes too.
	req, _ = http.NewRequest("GET", front.URL+"/examplepay/x", nil)
	req.Host = "localhost:4700"
	resp, err = http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("localhost host must pass: %v %d", err, resp.StatusCode)
	}
	resp.Body.Close()
}

// The token header never reaches the upstream — in EVERY configuration,
// tokenless loopback included.
func TestTokenHeaderNeverForwardedUpstream(t *testing.T) {
	var sawToken string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawToken = r.Header.Get("X-Pikopod-Token")
	}))
	defer up.Close()
	s := testServer(t, up.URL, 8) // NO token configured
	front := httptest.NewServer(s)
	defer front.Close()

	req, _ := http.NewRequest("GET", front.URL+"/examplepay/x", nil)
	req.Header.Set("X-Pikopod-Token", "leaky-shim-value")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if sawToken != "" {
		t.Fatalf("token header leaked upstream on a tokenless setup: %q", sawToken)
	}
}

// captureWriter must expose Unwrap so ReverseProxy's protocol-upgrade path
// (ResponseController → Hijacker) can reach the real conn — a WebSocket
// upgrade through the proxy must not 502.
func TestCaptureWriterUnwrapsForUpgrades(t *testing.T) {
	var cw interface{ Unwrap() http.ResponseWriter } = &captureWriter{}
	if cw == nil {
		t.Fatal("unreachable")
	}
}
