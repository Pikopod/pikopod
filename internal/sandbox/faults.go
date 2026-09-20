// Fault injection: simulated infrastructure failure, evaluated FIRST (after
// route match, before auth), virtualized by default and wallclock by opt-in.
package sandbox

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/pikopod/pikopod/internal/ir"
)

// FaultRule is one armed fault. A webhook-kind rule matches on the delivery
// Event, never method/path, and is consumed by the outbox, not the pipeline.
type FaultRule struct {
	Method      string  `json:"method"`
	Path        string  `json:"path"` // matched against the endpoint's pathTemplate
	Kind        string  `json:"kind"` // "error" | "latency" | "hang" | "slow_body" | transport/webhook kinds below
	Status      int     `json:"status,omitempty"`
	DelayMs     int64   `json:"delayMs,omitempty"`
	Probability float64 `json:"probability"` // 0..1; 1 = always
	ID          string  `json:"id,omitempty"`
	// Event is the webhook event a webhook-kind rule matches ("" = any).
	Event string `json:"event,omitempty"`
	// Wallclock opts THIS rule into real wire delay (see the package note).
	Wallclock bool `json:"wallclock,omitempty"`
	// Times fires the rule for the FIRST N matching requests then RECOVERS:
	// a deterministic counter, so Probability is ignored while Times > 0.
	Times int `json:"times,omitempty"`
	// Per scopes the Times window: "" / "global", "idempotency-key" (header
	// absent ⇒ the global window), or "resource" (per concrete request path).
	Per string `json:"per,omitempty"`
	// Delay samples latency from a seeded distribution instead of DelayMs.
	Delay *DelayDistribution `json:"delayDistribution,omitempty"`

	// consumed counts fired requests per window key (guarded by faultMu);
	// past maxFaultWindowKeys, unseen keys count as already recovered.
	consumed map[string]int

	// held carries the first delivery a reorder_webhook rule captured, until
	// the second matching delivery releases both in swapped order.
	held *WebhookDelivery
}

// maxFaultWindowKeys bounds per-key Times tracking (a hostile client
// minting fresh idempotency keys must not grow memory without bound).
const maxFaultWindowKeys = 1024

// windowKey scopes a Times window per FaultRule.Per.
func windowKey(per string, req *ingressRequest, innerPath string) string {
	switch per {
	case "idempotency-key":
		if k := req.header("idempotency-key"); k != nil {
			return "ik:" + *k
		}
		return ""
	case "resource":
		return "path:" + innerPath
	default:
		return ""
	}
}

// DelayDistribution is a seeded latency model that replays identically for
// the same seed and request. Exactly one shape applies.
type DelayDistribution struct {
	Type     string  `json:"type"` // "lognormal" | "uniform" | "band"
	MedianMs int64   `json:"medianMs,omitempty"`
	Sigma    float64 `json:"sigma,omitempty"`
	MaxMs    int64   `json:"maxMs,omitempty"`
	LowerMs  int64   `json:"lowerMs,omitempty"`
	UpperMs  int64   `json:"upperMs,omitempty"`
	BaseMs   int64   `json:"baseMs,omitempty"`
	Pct      float64 `json:"pct,omitempty"`
}

// sample draws one delay from the distribution using the given PRNG.
func (d *DelayDistribution) sample(prng *Prng) int64 {
	switch d.Type {
	case "lognormal":
		median := float64(d.MedianMs)
		for attempt := 0; attempt < 10; attempt++ {
			v := int64(math.Round(math.Exp(gaussian(prng)*d.Sigma) * median))
			if v < 0 {
				v = 0
			}
			if d.MaxMs <= 0 || v <= d.MaxMs {
				return v
			}
		}
		return d.MaxMs // resamples exhausted: clamp as the last resort
	case "uniform":
		if d.UpperMs <= d.LowerMs {
			return d.LowerMs
		}
		return d.LowerMs + int64(prng.Next()*float64(d.UpperMs-d.LowerMs+1))
	case "band":
		// base ± pct%: factor in [1-pct/100, 1+pct/100].
		factor := 1 + (prng.Next()*2-1)*d.Pct/100
		v := int64(math.Round(float64(d.BaseMs) * factor))
		if v < 0 {
			v = 0
		}
		return v
	default:
		return 0
	}
}

// gaussian is one standard-normal draw (Box-Muller over two PRNG draws).
func gaussian(prng *Prng) float64 {
	u1 := prng.Next()
	if u1 < 1e-12 {
		u1 = 1e-12
	}
	u2 := prng.Next()
	return math.Sqrt(-2*math.Log(u1)) * math.Cos(2*math.Pi*u2)
}

// Webhook fault kinds: deterministic and one-shot, acting on the next
// matching delivery (reorder, on the next two).
const (
	FaultDuplicateWebhook = "duplicate_webhook"
	FaultDropWebhook      = "drop_webhook"
	FaultReorderWebhook   = "reorder_webhook"
	FaultDelayWebhook     = "delay_webhook"
)

func isWebhookFaultKind(kind string) bool {
	return kind == FaultDuplicateWebhook || kind == FaultDropWebhook || kind == FaultReorderWebhook || kind == FaultDelayWebhook
}

const maxFaultDelayMs = int64(60_000)

// Transport-fault kinds: lies at the HTTP/TCP layer, wire-only — virtualized
// they degrade to header annotation, since a lying transcript breaks replay.
const (
	// FaultConnectionReset drops the connection with an RST (SO_LINGER 0),
	// not a FIN — the "connection reset by peer" your retry logic must eat.
	FaultConnectionReset = "connection_reset"
	// FaultMalformedResponse writes a valid status line, then garbage.
	FaultMalformedResponse = "malformed_response"
	// FaultWrongContentLength declares more bytes than it sends — clients
	// see an unexpected EOF mid-body.
	FaultWrongContentLength = "wrong_content_length"
	// FaultEmptyResponse closes an accepted connection without writing any
	// response bytes.
	FaultEmptyResponse = "empty_response"
	// FaultRandomDataThenClose writes non-HTTP bytes and closes the connection.
	FaultRandomDataThenClose = "random_data_then_close"
)

// Fault response annotation headers (consumed by the scenario runner).
const (
	FaultDelayHeader   = "x-pikopod-fault-delay-ms"
	FaultAppliedHeader = "x-pikopod-fault-applied"
)

// ArmFault appends a standing fault rule (behaviour-doc order preserved).
func (e *Engine) ArmFault(f FaultRule) {
	e.faultMu.Lock()
	defer e.faultMu.Unlock()
	e.faults = append(e.faults, f)
}

// ClearFaults removes matching faults; empty method/path act as wildcards.
// Returns how many were removed.
func (e *Engine) ClearFaults(method, path string) int {
	e.faultMu.Lock()
	kept := e.faults[:0]
	removed := 0
	var release []*WebhookDelivery
	for _, f := range e.faults {
		match := (method == "" || f.Method == method) && (path == "" || f.Path == path)
		if match {
			removed++
			if f.held != nil {
				// A held delivery is RELEASED on clear, not swallowed, and
				// emitted after unlock: webhookMu must never nest under faultMu.
				release = append(release, f.held)
			}
			continue
		}
		kept = append(kept, f)
	}
	e.faults = kept
	e.faultMu.Unlock()
	for _, d := range release {
		e.appendDelivery(d)
	}
	return removed
}

// removeFaultAt deletes one rule by index, preserving order. Caller holds
// faultMu.
func (e *Engine) removeFaultAt(i int) {
	e.faults = append(e.faults[:i], e.faults[i+1:]...)
}

// Faults returns a snapshot of the armed fault rules.
func (e *Engine) Faults() []FaultRule {
	e.faultMu.Lock()
	defer e.faultMu.Unlock()
	out := make([]FaultRule, len(e.faults))
	copy(out, e.faults)
	return out
}

// wireFault is what wallclock mode does ON THE WIRE; nil when virtualized.
type wireFault struct {
	sleepMs    int64 // sleep before responding (fixed delay / delay-then-error)
	hangMs     int64 // >0: hold, then drop the connection with NO response
	slowBodyMs int64 // >0: headers immediately, body dribbled over this window
	// Transport lies (see the Fault* kind constants).
	resetConn   bool
	malformed   bool
	wrongLength bool
	empty       bool
	randomData  bool
}

func (e *Engine) handleWire(req *ingressRequest, innerPath string) (*RawResponse, *wireFault, error) {
	result := matchRoute(e.def.Endpoints, req.method, innerPath)
	req.route = result
	if result.kind != matchFound {
		// A traffic-admitted endpoint serves before the 404; the recordings
		// tier (replay_tier.go) is the final fallback after it.
		recordingsMissed := false
		if result.kind == matchNotFound {
			if observed := e.serveObserved(req.method, innerPath); observed != nil {
				e.tracef("overlay", "served by a traffic-admitted OBSERVED endpoint (no spec route)")
				return observed, nil, nil
			}
			if recorded := e.serveRecording(req, innerPath); recorded != nil {
				e.tracef("recordings", "served from the recordings tier (no spec route, no admitted endpoint)")
				return recorded, nil, nil
			}
			recordingsMissed = e.recordings != nil
		}
		resp, err := e.serve(req, innerPath) // 404/405 come from serve; faults need a routed endpoint
		if resp != nil && recordingsMissed {
			// The whole ladder missed: name the nearest RECORDED endpoint too,
			// so the miss says how far EVERY tier got.
			if closest, n := e.recordings.ExplainMiss(req.method, innerPath); closest != "" {
				setHeader(resp, ReplayClosestHeader, fmt.Sprintf("%s (%d recording(s))", closest, n))
			}
		}
		if resp != nil && e.effective != nil {
			if resp.Headers == nil {
				resp.Headers = map[string]string{}
			}
			resp.Headers[ContractVersionHeader] = strconv.Itoa(e.effective.Version)
		}
		return resp, nil, err
	}
	fx := e.evaluateFaults(result.endpoint, req, innerPath)
	if len(fx.kinds) > 0 {
		e.tracef("faults", "fired: %s (delay %dms, wallclock=%v)", strings.Join(fx.kinds, ","), fx.delayMs, fx.wallclock)
	} else {
		e.tracef("faults", "no armed fault fired")
	}
	var wf *wireFault
	if fx.wallclock {
		wf = &wireFault{sleepMs: fx.delayMs, hangMs: fx.hangMs, slowBodyMs: fx.slowBodyMs,
			resetConn: fx.resetConn, malformed: fx.malformed, wrongLength: fx.wrongLength,
			empty: fx.empty, randomData: fx.randomData}
	}
	if fx.errResp != nil {
		annotateFault(fx.errResp, fx.delayMs, fx.kinds)
		setHeader(fx.errResp, OperationHeader, operationLabel(result.endpoint))
		return fx.errResp, wf, nil
	}
	resp, err := e.serve(req, innerPath)
	if err == nil && resp != nil && (fx.delayMs > 0 || len(fx.kinds) > 0) {
		annotateFault(resp, fx.delayMs, fx.kinds)
	}
	if err == nil {
		e.applyContract(resp, req.method, result.endpoint.PathTemplate.Value, innerPath)
	}
	if err == nil && resp != nil {
		// Name the operation on every matched response (diagnostics.go).
		setHeader(resp, OperationHeader, operationLabel(result.endpoint))
	}
	return resp, wf, err
}

func setHeader(resp *RawResponse, key, value string) {
	if resp.Headers == nil {
		resp.Headers = map[string]string{}
	}
	resp.Headers[key] = value
}

func annotateFault(resp *RawResponse, delayMs int64, kinds []string) {
	if resp.Headers == nil {
		resp.Headers = map[string]string{}
	}
	if delayMs > 0 {
		resp.Headers[FaultDelayHeader] = strconv.FormatInt(delayMs, 10)
	}
	if len(kinds) > 0 {
		resp.Headers[FaultAppliedHeader] = strings.Join(kinds, ",")
	}
}

// faultOutcome is one request's evaluated fault effects.
type faultOutcome struct {
	delayMs     int64
	kinds       []string
	errResp     *RawResponse
	hangMs      int64
	slowBodyMs  int64
	wallclock   bool
	resetConn   bool
	malformed   bool
	wrongLength bool
	empty       bool
	randomData  bool
}

// evaluateFaults keeps rule order, accumulates latency (capped 60s) and lets
// the first error win; Times windows gate→consume→commit under one faultMu.
func (e *Engine) evaluateFaults(endpoint *ir.Endpoint, req *ingressRequest, innerPath string) faultOutcome {
	method := req.method
	var out faultOutcome
	e.faultMu.Lock()
	defer e.faultMu.Unlock()

	matchIdx := 0
	for ri := range e.faults {
		f := &e.faults[ri]
		if isWebhookFaultKind(f.Kind) {
			continue // webhook rules are consumed by the outbox, never here
		}
		if f.Method != strings.ToUpper(endpoint.Method.Value) || f.Path != endpoint.PathTemplate.Value {
			continue
		}
		i := matchIdx
		matchIdx++

		if f.Times > 0 {
			// Probability is ignored here: "fails twice then succeeds" must
			// not be a maybe.
			key := windowKey(f.Per, req, innerPath)
			if f.consumed == nil {
				f.consumed = map[string]int{}
			}
			count, seen := f.consumed[key]
			if count >= f.Times {
				continue // recovered
			}
			if !seen && len(f.consumed) >= maxFaultWindowKeys {
				continue // key-tracking cap: unseen keys count as recovered
			}
			f.consumed[key] = count + 1
		} else if !e.faultFires(f, i, method, innerPath, f.Probability) {
			continue
		}
		if f.Wallclock || e.wallclockFaults {
			out.wallclock = true
		}
		if f.Delay != nil {
			// Seeded per (rule, request identity) so the same request replays
			// the same jitter.
			prng := NewPrng(fmt.Sprintf("%s:delaydist:%s:%s:%s", e.seed, ruleSeedKey(f, i), method, innerPath))
			out.delayMs += f.Delay.sample(prng)
			out.kinds = append(out.kinds, "latency")
		}
		switch f.Kind {
		case "latency":
			out.delayMs += f.DelayMs
			out.kinds = append(out.kinds, "latency")
		case "error":
			if out.errResp == nil {
				status := f.Status
				if status == 0 {
					status = 500
				}
				// The armed rule carries no body: a body-less response with no
				// content-type.
				out.errResp = &RawResponse{Status: status, Headers: map[string]string{}, Body: nil}
				out.kinds = append(out.kinds, "error")
				out.delayMs += f.DelayMs // delay-then-error composition
			}
		case "hang":
			hold := f.DelayMs
			if hold <= 0 {
				hold = maxFaultDelayMs
			}
			if hold > out.hangMs {
				out.hangMs = hold
			}
			out.kinds = append(out.kinds, "hang")
		case "slow_body":
			window := f.DelayMs
			if window <= 0 {
				window = 30000
			}
			if window > out.slowBodyMs {
				out.slowBodyMs = window
			}
			out.kinds = append(out.kinds, "slow_body")
		case FaultConnectionReset:
			out.resetConn = true
			out.kinds = append(out.kinds, FaultConnectionReset)
		case FaultMalformedResponse:
			out.malformed = true
			out.kinds = append(out.kinds, FaultMalformedResponse)
		case FaultWrongContentLength:
			out.wrongLength = true
			out.kinds = append(out.kinds, FaultWrongContentLength)
		case FaultEmptyResponse:
			out.empty = true
			out.kinds = append(out.kinds, FaultEmptyResponse)
		case FaultRandomDataThenClose:
			out.randomData = true
			out.kinds = append(out.kinds, FaultRandomDataThenClose)
		}
	}
	if out.delayMs > maxFaultDelayMs {
		out.delayMs = maxFaultDelayMs
	}
	if out.hangMs > maxFaultDelayMs {
		out.hangMs = maxFaultDelayMs
	}
	if out.slowBodyMs > maxFaultDelayMs {
		out.slowBodyMs = maxFaultDelayMs
	}
	return out
}

// faultFires is a seeded roll in DETERMINISTIC mode, over the seed string
// `${runSeed}:fault:${index}:${method}:${innerPath}`.
func (e *Engine) faultFires(f *FaultRule, index int, method, innerPath string, probability float64) bool {
	if probability >= 1 {
		return true
	}
	if probability <= 0 {
		return false
	}
	var roll float64
	if e.deterministic {
		roll = NewPrng(fmt.Sprintf("%s:fault:%s:%s:%s", e.seed, ruleSeedKey(f, index), method, innerPath)).Next()
	} else {
		var b [4]byte
		rand.Read(b[:])
		roll = float64(binary.BigEndian.Uint32(b[:])) / float64(1<<32)
	}
	return roll < probability
}

// ruleSeedKey seeds fault rolls by the rule's STABLE ID when it has one, so
// jitter survives other rules being cleared; index is the ID-less fallback.
func ruleSeedKey(f *FaultRule, index int) string {
	if f.ID != "" {
		return f.ID
	}
	return strconv.Itoa(index)
}
