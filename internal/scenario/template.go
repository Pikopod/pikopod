// The bounded template engine (`{{name}}`, `{{seq()}}`, `{{randomString(n)}}`,
// `{{now()}}`) — substitution only, single-pass; unresolved variables error.
package scenario

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pikopod/pikopod/internal/sandbox"
)

// GeneratorContext makes all output a pure function of the run seed and the
// virtual clock at run start.
type GeneratorContext struct {
	seed           string
	virtualClockMs int64
	seqCounter     int
	randCounter    int
}

func NewGeneratorContext(seed string, virtualClockMs int64) *GeneratorContext {
	return &GeneratorContext{seed: seed, virtualClockMs: virtualClockMs}
}

// Seq is a monotonic, run-scoped sequence number.
func (g *GeneratorContext) Seq() string {
	g.seqCounter++
	return strconv.Itoa(g.seqCounter)
}

// RandomString is a seeded random alphanumeric string; each call advances the
// stream.
func (g *GeneratorContext) RandomString(length int) string {
	g.randCounter++
	prng := sandbox.NewPrng(fmt.Sprintf("%s:rnd:%d", g.seed, g.randCounter))
	if length < 0 {
		length = 0
	}
	if length > 4096 {
		length = 4096
	}
	return prng.Token(length)
}

// Now is the virtual clock as an ISO-8601 instant — NOT the wall clock.
func (g *GeneratorContext) Now() string {
	return time.UnixMilli(g.virtualClockMs).UTC().Format("2006-01-02T15:04:05.000Z")
}

// TemplateContext resolves template variables. Precedence:
// inputs > captures > defaults.
type TemplateContext struct {
	Inputs     map[string]any
	Captures   map[string]any
	Defaults   map[string]any
	Generators *GeneratorContext
}

const maxTemplateOutput = 64 * 1024

var (
	tplVarRe      = regexp.MustCompile(`\{\{\s*([^}]+?)\s*\}\}`)
	tplBareNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	tplMatcherRe  = regexp.MustCompile(`^any:(string|number|boolean|iso8601|uuid)$`)
	tplRandomRe   = regexp.MustCompile(`^randomString\((\d+)\)$`)
	tplWholeVarRe = regexp.MustCompile(`^\{\{\s*([^}]+?)\s*\}\}$`)
	tplReservedRe = regexp.MustCompile(`(?i)^(secret|credential|env|process)\b`)
)

func resolveExpr(expr string, ctx *TemplateContext) (render bool, value string, err error) {
	// Matchers are for assertion evaluation; leave them verbatim in request text.
	if tplMatcherRe.MatchString(expr) {
		return false, "", nil
	}
	if expr == "seq()" {
		return true, ctx.Generators.Seq(), nil
	}
	if expr == "now()" {
		return true, ctx.Generators.Now(), nil
	}
	if m := tplRandomRe.FindStringSubmatch(expr); m != nil {
		n, _ := strconv.Atoi(m[1])
		return true, ctx.Generators.RandomString(n), nil
	}
	if !tplBareNameRe.MatchString(expr) {
		return false, "", fmt.Errorf("'%s' is not a plain variable or a permitted generator — no expressions or property traversal are allowed", expr)
	}
	if tplReservedRe.MatchString(expr) {
		return false, "", fmt.Errorf("variable '%s' is reserved; credentials are injected by the engine, never templated", expr)
	}
	for _, source := range []map[string]any{ctx.Inputs, ctx.Captures, ctx.Defaults} {
		if v, has := source[expr]; has {
			s, err := stringifyValue(v)
			if err != nil {
				return false, "", err
			}
			return true, s, nil
		}
	}
	return false, "", fmt.Errorf("unresolved variable '%s'", expr)
}

func stringifyValue(value any) (string, error) {
	if value == nil {
		return "", fmt.Errorf("variable resolved to null/undefined")
	}
	switch v := value.(type) {
	case string:
		return v, nil
	case bool:
		if v {
			return "true", nil
		}
		return "false", nil
	case float64:
		return jsNumberString(v), nil
	case int:
		return strconv.Itoa(v), nil
	case int64:
		return strconv.FormatInt(v, 10), nil
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return string(raw), nil
	}
}

// jsNumberString formats a float the way JS String(n) does for the common
// integer case.
func jsNumberString(f float64) string {
	if f == float64(int64(f)) {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// interpolate renders a single string. Errors on any unresolved or illegal
// expression. Single-pass: a resolved value is never re-scanned.
func interpolate(text string, ctx *TemplateContext) (string, error) {
	var out strings.Builder
	last := 0
	for _, m := range tplVarRe.FindAllStringSubmatchIndex(text, -1) {
		expr := strings.TrimSpace(text[m[2]:m[3]])
		render, value, err := resolveExpr(expr, ctx)
		if err != nil {
			return "", err
		}
		out.WriteString(text[last:m[0]])
		if render {
			out.WriteString(value)
		} else {
			out.WriteString(text[m[0]:m[1]]) // matcher passthrough keeps `{{any:*}}`
		}
		last = m[1]
		if out.Len() > maxTemplateOutput {
			return "", fmt.Errorf("interpolated output exceeds the maximum size")
		}
	}
	out.WriteString(text[last:])
	if out.Len() > maxTemplateOutput {
		return "", fmt.Errorf("interpolated output exceeds the maximum size")
	}
	return out.String(), nil
}

// interpolateDeep interpolates every string within a JSON-shaped value. A
// string that is exactly one variable resolves to the variable's TYPED value.
func interpolateDeep(value any, ctx *TemplateContext) (any, error) {
	switch v := value.(type) {
	case string:
		if m := tplWholeVarRe.FindStringSubmatch(v); m != nil {
			expr := strings.TrimSpace(m[1])
			if tplMatcherRe.MatchString(expr) {
				return v, nil // matcher passthrough
			}
			if expr == "seq()" || expr == "now()" || tplRandomRe.MatchString(expr) || !tplBareNameRe.MatchString(expr) {
				return interpolate(v, ctx)
			}
			if tplReservedRe.MatchString(expr) {
				return nil, fmt.Errorf("variable '%s' is reserved", expr)
			}
			for _, source := range []map[string]any{ctx.Inputs, ctx.Captures, ctx.Defaults} {
				if val, has := source[expr]; has {
					return val, nil
				}
			}
			return nil, fmt.Errorf("unresolved variable '%s'", expr)
		}
		return interpolate(v, ctx)
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			r, err := interpolateDeep(item, ctx)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			r, err := interpolateDeep(item, ctx)
			if err != nil {
				return nil, err
			}
			out[k] = r
		}
		return out, nil
	default:
		return value, nil
	}
}
