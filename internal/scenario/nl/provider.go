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

const DefaultProviderName = llmprovider.Default

type Provider interface {
	Name() string
	Complete(ctx context.Context, systemPrompt, userPrompt string) (string, error)
}

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

func registerProvider(name string, factory providerFactory) {
	if !llmprovider.Known(name) {
		panic("nl: provider " + name + " is not in the llmprovider table")
	}
	providerRegistry[normalizeProviderName(name)] = factory
}

func normalizeProviderName(name string) string { return llmprovider.Normalize(name) }

func ProviderNames() []string {
	names := make([]string, 0, len(providerRegistry))
	for name := range providerRegistry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

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
