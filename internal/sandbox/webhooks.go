// Webhook outbox: every state change enqueues a delivery whose envelope, id
// and signature are seeded and virtual-clocked, never wall-clocked.
package sandbox

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/pikopod/pikopod/internal/errfmt"
	"github.com/pikopod/pikopod/internal/ir"
)

// Webhook actions. The emitted event name is `<typeSlug>.<action>`.
const (
	webhookActionCreated = "created"
	webhookActionUpdated = "updated"
	webhookActionDeleted = "deleted"
)

// Outbox bounds: the log is drop-oldest and the sink queue drops (and counts)
// rather than ever blocking a request.
const (
	maxWebhookLog      = 1000
	webhookSinkQueue   = 256
	webhookSinkTimeout = 5 * time.Second
)

// WebhookDelivery is one enqueued delivery. Payload IS the signed body — the
// exact bytes a sink receives, with `created` in virtual-clock seconds.
type WebhookDelivery struct {
	ID            string `json:"id"`
	Event         string `json:"event"`
	Seq           int64  `json:"seq"`
	VirtualTimeMs int64  `json:"virtualTimeMs"`
	// DueMs is when the delivery becomes visible (a delay_webhook fault
	// pushes it past VirtualTimeMs — virtualized, never a real sleep).
	DueMs   int64           `json:"dueMs,omitempty"`
	Payload json.RawMessage `json:"payload"`
	// Signature is hex(HMAC-SHA256(secret, "<created>.<payload>")) — the value
	// carried (sha256=-prefixed) in x-pikopod-webhook-signature.
	Signature string `json:"signature"`
}

// SinkStats counts sink outcomes (best-effort delivery; never fatal).
type SinkStats struct {
	Delivered int64  `json:"delivered"`
	Failed    int64  `json:"failed"`
	Dropped   int64  `json:"dropped"`
	LastError string `json:"lastError,omitempty"`
}

// webhookSecretFor derives the sandbox's webhook signing secret from the run
// seed.
func webhookSecretFor(seed string) string {
	return SandboxWebhookSecretPrefix + NewPrng(seed+":webhook:secret").Hex(48)
}

// WebhookSecret returns the secret a receiver verifies signatures against.
func (e *Engine) WebhookSecret() string { return e.webhookSecret }

// IssuedWebhookSecret exposes the derivation for the CLI (printed once at
// sandbox add, like the issued credential).
func IssuedWebhookSecret(seed string) string { return webhookSecretFor(seed) }

// signWebhookPayload computes HMAC-SHA256 over "<timestampSec>.<body>", hex.
func signWebhookPayload(secret string, timestampSec int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(timestampSec, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// webhooksEnabled: an IR that declares webhooks is the implicit subscription.
func (e *Engine) webhooksEnabled() bool { return len(e.def.Webhooks) > 0 }

// enqueueWebhookFor fires every delivery one state change produces, resolving
// trigger-matched webhooks first, then by event name, then a generic envelope.
func (e *Engine) enqueueWebhookFor(endpoint *ir.Endpoint, action, slug string, data any) {
	if !e.webhooksEnabled() {
		return
	}
	e.tracef("webhook", "enqueueing %q for %s", action, slug)
	fallback := slug + "." + action

	e.webhookMu.Lock()
	defer e.webhookMu.Unlock()

	var ds []*WebhookDelivery
	for i := range e.def.Webhooks {
		w := &e.def.Webhooks[i]
		byTrigger := endpoint != nil && w.Trigger != nil &&
			strings.EqualFold(w.Trigger.Method, endpoint.Method.Value) &&
			w.Trigger.PathTemplate == endpoint.PathTemplate.Value &&
			triggerActionMatches(w.Event.Value, action)
		byName := w.Trigger == nil && w.Event.Value == fallback
		if !byTrigger && !byName {
			continue
		}
		if d := e.buildProviderDelivery(w, data); d != nil {
			ds = append(ds, d)
		}
		if byName {
			// One delivery per event NAME: the IR holds one entry per method
			// under a path item, and matching all would duplicate deliveries.
			break
		}
	}
	// No declared event for this change means nothing is sent. The sandbox
	// never invents one: at the handler it would look exactly like a real one.
	if len(ds) == 0 {
		e.tracef("webhook", "no declared event for %s %s; nothing sent", slug, action)
	}
	for _, d := range ds {
		e.emitThroughFaults(d)
	}
}

// triggerActionMatches: an event suffixed with a known action fires only on
// that action; any other event name fires on any state change.
func triggerActionMatches(event, action string) bool {
	for _, a := range []string{webhookActionCreated, webhookActionUpdated, webhookActionDeleted} {
		if strings.HasSuffix(event, "."+a) {
			return a == action
		}
	}
	return true
}

// buildProviderDelivery synthesizes the provider's documented payload shape,
// overlays the real resource data, and signs it. Caller holds webhookMu.
func (e *Engine) buildProviderDelivery(w *ir.Webhook, data any) *WebhookDelivery {
	if w.PayloadSchema == nil {
		return e.buildDelivery(w.Event.Value, data)
	}
	e.webhookSeq++
	seq := e.webhookSeq
	synth := makeContext(e.seed+":webhook:"+w.Event.Value+":"+strconv.FormatInt(seq, 10), e.virtualClockMs, e.namedSchemas)
	payload := synthesize(w.PayloadSchema, synth, 0, "")
	if _, isObject := payload.(*JSONObject); !isObject {
		e.tracef("webhook", "%s's documented payload is not an object; using the event envelope", w.Event.Value)
		e.webhookSeq--
		return e.buildDelivery(w.Event.Value, data)
	}
	if !overlayResourceData(payload, data) {
		e.tracef("webhook", "no field of %s overlaps the resource; payload is synthesized", w.Event.Value)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		e.webhookSeq--
		return nil
	}
	id := "sbxd_" + NewPrng(e.seed+":webhook:"+strconv.FormatInt(seq, 10)).Hex(24)
	createdSec := e.virtualClockMs / 1000
	return &WebhookDelivery{
		ID: id, Event: w.Event.Value, Seq: seq, VirtualTimeMs: e.virtualClockMs,
		Payload:   body,
		Signature: signWebhookPayload(e.webhookSecret, createdSec, body),
	}
}

// overlayResourceData grafts real resource attributes into whichever object in
// the synthesized payload shares the most keys with them; false when none do.
func overlayResourceData(payload, data any) bool {
	attrs := toPlainMap(data)
	if len(attrs) == 0 {
		return false
	}
	var best *JSONObject
	bestScore := 0
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case *JSONObject:
			score := 0
			for _, k := range x.Keys() {
				if _, ok := attrs[k]; ok {
					score++
				}
			}
			if score > bestScore {
				best, bestScore = x, score
			}
			for _, k := range x.Keys() {
				if inner, ok := x.Get(k); ok {
					walk(inner)
				}
			}
		case []any:
			for _, item := range x {
				walk(item)
			}
		}
	}
	walk(payload)
	if best == nil {
		return false
	}
	for _, k := range best.Keys() {
		if v, ok := attrs[k]; ok {
			best.Set(k, v)
		}
	}
	return true
}

// EmitWebhook fires a DECLARED event on demand, for events no API call causes
// (money landing, a chargeback). Undeclared names are refused, never invented.
func (e *Engine) EmitWebhook(event string, data json.RawMessage) error {
	var w *ir.Webhook
	for i := range e.def.Webhooks {
		if e.def.Webhooks[i].Event.Value == event {
			w = &e.def.Webhooks[i]
			break
		}
	}
	if w == nil {
		return errfmt.New("webhook event is not declared",
			event+" is not in this sandbox's spec, and pikopod never invents an event",
			"declare it under webhooks in the spec, with x-pikopod-emit-only: true if no API call causes it",
			"scenarios/README.md")
	}
	var payload any
	if len(data) > 0 {
		if err := json.Unmarshal(data, &payload); err != nil {
			return errfmt.Newf("webhook data is not valid JSON", "pass a JSON object", "scenarios/README.md", "%v", err)
		}
	}
	e.webhookMu.Lock()
	defer e.webhookMu.Unlock()
	d := e.buildProviderDelivery(w, payload)
	if d == nil {
		return errfmt.New("could not build the "+event+" delivery", "the documented payload did not serialize", "check the webhook's schema in the spec", "scenarios/README.md")
	}
	e.emitThroughFaults(d)
	return nil
}

func toPlainMap(data any) map[string]any {
	raw, err := json.Marshal(data)
	if err != nil {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m
}

// emitThroughFaults runs one delivery through the armed webhook faults and
// records the survivors. Caller holds webhookMu.
func (e *Engine) emitThroughFaults(d *WebhookDelivery) {

	// First matching rule wins; duplicate/drop are one-shot, reorder consumes
	// after its second delivery.
	emit := []*WebhookDelivery{d}
	e.faultMu.Lock()
	for i := range e.faults {
		f := &e.faults[i]
		if !isWebhookFaultKind(f.Kind) || (f.Event != "" && f.Event != d.Event) {
			continue
		}
		switch f.Kind {
		case FaultDuplicateWebhook:
			// At-least-once replay: identical envelope id, payload and
			// signature — only the log seq advances.
			dup := *d
			e.webhookSeq++
			dup.Seq = e.webhookSeq
			emit = append(emit, &dup)
			e.removeFaultAt(i)
		case FaultDropWebhook:
			emit = nil
			e.removeFaultAt(i)
		case FaultDelayWebhook:
			// Virtualized: the delivery exists but is visible only once the
			// virtual clock reaches DueMs — never a real sleep.
			delay := f.DelayMs
			if delay <= 0 {
				delay = 30000
			}
			d.DueMs = d.VirtualTimeMs + delay
			e.removeFaultAt(i)
		case FaultReorderWebhook:
			if f.held == nil {
				f.held = d // hold the first matching delivery…
				emit = nil
			} else {
				emit = []*WebhookDelivery{d, f.held} // …emit the second first
				e.removeFaultAt(i)
			}
		}
		break
	}
	e.faultMu.Unlock()

	for _, out := range emit {
		e.appendDelivery(out)
	}
}

// buildDelivery assembles the deterministic envelope + signature. Caller
// holds webhookMu (the seq counter and id derivation are one stream).
func (e *Engine) buildDelivery(event string, data any) *WebhookDelivery {
	e.webhookSeq++
	seq := e.webhookSeq
	id := "sbxd_" + NewPrng(e.seed+":webhook:"+strconv.FormatInt(seq, 10)).Hex(24)
	createdSec := e.virtualClockMs / 1000 // virtual-clock seconds

	env := NewJSONObject()
	env.Set("id", id)
	env.Set("event", event)
	env.Set("created", json.Number(strconv.FormatInt(createdSec, 10)))
	env.Set("data", data)
	payload, err := marshalJSValue(env)
	if err != nil {
		e.webhookSeq-- // roll the stream back; nothing was emitted
		return nil
	}
	return &WebhookDelivery{
		ID:            id,
		Event:         event,
		Seq:           seq,
		VirtualTimeMs: e.virtualClockMs,
		Payload:       payload,
		Signature:     signWebhookPayload(e.webhookSecret, createdSec, payload),
	}
}

// appendDelivery records into the bounded log and hands off to the sink.
// Caller holds webhookMu.
func (e *Engine) appendDelivery(d *WebhookDelivery) {
	e.webhookLog = append(e.webhookLog, *d)
	if len(e.webhookLog) > maxWebhookLog {
		// Drop-oldest: copy down so the backing array does not pin dropped rows.
		n := copy(e.webhookLog, e.webhookLog[len(e.webhookLog)-maxWebhookLog:])
		e.webhookLog = e.webhookLog[:n]
	}
	if e.sinkCh != nil {
		select {
		case e.sinkCh <- *d:
		default:
			// A wedged sink NEVER back-pressures the data plane: drop + count.
			atomic.AddInt64(&e.sinkDropped, 1)
		}
	}
}

// Deliveries returns recorded deliveries in delivery order (a reorder fault
// deliberately makes that differ from seq order), optionally filtered by event.
func (e *Engine) Deliveries(eventFilter string) []WebhookDelivery {
	e.webhookMu.Lock()
	defer e.webhookMu.Unlock()
	out := make([]WebhookDelivery, 0, len(e.webhookLog))
	for i := range e.webhookLog {
		if eventFilter == "" || e.webhookLog[i].Event == eventFilter {
			out = append(out, e.webhookLog[i])
		}
	}
	return out
}

// DeliveriesDueBy returns deliveries due by a virtual horizon plus the last
// arrival's clock, so EXPECT_WEBHOOK jumps there instead of burning a timeout.
func (e *Engine) DeliveriesDueBy(eventFilter string, horizonMs int64) (out []WebhookDelivery, lastArrivalMs int64) {
	e.webhookMu.Lock()
	defer e.webhookMu.Unlock()
	for i := range e.webhookLog {
		d := &e.webhookLog[i]
		if eventFilter != "" && d.Event != eventFilter {
			continue
		}
		due := d.DueMs
		if due == 0 {
			due = d.VirtualTimeMs
		}
		if due > horizonMs {
			continue
		}
		if due > lastArrivalMs {
			lastArrivalMs = due
		}
		out = append(out, *d)
	}
	return out, lastArrivalMs
}

// WebhookSinkStats snapshots sink outcome counters (tests poll Delivered).
func (e *Engine) WebhookSinkStats() SinkStats {
	last, _ := e.sinkLastErr.Load().(string)
	return SinkStats{
		Delivered: atomic.LoadInt64(&e.sinkDelivered),
		Failed:    atomic.LoadInt64(&e.sinkFailed),
		Dropped:   atomic.LoadInt64(&e.sinkDropped),
		LastError: last,
	}
}

func (e *Engine) sinkFailure(d *WebhookDelivery, reason string) {
	atomic.AddInt64(&e.sinkFailed, 1)
	e.sinkLastErr.Store(d.Event + " (" + d.ID + "): " + reason)
}

// sinkLoop drains the queue with one bounded-timeout attempt per delivery and
// no retries, on its own goroutine for the engine's lifetime.
func (e *Engine) sinkLoop() {
	client := &http.Client{Timeout: webhookSinkTimeout}
	for d := range e.sinkCh {
		e.deliverToSink(client, &d)
	}
}

// deliverToSink is panic-isolated: a webhook sink failure is counted, never
// propagated anywhere near a request.
func (e *Engine) deliverToSink(client *http.Client, d *WebhookDelivery) {
	defer func() {
		if r := recover(); r != nil {
			e.sinkFailure(d, fmt.Sprint("panic: ", r))
		}
	}()
	body := d.Payload
	headers := map[string]string{
		"content-type":                "application/json",
		"x-pikopod-webhook-id":        d.ID,
		"x-pikopod-webhook-event":     d.Event,
		"x-pikopod-webhook-timestamp": strconv.FormatInt(d.VirtualTimeMs/1000, 10),
		"x-pikopod-webhook-signature": "sha256=" + d.Signature,
	}
	if e.envelope != nil {
		wire, err := e.envelope.render(d)
		if err != nil {
			e.tracef("webhook", "envelope for %s: %v", d.Event, err)
			e.sinkFailure(d, "envelope: "+err.Error())
			return
		}
		body, headers = wire.Body, wire.Headers
	}
	req, err := http.NewRequest(http.MethodPost, e.webhookURL, bytes.NewReader(body))
	if err != nil {
		e.sinkFailure(d, err.Error())
		return
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		e.sinkFailure(d, err.Error())
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		atomic.AddInt64(&e.sinkDelivered, 1)
	} else {
		e.sinkFailure(d, "sink answered "+strconv.Itoa(resp.StatusCode))
	}
}
