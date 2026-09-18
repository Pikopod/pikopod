package nl

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func openAIStub(t *testing.T, status int, content string, inspect func(*http.Request, chatRequest)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if inspect != nil {
			inspect(r, req)
		}
		w.WriteHeader(status)
		if status != http.StatusOK {
			_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
			return
		}
		if err := json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"message": map[string]string{"content": content},
			}},
		}); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
}

func TestOpenAIProviderDefaults(t *testing.T) {
	t.Setenv("PIKOPOD_LLM_BASE", "")
	client := NewClientForProvider("openai", "test-key", "")
	if client.BaseURL != "https://api.openai.com/v1" {
		t.Fatalf("default base URL = %q", client.BaseURL)
	}
	if client.Model != "gpt-4o-mini" {
		t.Fatalf("default model = %q", client.Model)
	}
}

func TestOpenAIProviderRequestContractAndBareJSON(t *testing.T) {
	var sawRequest bool
	srv := openAIStub(t, http.StatusOK, `{"ok":true}`, func(r *http.Request, req chatRequest) {
		sawRequest = true
		if r.URL.Path != "/chat/completions" {
			t.Errorf("request path = %q, want /chat/completions", r.URL.Path)
		}
		if r.URL.Query().Get("key") != "" {
			t.Error("OpenAI key must not be sent in the query string")
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("authorization header = %q", got)
		}
		if req.Model != "gpt-4o-mini" {
			t.Errorf("default model = %q, want gpt-4o-mini", req.Model)
		}
		if req.ResponseFormat.Type != "json_object" {
			t.Errorf("response format = %q, want json_object", req.ResponseFormat.Type)
		}
		if req.MaxCompletionTokens != 2048 || req.MaxTokens != 0 {
			t.Errorf("token fields = max_completion_tokens:%d max_tokens:%d", req.MaxCompletionTokens, req.MaxTokens)
		}
		if len(req.Messages) != 2 || req.Messages[0].Role != "system" || req.Messages[1].Role != "user" {
			t.Errorf("unexpected messages: %+v", req.Messages)
		}
	})
	defer srv.Close()

	t.Setenv("PIKOPOD_LLM_BASE", srv.URL)
	client := NewClientForProvider("openai", "test-key", "")
	got, err := client.CompleteJSON(context.Background(), "return an object", "payload")
	if err != nil {
		t.Fatal(err)
	}
	if !sawRequest {
		t.Fatal("OpenAI stub received no request")
	}
	obj, ok := got.(map[string]any)
	if !ok || obj["ok"] != true {
		t.Fatalf("unexpected parsed JSON: %#v", got)
	}
}

func TestOpenAIProviderKeepsFencedJSONFallback(t *testing.T) {
	srv := openAIStub(t, http.StatusOK, "```json\n{\"ok\":true}\n```", nil)
	defer srv.Close()

	t.Setenv("PIKOPOD_LLM_BASE", srv.URL)
	client := NewClientForProvider("openai", "test-key", "")
	got, err := client.CompleteJSON(context.Background(), "return an object", "payload")
	if err != nil {
		t.Fatal(err)
	}
	obj, ok := got.(map[string]any)
	if !ok || obj["ok"] != true {
		t.Fatalf("unexpected parsed JSON: %#v", got)
	}
}

func TestOpenAIProviderUnauthorizedNamesOpenAIKey(t *testing.T) {
	srv := openAIStub(t, http.StatusUnauthorized, "", nil)
	defer srv.Close()

	t.Setenv("PIKOPOD_LLM_BASE", srv.URL)
	client := NewClientForProvider("openai", "test-key", "")
	_, err := client.CompleteJSON(context.Background(), "return an object", "payload")
	if err == nil || !strings.Contains(err.Error(), "OPENAI_API_KEY") {
		t.Fatalf("401 error must point to OPENAI_API_KEY, got %v", err)
	}
}

func TestOpenAIProviderNonJSONFailsAfterOneRetry(t *testing.T) {
	var calls atomic.Int32
	srv := openAIStub(t, http.StatusOK, "not json", func(*http.Request, chatRequest) {
		calls.Add(1)
	})
	defer srv.Close()

	t.Setenv("PIKOPOD_LLM_BASE", srv.URL)
	client := NewClientForProvider("openai", "test-key", "")
	if _, err := client.CompleteJSON(context.Background(), "return an object", "payload"); err == nil {
		t.Fatal("non-JSON OpenAI output must fail")
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("completion calls = %d, want 2", got)
	}
}

func TestOpenAIProviderMissingKeyNamesOpenAIKey(t *testing.T) {
	client := NewClientForProvider("openai", "", "")
	_, err := client.CompleteJSON(context.Background(), "return an object", "payload")
	if err == nil || !strings.Contains(err.Error(), "OPENAI_API_KEY") {
		t.Fatalf("missing-key error must point to OPENAI_API_KEY, got %v", err)
	}
}

func TestOpenAIProviderStreamsChatDeltas(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		if !req.Stream {
			t.Error("streaming client must send stream=true")
		}
		w.Header().Set("content-type", "text/event-stream")
		for _, content := range []string{`{"ok":`, `true}`} {
			chunk, err := json.Marshal(map[string]any{
				"choices": []any{map[string]any{
					"delta": map[string]string{"content": content},
				}},
			})
			if err != nil {
				t.Errorf("encode stream chunk: %v", err)
				return
			}
			_, _ = w.Write([]byte("data: " + string(chunk) + "\n\n"))
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	t.Setenv("PIKOPOD_LLM_BASE", srv.URL)
	client := NewClientForProvider("openai", "test-key", "")
	client.Stream = true
	got, err := client.CompleteJSON(context.Background(), "return an object", "payload")
	if err != nil {
		t.Fatal(err)
	}
	obj, ok := got.(map[string]any)
	if !ok || obj["ok"] != true {
		t.Fatalf("unexpected streamed JSON: %#v", got)
	}
}
