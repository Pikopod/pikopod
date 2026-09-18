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
	openAIProviderName = "openai"
	openAIBaseURL      = "https://api.openai.com/v1"
	openAIDefaultModel = "gpt-4o-mini"
)

type openAIProvider struct {
	opts ProviderOptions
}

func init() {
	registerProvider(openAIProviderName, func(opts ProviderOptions) Provider {
		return newOpenAIProvider(opts)
	})
}

func newOpenAIProvider(opts ProviderOptions) *openAIProvider {
	p := &openAIProvider{}
	p.configure(opts)
	return p
}

func (p *openAIProvider) Name() string { return openAIProviderName }

func (p *openAIProvider) configure(opts ProviderOptions) {
	if opts.BaseURL == "" {
		opts.BaseURL = openAIBaseURL
	}
	if opts.Model == "" {
		opts.Model = openAIDefaultModel
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: requestTimeout}
	}
	p.opts = opts
}

func (p *openAIProvider) providerOptions() ProviderOptions { return p.opts }

func (p *openAIProvider) Complete(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	if p.opts.APIKey == "" {
		return "", errfmt.New(
			"plain-English scenario drafting is disabled",
			"no OpenAI key is configured (llm.api_key / PIKOPOD_LLM_KEY / OPENAI_API_KEY)",
			"set llm.api_key, PIKOPOD_LLM_KEY, or OPENAI_API_KEY; deterministic scenario packs work without one",
			"docs/config-reference.md#llm",
		)
	}
	maxTokens := p.opts.MaxTokens
	if maxTokens == 0 {
		maxTokens = 2048
	}
	reqBody := chatRequest{Model: p.opts.Model, Temperature: 0, MaxCompletionTokens: maxTokens, Stream: p.opts.Stream}
	reqBody.Messages = []chatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userPrompt},
	}
	reqBody.ResponseFormat.Type = "json_object"
	raw, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}
	endpoint := strings.TrimRight(p.opts.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	req.Header.Set("authorization", "Bearer "+p.opts.APIKey)
	req.Header.Set("content-type", "application/json")
	resp, err := p.opts.HTTPClient.Do(req)
	if err != nil {
		return "", errfmt.Newf("cannot reach OpenAI", "check your network and OpenAI API configuration", "docs/config-reference.md#llm", "%v", err)
	}
	defer resp.Body.Close()
	if p.opts.Stream && resp.StatusCode == http.StatusOK {
		return readChatStream(resp.Body, "OpenAI")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", errfmt.Newf("OpenAI response could not be read", "retry; the connection dropped mid-response", "", "%v", err)
	}
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusUnauthorized {
			return "", errfmt.New(
				"OpenAI refused the request",
				fmt.Sprintf("it answered %d; the configured OpenAI key was not accepted", resp.StatusCode),
				"set llm.api_key, PIKOPOD_LLM_KEY, or OPENAI_API_KEY to a valid OpenAI API key",
				"docs/config-reference.md#llm",
			)
		}
		return "", errfmt.New(
			"OpenAI refused the request",
			fmt.Sprintf("it answered %d", resp.StatusCode),
			"check the configured OpenAI key and llm.model, then retry",
			"docs/config-reference.md#llm",
		)
	}
	var parsed chatResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", errfmt.Newf("OpenAI answered strangely", "retry; if it persists, check llm.model", "docs/config-reference.md#llm", "%v", err)
	}
	if len(parsed.Choices) == 0 {
		return "", errfmt.New("OpenAI returned no choices", "the model produced no output", "retry, or try another llm.model", "docs/config-reference.md#llm")
	}
	return parsed.Choices[0].Message.Content, nil
}
