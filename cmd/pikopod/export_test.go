package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pikopod/pikopod/internal/proxy"
)

func exportRecord() *proxy.Record {
	return &proxy.Record{
		TS: time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC), Upstream: "pay",
		Method: "POST", Path: "/charges?limit=1", Status: 201, DurMS: 42,
		ReqHeader:  map[string]any{"content-type": "application/json", "authorization": "<<SUBSTITUTE:token>>"},
		RespHeader: map[string]any{"content-type": "application/json"},
		ReqBody:    map[string]any{"amount": float64(900), "note": "it's fine"},
		RespBody:   map[string]any{"id": "tok_abc", "status": "success"},
		ReqSize:    30, RespSize: 40, ReqKind: "json", RespKind: "json",
	}
}

func TestRenderCurl(t *testing.T) {
	var out bytes.Buffer
	renderCurl(&out, "https://api.pay.example/", exportRecord())
	got := out.String()
	for _, want := range []string{
		"curl -X POST 'https://api.pay.example/charges?limit=1'",
		`-H 'authorization: <<SUBSTITUTE:token>>'`,
		`"amount":900`,
		"# 2026-09-06T12:00:00Z → 201 (42ms)",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("curl export missing %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, `it'\''s fine`) {
		t.Fatalf("single quotes must be shell-escaped:\n%s", got)
	}
}

func TestHarArchive(t *testing.T) {
	doc := harArchive("https://api.pay.example", []*proxy.Record{exportRecord()})
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Log struct {
			Version string `json:"version"`
			Entries []struct {
				Request struct {
					Method   string `json:"method"`
					URL      string `json:"url"`
					PostData struct {
						Text string `json:"text"`
					} `json:"postData"`
				} `json:"request"`
				Response struct {
					Status  int `json:"status"`
					Content struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"response"`
			} `json:"entries"`
		} `json:"log"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Log.Version != "1.2" || len(parsed.Log.Entries) != 1 {
		t.Fatalf("archive shape wrong: %s", raw)
	}
	e := parsed.Log.Entries[0]
	if e.Request.Method != "POST" || e.Request.URL != "https://api.pay.example/charges?limit=1" {
		t.Fatalf("request wrong: %+v", e.Request)
	}
	if e.Response.Status != 201 || !strings.Contains(e.Response.Content.Text, "tok_abc") {
		t.Fatalf("response wrong: %+v", e.Response)
	}
}
