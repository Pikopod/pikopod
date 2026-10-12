package scenario

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pikopod/pikopod/internal/errfmt"
	"gopkg.in/yaml.v3"
)

type Pack struct {
	Name        string `json:"name"`
	Provider    string `json:"provider"`
	Description string `json:"description"`

	RawDefinition any                 `json:"definition"`
	Definition    *ScenarioDefinition `json:"-"`

	Path string `json:"-"`

	ContractVersion int `json:"contractVersion,omitempty"`
}

func jsonify(v any) (any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

const maxPackBytes = 1 << 20

func LoadPack(path string) (*Pack, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, errfmt.Newf("cannot read scenario pack", "check the path and permissions", "scenarios/README.md#where-packs-come-from", "%v", err)
	}
	if len(raw) > maxPackBytes {
		return nil, errfmt.New("scenario pack is too large", fmt.Sprintf("%s is %d bytes; packs are capped at 1 MiB", path, len(raw)), "split the scenario, or trim seed data", "scenarios/README.md#limits")
	}
	return ParsePack(raw, path)
}

func ParsePack(raw []byte, path string) (*Pack, error) {
	var doc any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, errfmt.Newf("scenario pack is not valid YAML", "fix the syntax error and retry", "scenarios/README.md#writing-a-pack-by-hand", "%s: %v", path, err)
	}
	jsonDoc, err := jsonify(doc)
	if err != nil {
		return nil, errfmt.Newf("scenario pack is not JSON-shaped", "packs must contain only plain maps, lists, and scalars", "scenarios/README.md#writing-a-pack-by-hand", "%s: %v", path, err)
	}
	obj, ok := jsonDoc.(map[string]any)
	if !ok {
		return nil, errfmt.New("scenario pack is not a mapping", path+" does not contain a YAML mapping at the top level", "a pack is {name, provider, description, definition}", "scenarios/README.md#writing-a-pack-by-hand")
	}
	pack := &Pack{Path: path}
	if s, ok := obj["name"].(string); ok && s != "" {
		pack.Name = s
	} else {
		return nil, errfmt.New("scenario pack is missing 'name'", path+" has no non-empty name field", "add `name: <slug>` to the pack", "scenarios/README.md#writing-a-pack-by-hand")
	}
	if s, ok := obj["provider"].(string); ok && s != "" {
		pack.Provider = s
	} else {
		return nil, errfmt.New("scenario pack is missing 'provider'", path+" has no non-empty provider field", "add `provider: <sandbox-name>` to the pack", "scenarios/README.md#writing-a-pack-by-hand")
	}
	if s, ok := obj["description"].(string); ok {
		pack.Description = s
	}
	if v, ok := obj["contractVersion"].(float64); ok {
		pack.ContractVersion = int(v)
	}
	defRaw, has := obj["definition"]
	if !has {
		return nil, errfmt.New("scenario pack is missing 'definition'", path+" has no definition field", "add a `definition:` block with steps", "scenarios/README.md#writing-a-pack-by-hand")
	}
	pack.RawDefinition = defRaw
	def, errs := ParseDefinition(defRaw)
	if len(errs) > 0 {
		return nil, errfmt.New("scenario pack definition is invalid", fmt.Sprintf("%s: %s (at %s)", path, errs[0].Message, errs[0].Pointer), "fix the definition to match schema/scenario-pack.schema.json", "scenarios/README.md#writing-a-pack-by-hand")
	}
	pack.Definition = def
	return pack, nil
}

func ListPacks(dirs ...string) ([]*Pack, map[string]error) {
	var packs []*Pack
	fails := map[string]error{}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
				continue
			}
			path := filepath.Join(dir, name)
			pack, err := LoadPack(path)
			if err != nil {
				fails[path] = err
				continue
			}
			packs = append(packs, pack)
		}
	}
	sort.Slice(packs, func(i, j int) bool { return packs[i].Name < packs[j].Name })
	return packs, fails
}
