// Match a scenario REQUEST against the pinned ApiDefinition's endpoints.
// Deterministic and pure.
package scenario

import (
	"regexp"
	"sort"
	"strings"

	"github.com/pikopod/pikopod/internal/ir"
)

var (
	templateParamRe = regexp.MustCompile(`^\{.+\}$`)
	scenarioVarRe   = regexp.MustCompile(`\{\{.+\}\}`)
	absoluteURLRe   = regexp.MustCompile(`(?i)^[a-z][a-z0-9+.-]*://`)
	varExprRe       = regexp.MustCompile(`\{\{\s*([^}]+?)\s*\}\}`)
	generatorRe     = regexp.MustCompile(`^(seq\(\)|randomString\(\d+\)|now\(\)|any:(string|number|boolean|iso8601|uuid))$`)
)

func pathSegments(path string) []string {
	noQuery := path
	if i := strings.Index(noQuery, "?"); i != -1 {
		noQuery = noQuery[:i]
	}
	var out []string
	for _, s := range strings.Split(noQuery, "/") {
		if len(s) > 0 {
			out = append(out, s)
		}
	}
	return out
}

func segmentMatches(scenarioSeg, endpointSeg string) bool {
	if templateParamRe.MatchString(endpointSeg) {
		return true // endpoint param accepts any value
	}
	if scenarioVarRe.MatchString(scenarioSeg) {
		return false // a var can only fill a param slot
	}
	return scenarioSeg == endpointSeg
}

// MatchEndpoint returns the endpoint serving method+path, or nil. Most
// specific (fewest template params) first; stable code-point tiebreak.
func MatchEndpoint(endpoints []ir.Endpoint, method, path string) *ir.Endpoint {
	target := pathSegments(path)
	upper := strings.ToUpper(method)
	var candidates []*ir.Endpoint
	for i := range endpoints {
		e := &endpoints[i]
		if strings.ToUpper(e.Method.Value) != upper {
			continue
		}
		tpl := pathSegments(e.PathTemplate.Value)
		if len(tpl) != len(target) {
			continue
		}
		all := true
		for j, seg := range target {
			if !segmentMatches(seg, tpl[j]) {
				all = false
				break
			}
		}
		if all {
			candidates = append(candidates, e)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	paramCount := func(e *ir.Endpoint) int {
		n := 0
		for _, s := range pathSegments(e.PathTemplate.Value) {
			if templateParamRe.MatchString(s) {
				n++
			}
		}
		return n
	}
	sort.SliceStable(candidates, func(a, b int) bool {
		pa, pb := paramCount(candidates[a]), paramCount(candidates[b])
		if pa != pb {
			return pa < pb
		}
		return candidates[a].PathTemplate.Value < candidates[b].PathTemplate.Value
	})
	return candidates[0]
}

// ResourceTypeOf is the collection-path resource type for an endpoint (drop a
// trailing param).
func ResourceTypeOf(endpoint *ir.Endpoint) string {
	segs := pathSegments(endpoint.PathTemplate.Value)
	if len(segs) > 0 && templateParamRe.MatchString(segs[len(segs)-1]) {
		segs = segs[:len(segs)-1]
	}
	return "/" + strings.Join(segs, "/")
}

func IsAbsoluteURL(path string) bool {
	return absoluteURLRe.MatchString(path) || strings.HasPrefix(path, "//")
}

// ExtractVarExprs extracts `{{...}}` expressions from a string.
func ExtractVarExprs(text string) []string {
	var out []string
	for _, m := range varExprRe.FindAllStringSubmatch(text, -1) {
		out = append(out, strings.TrimSpace(m[1]))
	}
	return out
}

// IsGeneratorExpr: a generator / matcher expression resolves itself.
func IsGeneratorExpr(expr string) bool {
	return generatorRe.MatchString(expr)
}
