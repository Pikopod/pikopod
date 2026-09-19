package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/pikopod/pikopod/internal/config"
	"github.com/pikopod/pikopod/internal/errfmt"
	"github.com/pikopod/pikopod/internal/importer"
	"github.com/pikopod/pikopod/internal/ir"
	"github.com/pikopod/pikopod/internal/sandbox"
	"github.com/pikopod/pikopod/internal/store"
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

// applyWebhookSidecar attaches an envelope from a YAML or JSON file to an
// already imported sandbox, for specs that do not carry the extension.
func applyWebhookSidecar(cfg *config.Config, name, path string, out io.Writer) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return errfmt.Newf("cannot read --webhooks file", "check the path", envelopeDocs, "%v", err)
	}
	var parsed any
	if err := yaml.Unmarshal(raw, &parsed); err != nil {
		return errfmt.Newf("--webhooks file is not YAML or JSON", "it must hold one object with wrap, headers and/or signature", envelopeDocs, "%v", err)
	}
	env, err := importer.DecodeWebhookEnvelope(parsed)
	if err != nil {
		return errfmt.Newf("--webhooks file is not a valid envelope", "see the reference for the fields and template references allowed", envelopeDocs, "%v", err)
	}
	entry, def, err := loadSandboxDef(cfg, name)
	if err != nil {
		return err
	}
	def.WebhookEnvelope = env
	irRaw, err := json.Marshal(def)
	if err != nil {
		return err
	}
	irAbs := filepath.Join(cfg.DataDir, entry.IRFile)
	if err := store.WriteFileAtomic(irAbs, irRaw); err != nil {
		return errfmt.Newf("cannot persist the IR", "check permissions on "+irAbs, "docs/config-reference.md#data_dir", "%v", err)
	}
	printEnvelope(out, def)
	return nil
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
