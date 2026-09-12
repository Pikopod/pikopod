package store

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// NDJSON is an append-only newline-delimited JSON log, rotated drop-oldest at
// maxBytes or a generational TTL (at most two generations; TTL 0 = size only).
type NDJSON struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	f        *os.File
	w        *bufio.Writer
	size     int64
	ttl      time.Duration
	// genStart is when the CURRENT generation's oldest record was written
	// (recovered on open; zero until the first append on a fresh generation).
	genStart time.Time
	now      func() time.Time
}

func OpenNDJSON(path string, maxBytes int64) (*NDJSON, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	n := &NDJSON{path: path, maxBytes: maxBytes, f: f, w: bufio.NewWriter(f), size: st.Size(), now: time.Now}
	if st.Size() > 0 {
		n.genStart = firstRecordTime(path)
	}
	return n, nil
}

// SetTTL arms age-based retention (0 disables — size-only rotation).
func (n *NDJSON) SetTTL(ttl time.Duration) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.ttl = ttl
}

// firstRecordTime recovers the generation's birth from its first line so the
// TTL clock survives restarts; an unparseable line only lengthens retention.
func firstRecordTime(path string) time.Time {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}
	}
	defer f.Close()
	line, err := bufio.NewReaderSize(f, 1<<20).ReadBytes('\n')
	if err != nil {
		return time.Time{}
	}
	var probe struct {
		TS        time.Time `json:"ts"`
		FirstSeen time.Time `json:"first_seen"`
	}
	if json.Unmarshal(line, &probe) != nil {
		return time.Time{}
	}
	if !probe.TS.IsZero() {
		return probe.TS
	}
	return probe.FirstSeen
}

// Append writes one record. On error the caller counts and drops — the hot
// path never blocks on storage.
func (n *NDJSON) Append(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	needRotate := n.maxBytes > 0 && n.size+int64(len(raw))+1 > n.maxBytes
	if n.ttl > 0 && !n.genStart.IsZero() && n.now().Sub(n.genStart) > n.ttl {
		needRotate = needRotate || n.size > 0
	}
	if needRotate {
		if err := n.rotateLocked(); err != nil {
			return err
		}
	}
	if n.w == nil {
		// A previous rotation failed after the rename (see rotateLocked):
		// recover by creating the fresh generation now.
		f, oErr := os.OpenFile(n.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if oErr != nil {
			return oErr
		}
		n.f, n.w, n.size = f, bufio.NewWriter(f), 0
	}
	if n.genStart.IsZero() {
		n.genStart = n.now()
	}
	if _, err := n.w.Write(append(raw, '\n')); err != nil {
		return err
	}
	n.size += int64(len(raw)) + 1
	return n.w.Flush()
}

// Sweep enforces the TTL between appends (a quiet log must still age out);
// rename preserves mtime, so mtime(.1) is the previous gen's newest record.
func (n *NDJSON) Sweep() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.ttl <= 0 {
		return nil
	}
	if !n.genStart.IsZero() && n.now().Sub(n.genStart) > n.ttl && n.size > 0 {
		if err := n.rotateLocked(); err != nil {
			return err
		}
	}
	if st, err := os.Stat(n.path + ".1"); err == nil && n.now().Sub(st.ModTime()) > n.ttl {
		return os.Remove(n.path + ".1")
	}
	return nil
}

func (n *NDJSON) rotateLocked() error {
	n.w.Flush()
	n.f.Close()
	_ = os.Remove(n.path + ".1")
	if err := os.Rename(n.path, n.path+".1"); err != nil {
		// The file was closed above; without a reopen the log wedges forever.
		// A failed rotation must degrade to an oversized log, not to silence.
		if f, oErr := os.OpenFile(n.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); oErr == nil {
			n.f, n.w = f, bufio.NewWriter(f)
		}
		return err
	}
	f, err := os.OpenFile(n.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		// Rename already happened: leave state so the NEXT Append retries this
		// exact step (creating the file fresh is the recovery).
		n.f, n.w, n.size = nil, nil, 0
		n.genStart = time.Time{}
		return err
	}
	n.f, n.w, n.size = f, bufio.NewWriter(f), 0
	n.genStart = time.Time{} // fresh generation: clock restarts on first append
	return nil
}

// ReadLast returns up to limit most-recent records (current generation only)
// decoded into out slices by the caller via json.RawMessage.
func (n *NDJSON) ReadLast(limit int) ([]json.RawMessage, error) {
	n.mu.Lock()
	n.w.Flush()
	n.mu.Unlock()
	raw, err := os.ReadFile(n.path)
	if err != nil {
		return nil, err
	}
	var out []json.RawMessage
	start := 0
	for i, b := range raw {
		if b == '\n' {
			if i > start {
				out = append(out, json.RawMessage(raw[start:i]))
			}
			start = i + 1
		}
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

func (n *NDJSON) Close() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.w.Flush()
	return n.f.Close()
}

// WriteFileAtomic writes raw to path via temp+rename (0600, parent dirs made).
func WriteFileAtomic(path string, raw []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
