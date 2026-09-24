package sandbox

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/pikopod/pikopod/internal/ir"
)

type FaultRule struct {
	Method      string  `json:"method"`
	Path        string  `json:"path"`
	Kind        string  `json:"kind"`
	Status      int     `json:"status,omitempty"`
	DelayMs     int64   `json:"delayMs,omitempty"`
	Probability float64 `json:"probability"`
	ID          string  `json:"id,omitempty"`

	Event string `json:"event,omitempty"`

	Wallclock bool `json:"wallclock,omitempty"`

	Times int `json:"times,omitempty"`

	Per string `json:"per,omitempty"`

	Delay *DelayDistribution `json:"delayDistribution,omitempty"`

	consumed map[string]int

	held *WebhookDelivery
}

const maxFaultWindowKeys = 1024

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

type DelayDistribution struct {
	Type     string  `json:"type"`
	MedianMs int64   `json:"medianMs,omitempty"`
	Sigma    float64 `json:"sigma,omitempty"`
	MaxMs    int64   `json:"maxMs,omitempty"`
	LowerMs  int64   `json:"lowerMs,omitempty"`
	UpperMs  int64   `json:"upperMs,omitempty"`
	BaseMs   int64   `json:"baseMs,omitempty"`
	Pct      float64 `json:"pct,omitempty"`
}

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
		return d.MaxMs
	case "uniform":
		if d.UpperMs <= d.LowerMs {
			return d.LowerMs
		}
		return d.LowerMs + int64(prng.Next()*float64(d.UpperMs-d.LowerMs+1))
	case "band":

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

func gaussian(prng *Prng) float64 {
	u1 := prng.Next()
	if u1 < 1e-12 {
		u1 = 1e-12
	}
	u2 := prng.Next()
	return math.Sqrt(-2*math.Log(u1)) * math.Cos(2*math.Pi*u2)
}

const (
	FaultDuplicateWebhook = "duplicate_webhook"
	FaultDropWebhook      = "drop_webhook"
	FaultReorderWebhook   = "reorder_webhook"
	FaultDelayWebhook     = "delay_webhook"
)

func isWebhookFaultKind(kind string) bool {
	return kind == FaultDuplicateWebhook || kind == FaultDropWebhook || kind == FaultReorderWebhook || kind == FaultDelayWebhook
}

func IsWebhookFaultKind(kind string) bool { return isWebhookFaultKind(kind) }

var faultKinds = []string{
	"error", "latency", "hang", "slow_body", "rate_limit",
	FaultConnectionReset, FaultMalformedResponse, FaultWrongContentLength,
	FaultDuplicateWebhook, FaultDropWebhook, FaultReorderWebhook, FaultDelayWebhook,
}

func FaultKinds() []string { return append([]string(nil), faultKinds...) }

func ValidFaultKind(kind string) bool {
	for _, k := range faultKinds {
		if k == kind {
			return true
		}
	}
	return false
}

func ResolveFaultKind(kind string, status int) (engineKind string, engineStatus int) {
	switch kind {
	case "rate_limit":
		return "error", 429
	case "error":
		if status == 0 {
			status = 500
		}
		return "error", status
	}
	return kind, status
}

const maxFaultDelayMs = int64(60_000)

const RateLimitRetryAfterSec = 30

const (
	FaultConnectionReset = "connection_reset"

	FaultMalformedResponse = "malformed_response"

	FaultWrongContentLength = "wrong_content_length"
)

const (
	FaultDelayHeader   = "x-pikopod-fault-delay-ms"
	FaultAppliedHeader = "x-pikopod-fault-applied"
)

func (e *Engine) ArmFault(f FaultRule) {
	e.faultMu.Lock()
	defer e.faultMu.Unlock()
	e.faults = append(e.faults, f)
}

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

func (e *Engine) removeFaultAt(i int) {
	e.faults = append(e.faults[:i], e.faults[i+1:]...)
}

func (e *Engine) Faults() []FaultRule {
	e.faultMu.Lock()
	defer e.faultMu.Unlock()
	out := make([]FaultRule, len(e.faults))
	copy(out, e.faults)
	return out
}

type wireFault struct {
	sleepMs    int64
	hangMs     int64
	slowBodyMs int64

	resetConn   bool
	malformed   bool
	wrongLength bool
}

func (e *Engine) handleWire(req *ingressRequest, innerPath string) (*RawResponse, *wireFault, error) {
	result := matchRoute(e.def.Endpoints, req.method, innerPath)
	req.route = result
	if result.kind != matchFound {

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
		resp, err := e.serve(req, innerPath)
		if resp != nil && recordingsMissed {

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
			resetConn: fx.resetConn, malformed: fx.malformed, wrongLength: fx.wrongLength}
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
}

func (e *Engine) evaluateFaults(endpoint *ir.Endpoint, req *ingressRequest, innerPath string) faultOutcome {
	method := req.method
	var out faultOutcome
	e.faultMu.Lock()
	defer e.faultMu.Unlock()

	matchIdx := 0
	for ri := range e.faults {
		f := &e.faults[ri]
		if isWebhookFaultKind(f.Kind) {
			continue
		}
		if f.Method != strings.ToUpper(endpoint.Method.Value) || f.Path != endpoint.PathTemplate.Value {
			continue
		}
		i := matchIdx
		matchIdx++

		if f.Times > 0 {

			key := windowKey(f.Per, req, innerPath)
			if f.consumed == nil {
				f.consumed = map[string]int{}
			}
			count, seen := f.consumed[key]
			if count >= f.Times {
				continue
			}
			if !seen && len(f.consumed) >= maxFaultWindowKeys {
				continue
			}
			f.consumed[key] = count + 1
		} else if !e.faultFires(f, i, method, innerPath, f.Probability) {
			continue
		}
		if f.Wallclock || e.wallclockFaults {
			out.wallclock = true
		}
		if f.Delay != nil {

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

				out.errResp = &RawResponse{Status: status, Headers: map[string]string{}, Body: nil}

				if status == http.StatusTooManyRequests {
					out.errResp.Headers["retry-after"] = strconv.Itoa(RateLimitRetryAfterSec)
				}
				out.kinds = append(out.kinds, "error")
				out.delayMs += f.DelayMs
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

func ruleSeedKey(f *FaultRule, index int) string {
	if f.ID != "" {
		return f.ID
	}
	return strconv.Itoa(index)
}
