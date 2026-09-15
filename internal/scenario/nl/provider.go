package nl

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/pikopod/pikopod/internal/errfmt"
	"github.com/pikopod/pikopod/internal/llmprovider"
)

// DefaultProviderName is used when llm.provider is unset.
const DefaultProviderName = llmprovider.Default

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

type configurableProvider interface {
	configure(ProviderOptions)
	providerOptions() ProviderOptions
}

var providerRegistry = map[string]providerFactory{}

// Panics at init on an unlisted name: config validates against the table
// without importing this package, so a name it lacks is unreachable.
func registerProvider(name string, factory providerFactory) {
	if !llmprovider.Known(name) {
		panic("nl: provider " + name + " is not in the llmprovider table")
	}
	providerRegistry[normalizeProviderName(name)] = factory
}

func normalizeProviderName(name string) string { return llmprovider.Normalize(name) }

// ProviderNames returns registered provider names in stable order.
func ProviderNames() []string {
	names := make([]string, 0, len(providerRegistry))
	for name := range providerRegistry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
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
	return providerRegistry[name](opts), nil
}

type errorProvider struct {
	err error
}

func (p *errorProvider) Name() string { return "invalid" }

func (p *errorProvider) Complete(context.Context, string, string) (string, error) {
	return "", p.err
}
