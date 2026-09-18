package resolve

import (
	"fmt"
	"strings"

	"github.com/pikopod/pikopod/internal/errfmt"
	"github.com/pikopod/pikopod/internal/ir"
	"github.com/pikopod/pikopod/internal/scenario"
	"github.com/pikopod/pikopod/internal/scenario/archetype"
)

type Options struct {
	PackDirs []string
	BindOverrides map[string]string
}

type RoleBinding struct {
	Role        string `json:"role"`
	OperationID string `json:"operationId"`
}

type Candidate struct {
	Roles []RoleBinding `json:"roles"`
}

type Binding struct {
	ID         string      `json:"id"`
	Title      string      `json:"title"`
	Applicable bool        `json:"applicable"`
	Reason     string      `json:"reason,omitempty"`
	Candidates []Candidate `json:"candidates,omitempty"`
}

func ListBindings(def *ir.ApiDefinition) []Binding {
	all := archetype.All()
	out := make([]Binding, 0, len(all))
	for i := range all {
		a := &all[i]
		b := archetype.Bind(a, def)
		entry := Binding{ID: a.ID, Title: a.Title, Applicable: b.Applicable, Reason: b.Reason}
		if b.Applicable {
			for _, c := range b.Candidates {
				cand := Candidate{}
				for _, r := range a.Requires {
					if opID, ok := c.Bindings[r.Role]; ok {
						cand.Roles = append(cand.Roles, RoleBinding{Role: r.Role, OperationID: opID})
					}
				}
				entry.Candidates = append(entry.Candidates, cand)
			}
		}
		out = append(out, entry)
	}
	return out
}

func Applicable(def *ir.ApiDefinition) []archetype.Archetype {
	all := archetype.All()
	var out []archetype.Archetype
	for i := range all {
		if archetype.Bind(&all[i], def).Applicable {
			out = append(out, all[i])
		}
	}
	return out
}

func Find(id string) *archetype.Archetype {
	all := archetype.All()
	for i := range all {
		if all[i].ID == id {
			return &all[i]
		}
	}
	return nil
}

func CandidateMatch(a *archetype.Archetype, def *ir.ApiDefinition, bindings map[string]string) (usesInferred, ok bool) {
	for _, c := range archetype.Bind(a, def).Candidates {
		match := true
		for _, r := range a.Requires {
			if c.Bindings[r.Role] != bindings[r.Role] {
				match = false
				break
			}
		}
		if match {
			return c.UsesInferred, true
		}
	}
	return false, false
}

func PackByName(name string, packDirs []string) *scenario.Pack {
	packs, _ := scenario.ListPacks(packDirs...)
	for _, p := range packs {
		if p.Name == name {
			return p
		}
	}
	return nil
}

func Resolve(def *ir.ApiDefinition, name string, opts Options) (*scenario.ScenarioDefinition, error) {
	all := archetype.All()
	for i := range all {
		a := &all[i]
		if a.ID != name {
			continue
		}
		binding := archetype.Bind(a, def)
		if !binding.Applicable {
			return nil, errfmt.New("archetype does not apply", name+": "+binding.Reason, "run `pikopod scenario list <sandbox>` to see what binds", "scenarios/README.md")
		}
		var firstErr string
		for _, cand := range binding.Candidates {
			bindings := cand.Bindings
			if len(opts.BindOverrides) > 0 {
				merged := map[string]string{}
				for k, v := range bindings {
					merged[k] = v
				}
				for k, v := range opts.BindOverrides {
					merged[k] = v
				}
				bindings = merged
			}
			exp, err := archetype.Expand(a, bindings, def)
			if err != nil {
				if firstErr == "" {
					firstErr = err.Error()
				}
				continue
			}
			vr, parsed := scenario.ValidateScenario(exp.Definition, def)
			if !vr.Valid {
				if firstErr == "" {
					firstErr = vr.Errors[0].Message
				}
				continue
			}
			return parsed, nil
		}
		return nil, errfmt.New("no candidate binding grounds", fmt.Sprintf("%s: %s", name, firstErr), "check --bind overrides name operations from `scenario list`", "scenarios/README.md")
	}

	// Not an archetype: a saved pack, by name or path.
	if strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml") {
		pack, err := scenario.LoadPack(name)
		if err != nil {
			return nil, err
		}
		return GroundPack(pack, def)
	}
	if p := PackByName(name, opts.PackDirs); p != nil {
		return GroundPack(p, def)
	}
	return nil, errfmt.New("unknown scenario", fmt.Sprintf("%q is neither an archetype nor a saved pack", name), "see `pikopod scenario list <sandbox>` for archetypes, scenarios/ for packs", "scenarios/README.md")
}

func GroundPack(pack *scenario.Pack, def *ir.ApiDefinition) (*scenario.ScenarioDefinition, error) {
	vr, parsed := scenario.ValidateScenario(pack.RawDefinition, def)
	if !vr.Valid {
		return nil, errfmt.New("pack does not ground against this sandbox's API", fmt.Sprintf("%s: %s", pack.Name, vr.Errors[0].Message), "the pack was authored for a different API shape; regenerate it with `scenario create`", "scenarios/README.md")
	}
	return parsed, nil
}
