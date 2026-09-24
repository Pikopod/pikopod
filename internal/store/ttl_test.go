package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type tsRow struct {
	TS time.Time `json:"ts"`
	N  int       `json:"n"`
}

func TestNDJSONTTLWindow(t *testing.T) {
	const ttl = 1 * time.Second
	path := filepath.Join(t.TempDir(), "log.ndjson")
	nd, err := OpenNDJSON(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer nd.Close()
	nd.SetTTL(ttl)

	nd.Append(tsRow{TS: time.Now(), N: 1})
	time.Sleep(600 * time.Millisecond)
	nd.Append(tsRow{TS: time.Now(), N: 2})
	time.Sleep(600 * time.Millisecond)
	nd.Append(tsRow{TS: time.Now(), N: 3})

	prev, err := os.ReadFile(path + ".1")
	if err != nil || !containsN(prev, 1) || !containsN(prev, 2) {
		t.Fatalf("expired generation must rotate on append: %v %s", err, prev)
	}
	cur, _ := os.ReadFile(path)
	if !containsN(cur, 3) || containsN(cur, 1) {
		t.Fatalf("current generation wrong: %s", cur)
	}

	if err := nd.Sweep(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatal("previous generation must survive until its newest record expires")
	}

	time.Sleep(1200 * time.Millisecond)
	nd.Sweep()
	if cur, _ := os.ReadFile(path); containsN(cur, 3) {
		t.Fatalf("quiet current generation must rotate out via sweep: %s", cur)
	}
	time.Sleep(1200 * time.Millisecond)
	nd.Sweep()
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Fatalf("expired previous generation must be deleted: %v", err)
	}
}

func TestNDJSONTTLSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.ndjson")
	nd, _ := OpenNDJSON(path, 0)
	nd.Append(tsRow{TS: time.Now().Add(-time.Hour), N: 1})
	nd.Close()

	nd2, err := OpenNDJSON(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer nd2.Close()
	nd2.SetTTL(time.Minute)
	nd2.Append(tsRow{TS: time.Now(), N: 2})
	prev, err := os.ReadFile(path + ".1")
	if err != nil || !containsN(prev, 1) {
		t.Fatalf("recovered genStart must expire the old generation on reopen: %v %s", err, prev)
	}
}

func TestNDJSONTTLOffIsInert(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.ndjson")
	nd, _ := OpenNDJSON(path, 0)
	defer nd.Close()
	nd.Append(tsRow{TS: time.Now().Add(-24 * time.Hour), N: 1})
	if err := nd.Sweep(); err != nil {
		t.Fatal(err)
	}
	if cur, _ := os.ReadFile(path); !containsN(cur, 1) {
		t.Fatal("without a TTL nothing may age out")
	}
}

func containsN(raw []byte, n int) bool {
	for _, line := range splitLines(raw) {
		var r tsRow
		if json.Unmarshal(line, &r) == nil && r.N == n {
			return true
		}
	}
	return false
}

func splitLines(raw []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, b := range raw {
		if b == '\n' {
			if i > start {
				out = append(out, raw[start:i])
			}
			start = i + 1
		}
	}
	return out
}
