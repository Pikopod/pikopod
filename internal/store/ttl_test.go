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

// The TTL contract: a record survives at least TTL, is deleted by ~2×TTL
// (+ one sweep), and a QUIET log still ages out via Sweep. Real clock with
// margins ≫ the TTL so timing noise cannot flip outcomes.
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
	nd.Append(tsRow{TS: time.Now(), N: 2}) // same generation, under TTL (400ms margin)
	time.Sleep(600 * time.Millisecond)     // generation now ~1.2s old (400ms past TTL)
	nd.Append(tsRow{TS: time.Now(), N: 3})

	// The expired generation rotated ON WRITE: r1,r2 → .1, r3 current.
	prev, err := os.ReadFile(path + ".1")
	if err != nil || !containsN(prev, 1) || !containsN(prev, 2) {
		t.Fatalf("expired generation must rotate on append: %v %s", err, prev)
	}
	cur, _ := os.ReadFile(path)
	if !containsN(cur, 3) || containsN(cur, 1) {
		t.Fatalf("current generation wrong: %s", cur)
	}
	// .1's newest record (r2, ~600ms old — rename preserves mtime) is still
	// inside the TTL: the generation survives the sweep.
	if err := nd.Sweep(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatal("previous generation must survive until its newest record expires")
	}

	// No further appends: the SWEEP alone must age everything out.
	time.Sleep(1200 * time.Millisecond)
	nd.Sweep() // r3's generation expired → rotates (replacing the old .1)
	if cur, _ := os.ReadFile(path); containsN(cur, 3) {
		t.Fatalf("quiet current generation must rotate out via sweep: %s", cur)
	}
	time.Sleep(1200 * time.Millisecond)
	nd.Sweep() // r3's .1 now expired → deleted; nothing remains
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Fatalf("expired previous generation must be deleted: %v", err)
	}
}

// The TTL clock survives restarts: genStart is recovered from the first
// record's ts, so a reopened old log rotates on the next write instead of
// getting a fresh lease on life.
func TestNDJSONTTLSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.ndjson")
	nd, _ := OpenNDJSON(path, 0)
	nd.Append(tsRow{TS: time.Now().Add(-time.Hour), N: 1}) // an old record
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

// TTL off (the default) keeps size-only behavior: Sweep is a no-op.
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
