// Package proxy is pikopod's fail-open data plane: no internal error may alter,
// delay or drop proxied traffic, and it never retries (double-charge window).
package proxy

import (
	"bytes"
	"crypto/subtle"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/pikopod/pikopod/internal/config"
	"github.com/pikopod/pikopod/internal/errfmt"
)

// tokenMatches compares in constant time — a non-loopback listener must not
// leak the token byte-by-byte through response timing.
func tokenMatches(got, want string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// Exchange is one observed request/response pair, captured raw; the recorder
// sanitizes before anything touches disk (redact-at-write).
type Exchange struct {
	Upstream   string
	Method     string
	Path       string // raw path incl. query, upstream-relative
	Status     int
	ReqHeader  http.Header
	RespHeader http.Header
	ReqBody    []byte
	RespBody   []byte
	Truncated  bool
	Start      time.Time
	Duration   time.Duration
	// release returns this exchange's bytes to the capture budget; the
	// recorder calls it once processing ends (nil-safe).
	release func()
}

// Release returns the exchange's bytes to the capture budget (recorder-side).
func (ex *Exchange) Release() {
	if ex.release != nil {
		ex.release()
		ex.release = nil
	}
}

// Metrics are exposed on /healthz (JSON) — the agent proves liveness and
// value in one curl.
type Metrics struct {
	RequestsProxied   atomic.Int64
	UpstreamErrors    atomic.Int64
	ObserverPanics    atomic.Int64
	CapturesDropped   atomic.Int64
	CapturesQueued    atomic.Int64
	RecordingsWritten atomic.Int64
	RecordingErrors   atomic.Int64
	// RecordingsSampledOut counts routine records learned from but not
	// persisted, per the sampling rate.
	RecordingsSampledOut atomic.Int64
}

const maxCapturedBody = 1 << 20 // 1 MiB per body captured for observation; pass-through is unlimited

type upstreamProxy struct {
	name   string
	prefix string
	rp     *httputil.ReverseProxy
	target *url.URL
}

// maxCaptureBytes bounds TOTAL queued body bytes: the count-bounded channel
// alone let a stalled recorder OOM-kill pass-through. Past it, captures drop.
const maxCaptureBytes = 256 << 20

// Server routes /<upstream>/... to its target, capturing exchanges.
type Server struct {
	Metrics     *Metrics
	captures    chan *Exchange
	queuedBytes atomic.Int64
	proxies     []*upstreamProxy
	healthz     http.Handler
	ack         http.Handler
	accept      http.Handler
	token       string
}

// New builds the agent handler. If nobody consumes captures, the channel
// fills and drops — the proxy is indifferent by design.
func New(cfg *config.Config, m *Metrics, captureDepth int) (*Server, error) {
	if captureDepth <= 0 {
		captureDepth = 1024
	}
	s := &Server{Metrics: m, captures: make(chan *Exchange, captureDepth), token: cfg.Token()}
	for _, name := range cfg.UpstreamNames() {
		u := cfg.Upstreams[name]
		target, err := url.Parse(u.Target)
		if err != nil || target.Scheme == "" || target.Host == "" {
			return nil, errfmt.New("invalid upstream target", "upstreams."+name+".target is not an absolute URL", "use a full base URL like https://api.examplepay.com", "docs/config-reference.md#upstreams")
		}
		up := &upstreamProxy{name: name, prefix: strings.TrimSuffix(u.Listen, "/"), target: target}
		rp := &httputil.ReverseProxy{
			// Flush immediately so streaming/SSE passes through unbuffered.
			FlushInterval: -1,
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(target)
				pr.Out.URL.Path = joinPath(target.Path, strings.TrimPrefix(pr.In.URL.Path, up.prefix))
				pr.Out.URL.RawQuery = pr.In.URL.RawQuery
				pr.Out.Host = target.Host
			},
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				// Honest 502 with a marker; NEVER an automatic retry.
				m.UpstreamErrors.Add(1)
				w.Header().Set("X-Pikopod-Error", "upstream-unreachable")
				http.Error(w, "pikopod: upstream unreachable: "+err.Error(), http.StatusBadGateway)
			},
		}
		up.rp = rp
		s.proxies = append(s.proxies, up)
	}
	return s, nil
}

// Captures exposes the observation stream to the recorder.
func (s *Server) Captures() <-chan *Exchange { return s.captures }

// SetHealthz mounts the /healthz handler (wired by the agent runner).
func (s *Server) SetHealthz(h http.Handler) { s.healthz = h }

// SetAck mounts /ack. Mutating, so it sits BEHIND the token gate.
func (s *Server) SetAck(h http.Handler) { s.ack = h }

// SetAccept mounts /accept (refreeze + ack). Token-gated like /ack.
func (s *Server) SetAccept(h http.Handler) { s.accept = h }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	authed := s.token == "" || tokenMatches(r.Header.Get("X-Pikopod-Token"), s.token)
	// Stripped unconditionally: the token must never travel past this hop,
	// in EVERY configuration (including default tokenless loopback).
	r.Header.Del("X-Pikopod-Token")
	// DNS-rebinding/CSRF gate for tokenless setups: rebinding defeats
	// same-origin, but a foreign Host header still gives it away.
	if s.token == "" && !loopbackHost(r.Host) {
		http.Error(w, "pikopod: refusing non-local Host header on a tokenless listener (DNS-rebinding guard) — set PIKOPOD_TOKEN to serve other hostnames", http.StatusForbidden)
		return
	}
	if r.URL.Path == "/healthz" {
		// Liveness is public, but the inventory it carries is what the token
		// rule protects — so the full payload needs the token when one is set.
		if s.healthz != nil && authed {
			s.healthz.ServeHTTP(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
		return
	}
	if s.token != "" && !authed {
		http.Error(w, "pikopod: missing or wrong X-Pikopod-Token", http.StatusUnauthorized)
		return
	}
	if r.URL.Path == "/ack" && s.ack != nil {
		s.ack.ServeHTTP(w, r)
		return
	}
	if r.URL.Path == "/accept" && s.accept != nil {
		s.accept.ServeHTTP(w, r)
		return
	}
	for _, up := range s.proxies {
		if r.URL.Path == up.prefix || strings.HasPrefix(r.URL.Path, up.prefix+"/") {
			s.forward(up, w, r)
			return
		}
	}
	http.Error(w, "pikopod: no upstream matches "+r.URL.Path+" — check upstreams in pikopod.yaml", http.StatusNotFound)
}

func (s *Server) forward(up *upstreamProxy, w http.ResponseWriter, r *http.Request) {
	s.Metrics.RequestsProxied.Add(1)
	start := time.Now()

	// Tee the request body up to the capture cap; pass-through is unaffected
	// (the proxy reads from the TeeReader, tail beyond the cap flows through).
	var reqBuf cappedBuffer
	if r.Body != nil {
		r.Body = &teeReadCloser{rc: r.Body, w: &reqBuf}
	}

	rec := &captureWriter{ResponseWriter: w}
	up.rp.ServeHTTP(rec, r)

	// Runs after the response is fully written and is panic-isolated: a
	// capture bug must never surface (CRITICAL fail-open contract).
	func() {
		defer func() {
			if p := recover(); p != nil {
				s.Metrics.ObserverPanics.Add(1)
			}
		}()
		ex := &Exchange{
			Upstream:   up.name,
			Method:     r.Method,
			Path:       strings.TrimPrefix(r.URL.Path, up.prefix) + querysuffix(r.URL),
			Status:     rec.status,
			ReqHeader:  r.Header.Clone(),
			RespHeader: rec.Header().Clone(),
			ReqBody:    reqBuf.Bytes(),
			RespBody:   rec.body.Bytes(),
			Truncated:  reqBuf.truncated || rec.body.truncated,
			Start:      start,
			Duration:   time.Since(start),
		}
		// Byte budget check BEFORE enqueue: with a stalled recorder the
		// count-bounded channel could hold gigabytes of bodies.
		size := int64(len(ex.ReqBody) + len(ex.RespBody))
		if s.queuedBytes.Load()+size > maxCaptureBytes {
			s.Metrics.CapturesDropped.Add(1)
			return
		}
		s.queuedBytes.Add(size)
		ex.release = func() { s.queuedBytes.Add(-size) }
		select {
		case s.captures <- ex:
			s.Metrics.CapturesQueued.Add(1)
		default:
			// Channel full: drop-oldest. Only records actually lost are
			// counted, and every drop RELEASES its bytes back to the budget.
			select {
			case evicted := <-s.captures:
				evicted.Release()
				s.Metrics.CapturesDropped.Add(1)
			default:
			}
			select {
			case s.captures <- ex:
				s.Metrics.CapturesQueued.Add(1)
			default:
				ex.Release()
				s.Metrics.CapturesDropped.Add(1)
			}
		}
	}()
}

// cappedBuffer keeps at most maxCapturedBody bytes and flags truncation.
type cappedBuffer struct {
	bytes.Buffer
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	room := maxCapturedBody - b.Len()
	if room <= 0 {
		b.truncated = true
		return len(p), nil
	}
	if len(p) > room {
		b.truncated = true
		b.Buffer.Write(p[:room])
		return len(p), nil
	}
	return b.Buffer.Write(p)
}

type teeReadCloser struct {
	rc io.ReadCloser
	w  io.Writer
}

func (t *teeReadCloser) Read(p []byte) (int, error) {
	n, err := t.rc.Read(p)
	if n > 0 {
		t.w.Write(p[:n]) // cappedBuffer never errors
	}
	return n, err
}
func (t *teeReadCloser) Close() error { return t.rc.Close() }

// captureWriter records status + a capped copy of the body while writing
// through to the client unmodified.
type captureWriter struct {
	http.ResponseWriter
	status int
	body   cappedBuffer
}

func (c *captureWriter) WriteHeader(status int) {
	c.status = status
	c.ResponseWriter.WriteHeader(status)
}

func (c *captureWriter) Write(p []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	c.body.Write(p)
	return c.ResponseWriter.Write(p)
}

// Flush keeps streaming pass-through working (FlushInterval: -1).
func (c *captureWriter) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap keeps ReverseProxy's protocol-upgrade path (Hijack) working through
// the wrapper; without it upgrades 502, violating the fail-open contract.
func (c *captureWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

// loopbackHost reports whether a Host header addresses this machine's loopback
// surface — the only legitimate way to reach a tokenless listener.
func loopbackHost(host string) bool {
	if host == "" {
		return true // HTTP/1.0 clients may omit it; nothing to rebind with
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func joinPath(base, rest string) string {
	base = strings.TrimSuffix(base, "/")
	if rest == "" {
		rest = "/"
	} else if !strings.HasPrefix(rest, "/") {
		rest = "/" + rest
	}
	return base + rest
}

func querysuffix(u *url.URL) string {
	if u.RawQuery == "" {
		return ""
	}
	return "?" + u.RawQuery
}
