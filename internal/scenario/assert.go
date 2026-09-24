package scenario

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

const (
	StatusPassed       = "PASSED"
	StatusFailed       = "FAILED"
	StatusNotEvaluated = "NOT_EVALUATED"
)

type AssertionResult struct {
	Status   string      `json:"status"`
	Target   string      `json:"target"`
	Op       string      `json:"op"`
	Subject  string      `json:"subject"`
	Soft     bool        `json:"soft"`
	Pointer  *string     `json:"pointer"`
	Expected any         `json:"expected"`
	Actual   any         `json:"actual"`
	Message  string      `json:"message,omitempty"`
	Diff     []DiffEntry `json:"diff,omitempty"`
}

type EvalDocs struct {
	Response          *ResponseDoc
	LatencyMs         *float64
	State             any
	StateFound        bool
	StateCount        *float64
	WebhookDeliveries []any
	HasWebhooks       bool
	FaultApplied      *bool

	RequestCount     *float64
	LastRequest      any
	LastRequestFound bool

	LastRequestHeaders any
	LastRequestQuery   any

	LastRequestExists   bool
	LastRequestCaptured bool
}

type ResponseDoc struct {
	Status  float64
	Headers map[string]string
	Body    any
}

const maxRegexInput = 8 * 1024

var (
	assertMatcherRe = regexp.MustCompile(`^\{\{\s*any:(string|number|boolean|iso8601|uuid)\s*\}\}$`)
	uuidRe          = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	isoRe           = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$`)
	catastrophicRe  = regexp.MustCompile(`(\([^)]*[+*][^)]*\))[+*]`)
)

func isCatastrophicRegex(pattern string) bool {
	if catastrophicRe.MatchString(pattern) {
		return true
	}
	if _, err := regexp.Compile(pattern); err != nil {
		return true
	}
	return false
}

func safeRegexTest(pattern, input string) (bool, error) {
	if isCatastrophicRegex(pattern) {
		return false, fmt.Errorf("unsafe or uncompilable regex: %s", pattern)
	}
	if len(input) > maxRegexInput {
		input = input[:maxRegexInput]
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return false, fmt.Errorf("unsafe or uncompilable regex: %s", pattern)
	}
	return re.MatchString(input), nil
}

func matchesMatcher(kind string, actual any) bool {
	switch kind {
	case "string":
		_, ok := actual.(string)
		return ok
	case "number":
		return isNumber(actual)
	case "boolean":
		_, ok := actual.(bool)
		return ok
	case "uuid":
		s, ok := actual.(string)
		return ok && uuidRe.MatchString(s)
	case "iso8601":
		s, ok := actual.(string)
		return ok && isoRe.MatchString(s)
	default:
		return false
	}
}

func isNumber(v any) bool {
	switch v.(type) {
	case float64, int, int64:
		return true
	}
	return false
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

func deepEqual(a, b any) bool {
	if fa, ok := toFloat(a); ok {
		fb, ok := toFloat(b)
		return ok && fa == fb
	}
	switch av := a.(type) {
	case nil:
		return b == nil
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !deepEqual(av[i], bv[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, v := range av {
			bvv, has := bv[k]
			if !has || !deepEqual(v, bvv) {
				return false
			}
		}
		return true
	}
	return false
}

func lengthOf(v any) (int, bool) {
	switch t := v.(type) {
	case string:
		return len(t), true
	case []any:
		return len(t), true
	case map[string]any:
		return len(t), true
	}
	return 0, false
}

func applyOp(op string, actual, expected any, found bool) (bool, error) {

	if s, ok := expected.(string); ok {
		if m := assertMatcherRe.FindStringSubmatch(s); m != nil && (op == "equals" || op == "matches") {
			return matchesMatcher(m[1], actual), nil
		}
	}
	switch op {
	case "exists":
		return found && actual != nil, nil
	case "absent":
		return !found || actual == nil, nil
	case "equals":
		return deepEqual(actual, expected), nil
	case "notEquals":
		return !deepEqual(actual, expected), nil
	case "contains":
		if s, ok := actual.(string); ok {
			return strings.Contains(s, jsStringify(expected)), nil
		}
		if arr, ok := actual.([]any); ok {
			for _, v := range arr {
				if deepEqual(v, expected) {
					return true, nil
				}
			}
			return false, nil
		}
		return false, nil
	case "notContains":
		ok, err := applyOp("contains", actual, expected, found)
		return !ok, err
	case "in", "isOneOf":
		arr, ok := expected.([]any)
		if !ok {
			return false, nil
		}
		for _, v := range arr {
			if deepEqual(actual, v) {
				return true, nil
			}
		}
		return false, nil
	case "matches":
		s, ok := actual.(string)
		if !ok {
			return false, nil
		}
		return safeRegexTest(jsStringify(expected), s)
	case "gt", "gte", "lt", "lte":
		fa, aok := toFloat(actual)
		fe, eok := toFloat(expected)
		if !aok || !eok {
			return false, nil
		}
		switch op {
		case "gt":
			return fa > fe, nil
		case "gte":
			return fa >= fe, nil
		case "lt":
			return fa < fe, nil
		default:
			return fa <= fe, nil
		}
	case "countEquals", "lengthEquals":
		l, ok := lengthOf(actual)
		if !ok {
			return false, nil
		}
		fe, eok := toFloat(expected)
		return eok && float64(l) == fe, nil
	case "matchesSchema":
		return false, fmt.Errorf("matchesSchema is not evaluated in this engine version")
	default:
		return false, fmt.Errorf("unknown operator '%s'", op)
	}
}

func jsStringify(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return "null"
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		return jsNumberString(t)
	default:
		raw, _ := json.Marshal(t)
		return string(raw)
	}
}

func resolveActual(a *Assertion, docs *EvalDocs) (found bool, value any, pointer *string) {
	ptr := func(s string) *string { return &s }
	switch a.Target {
	case "response.status":
		if docs.Response == nil {
			return false, nil, ptr("/status")
		}
		return true, docs.Response.Status, ptr("/status")
	case "response.latencyMs":
		if docs.LatencyMs == nil {
			return false, nil, ptr("/latencyMs")
		}
		return true, *docs.LatencyMs, ptr("/latencyMs")
	case "response.headers":
		key := ""
		if a.Key != nil {
			key = strings.ToLower(*a.Key)
		}
		var headers map[string]string
		if docs.Response != nil {
			headers = docs.Response.Headers
		}
		v, has := headers[key]
		if docs.Response == nil || !has {
			return false, nil, ptr("/headers/" + key)
		}
		return true, v, ptr("/headers/" + key)
	case "response.body":
		if docs.Response == nil {
			return resolveBody(nil, false, a.Path)
		}
		return resolveBody(docs.Response.Body, true, a.Path)
	case "state.resource":
		return resolveBody(docs.State, docs.StateFound, a.Path)
	case "state.resourceCount":
		if docs.StateCount == nil {
			return false, nil, ptr("/count")
		}
		return true, *docs.StateCount, ptr("/count")
	case "webhook.count":
		if !docs.HasWebhooks {
			return false, float64(0), ptr("/webhookCount")
		}
		return true, float64(len(docs.WebhookDeliveries)), ptr("/webhookCount")
	case "webhook.delivery":
		if len(docs.WebhookDeliveries) == 0 {
			return false, nil, ptr("/webhook/0")
		}

		return resolveBody(docs.WebhookDeliveries[0], true, a.Path)
	case "sandbox.requestCount":
		if docs.RequestCount == nil {
			return false, nil, ptr("/requestCount")
		}
		return true, *docs.RequestCount, ptr("/requestCount")
	case "sandbox.request":
		return resolveBody(docs.LastRequest, docs.LastRequestFound, a.Path)
	case "sandbox.request.headers":
		return resolveBody(docs.LastRequestHeaders, docs.LastRequestExists && docs.LastRequestCaptured, a.Path)
	case "sandbox.request.query":
		return resolveBody(docs.LastRequestQuery, docs.LastRequestExists && docs.LastRequestCaptured, a.Path)
	case "execution.faultApplied":
		if docs.FaultApplied == nil {
			return false, nil, ptr("/faultApplied")
		}
		return true, *docs.FaultApplied, ptr("/faultApplied")
	default:
		return false, nil, nil
	}
}

func resolveBody(body any, present bool, path *string) (bool, any, *string) {
	root := "/"
	if !present {
		if path != nil {
			return false, nil, path
		}
		return false, nil, &root
	}
	if path == nil {
		return true, body, &root
	}
	found, value, err := getByPath(body, *path)
	if err != nil {

		return false, nil, path
	}
	return found, value, path
}

func EvaluateAssertion(a *Assertion, docs *EvalDocs, subjectsPresent []string) AssertionResult {
	base := AssertionResult{Target: a.Target, Op: a.Op, Subject: a.Subject, Soft: a.Soft, Expected: a.Expected}
	present := false
	for _, s := range subjectsPresent {
		if s == a.Subject {
			present = true
			break
		}
	}
	if !present {
		base.Status = StatusNotEvaluated
		base.Message = fmt.Sprintf("subject %s is not present in this run", a.Subject)
		return base
	}

	if a.Path != nil && (a.Target == "response.body" || a.Target == "state.resource") {
		if _, err := parseJSONPath(*a.Path); err != nil {
			base.Status = StatusNotEvaluated
			base.Message = err.Error()
			return base
		}
	}
	found, value, pointer := resolveActual(a, docs)
	base.Pointer = pointer
	base.Actual = value
	ok, err := applyOp(a.Op, value, a.Expected, found)
	if err != nil {
		base.Status = StatusNotEvaluated
		base.Message = err.Error()
		return base
	}
	if ok {
		base.Status = StatusPassed
		return base
	}
	base.Status = StatusFailed
	pathNote := ""
	if a.Path != nil {
		pathNote = " " + *a.Path
	}
	expJSON, _ := json.Marshal(a.Expected)
	actJSON, _ := json.Marshal(value)
	base.Message = fmt.Sprintf("expected %s%s %s %s, got %s", a.Target, pathNote, a.Op, string(expJSON), string(actJSON))

	if a.Op == "equals" || a.Op == "notEquals" {
		if isComposite(value) || isComposite(a.Expected) {
			base.Diff = structuralDiff(a.Expected, value)
		}
	}
	return base
}

func isComposite(v any) bool {
	switch v.(type) {
	case map[string]any, []any:
		return true
	}
	return v == nil
}

type StepAssertionSummary struct {
	Status       string            `json:"status"`
	Results      []AssertionResult `json:"results"`
	NotEvaluated int               `json:"notEvaluated"`
	FirstFailure *AssertionResult  `json:"firstFailure,omitempty"`
}

func EvaluateStepAssertions(assertions []Assertion, docs *EvalDocs, subjectsPresent []string) StepAssertionSummary {
	results := make([]AssertionResult, 0, len(assertions))
	for i := range assertions {
		results = append(results, EvaluateAssertion(&assertions[i], docs, subjectsPresent))
	}
	var hardFail *AssertionResult
	evaluated := 0
	for i := range results {
		if results[i].Status == StatusFailed && !results[i].Soft && hardFail == nil {
			hardFail = &results[i]
		}
		if results[i].Status != StatusNotEvaluated {
			evaluated++
		}
	}
	status := StatusNotEvaluated
	if hardFail != nil {
		status = StatusFailed
	} else if evaluated > 0 {
		status = StatusPassed
	}
	return StepAssertionSummary{Status: status, Results: results, NotEvaluated: len(results) - evaluated, FirstFailure: hardFail}
}
