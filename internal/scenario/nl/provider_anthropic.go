package nl

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/pikopod/pikopod/internal/errfmt"
)

const (
	anthropicProviderName = "anthropic"
	anthropicBaseURL      = "https://api.anthropic.com"
	anthropicDefaultModel = "claude-3-5-haiku-20241022"
	anthropicAPIVersion   = "2023-06-01"
)

type anthropicProvider struct {
	opts ProviderOptions
}

func init() {
	registerProvider(anthropicProviderName, func(opts ProviderOptions) Provider {
		return newAnthropicProvider(opts)
	})
}

func newAnthropicProvider(opts ProviderOptions) *anthropicProvider {
	p := &anthropicProvider{}
	p.configure(opts)
	return p
}

func (p *anthropicProvider) Name() string { return anthropicProviderName }

func (p *anthropicProvider) configure(opts ProviderOptions) {
	if opts.BaseURL == "" {
		opts.BaseURL = anthropicBaseURL
	}
	if opts.Model == "" {
		opts.Model = anthropicDefaultModel
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: requestTimeout}
	}
	p.opts = opts
}

func (p *anthropicProvider) providerOptions() ProviderOptions { return p.opts }

type anthropicRequest struct {
	Model     string        `json:"model"`
	System    string        `json:"system"`
	Messages  []chatMessage `json:"messages"`
	MaxTokens int           `json:"max_tokens"`
}

type anthropicResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

func (p *anthropicProvider) Complete(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	if p.opts.APIKey == "" {
		return "", errfmt.New(
			"plain-English scenario drafting is disabled",
			"no Anthropic key is configured (llm.api_key / PIKOPOD_LLM_KEY / ANTHROPIC_API_KEY)",
			"set llm.api_key, PIKOPOD_LLM_KEY, or ANTHROPIC_API_KEY; deterministic scenario packs work without one",
			"docs/config-reference.md#llm",
		)
	}
	maxTokens := p.opts.MaxTokens
	if maxTokens == 0 {
		maxTokens = 2048
	}
	reqBody := anthropicRequest{
		Model:     p.opts.Model,
		System:    systemPrompt,
		Messages:  []chatMessage{{Role: "user", Content: userPrompt}},
		MaxTokens: maxTokens,
	}
	raw, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}
	endpoint := strings.TrimRight(p.opts.BaseURL, "/") + "/v1/messages"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	req.Header.Set("x-api-key", p.opts.APIKey)
	req.Header.Set("anthropic-version", anthropicAPIVersion)
	req.Header.Set("content-type", "application/json")
	resp, err := p.opts.HTTPClient.Do(req)
	if err != nil {
		return "", errfmt.Newf("cannot reach Anthropic", "check your network and Anthropic API configuration", "docs/config-reference.md#llm", "%v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", errfmt.Newf("Anthropic response could not be read", "retry; the connection dropped mid-response", "", "%v", err)
	}
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusUnauthorized {
			return "", errfmt.New(
				"Anthropic refused the request",
				fmt.Sprintf("it answered %d; the configured Anthropic key was not accepted", resp.StatusCode),
				"set llm.api_key, PIKOPOD_LLM_KEY, or ANTHROPIC_API_KEY to a valid Anthropic API key",
				"docs/config-reference.md#llm",
			)
		}
		return "", errfmt.New(
			"Anthropic refused the request",
			fmt.Sprintf("it answered %d", resp.StatusCode),
			"check the configured Anthropic key and llm.model, then retry",
			"docs/config-reference.md#llm",
		)
	}
	var parsed anthropicResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", errfmt.Newf("Anthropic answered strangely", "retry; if it persists, check llm.model", "docs/config-reference.md#llm", "%v", err)
	}
	var text strings.Builder
	for _, block := range parsed.Content {
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}
	if text.Len() == 0 {
		return "", errfmt.New("Anthropic returned no content", "the model produced no text blocks", "retry, or try another llm.model", "docs/config-reference.md#llm")
	}
	return text.String(), nil
}
