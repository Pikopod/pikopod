// Package llmprovider names the LLM providers pikopod can talk to. config
// validates against it and scenario/nl implements it; those two cannot import
// each other (nl → scenario → sandbox → replay → proxy → config).
package llmprovider

import (
	"sort"
	"strings"
)

const Default = "openrouter"

// Priority order within a provider. These rank below PIKOPOD_LLM_KEY.
var keyEnvs = map[string][]string{
	Default: {"OPENROUTER_API_KEY", "PIKOPOD_OPENROUTER_KEY"},
}

func Normalize(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return Default
	}
	return name
}

func Known(name string) bool {
	_, ok := keyEnvs[Normalize(name)]
	return ok
}

func Names() []string {
	out := make([]string, 0, len(keyEnvs))
	for name := range keyEnvs {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Empty when the name is unknown.
func KeyEnvs(name string) []string {
	return append([]string(nil), keyEnvs[Normalize(name)]...)
}
