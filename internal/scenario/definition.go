package scenario

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/pikopod/pikopod/internal/sandbox"
)

const (
	MaxSteps             = 200
	MaxAssertionsPerStep = 50
	MaxAssertionsTotal   = 1000
	MaxWaitMs            = int64(90 * 24 * 3600 * 1000)
	MaxStepKeyLength     = 80
	MaxInputs            = 50
	MaxSeedResources     = 1000
)

var validSubjects = map[string]bool{"SANDBOX": true, "CLIENT": true, "PRODUCTION": true}

var validTargets = map[string]bool{
	"response.status": true, "response.headers": true, "response.body": true,
	"response.latencyMs": true, "state.resource": true, "state.resourceCount": true,
	"webhook.delivery": true, "webhook.count": true, "execution.faultApplied": true,
	"sandbox.requestCount": true, "sandbox.request": true,
	"sandbox.request.headers": true, "sandbox.request.query": true,
}

var validOps = map[string]bool{
	"equals": true, "notEquals": true, "contains": true, "notContains": true,
	"in": true, "matches": true, "matchesSchema": true, "exists": true,
	"absent": true, "gt": true, "gte": true, "lt": true, "lte": true,
	"countEquals": true, "lengthEquals": true, "isOneOf": true,
}

var validHTTPMethods = map[string]bool{"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true}

var (
	stepKeyRe   = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	inputNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

type Assertion struct {
	Subject      string         `json:"subject"`
	Target       string         `json:"target"`
	Path         *string        `json:"path,omitempty"`
	Key          *string        `json:"key,omitempty"`
	ResourceType *string        `json:"resourceType,omitempty"`
	ResourceID   *string        `json:"resourceId,omitempty"`
	Match        map[string]any `json:"match,omitempty"`
	Op           string         `json:"op"`
	Expected     any            `json:"expected,omitempty"`
	HasExpected  bool           `json:"-"`
	SchemaRef    *string        `json:"schemaRef,omitempty"`
	Soft         bool           `json:"soft"`
}

type RequestConfig struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers,omitempty"`
	Query   map[string]string `json:"query,omitempty"`
	Body    any               `json:"body,omitempty"`
	HasBody bool              `json:"-"`
}

type WaitConfig struct {
	DurationMs int64 `json:"durationMs"`
}

type ExpectWebhookConfig struct {
	Match     map[string]any `json:"match"`
	TimeoutMs int64          `json:"timeoutMs"`
}

type InjectFaultConfig struct {
	Method      *string  `json:"method,omitempty"`
	Path        *string  `json:"path,omitempty"`
	Kind        string   `json:"kind"`
	Status      *int     `json:"status,omitempty"`
	DelayMs     *int64   `json:"delayMs,omitempty"`
	Probability *float64 `json:"probability,omitempty"`
	Target      *string  `json:"target,omitempty"`

	Wallclock bool `json:"wallclock,omitempty"`

	Times *int `json:"times,omitempty"`

	Per *string `json:"per,omitempty"`

	DelayDistribution map[string]any `json:"delayDistribution,omitempty"`
}

type VerifyRequestsConfig struct {
	Method string `json:"method,omitempty"`
	Path   string `json:"path"`
}

type VerifySequenceConfig struct {
	Requests []SequenceMatcher `json:"requests"`
}

type SequenceMatcher struct {
	Method   string            `json:"method,omitempty"`
	Path     string            `json:"path,omitempty"`
	Headers  map[string]string `json:"headers,omitempty"`
	Query    map[string]string `json:"query,omitempty"`
	MinGapMs *int64            `json:"minGapMs,omitempty"`
	MaxGapMs *int64            `json:"maxGapMs,omitempty"`
}

type EmitWebhookConfig struct {
	Event   string `json:"event"`
	Data    any    `json:"data,omitempty"`
	HasData bool   `json:"-"`
}

type ClearFaultConfig struct {
	Method *string `json:"method,omitempty"`
	Path   *string `json:"path,omitempty"`
}

type AssertStateConfig struct {
	ResourceType string `json:"resourceType"`
	ResourceID   string `json:"resourceId"`
}

type SeedResource struct {
	Type        string         `json:"type"`
	ResourceKey *string        `json:"resourceKey,omitempty"`
	Attributes  map[string]any `json:"attributes"`
}

type SeedStateConfig struct {
	Resources []SeedResource `json:"resources"`
}

type SnapshotConfig struct {
	Label string `json:"label"`
}

type NoteConfig struct {
	Text string `json:"text"`
}

type Step struct {
	Key               string            `json:"key"`
	Description       string            `json:"description,omitempty"`
	Assertions        []Assertion       `json:"assertions"`
	Capture           map[string]string `json:"capture"`
	ContinueOnFailure bool              `json:"continueOnFailure"`
	TimeoutMs         *int64            `json:"timeoutMs,omitempty"`
	Type              string            `json:"type"`
	Config            any               `json:"config"`
}

var StepClass = map[string]string{
	"REQUEST":         "driving",
	"WAIT":            "driving",
	"EXPECT_WEBHOOK":  "driving",
	"VERIFY_REQUESTS": "driving",
	"VERIFY_SEQUENCE": "driving",
	"EMIT_WEBHOOK":    "driving",
	"ASSERT_STATE":    "driving",
	"SNAPSHOT":        "driving",
	"NOTE":            "driving",
	"INJECT_FAULT":    "conditioning",
	"CLEAR_FAULT":     "conditioning",
	"SEED_STATE":      "conditioning",
}

type InputDecl struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Required   bool   `json:"required"`
	Default    any    `json:"default,omitempty"`
	HasDefault bool   `json:"-"`
}

type ScenarioDefinition struct {
	Inputs           []InputDecl    `json:"inputs"`
	Defaults         map[string]any `json:"defaults"`
	Steps            []Step         `json:"steps"`
	RequiresFidelity *string        `json:"requiresFidelity,omitempty"`
}

type ValidationError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	StepKey string `json:"stepKey,omitempty"`
	Pointer string `json:"pointer,omitempty"`
}

type ValidationResult struct {
	Valid  bool              `json:"valid"`
	Errors []ValidationError `json:"errors"`
}

type parser struct{ errs []ValidationError }

func (p *parser) fail(pointer, format string, args ...any) {
	p.errs = append(p.errs, ValidationError{Code: "SCHEMA", Message: fmt.Sprintf(format, args...), Pointer: pointer})
}

func ParseDefinition(raw any) (*ScenarioDefinition, []ValidationError) {
	p := &parser{}
	root, ok := raw.(map[string]any)
	if !ok {
		p.fail("/", "definition must be an object")
		return nil, p.errs
	}
	p.checkKeys("/", root, "inputs", "defaults", "steps", "requiresFidelity")

	def := &ScenarioDefinition{Inputs: []InputDecl{}, Defaults: map[string]any{}}

	if v, has := root["inputs"]; has {
		arr, ok := v.([]any)
		if !ok {
			p.fail("/inputs", "inputs must be an array")
		} else {
			if len(arr) > MaxInputs {
				p.fail("/inputs", "at most %d inputs", MaxInputs)
			}
			for i, item := range arr {
				def.Inputs = append(def.Inputs, p.parseInput(fmt.Sprintf("/inputs/%d", i), item))
			}
		}
	}
	if v, has := root["defaults"]; has {
		obj, ok := v.(map[string]any)
		if !ok {
			p.fail("/defaults", "defaults must be an object")
		} else {
			def.Defaults = obj
		}
	}
	if v, has := root["requiresFidelity"]; has {
		s, ok := v.(string)
		if !ok || (s != "L0" && s != "L1" && s != "L2" && s != "L3") {
			p.fail("/requiresFidelity", "requiresFidelity must be one of L0, L1, L2, L3")
		} else {
			def.RequiresFidelity = &s
		}
	}

	stepsRaw, has := root["steps"]
	if !has {
		p.fail("/steps", "steps is required")
		return nil, p.errs
	}
	arr, ok := stepsRaw.([]any)
	if !ok {
		p.fail("/steps", "steps must be an array")
		return nil, p.errs
	}
	if len(arr) < 1 {
		p.fail("/steps", "at least one step is required")
	}
	if len(arr) > MaxSteps {
		p.fail("/steps", "at most %d steps", MaxSteps)
	}
	for i, item := range arr {
		def.Steps = append(def.Steps, p.parseStep(fmt.Sprintf("/steps/%d", i), item))
	}
	if len(p.errs) > 0 {
		return nil, p.errs
	}
	return def, nil
}

func ParseDefinitionJSON(raw []byte) (*ScenarioDefinition, []ValidationError) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, []ValidationError{{Code: "SCHEMA", Message: "definition is not valid JSON: " + err.Error(), Pointer: "/"}}
	}
	return ParseDefinition(v)
}

func (p *parser) checkKeys(pointer string, obj map[string]any, allowed ...string) {
	ok := map[string]bool{}
	for _, k := range allowed {
		ok[k] = true
	}
	for k := range obj {
		if !ok[k] {
			p.fail(pointer+"/"+k, "unrecognized key %q", k)
		}
	}
}

func (p *parser) parseInput(pointer string, raw any) InputDecl {
	out := InputDecl{Required: true}
	obj, ok := raw.(map[string]any)
	if !ok {
		p.fail(pointer, "input must be an object")
		return out
	}
	p.checkKeys(pointer, obj, "name", "type", "required", "default")
	out.Name = p.str(pointer+"/name", obj, "name", true, 100)
	if out.Name != "" && !inputNameRe.MatchString(out.Name) {
		p.fail(pointer+"/name", "input name must match %s", inputNameRe.String())
	}
	typ := p.str(pointer+"/type", obj, "type", true, 20)
	if typ != "" && typ != "string" && typ != "number" && typ != "boolean" {
		p.fail(pointer+"/type", "input type must be string, number, or boolean")
	}
	out.Type = typ
	if v, has := obj["required"]; has {
		b, ok := v.(bool)
		if !ok {
			p.fail(pointer+"/required", "required must be a boolean")
		} else {
			out.Required = b
		}
	}
	if v, has := obj["default"]; has {
		out.Default = v
		out.HasDefault = true
	}
	return out
}

func (p *parser) str(pointer string, obj map[string]any, key string, required bool, maxLen int) string {
	v, has := obj[key]
	if !has {
		if required {
			p.fail(pointer, "%s is required", key)
		}
		return ""
	}
	s, ok := v.(string)
	if !ok {
		p.fail(pointer, "%s must be a string", key)
		return ""
	}
	if required && len(s) == 0 {
		p.fail(pointer, "%s must not be empty", key)
	}
	if maxLen > 0 && len(s) > maxLen {
		p.fail(pointer, "%s exceeds %d characters", key, maxLen)
	}
	return s
}

func (p *parser) optStr(pointer string, obj map[string]any, key string, maxLen int) *string {
	if _, has := obj[key]; !has {
		return nil
	}
	s := p.str(pointer, obj, key, false, maxLen)
	return &s
}

func (p *parser) intIn(pointer string, v any, min, max int64) (int64, bool) {
	f, ok := v.(float64)
	if !ok || f != float64(int64(f)) {
		p.fail(pointer, "must be an integer")
		return 0, false
	}
	n := int64(f)
	if n < min || n > max {
		p.fail(pointer, "must be between %d and %d", min, max)
		return 0, false
	}
	return n, true
}

func (p *parser) optGapMs(pointer string, obj map[string]any, key string) *int64 {
	v, has := obj[key]
	if !has {
		return nil
	}
	f, ok := v.(float64)
	if !ok || f < 0 || f != float64(int64(f)) {
		p.fail(pointer, "%s must be a non-negative whole number of milliseconds", key)
		return nil
	}
	ms := int64(f)
	return &ms
}

func (p *parser) stringMap(pointer string, v any) map[string]string {
	obj, ok := v.(map[string]any)
	if !ok {
		p.fail(pointer, "must be an object of strings")
		return nil
	}
	out := map[string]string{}
	for k, val := range obj {
		s, ok := val.(string)
		if !ok {
			p.fail(pointer+"/"+k, "must be a string")
			continue
		}
		out[k] = s
	}
	return out
}

func (p *parser) parseStep(pointer string, raw any) Step {
	out := Step{Assertions: []Assertion{}, Capture: map[string]string{}}
	obj, ok := raw.(map[string]any)
	if !ok {
		p.fail(pointer, "step must be an object")
		return out
	}
	p.checkKeys(pointer, obj, "key", "description", "assertions", "capture", "continueOnFailure", "timeoutMs", "type", "config")

	out.Key = p.str(pointer+"/key", obj, "key", true, MaxStepKeyLength)
	if out.Key != "" && !stepKeyRe.MatchString(out.Key) {
		p.fail(pointer+"/key", "step key must match %s", stepKeyRe.String())
	}
	out.Description = p.str(pointer+"/description", obj, "description", false, 1000)

	if v, has := obj["assertions"]; has {
		arr, ok := v.([]any)
		if !ok {
			p.fail(pointer+"/assertions", "assertions must be an array")
		} else {
			if len(arr) > MaxAssertionsPerStep {
				p.fail(pointer+"/assertions", "at most %d assertions per step", MaxAssertionsPerStep)
			}
			for i, a := range arr {
				out.Assertions = append(out.Assertions, p.parseAssertion(fmt.Sprintf("%s/assertions/%d", pointer, i), a))
			}
		}
	}
	if v, has := obj["capture"]; has {
		m := p.stringMap(pointer+"/capture", v)
		for k, expr := range m {
			if len(expr) > 300 {
				p.fail(pointer+"/capture/"+k, "capture expression exceeds 300 characters")
			}
		}
		if m != nil {
			out.Capture = m
		}
	}
	if v, has := obj["continueOnFailure"]; has {
		b, ok := v.(bool)
		if !ok {
			p.fail(pointer+"/continueOnFailure", "continueOnFailure must be a boolean")
		} else {
			out.ContinueOnFailure = b
		}
	}
	if v, has := obj["timeoutMs"]; has {
		if n, ok := p.intIn(pointer+"/timeoutMs", v, 0, MaxWaitMs); ok {
			out.TimeoutMs = &n
		}
	}

	out.Type = p.str(pointer+"/type", obj, "type", true, 40)
	cfgRaw, hasCfg := obj["config"]
	if !hasCfg {
		p.fail(pointer+"/config", "config is required")
		return out
	}
	cfg, ok := cfgRaw.(map[string]any)
	if !ok {
		p.fail(pointer+"/config", "config must be an object")
		return out
	}
	out.Config = p.parseConfig(pointer+"/config", out.Type, cfg)
	return out
}

func (p *parser) parseConfig(pointer, stepType string, cfg map[string]any) any {
	switch stepType {
	case "REQUEST":
		p.checkKeys(pointer, cfg, "method", "path", "headers", "query", "body")
		out := &RequestConfig{}
		out.Method = p.str(pointer+"/method", cfg, "method", true, 10)
		if out.Method != "" && !validHTTPMethods[out.Method] {
			p.fail(pointer+"/method", "method must be one of GET, POST, PUT, PATCH, DELETE")
		}
		out.Path = p.str(pointer+"/path", cfg, "path", true, 2000)
		if v, has := cfg["headers"]; has {
			out.Headers = p.stringMap(pointer+"/headers", v)
		}
		if v, has := cfg["query"]; has {
			out.Query = p.stringMap(pointer+"/query", v)
		}
		if v, has := cfg["body"]; has {
			out.Body = v
			out.HasBody = true
		}
		return out
	case "WAIT":
		p.checkKeys(pointer, cfg, "durationMs")
		out := &WaitConfig{}
		if v, has := cfg["durationMs"]; has {
			out.DurationMs, _ = p.intIn(pointer+"/durationMs", v, 0, MaxWaitMs)
		} else {
			p.fail(pointer+"/durationMs", "durationMs is required")
		}
		return out
	case "EXPECT_WEBHOOK":
		p.checkKeys(pointer, cfg, "match", "timeoutMs")
		out := &ExpectWebhookConfig{}
		if v, has := cfg["match"]; has {
			m, ok := v.(map[string]any)
			if !ok {
				p.fail(pointer+"/match", "match must be an object")
			} else {
				out.Match = m
			}
		} else {
			p.fail(pointer+"/match", "match is required")
		}
		if v, has := cfg["timeoutMs"]; has {
			out.TimeoutMs, _ = p.intIn(pointer+"/timeoutMs", v, 0, MaxWaitMs)
		} else {
			p.fail(pointer+"/timeoutMs", "timeoutMs is required")
		}
		return out
	case "INJECT_FAULT":
		p.checkKeys(pointer, cfg, "method", "path", "kind", "status", "delayMs", "probability", "target", "wallclock", "times", "per", "delayDistribution")
		out := &InjectFaultConfig{}
		if _, has := cfg["method"]; has {
			m := p.str(pointer+"/method", cfg, "method", false, 10)
			if !validHTTPMethods[m] {
				p.fail(pointer+"/method", "method must be one of GET, POST, PUT, PATCH, DELETE")
			}
			out.Method = &m
		}
		out.Path = p.optStr(pointer+"/path", cfg, "path", 2000)
		out.Kind = p.str(pointer+"/kind", cfg, "kind", true, 40)
		if out.Kind != "" && !sandbox.ValidFaultKind(out.Kind) {
			p.fail(pointer+"/kind", "kind must be one of %s", strings.Join(sandbox.FaultKinds(), ", "))
		}
		if v, has := cfg["status"]; has {
			if n, ok := p.intIn(pointer+"/status", v, 100, 599); ok {
				s := int(n)
				out.Status = &s
			}
		}
		if v, has := cfg["delayMs"]; has {
			if n, ok := p.intIn(pointer+"/delayMs", v, 0, MaxWaitMs); ok {
				out.DelayMs = &n
			}
		}
		if v, has := cfg["probability"]; has {
			f, ok := v.(float64)
			if !ok || f < 0 || f > 1 {
				p.fail(pointer+"/probability", "probability must be a number between 0 and 1")
			} else {
				out.Probability = &f
			}
		}
		out.Target = p.optStr(pointer+"/target", cfg, "target", 400)
		if v, has := cfg["wallclock"]; has {
			b, ok := v.(bool)
			if !ok {
				p.fail(pointer+"/wallclock", "wallclock must be a boolean")
			}
			out.Wallclock = b
		}
		if v, has := cfg["times"]; has {
			if n, ok := p.intIn(pointer+"/times", v, 1, 100000); ok {
				t := int(n)
				out.Times = &t
			}
		}
		if v, has := cfg["per"]; has {
			per, ok := v.(string)
			if !ok || (per != "global" && per != "idempotency-key" && per != "resource") {
				p.fail(pointer+"/per", "per must be one of global, idempotency-key, resource")
			} else {
				out.Per = &per
			}
		}
		if v, has := cfg["delayDistribution"]; has {
			dd, ok := v.(map[string]any)
			if !ok {
				p.fail(pointer+"/delayDistribution", "delayDistribution must be an object")
			} else {
				p.checkKeys(pointer+"/delayDistribution", dd, "type", "medianMs", "sigma", "maxMs", "lowerMs", "upperMs", "baseMs", "pct")
				typ, _ := dd["type"].(string)
				if typ != "lognormal" && typ != "uniform" && typ != "band" {
					p.fail(pointer+"/delayDistribution/type", "type must be one of lognormal, uniform, band")
				}
				out.DelayDistribution = dd
			}
		}
		return out
	case "CLEAR_FAULT":
		p.checkKeys(pointer, cfg, "method", "path")
		out := &ClearFaultConfig{}
		if _, has := cfg["method"]; has {
			m := p.str(pointer+"/method", cfg, "method", false, 10)
			if !validHTTPMethods[m] {
				p.fail(pointer+"/method", "method must be one of GET, POST, PUT, PATCH, DELETE")
			}
			out.Method = &m
		}
		out.Path = p.optStr(pointer+"/path", cfg, "path", 2000)
		return out
	case "VERIFY_REQUESTS":
		p.checkKeys(pointer, cfg, "method", "path")
		out := &VerifyRequestsConfig{}
		if _, has := cfg["method"]; has {
			m := p.str(pointer+"/method", cfg, "method", false, 10)
			if !validHTTPMethods[m] {
				p.fail(pointer+"/method", "method must be one of GET, POST, PUT, PATCH, DELETE")
			}
			out.Method = m
		}
		out.Path = p.str(pointer+"/path", cfg, "path", true, 2000)
		return out
	case "VERIFY_SEQUENCE":
		p.checkKeys(pointer, cfg, "requests")
		out := &VerifySequenceConfig{}
		raw, ok := cfg["requests"].([]any)
		if !ok || len(raw) == 0 {
			p.fail(pointer+"/requests", "requests must be a non-empty array of matchers")
			return out
		}
		if len(raw) > MaxSteps {
			p.fail(pointer+"/requests", "at most %d matchers", MaxSteps)
			return out
		}
		for i, item := range raw {
			ptr := fmt.Sprintf("%s/requests/%d", pointer, i)
			obj, ok := item.(map[string]any)
			if !ok {
				p.fail(ptr, "matcher must be an object")
				continue
			}
			p.checkKeys(ptr, obj, "method", "path", "headers", "query", "minGapMs", "maxGapMs")
			m := SequenceMatcher{}
			if _, has := obj["method"]; has {
				mth := p.str(ptr+"/method", obj, "method", false, 10)
				if !validHTTPMethods[mth] {
					p.fail(ptr+"/method", "method must be one of GET, POST, PUT, PATCH, DELETE")
				}
				m.Method = mth
			}
			if v := p.optStr(ptr+"/path", obj, "path", 2000); v != nil {
				m.Path = *v
			}
			if v, has := obj["headers"]; has {
				m.Headers = p.stringMap(ptr+"/headers", v)
			}
			if v, has := obj["query"]; has {
				m.Query = p.stringMap(ptr+"/query", v)
			}
			m.MinGapMs = p.optGapMs(ptr+"/minGapMs", obj, "minGapMs")
			m.MaxGapMs = p.optGapMs(ptr+"/maxGapMs", obj, "maxGapMs")
			if m.MinGapMs != nil && m.MaxGapMs != nil && *m.MinGapMs > *m.MaxGapMs {
				p.fail(ptr, "minGapMs must not exceed maxGapMs")
			}
			out.Requests = append(out.Requests, m)
		}
		return out
	case "EMIT_WEBHOOK":
		p.checkKeys(pointer, cfg, "event", "data")
		out := &EmitWebhookConfig{Event: p.str(pointer+"/event", cfg, "event", true, 200)}
		if v, has := cfg["data"]; has {
			if _, ok := v.(map[string]any); !ok {
				p.fail(pointer+"/data", "data must be a JSON object")
			}
			out.Data, out.HasData = v, true
		}
		return out
	case "ASSERT_STATE":
		p.checkKeys(pointer, cfg, "resourceType", "resourceId")
		return &AssertStateConfig{
			ResourceType: p.str(pointer+"/resourceType", cfg, "resourceType", true, 400),
			ResourceID:   p.str(pointer+"/resourceId", cfg, "resourceId", true, 400),
		}
	case "SEED_STATE":
		p.checkKeys(pointer, cfg, "resources")
		out := &SeedStateConfig{}
		v, has := cfg["resources"]
		if !has {
			p.fail(pointer+"/resources", "resources is required")
			return out
		}
		arr, ok := v.([]any)
		if !ok {
			p.fail(pointer+"/resources", "resources must be an array")
			return out
		}
		if len(arr) > MaxSeedResources {
			p.fail(pointer+"/resources", "at most %d resources", MaxSeedResources)
		}
		for i, item := range arr {
			rp := fmt.Sprintf("%s/resources/%d", pointer, i)
			obj, ok := item.(map[string]any)
			if !ok {
				p.fail(rp, "resource must be an object")
				continue
			}
			p.checkKeys(rp, obj, "type", "resourceKey", "attributes")
			r := SeedResource{Type: p.str(rp+"/type", obj, "type", true, 400)}
			r.ResourceKey = p.optStr(rp+"/resourceKey", obj, "resourceKey", 400)

			if r.ResourceKey != nil && *r.ResourceKey == "" {
				p.fail(rp+"/resourceKey", "resourceKey must not be empty (omit it to autogenerate)")
			}
			if av, has := obj["attributes"]; has {
				attrs, ok := av.(map[string]any)
				if !ok {
					p.fail(rp+"/attributes", "attributes must be an object")
				} else {
					r.Attributes = attrs
				}
			} else {
				p.fail(rp+"/attributes", "attributes is required")
			}
			out.Resources = append(out.Resources, r)
		}
		return out
	case "SNAPSHOT":
		p.checkKeys(pointer, cfg, "label")
		return &SnapshotConfig{Label: p.str(pointer+"/label", cfg, "label", true, 200)}
	case "NOTE":
		p.checkKeys(pointer, cfg, "text")
		return &NoteConfig{Text: p.str(pointer+"/text", cfg, "text", true, 4000)}
	default:
		p.fail(pointer, "unknown step type %q", stepType)
		return nil
	}
}

func (p *parser) parseAssertion(pointer string, raw any) Assertion {
	out := Assertion{Subject: "SANDBOX"}
	obj, ok := raw.(map[string]any)
	if !ok {
		p.fail(pointer, "assertion must be an object")
		return out
	}
	p.checkKeys(pointer, obj, "subject", "target", "path", "key", "resourceType", "resourceId", "match", "op", "expected", "schemaRef", "soft")
	if v, has := obj["subject"]; has {
		s, ok := v.(string)
		if !ok || !validSubjects[s] {
			p.fail(pointer+"/subject", "subject must be one of SANDBOX, CLIENT, PRODUCTION")
		} else {
			out.Subject = s
		}
	}
	out.Target = p.str(pointer+"/target", obj, "target", true, 60)
	if out.Target != "" && !validTargets[out.Target] {
		p.fail(pointer+"/target", "unknown assertion target %q", out.Target)
	}
	out.Path = p.optStr(pointer+"/path", obj, "path", 500)
	out.Key = p.optStr(pointer+"/key", obj, "key", 200)
	out.ResourceType = p.optStr(pointer+"/resourceType", obj, "resourceType", 400)
	out.ResourceID = p.optStr(pointer+"/resourceId", obj, "resourceId", 400)
	if v, has := obj["match"]; has {
		m, ok := v.(map[string]any)
		if !ok {
			p.fail(pointer+"/match", "match must be an object")
		} else {
			out.Match = m
		}
	}
	out.Op = p.str(pointer+"/op", obj, "op", true, 40)
	if out.Op != "" && !validOps[out.Op] {
		p.fail(pointer+"/op", "unknown assertion op %q", out.Op)
	}
	if v, has := obj["expected"]; has {
		out.Expected = v
		out.HasExpected = true
	}
	out.SchemaRef = p.optStr(pointer+"/schemaRef", obj, "schemaRef", 500)
	if v, has := obj["soft"]; has {
		b, ok := v.(bool)
		if !ok {
			p.fail(pointer+"/soft", "soft must be a boolean")
		} else {
			out.Soft = b
		}
	}
	return out
}
