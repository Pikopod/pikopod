package sandbox

import (
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pikopod/pikopod/internal/contract"
	"github.com/pikopod/pikopod/internal/errfmt"
	"github.com/pikopod/pikopod/internal/ir"
	"github.com/pikopod/pikopod/internal/replay"
	"github.com/pikopod/pikopod/internal/sanitize"
)

const SandboxBaseEpochMs int64 = 1735689600000

type Config struct {
	ID              string
	Seed            string
	Mode            string
	VirtualClockMs  int64
	MaxRequestBytes int64
	Credential      string
	WebhookURL      string

	WebhookSigningKey []byte
	Quota             QuotaLimits
	WallclockFaults   bool
	Effective         *contract.Effective
	Recordings        *replay.Set
}

type Engine struct {
	def             *ir.ApiDefinition
	store           *Store
	id              string
	seed            string
	deterministic   bool
	virtualClockMs  int64
	maxRequestBytes int64
	quota           QuotaLimits

	namedSchemas map[string]*ir.IrSchemaNode
	authSchemes  map[string]*ir.AuthScheme

	credential       string
	credentialHashes map[string]bool

	idemMu sync.Mutex
	idem   map[string]idemRecord

	faultMu         sync.Mutex
	faults          []FaultRule
	wallclockFaults bool
	effective       *contract.Effective
	journal         journal
	mountPrefix     string
	trace           func(stage, message string)
	recordings      *replay.Set

	webhookMu     sync.Mutex
	webhookSeq    int64
	webhookLog    []WebhookDelivery
	webhookSecret string
	envelope      *envelopeRenderer
	journalTok    *sanitize.Tokenizer
	webhookURL    string
	sinkCh        chan WebhookDelivery
	sinkClosed    bool
	sinkDone      chan struct{}
	sinkDelivered int64
	sinkFailed    int64
	sinkDropped   int64
	sinkLastErr   atomic.Value
}

func NewEngine(def *ir.ApiDefinition, cfg Config, store *Store) (*Engine, error) {
	if def == nil {
		return nil, errfmt.New("sandbox engine", "no API definition was provided", "import a spec first and pass its IR", "")
	}
	if store == nil {
		return nil, errfmt.New("sandbox engine", "no resource store was provided", "open a store with OpenStore or OpenMemoryStore", "")
	}
	if cfg.ID == "" {
		return nil, errfmt.New("sandbox engine", "the sandbox id is empty", "set Config.ID", "")
	}
	if cfg.Mode != "" && cfg.Mode != "deterministic" && cfg.Mode != "nondeterministic" {
		return nil, errfmt.Newf("sandbox engine", "use \"deterministic\" or \"nondeterministic\"", "", "unknown mode %q", cfg.Mode)
	}
	if err := store.EnsureSandbox(cfg.ID); err != nil {
		return nil, err
	}

	e := &Engine{
		def:             def,
		store:           store,
		id:              cfg.ID,
		seed:            cfg.Seed,
		deterministic:   cfg.Mode != "nondeterministic",
		virtualClockMs:  cfg.VirtualClockMs,
		maxRequestBytes: cfg.MaxRequestBytes,
		wallclockFaults: cfg.WallclockFaults,
		effective:       cfg.Effective,
		recordings:      cfg.Recordings,
		quota:           cfg.Quota,
		namedSchemas:    map[string]*ir.IrSchemaNode{},
		authSchemes:     map[string]*ir.AuthScheme{},
		idem:            map[string]idemRecord{},
	}
	if e.virtualClockMs == 0 {
		e.virtualClockMs = SandboxBaseEpochMs
	}
	if e.maxRequestBytes == 0 {
		e.maxRequestBytes = 1024 * 1024
	}
	if e.quota.MaxResources == 0 {
		e.quota.MaxResources = defaultMaxResources
	}
	if e.quota.MaxStorageBytes == 0 {
		e.quota.MaxStorageBytes = defaultMaxStorageBytes
	}
	if e.quota.MaxResourceBytes == 0 {
		e.quota.MaxResourceBytes = defaultMaxResourceBytes
	}
	for i := range def.Schemas {
		e.namedSchemas[def.Schemas[i].ID] = &def.Schemas[i].Schema
	}
	for i := range def.AuthSchemes {
		e.authSchemes[def.AuthSchemes[i].ID] = &def.AuthSchemes[i]
	}

	e.credential = cfg.Credential
	if e.credential == "" {
		e.credential = deriveCredential(cfg.Seed)
	}
	e.credentialHashes = map[string]bool{hashToken(e.credential): true}

	e.webhookSecret = webhookSecretFor(cfg.Seed)
	if env := def.WebhookEnvelope; env != nil {
		if err := env.Validate(); err != nil {
			return nil, errfmt.New("sandbox engine", err.Error(), "fix x-pikopod-webhook-envelope in the spec and re-import", "scenarios/README.md#webhook-envelope")
		}
		if env.Signature != nil && cfg.WebhookURL != "" && len(cfg.WebhookSigningKey) == 0 {
			return nil, errfmt.New("webhook signing key missing", "the spec signs deliveries with the key in $"+env.Signature.KeyEnv+", which is unset", "export "+env.Signature.KeyEnv+"=<the key the provider issued> and start again", "scenarios/README.md#webhook-envelope")
		}
		e.envelope = &envelopeRenderer{spec: env, key: cfg.WebhookSigningKey, seed: cfg.Seed}
	}
	e.journalTok = sanitize.NewTokenizer(cfg.Seed, "sandbox-journal", 1)
	if cfg.WebhookURL != "" {
		e.webhookURL = cfg.WebhookURL
		e.sinkCh = make(chan WebhookDelivery, webhookSinkQueue)
		e.sinkDone = make(chan struct{})
		go e.sinkLoop()
	}
	return e, nil
}

func (e *Engine) Credential() string { return e.credential }

func (e *Engine) VirtualClockMs() int64 { return e.virtualClockMs }

type ingressRequest struct {
	method      string
	headers     map[string]string
	query       map[string][]string
	bodyPresent bool
	bodyInvalid bool
	bodyValue   any

	route *matchResult
}

func (r *ingressRequest) header(name string) *string {
	if v, ok := r.headers[strings.ToLower(name)]; ok {
		return &v
	}
	return nil
}

func (r *ingressRequest) queryGet(name string) *string {
	if vs, ok := r.query[name]; ok && len(vs) > 0 {
		return &vs[0]
	}
	return nil
}

func normalizeInnerPath(path string) string {
	noQuery := path
	if i := strings.Index(noQuery, "?"); i != -1 {
		noQuery = noQuery[:i]
	}
	if noQuery == "" || noQuery == "/" {
		return "/"
	}
	if !strings.HasPrefix(noQuery, "/") {
		noQuery = "/" + noQuery
	}
	if len(noQuery) > 1 && strings.HasSuffix(noQuery, "/") {
		noQuery = noQuery[:len(noQuery)-1]
	}
	return noQuery
}

func (e *Engine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if recover() != nil {

			writeRaw(w, buildErrorResponse(500, "Internal Server Error", nil))
		}
	}()

	method := strings.ToUpper(r.Method)
	innerPath := normalizeInnerPath(r.URL.EscapedPath())

	headers := map[string]string{}
	for k, vs := range r.Header {
		headers[strings.ToLower(k)] = strings.Join(vs, ", ")
	}

	req := &ingressRequest{method: method, headers: headers, query: r.URL.Query()}

	wantsBody := method == "POST" || method == "PUT" || method == "PATCH"
	rawBodyLen := 0
	if wantsBody {
		raw, err := io.ReadAll(io.LimitReader(r.Body, e.maxRequestBytes+1))
		if err != nil {
			writeRaw(w, buildErrorResponse(500, "Internal Server Error", nil))
			return
		}
		if int64(len(raw)) > e.maxRequestBytes {
			writeRaw(w, buildErrorResponse(413, "Payload Too Large", nil))
			return
		}
		rawBodyLen = len(raw)
		parseRequestBody(req, headers["content-type"], raw)
	}

	started := time.Now()
	resp, wf, err := e.handleWire(req, innerPath)
	if err != nil {
		resp = buildErrorResponse(500, "Internal Server Error", nil)
		wf = nil
	}

	for _, h := range []string{"x-request-id", "x-correlation-id", "idempotency-key"} {
		if v, sent := headers[h]; sent && resp.Headers[h] == "" {
			setHeader(resp, h, v)
		}
	}

	entry := JournalEntry{Method: method, Path: innerPath, Status: resp.Status}
	if req.route != nil && req.route.kind == matchFound {
		entry.Template = req.route.endpoint.PathTemplate.Value
	}
	if req.bodyValue != nil {
		if rawBodyLen <= maxJournalBodyBytes {
			entry.Body = plainJSON(req.bodyValue)
		} else {
			entry.BodyTruncated = true
		}
	}
	entry.AtMs = e.virtualClockMs
	entry.Headers, entry.HeadersTruncated = e.journalHeaders(req.headers)
	entry.Query, entry.QueryTruncated = e.journalQuery(req.query)
	e.journal.record(entry)
	if wf == nil {
		writeRaw(w, resp)
		return
	}
	ctx := r.Context()
	if wf.sleepMs > 0 {
		remaining := wf.sleepMs - time.Since(started).Milliseconds()
		if remaining > 0 && !sleepCtx(ctx, remaining) {
			return
		}
	}
	if wf.resetConn {

		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				if tcp, ok := conn.(*net.TCPConn); ok {
					tcp.SetLinger(0)
				}
				conn.Close()
				return
			}
		}
		return
	}
	if wf.malformed {

		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n\r\nlskdu018973t09sylgasjkfg1][]'./.sdlv"))
				conn.Close()
				return
			}
		}
		return
	}
	if wf.hangMs > 0 {

		sleepCtx(ctx, wf.hangMs)
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				conn.Close()
				return
			}
		}
		return
	}
	if wf.slowBodyMs > 0 {
		writeRawSlow(w, resp, wf.slowBodyMs)
		return
	}
	if wf.wrongLength {

		for k, v := range resp.Headers {
			w.Header().Set(k, v)
		}
		w.Header().Set("content-length", strconv.Itoa(len(resp.Body)+7))
		w.WriteHeader(resp.Status)
		w.Write(resp.Body)
		return
	}
	writeRaw(w, resp)
}

func sleepCtx(ctx context.Context, ms int64) bool {
	timer := time.NewTimer(time.Duration(ms) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func writeRawSlow(w http.ResponseWriter, resp *RawResponse, totalMs int64) {
	for k, v := range resp.Headers {
		w.Header().Set(k, v)
	}
	w.WriteHeader(resp.Status)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}
	body := resp.Body
	if len(body) == 0 {
		time.Sleep(time.Duration(totalMs) * time.Millisecond)
		return
	}
	const chunks = 8
	step := (len(body) + chunks - 1) / chunks
	pause := time.Duration(totalMs/chunks) * time.Millisecond
	for start := 0; start < len(body); start += step {
		end := start + step
		if end > len(body) {
			end = len(body)
		}
		if _, err := w.Write(body[start:end]); err != nil {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(pause)
	}
}

func parseRequestBody(req *ingressRequest, contentType string, raw []byte) {
	if len(raw) == 0 {
		return
	}
	req.bodyPresent = true
	ct := strings.ToLower(contentType)
	text := string(raw)
	if strings.Contains(ct, "json") || ct == "" {
		value, err := parseJSONValue(text)
		if err != nil {
			req.bodyInvalid = true
			return
		}
		req.bodyValue = value
		return
	}
	req.bodyValue = text
}

func (e *Engine) serve(req *ingressRequest, innerPath string) (*RawResponse, error) {
	result := req.route
	if result == nil {
		result = matchRoute(e.def.Endpoints, req.method, innerPath)
	}
	if result.kind == matchNotFound {
		closest := closestOperations(e.def.Endpoints, req.method, innerPath)
		e.tracef("route", "no declared route matches %s %s (closest: %s)", req.method, innerPath, strings.Join(closest, "; "))
		headers := map[string]string{}
		if len(closest) > 0 {
			headers[ClosestHeader] = strings.Join(closest, ", ")
		}
		return buildErrorResponse(404, "Not Found", headers), nil
	}
	if result.kind == matchMethodNotAllowed {
		e.tracef("route", "path is declared but %s is not (allow: %s)", req.method, strings.Join(result.allow, ", "))
		return buildErrorResponse(405, "Method Not Allowed", map[string]string{"allow": strings.Join(result.allow, ", ")}), nil
	}
	e.tracef("route", "matched %s", operationLabel(result.endpoint))

	if authFail := e.enforceAuth(result.endpoint, req); authFail != nil {
		e.tracef("auth", "declared auth REFUSED the request (%d) — the issued credential is in `pikopod sandbox list`", authFail.Status)
		return authFail, nil
	}
	e.tracef("auth", "declared auth satisfied")

	if forced := e.forcedResponse(result.endpoint, req); forced != nil {
		e.tracef("forced", "%s forced status %d (peek: no state touch, no webhook)", ForcedStatusHeader, forced.Status)
		return forced, nil
	}

	op := deriveOperation(result.endpoint, result.pathParams)
	e.tracef("operation", "kind=%v resource=%s", op.kind, op.typ)

	if op.kind == opPassthrough {
		return e.buildSuccessResponse(result.endpoint), nil
	}

	isResource := deriveIsResource(result.endpoint)

	return e.execute(&storeCtx{
		endpoint:   result.endpoint,
		op:         op,
		req:        req,
		innerPath:  innerPath,
		isResource: isResource,
	})
}

var successCode = threeDigits

func deriveIsResource(endpoint *ir.Endpoint) bool {
	var reqRef *string
	if endpoint.RequestBody != nil && len(endpoint.RequestBody.Content) > 0 {
		reqRef = endpoint.RequestBody.Content[0].Schema.Ref
	}
	var respRefs []string
	for _, r := range endpoint.Responses {
		if !successCode.MatchString(r.StatusCode) {
			continue
		}
		if r.StatusCode[0] != '2' {
			continue
		}
		for _, c := range r.Content {
			if c.Schema.Ref != nil {
				respRefs = append(respRefs, *c.Schema.Ref)
			}
		}
	}
	if reqRef == nil || len(respRefs) == 0 {
		return true
	}
	for _, ref := range respRefs {
		if ref == *reqRef {
			return true
		}
	}
	return false
}

func writeRaw(w http.ResponseWriter, resp *RawResponse) {
	for k, v := range resp.Headers {
		w.Header().Set(k, v)
	}
	w.WriteHeader(resp.Status)
	if resp.Body != nil {
		w.Write(resp.Body)
	}
}
