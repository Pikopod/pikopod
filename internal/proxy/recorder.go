package proxy

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pikopod/pikopod/internal/sanitize"
	"github.com/pikopod/pikopod/internal/store"
)

type Record struct {
	TS         time.Time      `json:"ts"`
	Upstream   string         `json:"upstream"`
	Method     string         `json:"method"`
	Path       string         `json:"path"`
	Status     int            `json:"status"`
	DurMS      int64          `json:"dur_ms"`
	ReqHeader  map[string]any `json:"req_header,omitempty"`
	RespHeader map[string]any `json:"resp_header,omitempty"`
	ReqBody    any            `json:"req_body,omitempty"`
	RespBody   any            `json:"resp_body,omitempty"`

	ReqKind    string `json:"req_kind"`
	RespKind   string `json:"resp_kind"`
	ReqSize    int    `json:"req_size"`
	RespSize   int    `json:"resp_size"`
	Truncated  bool   `json:"truncated,omitempty"`
	Redactions int    `json:"redactions"`

	Redacted []SectionRedaction `json:"redacted,omitempty"`
}

type SectionRedaction struct {
	Section string `json:"s"`
	Pointer string `json:"p"`
	Mode    string `json:"m"`
}

const maxRedactionDetail = 256

type Recorder struct {
	tok     *sanitize.Tokenizer
	dataDir string
	m       *Metrics

	filesMu sync.Mutex
	files   map[string]*store.NDJSON
	maxFile int64
	ttl     time.Duration

	sampleRate float64

	acc float64

	observer func(*Record) bool

	specRules map[string][]sanitize.Rule
	done      chan struct{}
}

func (rec *Recorder) Done() <-chan struct{} { return rec.done }

func NewRecorder(dataDir string, tok *sanitize.Tokenizer, m *Metrics) *Recorder {
	return &Recorder{tok: tok, dataDir: dataDir, m: m, files: map[string]*store.NDJSON{}, maxFile: 64 << 20, sampleRate: 1, done: make(chan struct{})}
}

func (rec *Recorder) SetObserver(fn func(*Record) bool) { rec.observer = fn }

func (rec *Recorder) SetSampling(rate float64) {
	if rate < 0 {
		rate = 0
	}
	if rate > 1 {
		rate = 1
	}
	rec.sampleRate = rate
}

func (rec *Recorder) SetRetention(ttl time.Duration) { rec.ttl = ttl }

func (rec *Recorder) SetSpecRules(rules map[string][]sanitize.Rule) { rec.specRules = rules }

func (rec *Recorder) Sweep() {
	rec.filesMu.Lock()
	open := make([]*store.NDJSON, 0, len(rec.files))
	for _, f := range rec.files {
		open = append(open, f)
	}
	rec.filesMu.Unlock()
	for _, f := range open {
		f.Sweep()
	}
}

func (rec *Recorder) Run(captures <-chan *Exchange) {
	defer close(rec.done)
	for ex := range captures {
		func() {
			defer ex.Release()

			defer func() {
				if p := recover(); p != nil {
					rec.m.ObserverPanics.Add(1)
					rec.m.RecordingErrors.Add(1)
				}
			}()
			record, err := rec.sanitizeExchange(ex)
			if err != nil {
				rec.m.RecordingErrors.Add(1)
				return
			}

			notable := false
			if rec.observer != nil {
				notable = rec.observer(record)
			}
			keep := notable || record.Status >= 400 || rec.observer == nil || rec.takeSample()
			if !keep {
				rec.m.RecordingsSampledOut.Add(1)
				return
			}
			if err := rec.persist(record); err != nil {
				rec.m.RecordingErrors.Add(1)
				return
			}
			rec.m.RecordingsWritten.Add(1)
		}()
	}
	rec.filesMu.Lock()
	for _, f := range rec.files {
		f.Close()
	}
	rec.filesMu.Unlock()
}

func (rec *Recorder) takeSample() bool {
	if rec.sampleRate >= 1 {
		return true
	}
	rec.acc += rec.sampleRate
	if rec.acc >= 1 {
		rec.acc--
		return true
	}
	return false
}

func (rec *Recorder) persist(record *Record) error {
	rec.filesMu.Lock()
	f, ok := rec.files[record.Upstream]
	if !ok {
		var err error
		f, err = store.OpenNDJSON(filepath.Join(rec.dataDir, "recordings", record.Upstream+".ndjson"), rec.maxFile)
		if err != nil {
			rec.filesMu.Unlock()
			return err
		}
		if rec.ttl > 0 {
			f.SetTTL(rec.ttl)
		}
		rec.files[record.Upstream] = f
	}
	rec.filesMu.Unlock()
	return f.Append(record)
}

func (rec *Recorder) sanitizeExchange(ex *Exchange) (*Record, error) {
	redactions := 0
	var detail []SectionRedaction
	collect := func(section string, rs []sanitize.Redaction) {
		redactions += len(rs)
		for _, r := range rs {
			if len(detail) >= maxRedactionDetail {
				return
			}
			detail = append(detail, SectionRedaction{Section: section, Pointer: r.Pointer, Mode: string(r.Mode)})
		}
	}
	sanitizeHeaders := func(section string, h http.Header) map[string]any {
		flat := make(map[string]any, len(h))
		for k, v := range h {
			flat[strings.ToLower(k)] = strings.Join(v, ", ")
		}
		res := sanitize.Sanitize(flat, rec.tok, nil, true)
		collect(section, res.Redactions)
		out, _ := res.Sanitized.(map[string]any)
		return out
	}

	reqBody, reqKind := rec.sanitizeBody("req_body", ex.ReqBody, ex.ReqHeader, nil, collect)
	respBody, respKind := rec.sanitizeBody("resp_body", ex.RespBody, ex.RespHeader, rec.specRules[ex.Upstream], collect)

	safePath := rec.sanitizePath(ex.Path, &redactions)

	record := &Record{
		TS: ex.Start, Upstream: ex.Upstream, Method: ex.Method, Path: safePath,
		Status: ex.Status, DurMS: ex.Duration.Milliseconds(),
		ReqHeader: sanitizeHeaders("req_header", ex.ReqHeader), RespHeader: sanitizeHeaders("resp_header", ex.RespHeader),
		ReqBody: reqBody, RespBody: respBody,
		ReqKind: reqKind, RespKind: respKind,
		ReqSize: len(ex.ReqBody), RespSize: len(ex.RespBody),
		Truncated: ex.Truncated, Redactions: redactions, Redacted: detail,
	}
	return record, nil
}

func (rec *Recorder) sanitizeBody(section string, raw []byte, h http.Header, rules []sanitize.Rule, collect func(string, []sanitize.Redaction)) (any, string) {
	if len(raw) == 0 {
		return nil, "none"
	}
	if strings.EqualFold(h.Get("Content-Encoding"), "gzip") {
		if zr, err := gzip.NewReader(bytes.NewReader(raw)); err == nil {
			if dec, err := io.ReadAll(io.LimitReader(zr, maxCapturedBody)); err == nil {
				raw = dec
			}
			zr.Close()
		}
	}
	ct, _, _ := mime.ParseMediaType(h.Get("Content-Type"))
	switch {
	case strings.Contains(ct, "json") || looksLikeJSON(raw):
		var v any
		dec := json.NewDecoder(bytes.NewReader(raw))

		dec.UseNumber()
		if err := dec.Decode(&v); err == nil {
			res := sanitize.Sanitize(v, rec.tok, rules, false)
			collect(section, res.Redactions)
			return res.Sanitized, "json"
		}
		return nil, "binary"
	case ct == "application/x-www-form-urlencoded":

		vals := map[string]any{}
		for _, pair := range strings.Split(string(raw), "&") {
			k, v, _ := strings.Cut(pair, "=")
			if k != "" {
				vals[urlUnescape(k)] = urlUnescape(v)
			}
		}
		res := sanitize.Sanitize(vals, rec.tok, rules, false)
		collect(section, res.Redactions)
		return res.Sanitized, "form"
	default:
		return nil, "binary"
	}
}

func (rec *Recorder) sanitizePath(p string, redactions *int) string {
	path, query, hasQuery := strings.Cut(p, "?")
	segs := strings.Split(path, "/")
	for i, seg := range segs {
		if seg == "" {
			continue
		}

		useg := urlUnescape(seg)
		if mode := sanitize.Classify("", useg, false); mode != sanitize.ModeAllow {
			token, _ := rec.tok.Tokenize(useg)
			segs[i] = token
			*redactions++
		}
	}
	out := strings.Join(segs, "/")
	if hasQuery {

		pairs := strings.Split(query, "&")
		for i, pair := range pairs {
			k, v, _ := strings.Cut(pair, "=")

			uk := urlUnescape(k)
			if sanitize.KeyLooksLikeIdentifier(uk) {
				token, _ := rec.tok.Tokenize(uk)
				k = token
				pairs[i] = k
				if v != "" {
					pairs[i] = k + "=" + v
				}
				*redactions++
			}
			if v == "" {
				continue
			}
			uv := urlUnescape(v)
			if mode := sanitize.Classify(urlUnescape(k), uv, false); mode != sanitize.ModeAllow {
				token, _ := rec.tok.Tokenize(uv)
				pairs[i] = k + "=" + token
				*redactions++
			}
		}
		out += "?" + strings.Join(pairs, "&")
	}
	return out
}

func looksLikeJSON(b []byte) bool {
	for _, c := range b {
		switch c {
		case ' ', '\t', '\n', '\r':
			continue
		case '{', '[':
			return true
		default:
			return false
		}
	}
	return false
}

func urlUnescape(s string) string {
	s = strings.ReplaceAll(s, "+", " ")
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if hi, lo := unhex(s[i+1]), unhex(s[i+2]); hi >= 0 && lo >= 0 {
				b.WriteByte(byte(hi<<4 | lo))
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func unhex(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}
