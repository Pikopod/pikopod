package proxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pikopod/pikopod/internal/sanitize"
)

func TestSamplingKeepsGuaranteedClasses(t *testing.T) {
	dir := t.TempDir()
	m := &Metrics{}
	rec := NewRecorder(dir, sanitize.NewTokenizer("k", "local", 1), m)
	rec.SetSampling(0.25)
	observed := 0
	rec.SetObserver(func(r *Record) bool {
		observed++
		return strings.HasPrefix(r.Path, "/notable")
	})

	ch := make(chan *Exchange)
	done := make(chan struct{})
	go func() { rec.Run(ch); close(done) }()
	ex := func(path string, status int) *Exchange {
		return &Exchange{Upstream: "prov", Method: "GET", Path: path, Status: status,
			RespBody: []byte(`{"ok":true}`), Start: time.Now()}
	}
	for i := 0; i < 20; i++ {
		ch <- ex("/routine", 200)
	}
	for i := 0; i < 3; i++ {
		ch <- ex("/broken", 500)
	}
	for i := 0; i < 2; i++ {
		ch <- ex("/notable", 200)
	}
	close(ch)
	<-done

	if observed != 25 {
		t.Fatalf("the observer must see EVERY record (learning is never sampled): %d", observed)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "recordings", "prov.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var r Record
		if json.Unmarshal([]byte(line), &r) == nil {
			counts[r.Path]++
		}
	}

	if counts["/routine"] != 5 {
		t.Fatalf("rate 0.25 over 20 routine records must persist exactly 5, got %d", counts["/routine"])
	}
	if counts["/broken"] != 3 {
		t.Fatalf("error responses are always persisted, got %d of 3", counts["/broken"])
	}
	if counts["/notable"] != 2 {
		t.Fatalf("observer-notable records are always persisted, got %d of 2", counts["/notable"])
	}
	if m.RecordingsWritten.Load() != 10 || m.RecordingsSampledOut.Load() != 15 {
		t.Fatalf("metrics wrong: written=%d sampled_out=%d", m.RecordingsWritten.Load(), m.RecordingsSampledOut.Load())
	}
}

func TestSamplingRequiresObserver(t *testing.T) {
	dir := t.TempDir()
	rec := NewRecorder(dir, sanitize.NewTokenizer("k", "local", 1), &Metrics{})
	rec.SetSampling(0)
	ch := make(chan *Exchange)
	done := make(chan struct{})
	go func() { rec.Run(ch); close(done) }()
	for i := 0; i < 4; i++ {
		ch <- &Exchange{Upstream: "prov", Method: "GET", Path: "/x", Status: 200,
			RespBody: []byte(`{"ok":true}`), Start: time.Now()}
	}
	close(ch)
	<-done
	raw, _ := os.ReadFile(filepath.Join(dir, "recordings", "prov.ndjson"))
	if n := strings.Count(string(raw), "\n"); n != 4 {
		t.Fatalf("without an observer every record must persist, got %d of 4", n)
	}
}
