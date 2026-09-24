package sandbox

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pikopod/pikopod/internal/ir"
)

const (
	OperationHeader = "x-pikopod-operation"

	ClosestHeader = "x-pikopod-closest"
)

func operationLabel(endpoint *ir.Endpoint) string {
	if endpoint.OperationID != nil && endpoint.OperationID.Value != "" {
		return endpoint.OperationID.Value
	}
	return strings.ToUpper(endpoint.Method.Value) + " " + endpoint.PathTemplate.Value
}

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
				score += 0.5
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

func (e *Engine) SetTrace(fn func(stage, message string)) { e.trace = fn }

func (e *Engine) SetMountPrefix(prefix string) { e.mountPrefix = prefix }

func (e *Engine) tracef(stage, format string, args ...any) {
	if e.trace != nil {
		e.trace(stage, fmt.Sprintf(format, args...))
	}
}
