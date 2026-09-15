// Package llmprovider names the LLM providers pikopod can talk to. config
// validates against it and scenario/nl implements it; those two cannot import
// each other (nl → scenario → sandbox → replay → proxy → config).
package llmprovider

import (
	"sort"
	"strings"
)

// Default is used when llm.provider is unset.
const Default = "openrouter"

// keyEnvs lists each provider's native key variables in priority order. They
// rank below the provider-neutral PIKOPOD_LLM_KEY.
var keyEnvs = map[string][]string{
	Default: {"OPENROUTER_API_KEY", "PIKOPOD_OPENROUTER_KEY"},
}

// Normalize lowercases and trims a configured name, defaulting an empty one.
func Normalize(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return Default
	}
	return name
}

// Known reports whether name is a provider pikopod can talk to.
func Known(name string) bool {
	_, ok := keyEnvs[Normalize(name)]
	return ok
}

// Names returns every known provider in stable order.
func Names() []string {
	out := make([]string, 0, len(keyEnvs))
	for name := range keyEnvs {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// KeyEnvs returns the provider's native key variables in priority order.
// An empty result means the name is unknown.
func KeyEnvs(name string) []string {
	return append([]string(nil), keyEnvs[Normalize(name)]...)
}
