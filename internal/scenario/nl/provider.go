package nl

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/pikopod/pikopod/internal/errfmt"
)

// DefaultProviderName is used when llm.provider is unset.
const DefaultProviderName = "openrouter"

// Provider turns a system+user prompt into raw model text. Implementations
// own the wire format and nothing else.
type Provider interface {
	Name() string
	Complete(ctx context.Context, systemPrompt, userPrompt string) (string, error)
}

// ProviderOptions carries transport knobs used by the existing client. A
// provider may ignore options it does not support.
type ProviderOptions struct {
	APIKey     string
	Model      string
	BaseURL    string
	MaxTokens  int
	Stream     bool
	HTTPClient *http.Client
}

type providerFactory func(ProviderOptions) Provider

type providerRegistration struct {
	factory providerFactory
	keyEnvs []string
}

type configurableProvider interface {
	configure(ProviderOptions)
	providerOptions() ProviderOptions
}

var providerRegistry = map[string]providerRegistration{}

func registerProvider(name string, keyEnvs []string, factory providerFactory) {
	providerRegistry[normalizeProviderName(name)] = providerRegistration{
		factory: factory,
		keyEnvs: append([]string(nil), keyEnvs...),
	}
}

func normalizeProviderName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return DefaultProviderName
	}
	return name
}

// ProviderNames returns registered provider names in stable order.
func ProviderNames() []string {
	names := make([]string, 0, len(providerRegistry))
	for name := range providerRegistry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ProviderKeyEnvs returns the provider-native environment variables used at
// key-resolution step 3.
func ProviderKeyEnvs(name string) ([]string, bool) {
	reg, ok := providerRegistry[normalizeProviderName(name)]
	if !ok {
		return nil, false
	}
	return append([]string(nil), reg.keyEnvs...), true
}

// ValidateProvider rejects unknown names with the repository error contract.
func ValidateProvider(name string) error {
	name = normalizeProviderName(name)
	if _, ok := providerRegistry[name]; ok {
		return nil
	}
	valid := strings.Join(ProviderNames(), ", ")
	return errfmt.New(
		"unknown llm provider",
		fmt.Sprintf("%q is not registered; valid providers: %s", name, valid),
		"set llm.provider to one of: "+valid,
		"docs/config-reference.md#llm")
}

// NewProvider builds a registered provider, defaulting to OpenRouter.
func NewProvider(name string, opts ProviderOptions) (Provider, error) {
	name = normalizeProviderName(name)
	if err := ValidateProvider(name); err != nil {
		return nil, err
	}
	return providerRegistry[name].factory(opts), nil
}

type errorProvider struct {
	err error
}

func (p *errorProvider) Name() string { return "invalid" }

func (p *errorProvider) Complete(context.Context, string, string) (string, error) {
	return "", p.err
}
