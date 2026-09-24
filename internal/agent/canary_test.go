package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pikopod/pikopod/internal/alert"
	"github.com/pikopod/pikopod/internal/config"
)

func TestCanarySentinelGate(t *testing.T) {
	sentinels := []string{
		"xpay_secret_M6CANARY00000000000000000000",
		"m6.canary.person@leakmail.test",
		"4242424242424242",
		"+2348012345678",
		"0f8fad5b-d9cb-469f-a165-70867728950e",
		"tok_M6CANARY_response_secret_11112222",
		"drifted_M6CANARY_enum_value",

		"acct_M6canary99z",
		"m6.canary%40leakmail.test",
		"4556737586899855",
		"m6canaryfirstname",
		"934187",
		"91736408",
		"19470213",
		"1947-02-13",
		"9012345671",
		"91234567805",

		"m6canarykey@leakmail.test",
	}

	var mutated atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Provider-Secret", sentinels[5])
		status := "success"
		if mutated.Load() {
			status = sentinels[6]
		}
		fmt.Fprintf(w, `{
			"id": %q, "status": %q, "amount": 5000,
			"customer": {"email": %q, "phone": %q, "card": {"number": %q, "cvv2": "9471", "exp_month": 12, "exp_year": 2027, "pin": %s}, "first_name": %q, "dob": %s, "date_of_birth": %q},
			"api_key": %q,
			"balances": {%q: {"amount": 100}, %q: {"amount": 7}},
			"card_number": %s,
			"otp": %s, "otp_code": %q,
			"account_number": %s, "bvn": %s
		}`, sentinels[4], status, sentinels[1], sentinels[3], sentinels[2], sentinels[11], sentinels[10],
			sentinels[13], sentinels[14], sentinels[0], sentinels[7], sentinels[17], sentinels[9],
			sentinels[12], sentinels[12], sentinels[15], sentinels[16])
	}))
	defer upstream.Close()

	dir := t.TempDir()
	cfg := &config.Config{
		Listen: "127.0.0.1", AgentPort: 0, DataDir: dir,
		Upstreams: map[string]config.Upstream{"prov": {Listen: "/prov", Target: upstream.URL}},
		Warmup:    config.Warmup{MinSamples: 10, MinHours: new(int)},
	}
	sink := newCaptureSink()
	a, err := New(cfg, alert.Options{MinOccurrences: 2, Window: time.Minute}, sink)
	if err != nil {
		t.Fatal(err)
	}
	startPipeline(t, a)
	front := httptest.NewServer(a.Proxy)
	defer front.Close()

	hit := func(n int) {
		t.Helper()
		for i := 0; i < n; i++ {

			url := front.URL + "/prov/users/" + sentinels[8] + "/charges?api_key=" + sentinels[0] + "&card=" + sentinels[2]
			req, _ := http.NewRequest("POST", url, strings.NewReader(
				fmt.Sprintf(`{"email":%q,"card_number":%q,"secret":%q,"cvv":9471,"pin":%q,"otp":%s}`,
					sentinels[1], sentinels[2], sentinels[0], sentinels[11], sentinels[12])))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+sentinels[0])
			req.Header.Set("X-Api-Key", sentinels[0])
			resp, err := http.DefaultClient.Do(req)
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
	waitFor(t, func() bool { return a.Metrics.RecordingsWritten.Load() >= 18 && sink.n.Load() >= 1 })
	a.persistAll()

	swept := 0
	err = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		swept++
		text := strings.ToLower(string(raw))
		for _, s := range sentinels {
			if strings.Contains(text, strings.ToLower(s)) {
				t.Errorf("CANARY LEAK: %q reached %s at rest", s, path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if swept < 3 {
		t.Fatalf("sweep only saw %d files — the pipeline did not persist what this gate audits", swept)
	}

	shortSecretKey := regexp.MustCompile(`(?i)(^|_|-)(cvv2?|cvc2?|pin|otp|exp[_-]?month|exp[_-]?year)($|_|-)`)
	recRaw, err := os.ReadFile(filepath.Join(dir, "recordings", "prov.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(recRaw)), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		var walk func(node any)
		walk = func(node any) {
			switch n := node.(type) {
			case map[string]any:
				for k, v := range n {
					if shortSecretKey.MatchString(k) {
						s, isStr := v.(string)
						if !isStr || !(strings.HasPrefix(s, "<<SUBSTITUTE") || len(s) == 0) {

							if s == "9471" || s == "12" || s == "2027" || !isStr {
								t.Errorf("CANARY LEAK: short secret %q survived with raw-typed value %v", k, v)
							}
						}
					}
					walk(v)
				}
			case []any:
				for _, v := range n {
					walk(v)
				}
			}
		}
		walk(rec)
	}

	delivered := strings.ToLower(strings.Join(sink.texts, "\n"))
	if delivered == "" {
		t.Fatal("expected at least one alert to have been delivered")
	}
	for _, s := range sentinels {
		if strings.Contains(delivered, strings.ToLower(s)) {
			t.Errorf("CANARY LEAK: %q egressed via the alert transport", s)
		}
	}
}
