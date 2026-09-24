package nl

import (
	"fmt"

	"github.com/pikopod/pikopod/internal/scenario"
)

type Intent struct {
	ArchetypeID          string               `json:"archetypeId"`
	Bindings             map[string]string    `json:"bindings"`
	AdditionalAssertions []scenario.Assertion `json:"additionalAssertions"`

	UnmappedIntent string  `json:"unmappedIntent"`
	Confidence     float64 `json:"confidence"`
}

const MaxGeneratedAssertions = 5

type IntentError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func ParseIntent(raw any) (*Intent, error) {
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("intent must be a JSON object")
	}
	for k := range obj {
		switch k {
		case "archetypeId", "bindings", "additionalAssertions", "unmappedIntent", "confidence":
		default:
			return nil, fmt.Errorf("intent has unrecognized key %q", k)
		}
	}
	out := &Intent{Bindings: map[string]string{}, AdditionalAssertions: []scenario.Assertion{}}
	id, ok := obj["archetypeId"].(string)
	if !ok || id == "" || len(id) > 100 {
		return nil, fmt.Errorf("intent archetypeId must be a non-empty string")
	}
	out.ArchetypeID = id
	bindings, ok := obj["bindings"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("intent bindings must be an object of strings")
	}
	for role, v := range bindings {
		s, ok := v.(string)
		if !ok || s == "" || len(s) > 400 {
			return nil, fmt.Errorf("intent binding %q must be a non-empty string", role)
		}
		out.Bindings[role] = s
	}
	if v, has := obj["additionalAssertions"]; has {
		arr, ok := v.([]any)
		if !ok {
			return nil, fmt.Errorf("intent additionalAssertions must be an array")
		}
		if len(arr) > 20 {
			return nil, fmt.Errorf("intent additionalAssertions exceeds 20")
		}

		synthetic := map[string]any{
			"steps": []any{map[string]any{"key": "a", "type": "NOTE", "config": map[string]any{"text": "x"}, "assertions": v}},
		}
		def, errs := scenario.ParseDefinition(synthetic)
		if len(errs) > 0 {
			return nil, fmt.Errorf("intent assertion invalid: %s", errs[0].Message)
		}
		out.AdditionalAssertions = def.Steps[0].Assertions
	}
	unmapped, ok := obj["unmappedIntent"].(string)
	if !ok || len(unmapped) > 2000 {
		return nil, fmt.Errorf("intent unmappedIntent is required (may be empty, must be a string)")
	}
	out.UnmappedIntent = unmapped
	conf, ok := obj["confidence"].(float64)
	if !ok || conf < 0 || conf > 1 {
		return nil, fmt.Errorf("intent confidence must be a number between 0 and 1")
	}
	out.Confidence = conf
	return out, nil
}

func ValidateIntent(intent *Intent, inv *Inventory) []IntentError {
	var errors []IntentError

	var arch *InventoryArchetype
	for i := range inv.Archetypes {
		if inv.Archetypes[i].ID == intent.ArchetypeID {
			arch = &inv.Archetypes[i]
			break
		}
	}
	if arch == nil {
		errors = append(errors, IntentError{Code: "UNKNOWN_ARCHETYPE", Message: fmt.Sprintf("archetype '%s' is not applicable to this API version", intent.ArchetypeID)})
	} else {
		known := KnownBindingRefs(inv)
		for _, req := range arch.Requires {
			ref, has := intent.Bindings[req.Role]
			if !has {
				errors = append(errors, IntentError{Code: "MISSING_ROLE", Message: fmt.Sprintf("binding for required role '%s' is missing", req.Role)})
				continue
			}
			if !known[ref] {
				errors = append(errors, IntentError{Code: "UNKNOWN_REF", Message: fmt.Sprintf("binding '%s' names '%s', which is not an operation or webhook event in this API version", req.Role, ref)})
			}
		}
	}

	if len(intent.AdditionalAssertions) > MaxGeneratedAssertions {
		errors = append(errors, IntentError{Code: "TOO_MANY_ASSERTIONS", Message: fmt.Sprintf("generated assertions exceed the cap of %d", MaxGeneratedAssertions)})
	}

	for i := range intent.AdditionalAssertions {
		a := &intent.AdditionalAssertions[i]
		isBodyPath := a.Path != nil && (a.Target == "response.body" || a.Target == "state.resource")
		if isBodyPath && (a.Op == "equals" || a.Op == "notEquals") {
			errors = append(errors, IntentError{Code: "VOLATILE_LITERAL", Message: fmt.Sprintf("assertion on '%s' uses a literal '%s'; volatile paths must use a matcher (matches/in/exists), not a literal", *a.Path, a.Op)})
		}
	}

	return errors
}
