// Executes a store operation for a matched endpoint. CRUD mechanics only — no
// inferred relationship or state-machine enforcement (behaviour overlay, deferred).
package sandbox

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"

	"github.com/pikopod/pikopod/internal/ir"
)

// QuotaLimits bounds one sandbox's resource usage.
type QuotaLimits struct {
	MaxResources     int64
	MaxStorageBytes  int64
	MaxResourceBytes int64
}

const (
	defaultMaxResources     = 10_000
	defaultMaxStorageBytes  = 100 * 1024 * 1024
	defaultMaxResourceBytes = 256 * 1024
)

const maxIdemEntries = 1024

type idemRecord struct {
	requestHash string
	status      int
	body        []byte
}

// storeCtx carries one matched request through the CRUD handlers.
type storeCtx struct {
	endpoint   *ir.Endpoint
	op         operation
	req        *ingressRequest
	innerPath  string
	isResource bool
}

func etagFor(version int64) string { return `"` + strconv.FormatInt(version, 10) + `"` }

func parseIfMatch(raw *string) *int64 {
	if raw == nil {
		return nil
	}
	s := strings.TrimSpace(strings.ReplaceAll(*raw, `"`, ""))
	f, ok := jsNumber(s)
	if !ok || f != float64(int64(f)) {
		return nil
	}
	n := int64(f)
	return &n
}

func requestSchema(endpoint *ir.Endpoint) *ir.IrSchemaNode {
	if endpoint.RequestBody == nil {
		return nil
	}
	for i := range endpoint.RequestBody.Content {
		if strings.Contains(strings.ToLower(endpoint.RequestBody.Content[i].MediaType), "json") {
			return &endpoint.RequestBody.Content[i].Schema
		}
	}
	return nil
}

var slugPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// typeSlug: last static segment of a type path `/a/b/posts` → `posts`.
func typeSlug(typ string) string {
	parts := pathSegments(typ)
	if len(parts) == 0 {
		return "res"
	}
	last := parts[len(parts)-1]
	if slugPattern.MatchString(last) {
		return last
	}
	return "res"
}

// synthCtx builds a synthesis context seeded so a DETERMINISTIC sandbox
// replays byte-for-byte; a NONDETERMINISTIC one mixes in a crypto nonce.
func (e *Engine) synthCtx(parts ...string) *synthContext {
	base := e.seed + ":" + strings.Join(parts, ":")
	if !e.deterministic {
		nonce := make([]byte, 8)
		rand.Read(nonce)
		base = base + ":" + hex.EncodeToString(nonce)
	}
	return makeContext(base, e.virtualClockMs, e.namedSchemas)
}

// endpointError synthesizes the endpoint's declared error schema, else the
// neutral `{ message }`.
func (e *Engine) endpointError(ctx *storeCtx, status int, message string) *RawResponse {
	if schema := errorSchema(ctx.endpoint, status); schema != nil {
		synth := e.synthCtx("error", strconv.Itoa(status))
		if body := synthesize(schema, synth, 0, ""); body != nil {
			return jsonResponse(status, body, nil)
		}
	}
	return buildErrorResponse(status, message, nil)
}

func (e *Engine) execute(ctx *storeCtx) (*RawResponse, error) {
	switch ctx.op.kind {
	case opRead:
		return e.doRead(ctx)
	case opList:
		return e.doList(ctx)
	case opCreate:
		return e.doCreate(ctx)
	case opReplace, opMerge:
		return e.doModify(ctx)
	case opDelete:
		return e.doRemove(ctx)
	default:
		// Should not reach here (passthrough handled upstream), but stay safe.
		return buildErrorResponse(404, "Not Found", nil), nil
	}
}

func (e *Engine) doRead(ctx *storeCtx) (*RawResponse, error) {
	r, err := e.store.GetOne(e.id, ctx.op.typ, *ctx.op.key)
	if errors.Is(err, ErrNotFound) {
		e.tracef("store", "no %s resource with key %q — create one first (or seed it)", ctx.op.typ, *ctx.op.key)
		return e.endpointError(ctx, 404, "Not Found"), nil
	}
	if err != nil {
		// Checked BEFORE the trace below: a non-NotFound store error leaves r
		// nil, and tracing r.Version would panic per-request.
		return nil, err
	}
	e.tracef("store", "read %s/%s (version %d)", ctx.op.typ, *ctx.op.key, r.Version)
	return jsonResponse(200, json.RawMessage(r.Attributes), map[string]string{"etag": etagFor(r.Version)}), nil
}

func (e *Engine) doList(ctx *storeCtx) (*RawResponse, error) {
	limit := parseLimit(ctx.req.queryGet("limit"))
	cursorKey := decodeCursor(ctx.req.queryGet("cursor"))
	page, err := e.store.List(e.id, ctx.op.typ, limit, cursorKey)
	if err != nil {
		return nil, err
	}
	cursorSeed := ""
	if cursorKey != nil {
		cursorSeed = *cursorKey
	}
	synth := e.synthCtx("list", ctx.op.typ, cursorSeed)
	items := make([]any, len(page.Items))
	for i, r := range page.Items {
		items[i] = json.RawMessage(r.Attributes)
	}
	body := shapeListBody(successSchema(ctx.endpoint, 200), items, synth)
	headers := map[string]string{}
	if page.NextCursorKey != nil {
		// Standard, provider-neutral cursor surface: a Link header, rel="next".
		q := "limit=" + strconv.Itoa(limit) + "&cursor=" + encodeCursor(*page.NextCursorKey)
		headers["link"] = "<" + e.mountPrefix + ctx.innerPath + "?" + q + `>; rel="next"`
	}
	return jsonResponse(200, body, headers), nil
}

func (e *Engine) doCreate(ctx *storeCtx) (*RawResponse, error) {
	attrs, errResp := objectBody(ctx.req)
	if errResp != nil {
		return errResp, nil
	}

	if invalid := validateBody(requestSchema(ctx.endpoint), attrs); len(invalid) > 0 {
		return e.validationErrorResponse(ctx, invalid), nil
	}

	// Idempotency replay: a repeated key returns the original response.
	key := ctx.req.header("idempotency-key")
	reqHash, err := e.hashRequest(ctx)
	if err != nil {
		return nil, err
	}
	if key != nil {
		// Replay check and post-create record are ONE critical section, else
		// concurrent same-key creates both miss the cache and both create.
		e.idemMu.Lock()
		defer e.idemMu.Unlock()
		if replay := e.idempotentReplayLocked(*key, reqHash); replay != nil {
			return replay, nil
		}
	}

	seq, err := e.store.AllocateSeq(e.id)
	if err != nil {
		return nil, err
	}
	resourceKey := typeSlug(ctx.op.typ) + "_" + strconv.FormatInt(seq, 10)
	status := pickSuccessStatus(ctx.endpoint, 201)
	// Synthesized server fields fill gaps the client left; the client's values
	// always win; canonical `id` is the synthesized key (RESOURCE mode only).
	synth := e.synthCtx("create", resourceKey)
	stored := completeResource(successSchema(ctx.endpoint, status), attrs, synth, !ctx.isResource)
	if ctx.isResource {
		stored.Set("id", resourceKey)
	}

	// Enforce quotas BEFORE inserting, so a rejected create leaves no trace.
	marshaled, err := marshalJSValue(stored)
	if err != nil {
		return nil, err
	}
	totals, err := e.store.Totals(e.id)
	if err != nil {
		return nil, err
	}
	if denied := e.quotaGuard(int64(len(marshaled)), totals.Count+1, totals.Bytes+int64(len(marshaled))); denied != nil {
		return denied, nil
	}

	created, err := e.store.Insert(e.id, ctx.op.typ, resourceKey, marshaled, nil, e.virtualClockMs)
	if err != nil {
		return nil, err
	}
	// Outbox only after the insert committed, so an error path above never
	// emits a webhook.
	e.enqueueWebhookFor(ctx.endpoint, webhookActionCreated, typeSlug(ctx.op.typ), json.RawMessage(created.Attributes))

	response := jsonResponse(status, json.RawMessage(created.Attributes), map[string]string{"etag": etagFor(created.Version)})
	if key != nil {
		e.recordIdempotencyLocked(*key, reqHash, response) // idemMu held since the replay check
	}
	return response, nil
}

func (e *Engine) doModify(ctx *storeCtx) (*RawResponse, error) {
	attrs, errResp := objectBody(ctx.req)
	if errResp != nil {
		return errResp, nil
	}

	if invalid := validateBody(requestSchema(ctx.endpoint), attrs); len(invalid) > 0 {
		return e.validationErrorResponse(ctx, invalid), nil
	}

	expectedVersion := parseIfMatch(ctx.req.header("if-match"))
	reqAttrs := attrs

	// CAS on the version READ even without If-Match, so concurrent merge-PATCHes
	// compose; a CAS miss re-merges (bounded), or is the client's 412 if If-Match.
	const maxModifyRetries = 4
	for attempt := 0; ; attempt++ {
		current, err := e.store.GetOne(e.id, ctx.op.typ, *ctx.op.key)
		if errors.Is(err, ErrNotFound) {
			return e.endpointError(ctx, 404, "Not Found"), nil
		}
		if err != nil {
			return nil, err
		}
		if expectedVersion != nil && current.Version != *expectedVersion {
			return e.endpointError(ctx, 412, "Precondition Failed"), nil
		}
		if ctx.op.kind == opMerge {
			// Shallow merge over the stored attributes (JS object spread).
			parsed, err := parseJSONValue(string(current.Attributes))
			if err != nil {
				return nil, err
			}
			base, _ := parsed.(*JSONObject)
			if base == nil {
				base = NewJSONObject()
			}
			merged := base.Clone()
			for _, k := range reqAttrs.Keys() {
				v, _ := reqAttrs.Get(k)
				merged.Set(k, v)
			}
			attrs = merged
		} else {
			// Replace: re-complete absent declared fields from the response schema.
			synth := e.synthCtx("replace", *ctx.op.key, strconv.FormatInt(current.Version+1, 10))
			attrs = completeResource(successSchema(ctx.endpoint, 200), reqAttrs, synth, false)
		}
		// Preserve the resource's identity across the write.
		attrs.Set("id", *ctx.op.key)

		marshaled, err := marshalJSValue(attrs)
		if err != nil {
			return nil, err
		}
		totals, err := e.store.Totals(e.id)
		if err != nil {
			return nil, err
		}
		if denied := e.quotaGuard(int64(len(marshaled)), totals.Count, totals.Bytes-current.SizeBytes+int64(len(marshaled))); denied != nil {
			return denied, nil
		}

		cas := current.Version
		result, err := e.store.Update(e.id, ctx.op.typ, *ctx.op.key, marshaled, &cas, nil, false, e.virtualClockMs)
		if errors.Is(err, ErrNotFound) {
			return e.endpointError(ctx, 404, "Not Found"), nil
		}
		if errors.Is(err, ErrConflict) {
			if expectedVersion == nil && attempt < maxModifyRetries {
				continue // a concurrent writer advanced the version: re-read, re-merge
			}
			return e.endpointError(ctx, 412, "Precondition Failed"), nil
		}
		if err != nil {
			return nil, err
		}
		e.enqueueWebhookFor(ctx.endpoint, webhookActionUpdated, typeSlug(ctx.op.typ), json.RawMessage(result.Attributes))
		return jsonResponse(200, json.RawMessage(result.Attributes), map[string]string{"etag": etagFor(result.Version)}), nil
	}
}

func (e *Engine) doRemove(ctx *storeCtx) (*RawResponse, error) {
	ok, err := e.store.Remove(e.id, ctx.op.typ, *ctx.op.key)
	if err != nil {
		return nil, err
	}
	if !ok {
		return e.endpointError(ctx, 404, "Not Found"), nil
	}
	// On remove, the deleted event carries only { id }.
	deleted := NewJSONObject()
	deleted.Set("id", *ctx.op.key)
	e.enqueueWebhookFor(ctx.endpoint, webhookActionDeleted, typeSlug(ctx.op.typ), deleted)
	status := pickSuccessStatus(ctx.endpoint, 204)
	if status == 204 {
		return &RawResponse{Status: 204, Headers: map[string]string{}, Body: nil}, nil
	}
	body := NewJSONObject()
	body.Set("id", *ctx.op.key)
	return jsonResponse(status, body, nil), nil
}

func objectBody(req *ingressRequest) (*JSONObject, *RawResponse) {
	if req.bodyInvalid {
		return nil, buildErrorResponse(400, "Malformed JSON body", nil)
	}
	if !req.bodyPresent {
		return NewJSONObject(), nil
	}
	obj, ok := req.bodyValue.(*JSONObject)
	if !ok {
		return nil, buildErrorResponse(400, "Request body must be a JSON object", nil)
	}
	return obj, nil
}

func validationError(errs []string) *RawResponse {
	// Neutral, non-platform shape; field messages only, no internal codes.
	body := NewJSONObject()
	body.Set("message", "Validation failed")
	items := make([]any, len(errs))
	for i, e := range errs {
		items[i] = e
	}
	body.Set("errors", items)
	serialized, _ := marshalJSValue(body)
	return &RawResponse{Status: 400, Headers: map[string]string{"content-type": jsonContentType}, Body: serialized}
}

// validationErrorLadder: most specific first, so a spec declaring its own 422
// or 400 shape gets THAT shape back and clients parse provider-shaped errors.
var validationErrorLadder = []int{422, 400}

// validationErrorResponse prefers the PROVIDER'S declared error shape, else the
// neutral pikopod shape, which keeps transcript parity byte-identical.
func (e *Engine) validationErrorResponse(ctx *storeCtx, errs []string) *RawResponse {
	var resp *RawResponse
	for _, status := range validationErrorLadder {
		if schema := errorSchema(ctx.endpoint, status); schema != nil {
			synth := e.synthCtx("validation-error", strconv.Itoa(status))
			if body := synthesize(schema, synth, 0, ""); body != nil {
				resp = jsonResponse(status, body, nil)
				break
			}
		}
	}
	if resp == nil {
		resp = validationError(errs)
	}
	resp.Headers[violationsHeader] = renderViolations(errs)
	return resp
}

// violationsHeader carries the violation list whatever body shape was negotiated.
const violationsHeader = "x-pikopod-violations"

// Oversized headers silently 502 behind proxies.
const maxViolationsHeaderBytes = 8*1024 - 100

func renderViolations(errs []string) string {
	rendered, err := json.Marshal(errs)
	if err != nil {
		return "[]"
	}
	if len(rendered) <= maxViolationsHeaderBytes {
		return string(rendered)
	}
	for i := len(errs) - 1; i > 0; i-- {
		truncated, err := json.Marshal(append(errs[:i:i], "…truncated"))
		if err == nil && len(truncated) <= maxViolationsHeaderBytes {
			return string(truncated)
		}
	}
	return `["…truncated"]`
}

func (e *Engine) hashRequest(ctx *storeCtx) (string, error) {
	body := ""
	if ctx.req.bodyPresent {
		b, err := marshalJSValue(ctx.req.bodyValue)
		if err != nil {
			return "", err
		}
		body = string(b)
	}
	sum := sha256.Sum256([]byte(ctx.req.method + "\n" + ctx.innerPath + "\n" + body))
	return hex.EncodeToString(sum[:]), nil
}

// idempotentReplayLocked requires idemMu held (see doCreate).
func (e *Engine) idempotentReplayLocked(key, reqHash string) *RawResponse {
	existing, ok := e.idem[key]
	if !ok {
		return nil
	}
	if existing.requestHash != reqHash {
		return buildErrorResponse(422, "Idempotency-Key was reused with a different request", nil)
	}
	return &RawResponse{
		Status:  existing.status,
		Headers: map[string]string{"content-type": jsonContentType, "idempotent-replayed": "true"},
		Body:    existing.body,
	}
}

// recordIdempotencyLocked requires idemMu held (see doCreate).
func (e *Engine) recordIdempotencyLocked(key, reqHash string, response *RawResponse) {
	body := response.Body
	if body == nil {
		body = []byte("")
	}
	// Unbounded, a local client could exhaust memory. Eviction is arbitrary:
	// idempotency replay is a convenience window, not a ledger.
	if len(e.idem) >= maxIdemEntries {
		for k := range e.idem {
			delete(e.idem, k)
			break
		}
	}
	e.idem[key] = idemRecord{requestHash: reqHash, status: response.Status, body: body}
}

// quotaGuard maps a quota breach to a mirrored client error.
func (e *Engine) quotaGuard(resourceBytes, projectedCount, projectedBytes int64) *RawResponse {
	if resourceBytes > e.quota.MaxResourceBytes {
		return buildErrorResponse(413, "Payload Too Large", nil)
	}
	if projectedCount > e.quota.MaxResources || projectedBytes > e.quota.MaxStorageBytes {
		return buildErrorResponse(507, "Insufficient Storage", nil)
	}
	return nil
}
