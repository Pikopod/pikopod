package llmprovider

import (
	"sort"
	"strings"
)

const Default = "openrouter"

var keyEnvs = map[string][]string{
	Default:     {"OPENROUTER_API_KEY", "PIKOPOD_OPENROUTER_KEY"},
	"openai":    {"OPENAI_API_KEY"},
	"anthropic": {"ANTHROPIC_API_KEY"},
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

func KeyEnvs(name string) []string {
	return append([]string(nil), keyEnvs[Normalize(name)]...)
}
