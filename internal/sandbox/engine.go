// Package sandbox serves a simulated provider from a pinned ApiDefinition:
// route match → auth → operation → CRUD/synthesis, never leaking internals.
package sandbox

import (
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pikopod/pikopod/internal/contract"
	"github.com/pikopod/pikopod/internal/errfmt"
	"github.com/pikopod/pikopod/internal/ir"
	"github.com/pikopod/pikopod/internal/replay"
)

// SandboxBaseEpochMs is the virtual clock a sandbox starts at:
// 2025-01-01T00:00:00Z.
const SandboxBaseEpochMs int64 = 1735689600000

// Config configures one sandbox engine.
type Config struct {
	// ID namespaces rows in the shared store.
	ID string
	// Seed is the run seed every deterministic derivation starts from.
	Seed string
	// Mode is "deterministic" (default) or "nondeterministic".
	Mode string
	// VirtualClockMs is the fixed virtual clock (0 ⇒ SandboxBaseEpochMs).
	VirtualClockMs int64
	// MaxRequestBytes caps request bodies (0 ⇒ 1 MiB, the platform default).
	MaxRequestBytes int64
	// Credential overrides the issued test token (empty ⇒ derived from Seed);
	// transcript replays inject a captured token here.
	Credential string
	// WebhookURL is an optional HTTP(S) sink for signed delivery POSTs;
	// delivery is async and best-effort so a wedged sink never slows requests.
	WebhookURL string
	// Quota zero-fields fall back to the platform defaults.
	Quota QuotaLimits
	// WallclockFaults makes EVERY armed fault delay on the real wire; default
	// false keeps faults virtualized, deterministic and transcript-stable.
	WallclockFaults bool
	// Effective attaches the resolved traffic contract; nil keeps spec-only
	// behavior byte-identical.
	Effective *contract.Effective
	// Recordings is the FINAL resolution tier, consulted only when no spec
	// route and no traffic-admitted endpoint matches; nil 404s the request.
	Recordings *replay.Set
}

// Engine simulates the modelled provider for one sandbox. It implements
// http.Handler.
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

	// Standing fault rules armed by the scenario engine (faults.go), evaluated
	// before serve().
	faultMu         sync.Mutex
	faults          []FaultRule
	wallclockFaults bool
	// effective is the resolved traffic contract (contract_render.go).
	effective *contract.Effective
	// journal remembers what the client sent (journal.go).
	journal journal
	// mountPrefix prefixes self-referential URLs (diagnostics.go).
	mountPrefix string
	// trace narrates the pipeline for `pikopod why` (diagnostics.go).
	trace func(stage, message string)
	// recordings is the final resolution tier (replay_tier.go).
	recordings *replay.Set

	// Webhook outbox (webhooks.go). Lock order: webhookMu before faultMu,
	// never the reverse.
	webhookMu     sync.Mutex
	webhookSeq    int64
	webhookLog    []WebhookDelivery
	webhookSecret string
	webhookURL    string
	sinkCh        chan WebhookDelivery
	sinkDelivered int64 // atomics
	sinkFailed    int64
	sinkDropped   int64
}

// NewEngine builds an engine over a pinned ApiDefinition and a resource store.
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
	// Deterministic issued credential (derivation note in auth.go), unless the
	// caller injects one (e.g. a transcript's captured token).
	e.credential = cfg.Credential
	if e.credential == "" {
		e.credential = deriveCredential(cfg.Seed)
	}
	e.credentialHashes = map[string]bool{hashToken(e.credential): true}
	// The secret is always derived so the accessor stays truthful; the sink
	// goroutine starts only with a URL and lives for the engine's lifetime.
	e.webhookSecret = webhookSecretFor(cfg.Seed)
	if cfg.WebhookURL != "" {
		e.webhookURL = cfg.WebhookURL
		e.sinkCh = make(chan WebhookDelivery, webhookSinkQueue)
		go e.sinkLoop()
	}
	return e, nil
}

// Credential returns the sandbox's issued test token (`pikopod_sbx_test_…`).
func (e *Engine) Credential() string { return e.credential }

func (e *Engine) VirtualClockMs() int64 { return e.virtualClockMs }

// ingressRequest is the parsed customer request handed to the pipeline.
type ingressRequest struct {
	method      string
	headers     map[string]string // lower-cased names
	query       map[string][]string
	bodyPresent bool
	bodyInvalid bool
	bodyValue   any
	// route caches matchRoute for this request so serve and the journal do not
	// re-walk the route table (it was walked three times per request).
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

// normalizeInnerPath strips the query, ensures a leading slash, and drops a
// single trailing slash.
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

// ServeHTTP handles one customer request against the sandbox.
func (e *Engine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if recover() != nil {
			// Never leak internals to the customer app.
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

	// The sandbox reads its OWN request body (bounded).
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
	// Correlation echo: real providers reflect these request headers back and
	// client tracing depends on it. Only when the response has not set them.
	for _, h := range []string{"x-request-id", "x-correlation-id", "idempotency-key"} {
		if v, sent := headers[h]; sent && resp.Headers[h] == "" {
			setHeader(resp, h, v)
		}
	}

	// Journal BEFORE wallclock faults act, so a request the sandbox hung up on
	// is still on record with its would-be status.
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
	e.journal.record(entry)
	if wf == nil {
		writeRaw(w, resp)
		return
	}
	// WALLCLOCK mode: the fault acts on the real wire. Sleeps abort on client
	// disconnect so held goroutines never outlive their connections.
	ctx := r.Context()
	if wf.sleepMs > 0 {
		// The delay is the TOTAL observed latency, so sleep only the
		// remainder after real processing.
		remaining := wf.sleepMs - time.Since(started).Milliseconds()
		if remaining > 0 && !sleepCtx(ctx, remaining) {
			return // client gone mid-delay
		}
	}
	if wf.resetConn {
		// RST, not FIN: SO_LINGER 0 then close — "connection reset by peer".
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				if tcp, ok := conn.(*net.TCPConn); ok {
					tcp.SetLinger(0)
				}
				conn.Close()
				return
			}
		}
		return // non-hijackable writer (test recorder): drop with no response
	}
	if wf.malformed {
		// A valid status line, then garbage — the response that breaks HTTP
		// parsers, not error handlers.
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
		// Hold, then drop the connection with NO response — the shape that
		// exercises client timeouts hardest. Needs a hijackable writer.
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
		// Declare more bytes than are sent so the client hits an unexpected
		// EOF; net/http honors explicit Content-Length over chunked framing.
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

// sleepCtx sleeps for ms unless the request context ends first; reports
// whether the full sleep completed.
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

// writeRawSlow sends headers immediately, then dribbles the body across
// totalMs — the half-alive provider that defeats header-based health checks.
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
			return // client gave up — exactly the scenario under test
		}
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(pause)
	}
}

// parseRequestBody: empty ⇒ absent; a JSON (or unlabelled) body parses or is
// flagged invalid; other bodies are carried opaquely as text.
func parseRequestBody(req *ingressRequest, contentType string, raw []byte) {
	if len(raw) == 0 {
		return // present:false
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

// serve runs route match → auth → operation → CRUD/synthesis. Fault
// evaluation wraps it in handleWire (faults.go).
func (e *Engine) serve(req *ingressRequest, innerPath string) (*RawResponse, error) {
	result := req.route
	if result == nil {
		result = matchRoute(e.def.Endpoints, req.method, innerPath)
	}
	if result.kind == matchNotFound {
		// Near-miss diagnostics (diagnostics.go): the 404 names the closest
		// declared operations on a header — the body is unchanged.
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

	// Enforce the API's declared auth before doing anything.
	if authFail := e.enforceAuth(result.endpoint, req); authFail != nil {
		e.tracef("auth", "declared auth REFUSED the request (%d) — the issued credential is in `pikopod sandbox list`", authFail.Status)
		return authFail, nil
	}
	e.tracef("auth", "declared auth satisfied")

	// Forced responses (forced.go): after auth, before any state touch —
	// peek semantics by construction, since execute() is never reached.
	if forced := e.forcedResponse(result.endpoint, req); forced != nil {
		e.tracef("forced", "%s forced status %d (peek: no state touch, no webhook)", ForcedStatusHeader, forced.Status)
		return forced, nil
	}

	op := deriveOperation(result.endpoint, result.pathParams)
	e.tracef("operation", "kind=%v resource=%s", op.kind, op.typ)
	// Unrecognized shapes never touch state — the generic success shell stands.
	if op.kind == opPassthrough {
		return buildSuccessResponse(result.endpoint), nil
	}

	// An endpoint is an ACTION when its success response is a DIFFERENT named
	// schema than its request body; otherwise RESOURCE (read-your-write echo).
	isResource := deriveIsResource(result.endpoint)

	return e.execute(&storeCtx{
		endpoint:   result.endpoint,
		op:         op,
		req:        req,
		innerPath:  innerPath,
		isResource: isResource,
	})
}

var successCode = threeDigits // reuse ^\d{3}$

// deriveIsResource decides resource-ness from the request/response schema refs.
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
