package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type row struct {
	N int `json:"n"`
}

func TestNDJSONRotationDropOldest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log.ndjson")

	nd, err := OpenNDJSON(path, 50)
	if err != nil {
		t.Fatal(err)
	}
	defer nd.Close()
	for i := 0; i < 100; i++ {
		if err := nd.Append(row{N: i}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	cur, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	prev, err := os.Stat(path + ".1")
	if err != nil {
		t.Fatal("rotation must keep exactly one previous generation:", err)
	}
	if cur.Size() > 60 || prev.Size() > 60 {
		t.Fatalf("generations must stay near the cap: cur=%d prev=%d", cur.Size(), prev.Size())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("at most two generations may exist: %v", entries)
	}

	last, err := nd.ReadLast(1)
	if err != nil || len(last) != 1 {
		t.Fatalf("ReadLast: %v %v", last, err)
	}
	var r row
	json.Unmarshal(last[0], &r)
	if r.N != 99 {
		t.Fatalf("newest row must survive rotation: %+v", r)
	}
}

func TestNDJSONReadLast(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.ndjson")
	nd, err := OpenNDJSON(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		nd.Append(row{N: i})
	}
	nd.Close()

	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(`{"n":`)
	f.Close()

	nd2, err := OpenNDJSON(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer nd2.Close()
	last, err := nd2.ReadLast(3)
	if err != nil || len(last) != 3 {
		t.Fatalf("limit: got %d records (%v)", len(last), err)
	}
	var r row
	if json.Unmarshal(last[2], &r) != nil || r.N != 9 {
		t.Fatalf("the torn tail must be skipped, newest complete row last: %s", last[2])
	}
}

func TestSaltLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".salt")
	s1, err := LoadOrCreateSalt(path)
	if err != nil || len(s1) != 64 {
		t.Fatalf("fresh salt: %q %v", s1, err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("salt must be 0600, got %v", info.Mode().Perm())
	}
	s2, err := LoadOrCreateSalt(path)
	if err != nil || s2 != s1 {
		t.Fatalf("salt must be stable across loads: %q vs %q (%v)", s1, s2, err)
	}

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateSalt(path); err == nil {
		t.Fatal("a group/other-readable salt must be refused")
	}

	os.Chmod(path, 0o600)
	os.WriteFile(path, []byte("short"), 0o600)
	if _, err := LoadOrCreateSalt(path); err == nil {
		t.Fatal("a truncated salt must be refused, not silently regenerated")
	}
}
