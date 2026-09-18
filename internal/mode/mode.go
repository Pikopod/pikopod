// Package mode folds a scenario's CONDITIONING steps into a standing state a
// running sandbox can be put into, so a developer's own code meets the failure.
package mode

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/pikopod/pikopod/internal/errfmt"
	"github.com/pikopod/pikopod/internal/sandbox"
	"github.com/pikopod/pikopod/internal/scenario"
)

func marshalAttributes(attrs map[string]any) (json.RawMessage, error) {
	raw, err := json.Marshal(attrs)
	if err != nil {
		return nil, errfmt.Newf("a seeded resource is not serializable", "check the scenario's SEED_STATE step", "scenarios/README.md", "%v", err)
	}
	return raw, nil
}

// Spec is the compiled standing state: what to arm and what to seed.
type Spec struct {
	Name     string                  `json:"name"`
	Source   string                  `json:"source"`
	Faults   []sandbox.FaultRule     `json:"faults"`
	Seeds    []scenario.SeedResource `json:"seeds,omitempty"`
	Revision int64                   `json:"revision"`
}

// blockingKinds hold a real connection when wallclock is on, so an armed one
// with no expiry wedges the sandbox with no visible cause.
var blockingKinds = map[string]bool{"hang": true, "slow_body": true, "latency": true}

// Compile folds the conditioning PREFIX of def, up to the first driving step.
//
// The prefix, not the whole definition: `declines` ends with CLEAR_FAULT, so
// folding every conditioning step would arm a fault and immediately clear it.
func Compile(name, source string, def *scenario.ScenarioDefinition) (*Spec, error) {
	spec := &Spec{Name: name, Source: source}
	for i := range def.Steps {
		step := &def.Steps[i]
		if scenario.StepClass[step.Type] != "conditioning" {
			break
		}
		switch cfg := step.Config.(type) {
		case *scenario.InjectFaultConfig:
			rule, ok := scenario.FaultRuleFor(cfg, "mode-"+strconv.Itoa(len(spec.Faults)+1))
			if !ok {
				continue
			}
			if rule.Wallclock && blockingKinds[rule.Kind] && rule.Times == 0 {
				return nil, errfmt.New(
					"a wallclock "+rule.Kind+" fault with no expiry cannot be a mode",
					"it holds a real connection on every matching request, and nothing would ever release it",
					"give the step `times: N` so it recovers, or run the scenario instead of arming it as a mode",
					"scenarios/README.md")
			}
			spec.Faults = append(spec.Faults, rule)
		case *scenario.SeedStateConfig:
			spec.Seeds = append(spec.Seeds, cfg.Resources...)
		case *scenario.ClearFaultConfig:
			// A clear before any traffic cancels what this mode just armed.
			spec.Faults = nil
		}
	}
	if len(spec.Faults) == 0 && len(spec.Seeds) == 0 {
		first := "nothing"
		if len(def.Steps) > 0 {
			first = def.Steps[0].Type
		}
		return nil, errfmt.New(
			name+" has no standing state to enter",
			"its first step is "+first+", so it asserts behaviour rather than arming a condition",
			"run it instead: `pikopod scenario run <sandbox> "+name+"`",
			"scenarios/README.md")
	}
	return spec, nil
}

// Apply arms the compiled state on a live engine, replacing whatever the
// previous mode armed.
func Apply(eng *sandbox.Engine, spec *Spec) error {
	eng.ClearFaults("", "")
	for _, seed := range spec.Seeds {
		key := ""
		if seed.ResourceKey != nil {
			key = *seed.ResourceKey
		}
		raw, err := marshalAttributes(seed.Attributes)
		if err != nil {
			return err
		}
		if err := eng.SeedResource(seed.Type, key, raw); err != nil {
			return errfmt.Newf("cannot seed "+seed.Type, "check the scenario's SEED_STATE step", "scenarios/README.md", "%v", err)
		}
	}
	for _, rule := range spec.Faults {
		eng.ArmFault(rule)
	}
	return nil
}

// Describe renders the armed state for a human.
func (s *Spec) Describe() string {
	out := fmt.Sprintf("mode: %s (from %s)\n", s.Name, s.Source)
	for _, seed := range s.Seeds {
		out += fmt.Sprintf("  seeded  %s\n", seed.Type)
	}
	for _, f := range s.Faults {
		target := f.Method + " " + f.Path
		if sandbox.IsWebhookFaultKind(f.Kind) {
			target = f.Event
			if target == "" {
				target = "any event"
			}
		}
		line := fmt.Sprintf("  armed   %s", f.Kind)
		if f.Status != 0 {
			line += " " + strconv.Itoa(f.Status)
		}
		line += " on " + target
		if f.Times > 0 {
			line += fmt.Sprintf(" (first %d", f.Times)
			if f.Per != "" {
				line += " per " + f.Per
			}
			line += ")"
		}
		out += line + "\n"
	}
	return out
}
