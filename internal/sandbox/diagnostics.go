// Simulator self-explanation. Diagnostics ride HEADERS, never bodies: parity
// compares a fixed header subset, so additive x-pikopod-* headers keep parity.
package sandbox

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pikopod/pikopod/internal/ir"
)

const (
	// OperationHeader names the matched operation on every served response.
	OperationHeader = "x-pikopod-operation"
	// ClosestHeader lists the nearest declared operations on 404/405s.
	ClosestHeader = "x-pikopod-closest"
)

// operationLabel identifies an endpoint: its declared operationId when the
// spec has one, else "METHOD /template".
func operationLabel(endpoint *ir.Endpoint) string {
	if endpoint.OperationID != nil && endpoint.OperationID.Value != "" {
		return endpoint.OperationID.Value
	}
	return strings.ToUpper(endpoint.Method.Value) + " " + endpoint.PathTemplate.Value
}

// closestOperations scores every declared operation against the request and
// returns the top 3 as "METHOD /template" — a near-miss list sized for a header.
func closestOperations(endpoints []ir.Endpoint, method, innerPath string) []string {
	segs := strings.Split(strings.Trim(innerPath, "/"), "/")
	type scored struct {
		label string
		score float64
	}
	out := make([]scored, 0, len(endpoints))
	for i := range endpoints {
		e := &endpoints[i]
		tSegs := strings.Split(strings.Trim(e.PathTemplate.Value, "/"), "/")
		score := 0.0
		n := len(segs)
		if len(tSegs) < n {
			n = len(tSegs)
		}
		for j := 0; j < n; j++ {
			if strings.Contains(tSegs[j], "{") {
				score += 0.5 // a parameter accepts anything — near, not exact
			} else if tSegs[j] == segs[j] {
				score++
			}
		}
		score -= 0.25 * float64(abs(len(tSegs)-len(segs)))
		if strings.EqualFold(e.Method.Value, method) {
			score++
		}
		out = append(out, scored{
			label: strings.ToUpper(e.Method.Value) + " " + e.PathTemplate.Value,
			score: score,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].score > out[j].score })
	labels := make([]string, 0, 3)
	for i := 0; i < len(out) && i < 3; i++ {
		if out[i].score <= 0 {
			break
		}
		labels = append(labels, out[i].label)
	}
	return labels
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// SetTrace installs a pipeline narrator (nil disables — the default). Not safe
// to change while serving; `pikopod why` sets it once on a fresh fork.
func (e *Engine) SetTrace(fn func(stage, message string)) { e.trace = fn }

// SetMountPrefix records the server's route prefix so emitted URLs (pagination
// Link headers) resolve for link-following clients instead of losing it.
func (e *Engine) SetMountPrefix(prefix string) { e.mountPrefix = prefix }

// tracef narrates one pipeline decision when a tracer is installed.
func (e *Engine) tracef(stage, format string, args ...any) {
	if e.trace != nil {
		e.trace(stage, fmt.Sprintf(format, args...))
	}
}
