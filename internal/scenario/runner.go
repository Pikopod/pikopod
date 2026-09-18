// The in-process scenario runner. One run drives one sandbox Engine from one
// goroutine; the virtual clock does all waiting (a 60s WAIT completes in ms).
package scenario

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"strings"

	"github.com/pikopod/pikopod/internal/errfmt"
	"github.com/pikopod/pikopod/internal/sandbox"
)

// EngineVersion participates in the result hash: bump it when runner
// semantics change so old hashes stop comparing equal.
const EngineVersion = "pikopod-scenario/1"

// Step statuses beyond the assertion trio (assert.go).
const (
	StatusErrored = "ERRORED"
	StatusSkipped = "SKIPPED"
)

// Run verdicts. ERRORED is its own verdict, never a kind of FAILED.
const (
	RunPassed             = "PASSED"
	RunFailed             = "FAILED"
	RunConditionGenerated = "CONDITION_GENERATED"
	RunErrored            = "ERRORED"
)

// subjectsPresent: SANDBOX always; CLIENT never exists locally.
var subjectsPresent = []string{"SANDBOX"}

// StepResult records one executed (or skipped) step.
type StepResult struct {
	Key            string         `json:"key"`
	Type           string         `json:"type"`
	Status         string         `json:"status"`
	Summary        string         `json:"summary,omitempty"`
	VirtualStartMs int64          `json:"virtualStartMs"`
	VirtualEndMs   int64          `json:"virtualEndMs"`
	NotEvaluated   int            `json:"notEvaluated,omitempty"`
	Detail         map[string]any `json:"detail,omitempty"`
}

// RunResult is the run's terminal verdict.
type RunResult struct {
	Status       string       `json:"status"`
	Summary      string       `json:"summary"`
	NotEvaluated int          `json:"notEvaluated"`
	ResultHash   string       `json:"resultHash"`
	Steps        []StepResult `json:"steps"`
}

// stepOutcome is the result of executing one step.
type stepOutcome struct {
	status       string
	virtualEndMs int64
	summary      string
	detail       map[string]any
	captures     map[string]any
	notEvaluated int
}

// runner carries the per-run scope.
type runner struct {
	eng            Target
	seed           string
	generators     *GeneratorContext
	inputs         map[string]any
	captures       map[string]any
	defaults       map[string]any
	virtualClockMs int64
	seedCounter    int // SEED_STATE default-key stream
	faultCounter   int // INJECT_FAULT condition-id stream
}

func (r *runner) tpl() *TemplateContext {
	return &TemplateContext{Inputs: r.inputs, Captures: r.captures, Defaults: r.defaults, Generators: r.generators}
}

// ResolveInputs applies InputDecl defaults and type checks over the provided
// values; a missing required input or a type mismatch is a run-blocking error.
func ResolveInputs(def *ScenarioDefinition, provided map[string]any) (map[string]any, error) {
	out := map[string]any{}
	for _, decl := range def.Inputs {
		v, ok := provided[decl.Name]
		if !ok {
			if decl.HasDefault {
				out[decl.Name] = decl.Default
				continue
			}
			if decl.Required {
				return nil, errfmt.New("scenario input missing", "'"+decl.Name+"' is required and has no default", "pass it with --input "+decl.Name+"=<value>", "docs/config-reference.md#scenarios")
			}
			continue
		}
		okType := false
		switch decl.Type {
		case "string":
			_, okType = v.(string)
		case "number":
			_, okType = v.(float64)
		case "boolean":
			_, okType = v.(bool)
		}
		if !okType {
			return nil, errfmt.Newf("scenario input type mismatch", "'"+decl.Name+"' must be a "+decl.Type, "fix the input value", "got %T", v)
		}
		out[decl.Name] = v
	}
	// Unknown extra inputs are rejected — a typoed name must not silently no-op.
	for name := range provided {
		known := false
		for _, decl := range def.Inputs {
			if decl.Name == name {
				known = true
				break
			}
		}
		if !known {
			return nil, errfmt.New("unknown scenario input", "'"+name+"' is not declared by this scenario", "check the pack's inputs section", "docs/config-reference.md#scenarios")
		}
	}
	return out, nil
}

// Run executes a validated definition against a sandbox engine. Per-step errors
// become ERRORED outcomes; Run itself only fails on input resolution.
func Run(eng Target, def *ScenarioDefinition, provided map[string]any, seed string) (*RunResult, error) {
	inputs, err := ResolveInputs(def, provided)
	if err != nil {
		return nil, err
	}

	// The run's clock starts at the sandbox's clock (never below base epoch).
	startClock := eng.VirtualClockMs()
	r := &runner{
		eng:            eng,
		seed:           seed,
		generators:     NewGeneratorContext(seed, startClock),
		inputs:         inputs,
		captures:       map[string]any{},
		defaults:       def.Defaults,
		virtualClockMs: startClock,
	}

	res := &RunResult{Steps: make([]StepResult, 0, len(def.Steps))}
	stepHashes := make([]string, 0, len(def.Steps))
	notEvaluated, evaluated := 0, 0
	hardFailedAt := -1
	hardStatus := ""
	failureSummary := ""

	for i := range def.Steps {
		step := &def.Steps[i]
		out := r.executeStep(step)
		notEvaluated += out.notEvaluated
		if n := len(step.Assertions) - out.notEvaluated; n > 0 {
			evaluated += n
		}
		for k, v := range out.captures {
			r.captures[k] = v
		}
		res.Steps = append(res.Steps, StepResult{
			Key: step.Key, Type: step.Type, Status: out.status, Summary: out.summary,
			VirtualStartMs: r.virtualClockMs, VirtualEndMs: out.virtualEndMs,
			NotEvaluated: out.notEvaluated, Detail: out.detail,
		})
		stepHashes = append(stepHashes, fmt.Sprintf("%s:%s:%d", step.Key, out.status, out.virtualEndMs))
		r.virtualClockMs = out.virtualEndMs
		// A hard failure stops the run; remaining steps are SKIPPED, not FAILED.
		if (out.status == StatusFailed || out.status == StatusErrored) && !step.ContinueOnFailure {
			hardFailedAt = i
			hardStatus = out.status
			failureSummary = out.summary
			break
		}
	}
	if hardFailedAt >= 0 {
		for i := hardFailedAt + 1; i < len(def.Steps); i++ {
			res.Steps = append(res.Steps, StepResult{
				Key: def.Steps[i].Key, Type: def.Steps[i].Type, Status: StatusSkipped,
				Summary:        "skipped: an earlier step failed hard",
				VirtualStartMs: r.virtualClockMs, VirtualEndMs: r.virtualClockMs,
			})
		}
	}

	res.NotEvaluated = notEvaluated
	res.ResultHash = resultHash(def, seed, stepHashes)
	switch {
	case hardFailedAt >= 0 && hardStatus == StatusErrored:
		res.Status = RunErrored
		res.Summary = failureSummary
	case hardFailedAt >= 0:
		res.Status = RunFailed
		res.Summary = failureSummary
	case evaluated > 0:
		res.Status = RunPassed
		res.Summary = fmt.Sprintf("%d assertion(s) passed; %d not evaluated", evaluated, notEvaluated)
	default:
		res.Status = RunConditionGenerated
		res.Summary = fmt.Sprintf("%d step(s) ran; %d assertion(s) not evaluated (no evaluable subject)", len(def.Steps), notEvaluated)
	}
	return res, nil
}

// resultHash fingerprints a run: definition + engine version + seed +
// subjects + every step's (key, status, virtual end).
func resultHash(def *ScenarioDefinition, seed string, stepHashes []string) string {
	defJSON, _ := json.Marshal(def) // encoding/json sorts map keys → stable
	defHash := sha256.Sum256(defJSON)
	parts := append([]string{hex.EncodeToString(defHash[:]), EngineVersion, seed, strings.Join(subjectsPresent, ",")}, stepHashes...)
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(sum[:])
}

func (r *runner) executeStep(step *Step) stepOutcome {
	out, err := r.dispatch(step)
	if err != nil {
		return stepOutcome{
			status: StatusErrored, virtualEndMs: r.virtualClockMs,
			summary: err.Error(), notEvaluated: len(step.Assertions),
		}
	}
	return out
}

func (r *runner) dispatch(step *Step) (stepOutcome, error) {
	switch step.Type {
	case "REQUEST":
		return r.request(step)
	case "WAIT":
		return r.wait(step)
	case "EXPECT_WEBHOOK":
		return r.expectWebhook(step)
	case "INJECT_FAULT":
		return r.injectFault(step)
	case "CLEAR_FAULT":
		return r.clearFault(step)
	case "SEED_STATE":
		return r.seedState(step)
	case "ASSERT_STATE":
		return r.assertState(step)
	case "VERIFY_REQUESTS":
		return r.verifyRequests(step)
	case "SNAPSHOT":
		cfg := step.Config.(*SnapshotConfig)
		return r.note(fmt.Sprintf("snapshot '%s'", cfg.Label)), nil
	case "NOTE":
		cfg := step.Config.(*NoteConfig)
		return r.note(cfg.Text), nil
	}
	return stepOutcome{}, errfmt.Newf("unknown step type", "the definition was not validated", "run ValidateScenario first", "%s", step.Type)
}

func (r *runner) request(step *Step) (stepOutcome, error) {
	cfg := step.Config.(*RequestConfig)
	tpl := r.tpl()

	path, err := interpolate(cfg.Path, tpl)
	if err != nil {
		return stepOutcome{}, err
	}
	headers := map[string]string{}
	for k, v := range cfg.Headers {
		iv, err := interpolate(v, tpl)
		if err != nil {
			return stepOutcome{}, err
		}
		headers[strings.ToLower(k)] = iv
	}
	query := url.Values{}
	for k, v := range cfg.Query {
		iv, err := interpolate(v, tpl)
		if err != nil {
			return stepOutcome{}, err
		}
		query.Set(k, iv)
	}
	var body any
	if cfg.HasBody {
		body, err = interpolateDeep(cfg.Body, tpl)
		if err != nil {
			return stepOutcome{}, err
		}
	}

	// Auto-inject the issued credential unless the step authenticates itself — or
	// asserts an auth-failure status, where injecting would assert a contradiction.
	if !stepExpectsAuthFailure(step) {
		if name, value, ok := r.eng.AuthHeader(); ok {
			if _, set := headers[name]; !set {
				headers[name] = value
			}
		}
	}

	target := path
	if !strings.HasPrefix(target, "/") {
		target = "/" + target // httptest.NewRequest panics on relative targets
	}
	if enc := query.Encode(); enc != "" {
		target += "?" + enc
	}
	var bodyReader *strings.Reader
	if cfg.HasBody {
		raw, err := json.Marshal(body)
		if err != nil {
			return stepOutcome{}, err
		}
		bodyReader = strings.NewReader(string(raw))
	} else {
		bodyReader = strings.NewReader("")
	}
	req := httptest.NewRequest(cfg.Method, target, bodyReader)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if cfg.HasBody && req.Header.Get("content-type") == "" {
		req.Header.Set("content-type", "application/json")
	}
	rec := httptest.NewRecorder()
	r.eng.ServeHTTP(rec, req)

	status := rec.Code
	respHeaders := map[string]string{}
	for k, vs := range rec.Header() {
		respHeaders[strings.ToLower(k)] = strings.Join(vs, ", ")
	}
	// Fault annotations ride pikopod's own headers: map them into assertion docs
	// and strip them so scripted header assertions see the provider's surface.
	var latencyMs *float64
	var faultApplied *bool
	if v, ok := respHeaders[sandbox.FaultDelayHeader]; ok {
		var ms float64
		if _, err := fmt.Sscanf(v, "%f", &ms); err == nil {
			latencyMs = &ms
		}
		delete(respHeaders, sandbox.FaultDelayHeader)
	}
	if _, ok := respHeaders[sandbox.FaultAppliedHeader]; ok {
		t := true
		faultApplied = &t
		delete(respHeaders, sandbox.FaultAppliedHeader)
	}

	respBody := parseResponseBody(rec.Body.Bytes())

	headersDoc := map[string]any{}
	for k, v := range respHeaders {
		headersDoc[k] = v
	}
	captures, err := applyCaptures(step.Capture, captureDocuments{
		"response.body":    respBody,
		"response.headers": headersDoc,
	})
	if err != nil {
		return stepOutcome{}, err
	}

	docs := &EvalDocs{
		Response:     &ResponseDoc{Status: float64(status), Headers: respHeaders, Body: respBody},
		LatencyMs:    latencyMs,
		FaultApplied: faultApplied,
	}
	verdict := EvaluateStepAssertions(step.Assertions, docs, subjectsPresent)

	summary := fmt.Sprintf("%s %s → %d", cfg.Method, path, status)
	if verdict.FirstFailure != nil {
		summary = verdict.FirstFailure.Message
	}
	return stepOutcome{
		status: verdict.Status, virtualEndMs: r.virtualClockMs,
		captures: captures, notEvaluated: verdict.NotEvaluated, summary: summary,
		detail: map[string]any{
			"request":    map[string]any{"method": cfg.Method, "path": path, "headers": headers, "query": query.Encode(), "body": body},
			"response":   map[string]any{"status": status, "headers": respHeaders, "body": respBody},
			"assertions": verdict.Results,
		},
	}, nil
}

// stepExpectsAuthFailure reports whether any assertion pins response.status
// to 401 or 403.
func stepExpectsAuthFailure(step *Step) bool {
	for i := range step.Assertions {
		a := &step.Assertions[i]
		if a.Target != "response.status" || a.Op != "equals" {
			continue
		}
		if f, ok := a.Expected.(float64); ok && (f == 401 || f == 403) {
			return true
		}
	}
	return false
}

// expectWebhook queries the outbox, advancing the virtual clock by the timeout
// ONLY when nothing was delivered. The engine's clock is bumped only by WAIT.
func (r *runner) expectWebhook(step *Step) (stepOutcome, error) {
	cfg := step.Config.(*ExpectWebhookConfig)
	eventType, _ := cfg.Match["eventType"].(string)

	// Due-by semantics: a delivery a delay_webhook fault pushed into the future
	// arrives here if due inside the timeout window. Never a real sleep.
	deliveries, lastArrivalMs := r.eng.DeliveriesDueBy(eventType, r.virtualClockMs+cfg.TimeoutMs)
	if len(deliveries) > 100 {
		deliveries = deliveries[:100]
	}
	payloads := make([]any, 0, len(deliveries))
	for i := range deliveries {
		var payload any
		if err := json.Unmarshal(deliveries[i].Payload, &payload); err != nil {
			return stepOutcome{}, err
		}
		payloads = append(payloads, payload)
	}

	virtualEndMs := r.virtualClockMs
	if len(payloads) == 0 {
		virtualEndMs += cfg.TimeoutMs
	} else if lastArrivalMs > virtualEndMs {
		virtualEndMs = lastArrivalMs // waited (virtually) for the delayed delivery
		r.eng.SetVirtualClockMs(virtualEndMs)
	}
	captures := map[string]any{}
	if len(payloads) > 0 {
		var err error
		captures, err = applyCaptures(step.Capture, captureDocuments{"webhook.delivery": payloads[0]})
		if err != nil {
			return stepOutcome{}, err
		}
	}
	verdict := EvaluateStepAssertions(step.Assertions, &EvalDocs{WebhookDeliveries: payloads, HasWebhooks: true}, subjectsPresent)

	summary := fmt.Sprintf("observed %d webhook delivery(ies)", len(payloads))
	if eventType != "" {
		summary += " for " + eventType
	}
	if verdict.FirstFailure != nil {
		summary = verdict.FirstFailure.Message
	}
	return stepOutcome{
		status: verdict.Status, virtualEndMs: virtualEndMs,
		captures: captures, notEvaluated: verdict.NotEvaluated, summary: summary,
		detail: map[string]any{"matched": len(payloads), "eventType": eventType, "assertions": verdict.Results},
	}, nil
}

func (r *runner) wait(step *Step) (stepOutcome, error) {
	cfg := step.Config.(*WaitConfig)
	end := r.virtualClockMs + cfg.DurationMs
	// Advance the sandbox's own clock so time-dependent synthesis reflects the
	// wait. Virtual only — no real sleep.
	r.eng.SetVirtualClockMs(end)
	return stepOutcome{
		status: StatusNotEvaluated, virtualEndMs: end,
		summary: fmt.Sprintf("waited %dms (virtual)", cfg.DurationMs),
	}, nil
}

// FaultRuleFor maps a validated INJECT_FAULT config onto the rule the engine
// arms. ok is false when the step names no standing condition to arm.
func FaultRuleFor(cfg *InjectFaultConfig, id string) (rule sandbox.FaultRule, ok bool) {
	if sandbox.IsWebhookFaultKind(cfg.Kind) {
		// The engine owns the outbox, so the rule arms for real, keyed by the
		// webhook EVENT (config.target; absent = any event). See faults.go.
		event := ""
		if cfg.Target != nil {
			event = *cfg.Target
		}
		rule = sandbox.FaultRule{Kind: cfg.Kind, Event: event, Probability: 1, ID: id}
		if cfg.DelayMs != nil {
			rule.DelayMs = *cfg.DelayMs
		}
		return rule, true
	}
	if cfg.Method == nil || cfg.Path == nil {
		return sandbox.FaultRule{}, false
	}
	declared := 0
	if cfg.Status != nil {
		declared = *cfg.Status
	}
	kind, status := sandbox.ResolveFaultKind(cfg.Kind, declared)
	rule = sandbox.FaultRule{Method: *cfg.Method, Path: *cfg.Path, Kind: kind, Probability: 1, ID: id, Wallclock: cfg.Wallclock}
	if status != 0 {
		rule.Status = status
	}
	if cfg.DelayMs != nil {
		rule.DelayMs = *cfg.DelayMs
	}
	if cfg.Probability != nil {
		rule.Probability = *cfg.Probability
	}
	if cfg.Times != nil {
		rule.Times = *cfg.Times
	}
	if cfg.Per != nil {
		rule.Per = *cfg.Per
	}
	if dd := cfg.DelayDistribution; dd != nil {
		rule.Delay = delayDistributionFromConfig(dd)
	}
	return rule, true
}

func (r *runner) injectFault(step *Step) (stepOutcome, error) {
	cfg := step.Config.(*InjectFaultConfig)
	rule, ok := FaultRuleFor(cfg, r.nextFaultID())
	if !ok {
		// materializeArmedFault: only standing sandbox conditions arm.
		return stepOutcome{
			status: StatusNotEvaluated, virtualEndMs: r.virtualClockMs,
			summary: "fault skipped (not a standing condition)",
		}, nil
	}
	r.eng.ArmFault(rule)
	if sandbox.IsWebhookFaultKind(cfg.Kind) {
		label := rule.Event
		if label == "" {
			label = "any event"
		}
		return stepOutcome{
			status: StatusNotEvaluated, virtualEndMs: r.virtualClockMs,
			summary: fmt.Sprintf("armed %s on %s", cfg.Kind, label),
		}, nil
	}
	return stepOutcome{
		status: StatusNotEvaluated, virtualEndMs: r.virtualClockMs,
		summary: fmt.Sprintf("armed %s on %s %s", rule.Kind, rule.Method, rule.Path),
	}, nil
}

// delayDistributionFromConfig maps the validated definition object onto the
// engine's DelayDistribution (numbers arrive as float64 from JSON).
func delayDistributionFromConfig(dd map[string]any) *sandbox.DelayDistribution {
	num := func(key string) int64 {
		if f, ok := dd[key].(float64); ok {
			return int64(f)
		}
		return 0
	}
	flt := func(key string) float64 {
		f, _ := dd[key].(float64)
		return f
	}
	typ, _ := dd["type"].(string)
	return &sandbox.DelayDistribution{
		Type: typ, MedianMs: num("medianMs"), Sigma: flt("sigma"), MaxMs: num("maxMs"),
		LowerMs: num("lowerMs"), UpperMs: num("upperMs"), BaseMs: num("baseMs"), Pct: flt("pct"),
	}
}

func (r *runner) clearFault(step *Step) (stepOutcome, error) {
	cfg := step.Config.(*ClearFaultConfig)
	method, path := "", ""
	if cfg.Method != nil {
		method = *cfg.Method
	}
	if cfg.Path != nil {
		path = *cfg.Path
	}
	r.eng.ClearFaults(method, path)
	return stepOutcome{
		status: StatusNotEvaluated, virtualEndMs: r.virtualClockMs,
		summary: "cleared matching faults",
	}, nil
}

func (r *runner) seedState(step *Step) (stepOutcome, error) {
	cfg := step.Config.(*SeedStateConfig)
	for i := range cfg.Resources {
		res := &cfg.Resources[i]
		key := ""
		if res.ResourceKey != nil {
			key = *res.ResourceKey
		} else {
			key = "seed_" + r.nextSeedHex()
		}
		attrs, err := json.Marshal(res.Attributes)
		if err != nil {
			return stepOutcome{}, err
		}
		if err := r.eng.SeedResource(res.Type, key, attrs); err != nil {
			return stepOutcome{}, err
		}
	}
	return stepOutcome{
		status: StatusNotEvaluated, virtualEndMs: r.virtualClockMs,
		summary: fmt.Sprintf("seeded %d resource(s)", len(cfg.Resources)),
	}, nil
}

// upperBoundOps become UNPROVABLE once the journal has evicted entries: the true
// count may be higher, so any claim but a lower bound must fail closed.
var upperBoundOps = map[string]bool{
	"equals": true, "notEquals": true, "lt": true, "lte": true,
	"countEquals": true, "isOneOf": true, "in": true,
}

func (r *runner) verifyRequests(step *Step) (stepOutcome, error) {
	cfg := step.Config.(*VerifyRequestsConfig)
	tpl := r.tpl()
	path, err := interpolate(cfg.Path, tpl)
	if err != nil {
		return stepOutcome{}, err
	}
	count, evicted := r.eng.JournalCount(cfg.Method, path)
	last, found, _ := r.eng.JournalLast(cfg.Method, path)

	// Fail-closed gates BEFORE evaluation: an evicted journal cannot prove
	// upper bounds; a truncated body cannot prove body claims.
	for i := range step.Assertions {
		a := &step.Assertions[i]
		if evicted && a.Target == "sandbox.requestCount" && upperBoundOps[a.Op] {
			return r.failedVerification(step, fmt.Sprintf(
				"journal evicted entries — %q on sandbox.requestCount is unprovable (only gt/gte remain trustworthy); raise the journal cap or reset it", a.Op)), nil
		}
		if a.Target == "sandbox.request" && found && last.BodyTruncated {
			return r.failedVerification(step,
				"the matching request's body exceeded the journal cap — body assertions are unprovable"), nil
		}
	}

	docs := &EvalDocs{}
	c := float64(count)
	docs.RequestCount = &c
	if found {
		docs.LastRequest = last.Body
		docs.LastRequestFound = last.Body != nil
	}
	verdict := EvaluateStepAssertions(step.Assertions, docs, subjectsPresent)
	summary := fmt.Sprintf("sandbox received %d request(s) matching %s %s", count, cfg.Method, path)
	if verdict.FirstFailure != nil {
		summary = verdict.FirstFailure.Message
	}
	return stepOutcome{
		status: verdict.Status, virtualEndMs: r.virtualClockMs,
		summary: summary, notEvaluated: verdict.NotEvaluated,
		detail: map[string]any{"requestCount": count, "assertions": verdict.Results},
	}, nil
}

// failedVerification renders a fail-closed VERIFY_REQUESTS outcome.
func (r *runner) failedVerification(step *Step, reason string) stepOutcome {
	return stepOutcome{
		status: StatusFailed, virtualEndMs: r.virtualClockMs,
		summary: reason, detail: map[string]any{"failClosed": reason},
	}
}

func (r *runner) assertState(step *Step) (stepOutcome, error) {
	cfg := step.Config.(*AssertStateConfig)
	tpl := r.tpl()
	typ, err := interpolate(cfg.ResourceType, tpl)
	if err != nil {
		return stepOutcome{}, err
	}
	key, err := interpolate(cfg.ResourceID, tpl)
	if err != nil {
		return stepOutcome{}, err
	}
	raw, found, err := r.eng.LookupResource(typ, key)
	if err != nil {
		return stepOutcome{}, err
	}
	var state any
	if found {
		if err := json.Unmarshal(raw, &state); err != nil {
			return stepOutcome{}, err
		}
	}
	verdict := EvaluateStepAssertions(step.Assertions, &EvalDocs{State: state, StateFound: found}, subjectsPresent)
	summary := fmt.Sprintf("%s/%s not found", typ, key)
	if found {
		summary = fmt.Sprintf("read state of %s/%s", typ, key)
	}
	if verdict.FirstFailure != nil {
		summary = verdict.FirstFailure.Message
	}
	var stateDetail any
	if found {
		stateDetail = state
	}
	return stepOutcome{
		status: verdict.Status, virtualEndMs: r.virtualClockMs,
		notEvaluated: verdict.NotEvaluated, summary: summary,
		detail: map[string]any{"state.resource": stateDetail, "assertions": verdict.Results},
	}, nil
}

func (r *runner) note(text string) stepOutcome {
	summary := text
	if len(summary) > 200 {
		summary = summary[:200]
	}
	return stepOutcome{
		status: StatusPassed, virtualEndMs: r.virtualClockMs,
		summary: summary, detail: map[string]any{"note": text},
	}
}

// nextSeedHex / nextFaultID derive from the run seed, so identical runs
// produce identical rows.
func (r *runner) nextSeedHex() string {
	r.seedCounter++
	return sandbox.NewPrng(fmt.Sprintf("%s:seedstate:%d", r.seed, r.seedCounter)).Hex(12)
}

func (r *runner) nextFaultID() string {
	r.faultCounter++
	return "fc_" + sandbox.NewPrng(fmt.Sprintf("%s:faultid:%d", r.seed, r.faultCounter)).Hex(16)
}

func parseResponseBody(raw []byte) any {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	return v
}
