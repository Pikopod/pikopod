// Scenario CLI: names resolve to archetypes first, then saved packs. Runs are
// EPHEMERAL by default so a test never pollutes the served sandbox's state.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/pikopod/pikopod/internal/config"
	"github.com/pikopod/pikopod/internal/errfmt"
	"github.com/pikopod/pikopod/internal/ir"
	"github.com/pikopod/pikopod/internal/sandbox"
	"github.com/pikopod/pikopod/internal/scenario"
	"github.com/pikopod/pikopod/internal/scenario/archetype"
	"github.com/pikopod/pikopod/internal/scenario/nl"
	"github.com/pikopod/pikopod/internal/scenario/resolve"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// loadSandboxDef resolves a registered sandbox and its persisted IR.
func loadSandboxDef(cfg *config.Config, name string) (*sandboxEntry, *ir.ApiDefinition, error) {
	entries, err := loadRegistry(cfg.DataDir)
	if err != nil {
		return nil, nil, err
	}
	entry := findEntry(entries, name)
	if entry == nil {
		return nil, nil, errfmt.New("unknown sandbox", fmt.Sprintf("%q is not registered", name), "see `pikopod sandbox list`; add it with `pikopod sandbox add`", "")
	}
	raw, err := os.ReadFile(filepath.Join(cfg.DataDir, entry.IRFile))
	if err != nil {
		return nil, nil, errfmt.Newf("cannot read the persisted IR for "+name, "re-add the sandbox with `pikopod sandbox add`", "docs/config-reference.md#data_dir", "%v", err)
	}
	var def ir.ApiDefinition
	if err := json.Unmarshal(raw, &def); err != nil {
		return nil, nil, errfmt.Newf("persisted IR for "+name+" is corrupt", "re-add the sandbox with `pikopod sandbox add`", "docs/config-reference.md#data_dir", "%v", err)
	}
	return entry, &def, nil
}

// scenarioEngine builds the engine a run executes against (--persist uses the
// on-disk store). contractVersion 0 = latest; a pin can never be moved later.
func scenarioEngine(cfg *config.Config, entry *sandboxEntry, def *ir.ApiDefinition, persist bool, contractVersion int) (*sandbox.Engine, func(), error) {
	var st *sandbox.Store
	var err error
	if persist {
		st, err = sandbox.OpenStore(cfg.DataDir)
	} else {
		st, err = sandbox.OpenMemoryStore()
	}
	if err != nil {
		return nil, nil, err
	}
	id := entry.ID
	if !persist {
		id = entry.ID + "_ephemeral"
	}
	eng, err := sandbox.NewEngine(def, sandbox.Config{
		ID: id, Seed: entry.Seed, Mode: entry.Mode, VirtualClockMs: entry.CreatedClockMs,
		Effective: effectiveFor(cfg, entry, contractVersion),
		// Without these a scenario that arms a webhook fault delivers nothing,
		// and one that needs the recordings tier answers 404.
		WebhookURL: entry.WebhookURL,
		Recordings: recordingsFor(cfg, entry),
	}, st)
	if err != nil {
		st.Close()
		return nil, nil, err
	}
	return eng, func() { st.Close() }, nil
}

func packDirs(cfg *config.Config) []string {
	return []string{"scenarios", filepath.Join(cfg.DataDir, "scenarios")}
}

func scenarioList(cfg *config.Config, sandboxName string, verbose bool, out io.Writer) error {
	_, def, err := loadSandboxDef(cfg, sandboxName)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "archetypes vs %s (%d endpoints):\n", sandboxName, len(def.Endpoints))
	for _, b := range resolve.ListBindings(def) {
		if b.Applicable {
			fmt.Fprintf(out, "  ✓ %-26s %s  (%d candidate binding(s))\n", b.ID, b.Title, len(b.Candidates))
			if verbose {
				for _, c := range b.Candidates {
					for _, rb := range c.Roles {
						fmt.Fprintf(out, "      %s=%s\n", rb.Role, rb.OperationID)
					}
				}
			}
		} else {
			fmt.Fprintf(out, "  ✗ %-26s %s\n      %s\n", b.ID, b.Title, b.Reason)
		}
	}
	packs, fails := scenario.ListPacks(packDirs(cfg)...)
	if len(packs) > 0 {
		fmt.Fprintln(out, "\nsaved packs:")
		for _, p := range packs {
			fmt.Fprintf(out, "  • %-26s %s  (%s)\n", p.Name, p.Description, p.Path)
		}
	}
	for path, ferr := range fails {
		fmt.Fprintf(out, "  ! %s does not load: %v\n", path, ferr)
	}
	fmt.Fprintf(out, "\nrun one: pikopod scenario run %s <name> [<name>...]\n", sandboxName)
	return nil
}

// resolveRunnable turns a name into a parsed, grounded definition.
func resolveRunnable(cfg *config.Config, name string, def *ir.ApiDefinition, bindOverrides map[string]string) (*scenario.ScenarioDefinition, error) {
	return resolve.Resolve(def, name, resolve.Options{PackDirs: packDirs(cfg), BindOverrides: bindOverrides})
}

// packFor finds a saved pack by name (nil for archetypes/paths).
func packFor(cfg *config.Config, name string) *scenario.Pack {
	return resolve.PackByName(name, packDirs(cfg))
}

// coerceInputs converts --input k=v strings per the declared input types.
func coerceInputs(def *scenario.ScenarioDefinition, kvs []string) (map[string]any, error) {
	out := map[string]any{}
	for _, kv := range kvs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return nil, errfmt.New("bad --input", fmt.Sprintf("%q is not key=value", kv), "pass inputs as --input name=value", "")
		}
		typ := "string"
		for _, d := range def.Inputs {
			if d.Name == k {
				typ = d.Type
				break
			}
		}
		switch typ {
		case "number":
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return nil, errfmt.Newf("bad --input", k+" must be a number", "", "%v", err)
			}
			out[k] = f
		case "boolean":
			b, err := strconv.ParseBool(v)
			if err != nil {
				return nil, errfmt.Newf("bad --input", k+" must be true or false", "", "%v", err)
			}
			out[k] = b
		default:
			out[k] = v
		}
	}
	return out, nil
}

func scenarioRun(cmd *cobra.Command, args []string) error {
	cfg, err := loadConfig(cmd)
	if err != nil {
		return err
	}
	sandboxName := args[0]
	names := args[1:]
	entry, def, err := loadSandboxDef(cfg, sandboxName)
	if err != nil {
		return err
	}
	persist, _ := cmd.Flags().GetBool("persist")
	seed, _ := cmd.Flags().GetString("seed")
	if seed == "" {
		seed = entry.Seed
	}
	inputKVs, _ := cmd.Flags().GetStringArray("input")
	bindKVs, _ := cmd.Flags().GetStringArray("bind")
	bindOverrides := map[string]string{}
	for _, kv := range bindKVs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return errfmt.New("bad --bind", fmt.Sprintf("%q is not role=operation", kv), "pass bindings as --bind role=operationId", "")
		}
		bindOverrides[k] = v
	}
	out := cmd.OutOrStdout()

	// Remote target: URL and header values are operator-supplied — the same trust
	// model as curl; header values are never printed.
	targetURL, _ := cmd.Flags().GetString("target")
	targetHeaderKVs, _ := cmd.Flags().GetStringArray("target-header")
	var remote *scenario.RemoteTarget
	if targetURL != "" {
		headers := map[string]string{}
		for _, kv := range targetHeaderKVs {
			k, v, ok := strings.Cut(kv, ":")
			if !ok {
				return errfmt.New("bad --target-header", fmt.Sprintf("%q is not Name:value", kv), "pass headers as --target-header 'Authorization:Bearer …'", "")
			}
			headers[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
		remote, err = scenario.NewRemoteTarget(targetURL, headers)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "running against REAL endpoint %s — only REQUEST/NOTE/SNAPSHOT steps; conditioning and state assertions are refused\n", targetURL)
	}

	failed, errored := 0, 0
	for _, name := range names {
		parsed, err := resolveRunnable(cfg, name, def, bindOverrides)
		if err != nil {
			return err // config-class problem: exit 2, never conflated with a failing scenario
		}
		inputs, err := coerceInputs(parsed, inputKVs)
		if err != nil {
			return err
		}
		var res *scenario.RunResult
		if remote != nil {
			if err := scenario.RefuseUnsupportedSteps(parsed); err != nil {
				return err
			}
			res, err = scenario.Run(remote, parsed, inputs, seed)
			if err != nil {
				return err
			}
		} else {
			pinned := 0
			if pk := packFor(cfg, name); pk != nil {
				pinned = pk.ContractVersion
			}
			eng, done, err := scenarioEngine(cfg, entry, def, persist, pinned)
			if err != nil {
				return err
			}
			res, err = scenario.Run(eng, parsed, inputs, seed)
			done()
			if err != nil {
				return err
			}
		}

		mark := map[string]string{
			scenario.RunPassed: "✓", scenario.RunFailed: "✗",
			scenario.RunConditionGenerated: "○", scenario.RunErrored: "!",
		}[res.Status]
		fmt.Fprintf(out, "%s %s — %s (%s)\n", mark, name, res.Status, res.Summary)
		for _, s := range res.Steps {
			fmt.Fprintf(out, "    %-14s %-16s %s\n", s.Status, s.Key, s.Summary)
		}
		switch res.Status {
		case scenario.RunFailed:
			failed++
		case scenario.RunErrored:
			errored++
		}
	}

	if errored > 0 {
		return errfmt.New("scenario run errored", "a step could not execute (see the ! run above)", "fix the scenario or sandbox and retry", "docs/exit-codes.md")
	}
	if failed > 0 {
		fmt.Fprintf(out, "\n%d scenario(s) failed — exit 1\n", failed)
		os.Exit(1) // 1 = assertions failed, distinct from 2 = tool error
	}
	return nil
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func scenarioCreate(cmd *cobra.Command, args []string) error {
	cfg, err := loadConfig(cmd)
	if err != nil {
		return err
	}
	sandboxName := args[0]
	description := strings.Join(args[1:], " ")
	_, def, err := loadSandboxDef(cfg, sandboxName)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()

	applicable := resolve.Applicable(def)
	inv := nl.BuildInventory(def, applicable)

	key := cfg.LLM.APIKey
	if key == "" {
		return nl.ErrNoKey()
	}
	model, _ := cmd.Flags().GetString("model")
	client := newLLMClient(cfg, model)
	fmt.Fprintf(out, "grounding %q against %s (%d operations, %d applicable archetypes)…\n", description, sandboxName, len(inv.Operations), len(applicable))
	intent, err := client.CompleteIntent(context.Background(), description, inv)
	if err != nil {
		return err
	}
	if errs := nl.ValidateIntent(intent, inv); len(errs) > 0 {
		return errfmt.New("the described scenario could not be grounded", errs[0].Message, "rephrase using operations the API actually has (`pikopod scenario list "+sandboxName+"`)", "scenarios/README.md")
	}

	chosen := resolve.Find(intent.ArchetypeID)
	if chosen == nil {
		return errfmt.New("the model chose an unknown archetype", intent.ArchetypeID, "retry; if it persists, file an issue with the description you used", "")
	}
	usesInferred, valid := resolve.CandidateMatch(chosen, def, intent.Bindings)
	if !valid {
		return errfmt.New("the model's bindings are not a valid candidate", "the named operations do not satisfy the archetype's requirements", "rephrase, or run the archetype directly with --bind overrides", "scenarios/README.md")
	}

	exp, err := archetype.Expand(chosen, intent.Bindings, def)
	if err != nil {
		return err
	}
	definition := weaveAssertions(exp.Definition, intent)
	vr, _ := scenario.ValidateScenario(definition, def)
	if !vr.Valid {
		return errfmt.New("the drafted scenario does not validate", vr.Errors[0].Message, "retry; if it persists, file an issue with the description you used", "")
	}

	pack := map[string]any{
		"name":        slugify(description),
		"provider":    sandboxName,
		"description": description,
		"definition":  definition,
	}
	rendered, err := yaml.Marshal(pack)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "\ndrafted from archetype %s (confidence %.2f", chosen.ID, intent.Confidence)
	if usesInferred {
		fmt.Fprint(out, ", rests on inferred spec fields")
	}
	fmt.Fprintln(out, "):")
	if intent.UnmappedIntent != "" {
		fmt.Fprintf(out, "not captured by this draft: %s\n", intent.UnmappedIntent)
	}
	fmt.Fprintf(out, "\n%s\n", rendered)

	if yes, _ := cmd.Flags().GetBool("yes"); !yes {
		fmt.Fprint(out, "save this pack? [y/N] ")
		var answer string
		fmt.Fscanln(cmd.InOrStdin(), &answer)
		if !strings.HasPrefix(strings.ToLower(answer), "y") {
			fmt.Fprintln(out, "discarded — nothing written")
			return nil
		}
	}
	dir := filepath.Join(cfg.DataDir, "scenarios")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return errfmt.Newf("cannot create the scenarios dir", "check permissions on "+dir, "docs/config-reference.md#data_dir", "%v", err)
	}
	path := filepath.Join(dir, pack["name"].(string)+".yaml")
	if err := os.WriteFile(path, rendered, 0o600); err != nil {
		return errfmt.Newf("cannot save the pack", "check permissions on "+path, "", "%v", err)
	}
	fmt.Fprintf(out, "saved %s — run it: pikopod scenario run %s %s\n", path, sandboxName, pack["name"])
	return nil
}

// weaveAssertions attaches validated extra assertions to the first driving
// step that carries an assertions array.
func weaveAssertions(definition map[string]any, intent *nl.Intent) map[string]any {
	if len(intent.AdditionalAssertions) == 0 {
		return definition
	}
	steps, _ := definition["steps"].([]any)
	added := intent.AdditionalAssertions
	if len(added) > nl.MaxGeneratedAssertions {
		added = added[:nl.MaxGeneratedAssertions]
	}
	out := make([]any, len(steps))
	woven := false
	for i, s := range steps {
		step, _ := s.(map[string]any)
		typ, _ := step["type"].(string)
		if !woven && (typ == "REQUEST" || typ == "EXPECT_WEBHOOK") {
			copied := make(map[string]any, len(step))
			for k, v := range step {
				copied[k] = v
			}
			existing, _ := copied["assertions"].([]any)
			for _, a := range added {
				raw, _ := json.Marshal(a)
				var v any
				json.Unmarshal(raw, &v)
				existing = append(existing, v)
			}
			copied["assertions"] = existing
			out[i] = copied
			woven = true
			continue
		}
		out[i] = s
	}
	if !woven {
		return definition
	}
	result := make(map[string]any, len(definition))
	for k, v := range definition {
		result[k] = v
	}
	result["steps"] = out
	return result
}

func slugify(s string) string {
	slug := slugRe.ReplaceAllString(strings.ToLower(s), "-")
	slug = strings.Trim(slug, "-")
	if len(slug) > 48 {
		slug = slug[:48]
		slug = strings.Trim(slug, "-")
	}
	if slug == "" {
		slug = "scenario"
	}
	return slug
}
