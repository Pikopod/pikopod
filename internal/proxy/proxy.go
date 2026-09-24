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
	"sync"
	"sync/atomic"
	"time"

	"github.com/pikopod/pikopod/internal/config"
	"github.com/pikopod/pikopod/internal/errfmt"
)

func tokenMatches(got, want string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

type Exchange struct {
	Upstream   string
	Method     string
	Path       string
	Status     int
	ReqHeader  http.Header
	RespHeader http.Header
	ReqBody    []byte
	RespBody   []byte
	Truncated  bool
	Start      time.Time
	Duration   time.Duration

	release func()
}

func (ex *Exchange) Release() {
	if ex.release != nil {
		ex.release()
		ex.release = nil
	}
}

type Metrics struct {
	RequestsProxied    atomic.Int64
	UpstreamErrors     atomic.Int64
	UpstreamBodyErrors atomic.Int64
	ObserverPanics     atomic.Int64
	CapturesDropped    atomic.Int64
	CapturesQueued     atomic.Int64
	RecordingsWritten  atomic.Int64
	RecordingErrors    atomic.Int64

	RecordingsSampledOut atomic.Int64
}

const maxCapturedBody = 1 << 20

type upstreamProxy struct {
	name   string
	prefix string
	rp     *httputil.ReverseProxy
	target *url.URL
}

const maxCaptureBytes = 256 << 20

type Server struct {
	Metrics     *Metrics
	captures    chan *Exchange
	queuedBytes atomic.Int64
	proxies     []*upstreamProxy
	healthz     http.Handler
	ack         http.Handler
	accept      http.Handler
	token       string
	closeMu     sync.RWMutex
	closed      bool
	closeOnce   sync.Once
}

func (s *Server) Close() {
	s.closeOnce.Do(func() {
		s.closeMu.Lock()
		s.closed = true
		close(s.captures)
		s.closeMu.Unlock()
	})
}

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

			FlushInterval: -1,
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(target)
				pr.Out.URL.Path = joinPath(target.Path, strings.TrimPrefix(pr.In.URL.Path, up.prefix))
				pr.Out.URL.RawQuery = pr.In.URL.RawQuery
				pr.Out.Host = target.Host
			},

			ModifyResponse: func(res *http.Response) error {
				res.Body = &upstreamBody{rc: res.Body, m: m}
				return nil
			},
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {

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

func (s *Server) Captures() <-chan *Exchange { return s.captures }

func (s *Server) SetHealthz(h http.Handler) { s.healthz = h }

func (s *Server) SetAck(h http.Handler) { s.ack = h }

func (s *Server) SetAccept(h http.Handler) { s.accept = h }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	authed := s.token == "" || tokenMatches(r.Header.Get("X-Pikopod-Token"), s.token)

	r.Header.Del("X-Pikopod-Token")

	if s.token == "" && !loopbackHost(r.Host) {
		http.Error(w, "pikopod: refusing non-local Host header on a tokenless listener (DNS-rebinding guard) — set PIKOPOD_TOKEN to serve other hostnames", http.StatusForbidden)
		return
	}
	if r.URL.Path == "/healthz" {

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

	var reqBuf cappedBuffer
	if r.Body != nil {
		r.Body = &teeReadCloser{rc: r.Body, w: &reqBuf}
	}

	rec := &captureWriter{ResponseWriter: w}
	up.rp.ServeHTTP(rec, r)

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

		size := int64(len(ex.ReqBody) + len(ex.RespBody))
		if s.queuedBytes.Load()+size > maxCaptureBytes {
			s.Metrics.CapturesDropped.Add(1)
			return
		}
		s.queuedBytes.Add(size)
		ex.release = func() { s.queuedBytes.Add(-size) }
		s.closeMu.RLock()
		defer s.closeMu.RUnlock()
		if s.closed {
			ex.Release()
			s.Metrics.CapturesDropped.Add(1)
			return
		}
		select {
		case s.captures <- ex:
			s.Metrics.CapturesQueued.Add(1)
		default:

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

type upstreamBody struct {
	rc      io.ReadCloser
	m       *Metrics
	counted bool
}

func (u *upstreamBody) Read(p []byte) (int, error) {
	n, err := u.rc.Read(p)
	if err != nil && err != io.EOF && !u.counted {
		u.counted = true
		u.m.UpstreamBodyErrors.Add(1)
	}
	return n, err
}

func (u *upstreamBody) Close() error { return u.rc.Close() }

type teeReadCloser struct {
	rc io.ReadCloser
	w  io.Writer
}

func (t *teeReadCloser) Read(p []byte) (int, error) {
	n, err := t.rc.Read(p)
	if n > 0 {
		t.w.Write(p[:n])
	}
	return n, err
}
func (t *teeReadCloser) Close() error { return t.rc.Close() }

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

func (c *captureWriter) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (c *captureWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

func loopbackHost(host string) bool {
	if host == "" {
		return true
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
