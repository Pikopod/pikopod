// Package bridge turns a detected DriftEvent into a runnable scenario that
// pins the OLD contract, so the break happens locally and not in production.
package bridge

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pikopod/pikopod/internal/alert"
	"github.com/pikopod/pikopod/internal/drift"
	"github.com/pikopod/pikopod/internal/errfmt"
)

// FindEvent scans the local event log for a fingerprint. The last matching
// line wins (occurrence counters only grow).
func FindEvent(dataDir, fingerprint string) (*alert.DriftEvent, error) {
	raw, err := os.ReadFile(filepath.Join(dataDir, "events.ndjson"))
	if err != nil {
		return nil, errfmt.New("no drift events recorded", "the agent has not emitted any alerts (or data_dir differs)", "run `pikopod up`, let it alert, then retry with the fingerprint from the alert", "docs/config-reference.md#data_dir")
	}
	var found *alert.DriftEvent
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var ev alert.DriftEvent
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		if ev.Fingerprint == fingerprint {
			evCopy := ev
			found = &evCopy
		}
	}
	if found == nil {
		return nil, errfmt.New("unknown fingerprint", fingerprint+" is not in the event log", "copy the fp_… from the alert message, or see `pikopod status`", "docs/config-reference.md#alerts")
	}
	return found, nil
}

// Build renders the pack for one event, named drift-<fp>. contractVersion is
// the version AT PIN TIME (0 = none); later refinement can never move the pin.
func Build(ev *alert.DriftEvent, contractVersion int) (name string, pack map[string]any, err error) {
	def, err := definitionFor(ev)
	if err != nil {
		return "", nil, err
	}
	name = "drift-" + strings.TrimPrefix(ev.Fingerprint, "fp_")
	pack = map[string]any{
		"name":     name,
		"provider": ev.Upstream,
		"description": fmt.Sprintf("Pinned baseline for drift %s: %s on %s %s (%s). Fails when the sandbox adopts the provider's change.",
			ev.Fingerprint, ev.Kind, ev.Method, ev.Endpoint, describe(ev)),
		"definition": def,
	}
	if contractVersion > 0 {
		pack["contractVersion"] = contractVersion
	}
	return name, pack, nil
}

// Save writes the pack under <data_dir>/scenarios and returns the path.
func Save(dataDir string, name string, packYAML []byte) (string, error) {
	dir := filepath.Join(dataDir, "scenarios")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", errfmt.Newf("cannot create the scenarios dir", "check permissions on "+dir, "docs/config-reference.md#data_dir", "%v", err)
	}
	path := filepath.Join(dir, name+".yaml")
	if err := os.WriteFile(path, packYAML, 0o600); err != nil {
		return "", errfmt.Newf("cannot save the pack", "check permissions on "+path, "", "%v", err)
	}
	return path, nil
}

func describe(ev *alert.DriftEvent) string {
	switch ev.Kind {
	case drift.FieldAdded:
		return "field " + ev.Field + " appeared"
	case drift.FieldRemoved:
		return "field " + ev.Field + " disappeared"
	case drift.TypeChanged:
		return fmt.Sprintf("field %s changed type %s → %s", ev.Field, ev.Before, ev.After)
	case drift.EnumValueNew:
		return fmt.Sprintf("field %s grew a new value %q (known: %s)", ev.Field, ev.After, ev.Before)
	case drift.StatusNew:
		return "a new status class " + ev.After + " appeared"
	case drift.FieldNullable:
		return fmt.Sprintf("field %s (always %s) went nullable", ev.Field, ev.Before)
	case drift.StatusCodeChanged:
		return fmt.Sprintf("exact status changed %s → %s", ev.Before, ev.After)
	case drift.ErrorShapeChanged:
		return "the error body was restructured"
	}
	return string(ev.Kind)
}

// probeKey is the deterministic resource key seeded for item endpoints.
const probeKey = "drift_probe"

func definitionFor(ev *alert.DriftEvent) (map[string]any, error) {
	requestPath, seedType, needsSeed, err := concretize(ev.Endpoint)
	if err != nil {
		return nil, err
	}

	var steps []any
	steps = append(steps, map[string]any{
		"key": "context", "type": "NOTE",
		"config": map[string]any{"text": fmt.Sprintf("drift %s: %s", ev.Fingerprint, describe(ev))},
	})

	assertions, seedAttrs, err := assertionsFor(ev)
	if err != nil {
		return nil, err
	}

	if needsSeed {
		steps = append(steps, map[string]any{
			"key": "seed-probe", "type": "SEED_STATE",
			"config": map[string]any{"resources": []any{map[string]any{
				"type": seedType, "resourceKey": probeKey, "attributes": seedAttrs,
			}}},
		})
	}

	request := map[string]any{
		"key": "baseline-contract", "type": "REQUEST",
		"config":     map[string]any{"method": ev.Method, "path": requestPath},
		"assertions": assertions,
	}
	if ev.Method == "POST" || ev.Method == "PUT" || ev.Method == "PATCH" {
		request["config"].(map[string]any)["body"] = map[string]any{}
	}
	steps = append(steps, request)

	return map[string]any{"steps": steps}, nil
}

// concretize turns a path template into a requestable path. Only a single
// TRAILING parameter can be probed; anything else is an honest v1 limitation.
func concretize(template string) (requestPath, seedType string, needsSeed bool, err error) {
	segs := strings.Split(strings.TrimPrefix(template, "/"), "/")
	paramIdx := -1
	prefix := ""
	for i, s := range segs {
		// Both bare ({id}) and prefixed (tx_{id}) params count; the prefix is
		// preserved so the probe key matches the template's shape.
		if open := strings.Index(s, "{"); open != -1 && strings.HasSuffix(s, "}") {
			if paramIdx != -1 || i != len(segs)-1 {
				return "", "", false, errfmt.New(
					"cannot derive a scenario for this endpoint yet",
					template+" has a non-trailing or multiple path parameters",
					"from-drift v1 handles collection endpoints and single-id item endpoints; write the scenario by hand from schema/scenario-pack.schema.json",
					"scenarios/README.md")
			}
			paramIdx = i
			prefix = s[:open]
		}
	}
	if paramIdx == -1 {
		return template, "", false, nil
	}
	collection := "/" + strings.Join(segs[:paramIdx], "/")
	return collection + "/" + prefix + probeKey, collection, true, nil
}

// assertionsFor pins the baseline contract for the drift kind, plus the seed
// attributes that make the assertion meaningful on an echoing item read.
func assertionsFor(ev *alert.DriftEvent) (assertions []any, seedAttrs map[string]any, err error) {
	seedAttrs = map[string]any{}
	jsonPath, buildable := fieldToJSONPath(ev.Field)
	a := func(m map[string]any) { assertions = append(assertions, m) }

	switch ev.Kind {
	case drift.FieldAdded:
		if !buildable {
			return nil, nil, errUnbuildable(ev)
		}
		// Baseline: the field does not exist. Fails once the sandbox model
		// (or provider recording) carries it.
		a(map[string]any{"target": "response.body", "path": jsonPath, "op": "absent"})
	case drift.FieldRemoved:
		if !buildable {
			return nil, nil, errUnbuildable(ev)
		}
		setField(seedAttrs, ev.Field, sampleOfType(ev.Before))
		a(map[string]any{"target": "response.body", "path": jsonPath, "op": "exists"})
	case drift.TypeChanged:
		if !buildable {
			return nil, nil, errUnbuildable(ev)
		}
		setField(seedAttrs, ev.Field, sampleOfType(ev.Before))
		if m, ok := typeMatcher(ev.Before); ok {
			a(map[string]any{"target": "response.body", "path": jsonPath, "op": "equals", "expected": m})
		} else {
			a(map[string]any{"target": "response.body", "path": jsonPath, "op": "exists"})
		}
	case drift.EnumValueNew:
		if !buildable {
			return nil, nil, errUnbuildable(ev)
		}
		known := strings.Split(ev.Before, ",")
		if len(known) > 0 && known[len(known)-1] == "…" {
			known = known[:len(known)-1]
		}
		if len(known) > 0 && known[0] != "" {
			setField(seedAttrs, ev.Field, known[0])
		}
		// Baseline: the value stays inside the known set.
		vals := make([]any, 0, len(known))
		for _, v := range known {
			vals = append(vals, v)
		}
		a(map[string]any{"target": "response.body", "path": jsonPath, "op": "isOneOf", "expected": vals})
	case drift.StatusNew:
		switch {
		case strings.HasPrefix(ev.After, "5"):
			a(map[string]any{"target": "response.status", "op": "lt", "expected": 500})
		case strings.HasPrefix(ev.After, "4"):
			a(map[string]any{"target": "response.status", "op": "lt", "expected": 400})
		default:
			a(map[string]any{"target": "response.status", "op": "gte", "expected": 200})
		}
	case drift.StatusCodeChanged:
		// Pinnable when the baseline knew exactly ONE code: the scenario
		// asserts the endpoint still answers it.
		known := strings.Split(ev.Before, ",")
		if len(known) != 1 || known[0] == "" {
			return nil, nil, errfmt.New("cannot pin a multi-code baseline",
				fmt.Sprintf("the frozen reference saw several codes (%s) — asserting any one of them would be a guess", ev.Before),
				"write the scenario by hand with the code your integration depends on", "scenarios/README.md")
		}
		code, err := strconv.Atoi(known[0])
		if err != nil {
			return nil, nil, errfmt.Newf("cannot pin this status baseline", "the reference code is not numeric", "write the scenario by hand", "%s", ev.Before)
		}
		a(map[string]any{"target": "response.status", "op": "equals", "expected": code})
	case drift.FieldNullable, drift.ErrorShapeChanged:
		// Not pinnable yet: the assertion grammar has no not-null op, and
		// error-shape pins need the sandbox to model a specific error body.
		return nil, nil, errfmt.New("this drift kind is not pinnable as a scenario yet",
			fmt.Sprintf("%s events alert and dedupe, but from-drift cannot derive a deterministic assertion for them", ev.Kind),
			"write the scenario by hand from schema/scenario-pack.schema.json", "scenarios/README.md")
	default:
		if strings.HasPrefix(string(ev.Kind), "declared:") {
			// Declared (spec-vs-spec) findings are documentation events, not
			// wire behavior — there is no baseline response to pin.
			return nil, nil, errfmt.New("declared drift is not replayable as a scenario",
				"this fingerprint records a SPEC change (spec-diff), not observed traffic",
				"review it with `pikopod spec-diff`, then accept via `pikopod import <upstream> --update`",
				"docs/config-reference.md#spec_watch")
		}
		return nil, nil, errfmt.Newf("unknown drift kind", "the event log may be from a newer pikopod", "upgrade pikopod and retry", "%s", ev.Kind)
	}
	return assertions, seedAttrs, nil
}

func errUnbuildable(ev *alert.DriftEvent) error {
	return errfmt.New(
		"cannot derive an assertion for this field",
		fmt.Sprintf("field path %q inside an array cannot be pinned deterministically yet", ev.Field),
		"write the scenario by hand from schema/scenario-pack.schema.json",
		"scenarios/README.md")
}

// fieldToJSONPath converts a baseline field path into the assertion JSONPath
// dialect. Array elements pin index 0; nested arrays are refused.
func fieldToJSONPath(field string) (string, bool) {
	if field == "" {
		return "", false
	}
	if strings.Count(field, "[]") > 1 {
		return "", false
	}
	out := "$"
	for _, seg := range strings.Split(field, "/") {
		arr := strings.HasSuffix(seg, "[]")
		seg = strings.TrimSuffix(seg, "[]")
		seg = strings.ReplaceAll(seg, "~1", "/")
		out += "." + seg
		if arr {
			out += "[0]"
		}
	}
	return out, true
}

// setField writes a nested sample value along a baseline field path.
func setField(attrs map[string]any, field string, value any) {
	segs := strings.Split(field, "/")
	current := attrs
	for i, seg := range segs {
		arr := strings.HasSuffix(seg, "[]")
		seg = strings.TrimSuffix(seg, "[]")
		seg = strings.ReplaceAll(seg, "~1", "/")
		last := i == len(segs)-1
		switch {
		case last && !arr:
			current[seg] = value
		case last && arr:
			current[seg] = []any{value}
		case arr:
			inner := map[string]any{}
			current[seg] = []any{inner}
			current = inner
		default:
			inner, ok := current[seg].(map[string]any)
			if !ok {
				inner = map[string]any{}
				current[seg] = inner
			}
			current = inner
		}
	}
}

// sampleOfType renders a stand-in value for a baseline dominant type.
func sampleOfType(typ string) any {
	switch typ {
	case "number":
		return float64(1)
	case "bool":
		return true
	case "null":
		return nil
	case "object":
		return map[string]any{}
	case "array":
		return []any{}
	default:
		return "baseline"
	}
}

// typeMatcher maps a baseline type to the assertion matcher dialect.
func typeMatcher(typ string) (string, bool) {
	switch typ {
	case "string":
		return "{{any:string}}", true
	case "number":
		return "{{any:number}}", true
	case "bool":
		return "{{any:boolean}}", true
	}
	return "", false
}
