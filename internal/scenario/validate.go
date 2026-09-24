package scenario

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"sort"

	"github.com/pikopod/pikopod/internal/ir"
)

var varBaseNameRe = regexp.MustCompile(`[.\[]`)

func collectVars(value any, acc *[]string) {
	switch v := value.(type) {
	case string:
		*acc = append(*acc, ExtractVarExprs(v)...)
	case []any:
		for _, item := range v {
			collectVars(item, acc)
		}
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			collectVars(v[k], acc)
		}
	}
}

func configVars(step *Step, acc *[]string) {
	switch cfg := step.Config.(type) {
	case *RequestConfig:
		collectVars(cfg.Method, acc)
		collectVars(cfg.Path, acc)
		for _, k := range sortedKeys(cfg.Headers) {
			collectVars(cfg.Headers[k], acc)
		}
		for _, k := range sortedKeys(cfg.Query) {
			collectVars(cfg.Query[k], acc)
		}
		if cfg.HasBody {
			collectVars(cfg.Body, acc)
		}
	case *ExpectWebhookConfig:
		collectVars(cfg.Match, acc)
	case *InjectFaultConfig:
		if cfg.Method != nil {
			collectVars(*cfg.Method, acc)
		}
		if cfg.Path != nil {
			collectVars(*cfg.Path, acc)
		}
		if cfg.Target != nil {
			collectVars(*cfg.Target, acc)
		}
	case *ClearFaultConfig:
		if cfg.Method != nil {
			collectVars(*cfg.Method, acc)
		}
		if cfg.Path != nil {
			collectVars(*cfg.Path, acc)
		}
	case *AssertStateConfig:
		collectVars(cfg.ResourceType, acc)
		collectVars(cfg.ResourceID, acc)
	case *SeedStateConfig:
		for i := range cfg.Resources {
			collectVars(cfg.Resources[i].Type, acc)
			if cfg.Resources[i].ResourceKey != nil {
				collectVars(*cfg.Resources[i].ResourceKey, acc)
			}
			collectVars(cfg.Resources[i].Attributes, acc)
		}
	case *SnapshotConfig:
		collectVars(cfg.Label, acc)
	case *NoteConfig:
		collectVars(cfg.Text, acc)
	}
}

func ValidateScenario(raw any, apiDef *ir.ApiDefinition) (ValidationResult, *ScenarioDefinition) {
	def, schemaErrs := ParseDefinition(raw)
	if len(schemaErrs) > 0 {
		return ValidationResult{Valid: false, Errors: schemaErrs}, nil
	}
	var errors []ValidationError

	declared := map[string]bool{}
	for _, in := range def.Inputs {
		declared[in.Name] = true
	}
	for k := range def.Defaults {
		declared[k] = true
	}
	capturedSoFar := map[string]bool{}
	seenKeys := map[string]bool{}
	totalAssertions := 0
	var estimatedVirtualMs int64
	apiDeclaresWebhooks := apiDef != nil && len(apiDef.Webhooks) > 0

	for si := range def.Steps {
		step := &def.Steps[si]
		if seenKeys[step.Key] {
			errors = append(errors, ValidationError{Code: "DUPLICATE_STEP_KEY", Message: fmt.Sprintf("duplicate step key '%s'", step.Key), StepKey: step.Key})
		}
		seenKeys[step.Key] = true
		totalAssertions += len(step.Assertions)

		var refs []string
		configVars(step, &refs)
		for ai := range step.Assertions {
			collectVars(step.Assertions[ai].Expected, &refs)
		}
		for ai := range step.Assertions {
			if step.Assertions[ai].ResourceType != nil {
				collectVars(*step.Assertions[ai].ResourceType, &refs)
			}
			if step.Assertions[ai].ResourceID != nil {
				collectVars(*step.Assertions[ai].ResourceID, &refs)
			}
		}
		for _, expr := range refs {
			if IsGeneratorExpr(expr) {
				continue
			}
			name := varBaseNameRe.Split(expr, 2)[0]
			if declared[name] || capturedSoFar[name] {
				continue
			}
			capturedLater := false
			for sj := range def.Steps {
				if _, has := def.Steps[sj].Capture[name]; has {
					capturedLater = true
					break
				}
			}
			if capturedLater {
				errors = append(errors, ValidationError{Code: "FORWARD_CAPTURE", Message: fmt.Sprintf("variable '%s' is captured by a later step", name), StepKey: step.Key})
			} else {
				errors = append(errors, ValidationError{Code: "UNRESOLVED_VARIABLE", Message: fmt.Sprintf("variable '%s' is not an input, default, capture, or generator", name), StepKey: step.Key})
			}
		}

		if step.Type == "REQUEST" {
			cfg := step.Config.(*RequestConfig)
			if IsAbsoluteURL(cfg.Path) {
				errors = append(errors, ValidationError{Code: "ABSOLUTE_URL", Message: "REQUEST path must be sandbox-relative, not an absolute URL", StepKey: step.Key})
			} else if apiDef != nil && MatchEndpoint(apiDef.Endpoints, cfg.Method, cfg.Path) == nil {
				errors = append(errors, ValidationError{Code: "UNKNOWN_ENDPOINT", Message: fmt.Sprintf("no %s %s in the pinned API version", cfg.Method, cfg.Path), StepKey: step.Key})
			}
		}
		if step.Type == "EXPECT_WEBHOOK" {
			cfg := step.Config.(*ExpectWebhookConfig)
			estimatedVirtualMs += cfg.TimeoutMs
			if apiDef != nil && !apiDeclaresWebhooks {
				errors = append(errors, ValidationError{Code: "NO_WEBHOOKS", Message: "EXPECT_WEBHOOK but the API declares no webhooks", StepKey: step.Key})
			}
		}
		if step.Type == "WAIT" {
			estimatedVirtualMs += step.Config.(*WaitConfig).DurationMs
		}

		for ai := range step.Assertions {
			a := &step.Assertions[ai]
			if a.Op == "matches" {
				if s, ok := a.Expected.(string); ok && isCatastrophicRegex(s) {
					errors = append(errors, ValidationError{Code: "UNSAFE_REGEX", Message: fmt.Sprintf("regex '%s' is uncompilable or catastrophic", s), StepKey: step.Key})
				}
			}
			if a.Op == "matchesSchema" && a.SchemaRef == nil {
				errors = append(errors, ValidationError{Code: "MISSING_SCHEMA_REF", Message: "matchesSchema requires schemaRef", StepKey: step.Key})
			}
		}

		for name := range step.Capture {
			capturedSoFar[name] = true
		}
	}

	if totalAssertions > MaxAssertionsTotal {
		errors = append(errors, ValidationError{Code: "TOO_MANY_ASSERTIONS", Message: fmt.Sprintf("total assertions %d exceeds %d", totalAssertions, MaxAssertionsTotal)})
	}
	if estimatedVirtualMs > MaxWaitMs {
		errors = append(errors, ValidationError{Code: "DURATION_EXCEEDED", Message: "estimated virtual duration exceeds the limit"})
	}

	return ValidationResult{Valid: len(errors) == 0, Errors: errors}, def
}

func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}
