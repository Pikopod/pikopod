package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pikopod/pikopod/internal/sanitize"
)

func recordOne(t *testing.T, rules map[string][]sanitize.Rule, respBody, reqBody string) Record {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, respBody)
	}))
	defer up.Close()
	s := testServer(t, up.URL, 64)
	dir := t.TempDir()
	rec := NewRecorder(dir, sanitize.NewTokenizer("test-master", "local", 1), s.Metrics)
	rec.SetSpecRules(rules)
	done := make(chan struct{})
	go func() { rec.Run(s.captures); close(done) }()
	front := httptest.NewServer(s)
	resp, err := http.Post(front.URL+"/examplepay/charges", "application/json", strings.NewReader(reqBody))
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
	var record Record
	if err := json.Unmarshal(bytes.TrimSpace(raw), &record); err != nil {
		t.Fatalf("no valid recording: %v", err)
	}
	return record
}

var enumRules = map[string][]sanitize.Rule{"examplepay": {
	{Field: "currency", Mode: sanitize.ModeAllow, AllowedValues: []string{"NGN", "USD"}},
	{Field: "status", Mode: sanitize.ModeAllow, AllowedValues: []string{"ACTIVE", "PENDING"}},
}}

func TestSpecRulesKeepDeclaredEnumsOnDisk(t *testing.T) {
	rec := recordOne(t, enumRules, `{"status":"ACTIVE","currency":"NGN","note":"free text"}`, `{"currency":"NGN"}`)
	body, _ := rec.RespBody.(map[string]any)
	if body["status"] != "ACTIVE" || body["currency"] != "NGN" {
		t.Fatalf("declared values must survive: %v", body)
	}
	if _, kept := body["note"]; kept {
		t.Fatalf("free text is still dropped: %v", body)
	}
	req, _ := rec.ReqBody.(map[string]any)
	if _, kept := req["currency"]; kept {
		t.Fatalf("rules cover response bodies only; the request is treated as today: %v", req)
	}
	undeclared := recordOne(t, enumRules, `{"status":"some free text error","currency":"GHS"}`, `{}`)
	today := recordOne(t, nil, `{"status":"some free text error","currency":"GHS"}`, `{}`)
	if fmt.Sprint(undeclared.RespBody) != fmt.Sprint(today.RespBody) || undeclared.Redactions != today.Redactions {
		t.Fatalf("outside the declared set must classify exactly as today:\n with %v (%d)\n today %v (%d)", undeclared.RespBody, undeclared.Redactions, today.RespBody, today.Redactions)
	}
}

func TestNoSpecRulesIsByteIdenticalToToday(t *testing.T) {
	body := `{"status":"ACTIVE","currency":"NGN","amount":5000}`
	a := recordOne(t, nil, body, `{}`)
	b := recordOne(t, map[string][]sanitize.Rule{}, body, `{}`)
	ja, _ := json.Marshal(a.RespBody)
	jb, _ := json.Marshal(b.RespBody)
	if string(ja) != string(jb) {
		t.Fatalf("empty rule map must equal nil: %s vs %s", ja, jb)
	}
	resp, _ := a.RespBody.(map[string]any)
	if _, kept := resp["currency"]; kept {
		t.Fatalf("today an uppercase currency is dropped; pin it: %v", resp)
	}
}
