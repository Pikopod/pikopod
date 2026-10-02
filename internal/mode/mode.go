package mode

import (
	"encoding/json"
	"fmt"
	"net/http"
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

type Spec struct {
	Name     string                       `json:"name"`
	Source   string                       `json:"source"`
	Faults   []sandbox.FaultRule          `json:"faults"`
	Seeds    []scenario.SeedResource      `json:"seeds,omitempty"`
	Verify   *scenario.ScenarioDefinition `json:"verify,omitempty"`
	Revision int64                        `json:"revision"`
}

var blockingKinds = map[string]bool{"hang": true, "slow_body": true, "latency": true}

var verifyTypes = map[string]bool{"VERIFY_SEQUENCE": true, "VERIFY_REQUESTS": true, "ASSERT_STATE": true, "EXPECT_WEBHOOK": true}

var verifySubjects = []string{"SANDBOX", "CLIENT"}

func Compile(name, source string, def *scenario.ScenarioDefinition) (*Spec, error) {
	spec := &Spec{Name: name, Source: source}
	i := 0
	for ; i < len(def.Steps); i++ {
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
			"run it instead: `pikopod scenario check <sandbox> "+name+"`",
			"scenarios/README.md")
	}
	var verify []scenario.Step
	for ; i < len(def.Steps); i++ {
		if verifyTypes[def.Steps[i].Type] {
			verify = append(verify, def.Steps[i])
		}
	}
	if len(verify) > 0 {
		spec.Verify = &scenario.ScenarioDefinition{Inputs: def.Inputs, Defaults: def.Defaults, Steps: verify}
	}
	return spec, nil
}

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

type readOnly struct {
	scenario.Target
}

func (readOnly) SetVirtualClockMs(int64) {}

func (readOnly) ArmFault(sandbox.FaultRule) {}

func (readOnly) ClearFaults(string, string) int { return 0 }

func (readOnly) SeedResource(typ, _ string, _ json.RawMessage) error {
	return errfmt.New("verify cannot seed "+typ, "verification only reads the served sandbox", "seed state with `pikopod mode set`", "scenarios/README.md")
}

func (readOnly) EmitWebhook(event string, _ json.RawMessage) error {
	return errfmt.New("verify cannot emit "+event, "verification only reads the served sandbox", "emit it with `pikopod webhook emit`", "scenarios/README.md")
}

func (readOnly) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusMethodNotAllowed)
}

func Verify(eng scenario.Target, spec *Spec, seed string) (*scenario.RunResult, error) {
	if spec == nil {
		return nil, errfmt.New("no mode set", "nothing is armed on this sandbox, so there is nothing to verify against", "enter one with `pikopod mode set <sandbox> <scenario>`, run your tests, then verify", "scenarios/README.md")
	}
	if spec.Verify == nil || len(spec.Verify.Steps) == 0 {
		return nil, errfmt.New(
			spec.Name+" has nothing to verify",
			"its scenario has no VERIFY_SEQUENCE, VERIFY_REQUESTS, ASSERT_STATE or EXPECT_WEBHOOK step after the armed conditions",
			"add one to the pack, or read what your client sent with `pikopod requests <sandbox>`",
			"scenarios/README.md")
	}
	return scenario.RunWith(readOnly{eng}, spec.Verify, nil, seed, scenario.RunOptions{Subjects: verifySubjects, WallClockGaps: true})
}

func (s *Spec) Describe() string {
	out := fmt.Sprintf("mode: %s (from %s)\n", s.Name, s.Source)
	for _, seed := range s.Seeds {
		out += fmt.Sprintf("  seeded  %s\n", seed.Type)
	}
	for _, f := range s.Faults {
		out += "  armed   " + f.Describe() + "\n"
	}
	if s.Verify != nil {
		out += fmt.Sprintf("  verify  %d step(s) with `pikopod mode verify` after your tests run\n", len(s.Verify.Steps))
	}
	return out
}
