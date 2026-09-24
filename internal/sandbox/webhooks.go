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

const (
	webhookActionCreated = "created"
	webhookActionUpdated = "updated"
	webhookActionDeleted = "deleted"
)

const (
	maxWebhookLog      = 1000
	webhookSinkQueue   = 256
	webhookSinkTimeout = 5 * time.Second
)

type WebhookDelivery struct {
	ID            string `json:"id"`
	Event         string `json:"event"`
	Seq           int64  `json:"seq"`
	VirtualTimeMs int64  `json:"virtualTimeMs"`

	DueMs   int64           `json:"dueMs,omitempty"`
	Payload json.RawMessage `json:"payload"`

	Signature string `json:"signature"`
}

type SinkStats struct {
	Delivered int64  `json:"delivered"`
	Failed    int64  `json:"failed"`
	Dropped   int64  `json:"dropped"`
	LastError string `json:"lastError,omitempty"`
}

func webhookSecretFor(seed string) string {
	return SandboxWebhookSecretPrefix + NewPrng(seed+":webhook:secret").Hex(48)
}

func (e *Engine) WebhookSecret() string { return e.webhookSecret }

func IssuedWebhookSecret(seed string) string { return webhookSecretFor(seed) }

func signWebhookPayload(secret string, timestampSec int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(timestampSec, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func (e *Engine) webhooksEnabled() bool { return len(e.def.Webhooks) > 0 }

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

			break
		}
	}

	if len(ds) == 0 {
		e.tracef("webhook", "no declared event for %s %s; nothing sent", slug, action)
	}
	for _, d := range ds {
		e.emitThroughFaults(d)
	}
}

func triggerActionMatches(event, action string) bool {
	for _, a := range []string{webhookActionCreated, webhookActionUpdated, webhookActionDeleted} {
		if strings.HasSuffix(event, "."+a) {
			return a == action
		}
	}
	return true
}

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

func (e *Engine) emitThroughFaults(d *WebhookDelivery) {

	emit := []*WebhookDelivery{d}
	e.faultMu.Lock()
	for i := range e.faults {
		f := &e.faults[i]
		if !isWebhookFaultKind(f.Kind) || (f.Event != "" && f.Event != d.Event) {
			continue
		}
		switch f.Kind {
		case FaultDuplicateWebhook:

			dup := *d
			e.webhookSeq++
			dup.Seq = e.webhookSeq
			emit = append(emit, &dup)
			e.removeFaultAt(i)
		case FaultDropWebhook:
			emit = nil
			e.removeFaultAt(i)
		case FaultDelayWebhook:

			delay := f.DelayMs
			if delay <= 0 {
				delay = 30000
			}
			d.DueMs = d.VirtualTimeMs + delay
			e.removeFaultAt(i)
		case FaultReorderWebhook:
			if f.held == nil {
				f.held = d
				emit = nil
			} else {
				emit = []*WebhookDelivery{d, f.held}
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

func (e *Engine) buildDelivery(event string, data any) *WebhookDelivery {
	e.webhookSeq++
	seq := e.webhookSeq
	id := "sbxd_" + NewPrng(e.seed+":webhook:"+strconv.FormatInt(seq, 10)).Hex(24)
	createdSec := e.virtualClockMs / 1000

	env := NewJSONObject()
	env.Set("id", id)
	env.Set("event", event)
	env.Set("created", json.Number(strconv.FormatInt(createdSec, 10)))
	env.Set("data", data)
	payload, err := marshalJSValue(env)
	if err != nil {
		e.webhookSeq--
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

func (e *Engine) appendDelivery(d *WebhookDelivery) {
	e.webhookLog = append(e.webhookLog, *d)
	if len(e.webhookLog) > maxWebhookLog {

		n := copy(e.webhookLog, e.webhookLog[len(e.webhookLog)-maxWebhookLog:])
		e.webhookLog = e.webhookLog[:n]
	}
	if e.sinkCh != nil {
		if e.sinkClosed {
			atomic.AddInt64(&e.sinkDropped, 1)
			return
		}
		select {
		case e.sinkCh <- *d:
		default:

			atomic.AddInt64(&e.sinkDropped, 1)
		}
	}
}

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

func (e *Engine) sinkLoop() {
	defer close(e.sinkDone)
	client := &http.Client{Timeout: webhookSinkTimeout}
	for d := range e.sinkCh {
		e.deliverToSink(client, &d)
	}
}

func (e *Engine) Close() {
	e.webhookMu.Lock()
	if e.sinkCh == nil || e.sinkClosed {
		e.webhookMu.Unlock()
		return
	}
	e.sinkClosed = true
	close(e.sinkCh)
	e.webhookMu.Unlock()
	select {
	case <-e.sinkDone:
	case <-time.After(2 * webhookSinkTimeout):
	}
}

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
