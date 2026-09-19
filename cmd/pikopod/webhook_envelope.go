package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pikopod/pikopod/internal/config"
	"github.com/pikopod/pikopod/internal/errfmt"
	"github.com/pikopod/pikopod/internal/importer"
	"github.com/pikopod/pikopod/internal/ir"
	"github.com/pikopod/pikopod/internal/sandbox"
	"github.com/pikopod/pikopod/internal/store"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

const envelopeDocs = "scenarios/README.md#webhook-envelope"

// webhookSigningKey reads the key the declared envelope names; an unset
// variable yields no key, and the engine decides whether that matters.
func webhookSigningKey(def *ir.ApiDefinition) ([]byte, error) {
	if def.WebhookEnvelope == nil || def.WebhookEnvelope.Signature == nil {
		return nil, nil
	}
	sig := def.WebhookEnvelope.Signature
	value := os.Getenv(sig.KeyEnv)
	if value == "" {
		return nil, nil
	}
	key, err := sandbox.DecodeSigningKey(value, sig.KeyEncoding)
	if err != nil {
		return nil, errfmt.Newf("cannot decode $"+sig.KeyEnv, "the envelope declares keyEncoding "+sig.KeyEncoding+"; set the variable to the key exactly as the provider issued it", envelopeDocs, "%v", err)
	}
	return key, nil
}

func checkSigningKeys(cfg *config.Config, entries []sandboxEntry) error {
	for _, e := range entries {
		if e.WebhookURL == "" {
			continue
		}
		_, def, err := loadSandboxDef(cfg, e.Name)
		if err != nil || def.WebhookEnvelope == nil || def.WebhookEnvelope.Signature == nil {
			continue
		}
		key, err := webhookSigningKey(def)
		if err != nil {
			return err
		}
		if len(key) == 0 {
			return errfmt.New("webhook signing key missing for "+e.Name, "its spec signs deliveries with the key in $"+def.WebhookEnvelope.Signature.KeyEnv+", which is unset", "export "+def.WebhookEnvelope.Signature.KeyEnv+"=<the key the provider issued> and run `pikopod up` again", envelopeDocs)
		}
	}
	return nil
}

type sidecarTrigger struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}

type sidecarEvent struct {
	Trigger  *sidecarTrigger `json:"trigger,omitempty"`
	EmitOnly bool            `json:"emitOnly,omitempty"`
}

// applyWebhookSidecar attaches an envelope and event bindings from a YAML or
// JSON file to an already imported sandbox, rewriting only the persisted IR.
func applyWebhookSidecar(cfg *config.Config, name, path string, out io.Writer) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return errfmt.Newf("cannot read --webhooks file", "check the path", envelopeDocs, "%v", err)
	}
	var parsed map[string]any
	if err := yaml.Unmarshal(raw, &parsed); err != nil || parsed == nil {
		return errfmt.Newf("--webhooks file is not YAML or JSON", "it must hold one object with wrap, headers, signature and/or events", envelopeDocs, "%v", err)
	}
	events, err := decodeSidecarEvents(parsed["events"])
	if err != nil {
		return errfmt.Newf("--webhooks events are malformed", "each event is {trigger: {method, path}} or {emitOnly: true}", envelopeDocs, "%v", err)
	}
	delete(parsed, "events")
	var env *ir.WebhookEnvelope
	if len(parsed) > 0 {
		env, err = importer.DecodeWebhookEnvelope(parsed)
		if err != nil {
			return errfmt.Newf("--webhooks file is not a valid envelope", "see the reference for the fields and template references allowed", envelopeDocs, "%v", err)
		}
	}
	entry, def, err := loadSandboxDef(cfg, name)
	if err != nil {
		return err
	}
	if err := bindEvents(def, events); err != nil {
		return err
	}
	if env != nil {
		def.WebhookEnvelope = env
	}
	irRaw, err := json.Marshal(def)
	if err != nil {
		return err
	}
	irAbs := filepath.Join(cfg.DataDir, entry.IRFile)
	if err := store.WriteFileAtomic(irAbs, irRaw); err != nil {
		return errfmt.Newf("cannot persist the IR", "check permissions on "+irAbs, "docs/config-reference.md#data_dir", "%v", err)
	}
	for _, name := range sortedEventNames(events) {
		ev := events[name]
		switch {
		case ev.Trigger != nil:
			fmt.Fprintf(out, "  event %s fires on %s %s\n", name, strings.ToUpper(ev.Trigger.Method), ev.Trigger.Path)
		case ev.EmitOnly:
			fmt.Fprintf(out, "  event %s is emit-only (fire it with `pikopod webhook emit`)\n", name)
		}
	}
	printEnvelope(out, def)
	return nil
}

func decodeSidecarEvents(value any) (map[string]sidecarEvent, error) {
	if value == nil {
		return nil, nil
	}
	b, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var events map[string]sidecarEvent
	if err := dec.Decode(&events); err != nil {
		return nil, err
	}
	for name, ev := range events {
		if ev.Trigger == nil && !ev.EmitOnly {
			return nil, fmt.Errorf("%s declares neither a trigger nor emitOnly", name)
		}
		if ev.Trigger != nil && ev.EmitOnly {
			return nil, fmt.Errorf("%s cannot be both triggered and emit-only", name)
		}
		if ev.Trigger != nil && (ev.Trigger.Method == "" || ev.Trigger.Path == "") {
			return nil, fmt.Errorf("%s trigger needs method and path", name)
		}
	}
	return events, nil
}

// bindEvents applies sidecar bindings to declared events only; an unknown
// event or operation is refused, never invented.
func bindEvents(def *ir.ApiDefinition, events map[string]sidecarEvent) error {
	for _, name := range sortedEventNames(events) {
		ev := events[name]
		var hook *ir.Webhook
		for i := range def.Webhooks {
			if def.Webhooks[i].Event.Value == name {
				hook = &def.Webhooks[i]
				break
			}
		}
		if hook == nil {
			return errfmt.New("webhook event is not declared", name+" is not in this sandbox's spec, and pikopod never invents an event", "declare it in the spec (or the emitted spec) first; `pikopod sandbox list` shows what is declared", envelopeDocs)
		}
		if ev.Trigger != nil {
			method, path := strings.ToUpper(ev.Trigger.Method), ev.Trigger.Path
			if !hasOperation(def, method, path) {
				return errfmt.New("trigger names no operation", fmt.Sprintf("%s → %s %s is not in this sandbox's spec", name, method, path), "use the method and path template exactly as the spec declares them", envelopeDocs)
			}
			hook.Trigger = &ir.WebhookTrigger{Method: method, PathTemplate: path}
			hook.EmitOnly = false
		} else {
			hook.Trigger = nil
			hook.EmitOnly = true
		}
	}
	return nil
}

func hasOperation(def *ir.ApiDefinition, method, path string) bool {
	for i := range def.Endpoints {
		e := &def.Endpoints[i]
		if strings.EqualFold(e.Method.Value, method) && ir.CanonicalPathTemplate(e.PathTemplate.Value) == ir.CanonicalPathTemplate(path) {
			return true
		}
	}
	return false
}

func sortedEventNames(events map[string]sidecarEvent) []string {
	names := make([]string, 0, len(events))
	for n := range events {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// carryEventBindings keeps triggers and emit-only marks a person attached to
// events a re-import declares again without them.
func carryEventBindings(def, old *ir.ApiDefinition) {
	for i := range def.Webhooks {
		w := &def.Webhooks[i]
		if w.Trigger != nil || w.EmitOnly {
			continue
		}
		for j := range old.Webhooks {
			if o := &old.Webhooks[j]; o.Event.Value == w.Event.Value {
				w.Trigger, w.EmitOnly = o.Trigger, o.EmitOnly
				break
			}
		}
	}
}

func newSandboxWebhooksCmd() *cobra.Command {
	return &cobra.Command{Use: "webhooks <name> <file>", Short: "Attach a webhook envelope and event bindings to an existing sandbox (no re-import)", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			if err := applyWebhookSidecar(cfg, args[0], args[1], cmd.OutOrStdout()); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "restart `pikopod up` to serve the change")
			return nil
		}}
}

func envelopeSummary(def *ir.ApiDefinition) string {
	env := def.WebhookEnvelope
	if env == nil {
		return ""
	}
	if env.Signature == nil {
		return "envelope: wrapped, unsigned"
	}
	return fmt.Sprintf("envelope: %s in %s %q, key from $%s", env.Signature.Algorithm, env.Signature.In, env.Signature.Name, env.Signature.KeyEnv)
}

func printEnvelope(out io.Writer, def *ir.ApiDefinition) {
	summary := envelopeSummary(def)
	if summary == "" {
		return
	}
	fmt.Fprintln(out, "  webhook "+summary)
	if sig := def.WebhookEnvelope.Signature; sig != nil && os.Getenv(sig.KeyEnv) == "" {
		fmt.Fprintf(out, "  ⚠ $%s is not set; `pikopod up` needs it to deliver signed webhooks.\n", sig.KeyEnv)
	}
}
