// from-recordings generates a scenario pack from a recorded traffic window.
// Value chaining is deliberately conservative: identifier shapes only.
package bridge

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/pikopod/pikopod/internal/proxy"
)

// FromRecordingsOptions tunes pack generation.
type FromRecordingsOptions struct {
	// Last caps the number of REQUEST steps (default 20, newest window).
	Last int
	// Name overrides the pack slug (default traffic-<upstream>).
	Name string
}

const defaultFromRecordingsSteps = 20

// BuildFromRecordings renders a pack from a recorded window. Records must be
// in time order (readRecordings' order).
func BuildFromRecordings(upstream string, records []*proxy.Record, opts FromRecordingsOptions) (string, map[string]any, error) {
	limit := opts.Last
	if limit <= 0 {
		limit = defaultFromRecordingsSteps
	}
	if len(records) > limit {
		records = records[len(records)-limit:]
	}
	if len(records) == 0 {
		return "", nil, fmt.Errorf("no recordings to build from")
	}

	name := opts.Name
	if name == "" {
		name = "traffic-" + upstream
	}

	// producers: identifier-shaped value → variable binding on the step that
	// FIRST produced it, captured by concrete JSONPath, not value coincidence.
	type producer struct {
		varName string
		stepIdx int
		path    string // JSONPath into the producer's response body
		used    bool
	}
	producers := map[string]*producer{}
	varSeq := 0

	var steps []map[string]any
	chains := 0
	for i, rec := range records {
		stepKey := fmt.Sprintf("t%02d-%s-%s", i+1, strings.ToLower(rec.Method), slugPath(rec.Path))

		// Consume: rewrite identifier-shaped values this window already
		// produced. Path segments and request-body strings only.
		pathOut := rec.Path
		if q := strings.IndexByte(pathOut, '?'); q >= 0 {
			pathOut = pathOut[:q] // recorded query strings are per-call noise
		}
		segs := strings.Split(pathOut, "/")
		for j, seg := range segs {
			if p, ok := producers[seg]; ok && chainable(seg) {
				segs[j] = "{{" + p.varName + "}}"
				p.used = true
				chains++
			}
		}
		pathOut = strings.Join(segs, "/")

		var bodyOut any
		if rec.ReqKind == "json" && rec.ReqBody != nil {
			bodyOut = rewriteBody(rec.ReqBody, func(s string) (string, bool) {
				if p, ok := producers[s]; ok && chainable(s) {
					p.used = true
					chains++
					return "{{" + p.varName + "}}", true
				}
				return "", false
			})
		}

		step := map[string]any{
			"key": stepKey, "type": "REQUEST",
			"config": map[string]any{"method": rec.Method, "path": pathOut},
			"assertions": []any{map[string]any{
				"target": "response.status", "op": "equals", "expected": rec.Status,
			}},
		}
		if bodyOut != nil {
			step["config"].(map[string]any)["body"] = bodyOut
		}
		steps = append(steps, step)

		// Produce: register identifier-shaped response values for LATER
		// requests (never this one — a chain needs temporal order).
		if rec.RespKind == "json" && rec.RespBody != nil {
			walkProducerValues(rec.RespBody, "$", func(jsonPath, value string) {
				if !chainable(value) {
					return
				}
				if _, exists := producers[value]; exists {
					return // first producer wins — deterministic
				}
				varSeq++
				producers[value] = &producer{varName: fmt.Sprintf("chain_%d", varSeq), stepIdx: i, path: jsonPath}
			})
		}
	}

	// Attach captures to producer steps (only for values actually consumed).
	for _, p := range producers {
		if !p.used {
			continue
		}
		step := steps[p.stepIdx]
		capture, _ := step["capture"].(map[string]any)
		if capture == nil {
			capture = map[string]any{}
			step["capture"] = capture
		}
		capture[p.varName] = "response.body" + p.path
	}

	pack := map[string]any{
		"name":     name,
		"provider": upstream,
		"description": fmt.Sprintf(
			"Generated from %d recorded exchange(s) for %s: requests replay in recorded order with status assertions; %d producer→consumer value chain(s) captured.",
			len(steps), upstream, chains),
		"definition": map[string]any{"steps": toAnySlice(steps)},
	}
	return name, pack, nil
}

// chainable gates value chaining to identifier shapes; short literals, enum
// words, amounts and booleans never chain.
func chainable(v string) bool {
	if len(v) < 8 || len(v) > 200 || strings.ContainsAny(v, " \t\n{}") {
		return false
	}
	switch {
	case uuidRe.MatchString(v):
		return true
	case prefixedTokenRe.MatchString(v):
		return true
	case hexRe.MatchString(v) && len(v) >= 16:
		return true
	case digitsRe.MatchString(v) && len(v) >= 10:
		return true
	case mixedRe.MatchString(v) && len(v) >= 16 && hasLetterAndDigit(v):
		return true
	}
	return false
}

var (
	uuidRe          = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	prefixedTokenRe = regexp.MustCompile(`^[a-z]{2,10}_[A-Za-z0-9_-]{6,}$`)
	hexRe           = regexp.MustCompile(`^[0-9a-fA-F]+$`)
	digitsRe        = regexp.MustCompile(`^[0-9]+$`)
	mixedRe         = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

func hasLetterAndDigit(v string) bool {
	letter, digit := false, false
	for _, r := range v {
		switch {
		case r >= '0' && r <= '9':
			digit = true
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
			letter = true
		}
	}
	return letter && digit
}

// walkProducerValues visits string scalars under OBJECT keys only (an
// index-addressed capture is order-fragile), sorted so first-producer-wins.
func walkProducerValues(node any, jsonPath string, visit func(path, value string)) {
	obj, ok := node.(map[string]any)
	if !ok {
		return
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		child := jsonPath + "." + k
		switch val := obj[k].(type) {
		case string:
			visit(child, val)
		case map[string]any:
			walkProducerValues(val, child, visit)
		}
	}
}

// rewriteBody deep-copies a request body, replacing chained string values.
func rewriteBody(node any, replace func(string) (string, bool)) any {
	switch n := node.(type) {
	case map[string]any:
		out := make(map[string]any, len(n))
		for k, v := range n {
			out[k] = rewriteBody(v, replace)
		}
		return out
	case []any:
		out := make([]any, len(n))
		for i, v := range n {
			out[i] = rewriteBody(v, replace)
		}
		return out
	case string:
		if repl, ok := replace(n); ok {
			return repl
		}
		return n
	default:
		return n
	}
}

func slugPath(p string) string {
	if q := strings.IndexByte(p, '?'); q >= 0 {
		p = p[:q]
	}
	p = strings.Trim(p, "/")
	p = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r + 32
		default:
			return '-'
		}
	}, p)
	if len(p) > 40 {
		p = p[:40]
	}
	if p == "" {
		p = "root"
	}
	return p
}

func toAnySlice(steps []map[string]any) []any {
	out := make([]any, len(steps))
	for i, s := range steps {
		out[i] = s
	}
	return out
}
