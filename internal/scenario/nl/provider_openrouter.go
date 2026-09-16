package nl

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/pikopod/pikopod/internal/errfmt"
)

// DefaultBaseURL is the OpenRouter API base.
const DefaultBaseURL = "https://openrouter.ai/api/v1"

// DefaultModel is used when pikopod.yaml sets no llm.model.
const DefaultModel = "openai/gpt-4o-mini"

const requestTimeout = 60 * time.Second

type openRouterProvider struct {
	BaseURL    string
	APIKey     string
	Model      string
	MaxTokens  int
	Stream     bool
	HTTPClient *http.Client
}

func init() {
	registerProvider(DefaultProviderName, func(opts ProviderOptions) Provider {
		return newOpenRouterProvider(opts)
	})
}

func newOpenRouterProvider(opts ProviderOptions) *openRouterProvider {
	p := &openRouterProvider{}
	p.configure(opts)
	return p
}

func (p *openRouterProvider) Name() string { return DefaultProviderName }

func (p *openRouterProvider) configure(opts ProviderOptions) {
	if opts.BaseURL == "" {
		opts.BaseURL = DefaultBaseURL
	}
	if opts.Model == "" {
		opts.Model = DefaultModel
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: requestTimeout}
	}
	p.BaseURL = opts.BaseURL
	p.APIKey = opts.APIKey
	p.Model = opts.Model
	p.MaxTokens = opts.MaxTokens
	p.Stream = opts.Stream
	p.HTTPClient = opts.HTTPClient
}

func (p *openRouterProvider) providerOptions() ProviderOptions {
	return ProviderOptions{
		APIKey:     p.APIKey,
		Model:      p.Model,
		BaseURL:    p.BaseURL,
		MaxTokens:  p.MaxTokens,
		Stream:     p.Stream,
		HTTPClient: p.HTTPClient,
	}
}

// ErrNoKey is the contract error for a missing OpenRouter BYOK key.
func ErrNoKey() error {
	return errfmt.New(
		"plain-English scenario drafting is disabled",
		"no OpenRouter key is configured (llm.api_key / PIKOPOD_LLM_KEY / OPENROUTER_API_KEY / llm.openrouter_key / PIKOPOD_OPENROUTER_KEY)",
		"add your own key to pikopod.yaml or the environment to enable `scenario create`; deterministic scenario packs work without one",
		"docs/config-reference.md#llm")
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model          string        `json:"model"`
	Messages       []chatMessage `json:"messages"`
	ResponseFormat struct {
		Type string `json:"type"`
	} `json:"response_format"`
	Temperature         float64 `json:"temperature"`
	MaxTokens           int     `json:"max_tokens,omitempty"`
	MaxCompletionTokens int     `json:"max_completion_tokens,omitempty"`
	Stream              bool    `json:"stream,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

func (p *openRouterProvider) Complete(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	if p.APIKey == "" {
		return "", ErrNoKey()
	}
	maxTokens := p.MaxTokens
	if maxTokens == 0 {
		maxTokens = 2048
	}
	reqBody := chatRequest{Model: p.Model, Temperature: 0, MaxTokens: maxTokens, Stream: p.Stream}
	reqBody.Messages = []chatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userPrompt},
	}
	reqBody.ResponseFormat.Type = "json_object"
	raw, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	req.Header.Set("authorization", "Bearer "+p.APIKey)
	req.Header.Set("content-type", "application/json")
	resp, err := p.HTTPClient.Do(req)
	if err != nil {
		return "", errfmt.Newf("cannot reach OpenRouter", "check your network and the key in pikopod.yaml", "docs/config-reference.md#llm", "%v", err)
	}
	defer resp.Body.Close()
	if p.Stream && resp.StatusCode == http.StatusOK {
		return p.readStream(resp.Body)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", errfmt.Newf("OpenRouter response could not be read", "retry; the connection dropped mid-response", "", "%v", err)
	}
	if resp.StatusCode != http.StatusOK {
		detail := fmt.Sprintf("it answered %d", resp.StatusCode)
		if os.Getenv("PIKOPOD_DEBUG") != "" {
			snippet := body
			if len(snippet) > 2048 {
				snippet = snippet[:2048]
			}
			detail += ": " + string(snippet)
		}
		return "", errfmt.New("OpenRouter refused the request", detail, "check the key is valid and has credit; set PIKOPOD_DEBUG=1 to include the response body", "docs/config-reference.md#llm")
	}
	var parsed chatResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", errfmt.Newf("OpenRouter answered strangely", "retry; if it persists, try another llm.model", "docs/config-reference.md#llm", "%v", err)
	}
	if len(parsed.Choices) == 0 {
		return "", errfmt.New("OpenRouter returned no choices", "the model produced no output", "retry, or try another llm.model", "docs/config-reference.md#llm")
	}
	return parsed.Choices[0].Message.Content, nil
}

// readStream accumulates SSE deltas into the completion text. A server that
// ignored stream:true and answered plain JSON is parsed as a normal completion.
func (p *openRouterProvider) readStream(body io.Reader) (string, error) {
	return readChatStream(body, "OpenRouter")
}

func readChatStream(body io.Reader, provider string) (string, error) {
	raw, err := io.ReadAll(io.LimitReader(body, 16<<20))
	if err != nil {
		return "", errfmt.Newf(provider+" stream broke mid-response", "retry; the connection dropped", "docs/config-reference.md#llm", "%v", err)
	}
	if !bytes.Contains(raw, []byte("data: ")) {
		var parsed chatResponse
		if json.Unmarshal(raw, &parsed) == nil && len(parsed.Choices) > 0 {
			return parsed.Choices[0].Message.Content, nil
		}
	}
	var out strings.Builder
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		if len(chunk.Choices) > 0 {
			out.WriteString(chunk.Choices[0].Delta.Content)
		}
	}
	if err := scanner.Err(); err != nil {
		return "", errfmt.Newf(provider+" stream broke mid-response", "retry; the connection dropped", "docs/config-reference.md#llm", "%v", err)
	}
	if out.Len() == 0 {
		return "", errfmt.New(provider+" stream carried no content", "the model produced no output", "retry, or try another llm.model", "docs/config-reference.md#llm")
	}
	return out.String(), nil
}
