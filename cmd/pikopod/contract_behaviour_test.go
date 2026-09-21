package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pikopod/pikopod/internal/behaviour"
	"github.com/pikopod/pikopod/internal/proxy"
)

func TestContractRendersObservedStateMachine(t *testing.T) {
	cfg := testConfig(t, "https://example.invalid")
	zero := 0
	cfg.Warmup.MinSamples, cfg.Warmup.MinHours = 2, &zero
	spec := `{"openapi":"3.0.0","info":{"title":"W","version":"1"},"paths":{"/charges/{id}":{"get":{"parameters":[{"name":"id","in":"path","required":true,"schema":{"type":"string"}}],"responses":{"200":{"description":"ok"}}}}}}`
	specPath := filepath.Join(t.TempDir(), "w.json")
	os.WriteFile(specPath, []byte(spec), 0o600)
	if err := sandboxAdd(cfg, "pay", specPath, "s", "", "", false, io.Discard); err != nil {
		t.Fatal(err)
	}
	var before strings.Builder
	if err := contractReport(cfg, "pay", &before); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(before.String(), "no observed state machine yet") {
		t.Fatalf("without a graph the report says so:\n%s", before.String())
	}
	tr := behaviour.New("pay", cfg.DataDir, 1, 0)
	tr.SetClock(func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) })
	for _, id := range []string{"ch_0000000001", "ch_0000000002"} {
		for _, st := range []string{"pending", "succeeded"} {
			tr.Observe(&proxy.Record{Method: "GET", Path: "/charges/" + id, Status: 200, RespKind: "json", RespBody: map[string]any{"status": st}})
		}
	}
	if err := tr.Persist(); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := contractReport(cfg, "pay", &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"observed state machine — pay", "GET /charges/ch_{id} · status", "pending      → succeeded        2", "never observed: succeeded → pending"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q:\n%s", want, out.String())
		}
	}
	var js strings.Builder
	if err := contractJSON(cfg, "pay", &js); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Sandbox   string `json:"sandbox"`
		Behaviour struct {
			Fields []struct {
				Field         string   `json:"field"`
				WarmedUp      bool     `json:"warmedUp"`
				NeverObserved []string `json:"neverObserved"`
				Edges         []struct {
					From, To string
					Count    int64
				} `json:"edges"`
			} `json:"fields"`
		} `json:"behaviour"`
	}
	if err := json.Unmarshal([]byte(js.String()), &doc); err != nil {
		t.Fatalf("--format json must be valid JSON: %v\n%s", err, js.String())
	}
	f := doc.Behaviour.Fields[0]
	if doc.Sandbox != "pay" || f.Field != "status" || !f.WarmedUp || f.Edges[0].Count != 2 || len(f.NeverObserved) != 1 {
		t.Fatalf("json shape: %+v", doc)
	}
}
