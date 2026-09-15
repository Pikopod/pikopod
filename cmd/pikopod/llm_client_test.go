package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pikopod/pikopod/internal/config"
)

func openRouterStub(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("content-type", "application/json")
		content := string([]byte{123, 34, 111, 107, 34, 58, 116, 114, 117, 101, 125})
		if err := json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"message": map[string]string{"content": content},
			}},
		}); err != nil {
			t.Fatalf("encode stub response: %v", err)
		}
	}))
}

func TestNewLLMClientBaseURLOverrides(t *testing.T) {
	cfg := &config.Config{LLM: config.LLM{
		Provider:      "openrouter",
		APIKey:        "test-key",
		OpenRouterKey: "test-key",
	}}

	generic := openRouterStub(t)
	defer generic.Close()
	legacy := openRouterStub(t)
	defer legacy.Close()

	t.Setenv("PIKOPOD_LLM_BASE", generic.URL)
	t.Setenv("PIKOPOD_OPENROUTER_BASE", legacy.URL)
	client := newLLMClient(cfg, "")
	if _, err := client.CompleteJSON(context.Background(), "return ok", "payload"); err != nil {
		t.Fatalf("generic base override request failed: %v", err)
	}
	if got := client.BaseURL; got != generic.URL {
		t.Fatalf("effective generic base override = %q, want %q", got, generic.URL)
	}

	t.Setenv("PIKOPOD_LLM_BASE", "")
	client = newLLMClient(cfg, "")
	if _, err := client.CompleteJSON(context.Background(), "return ok", "payload"); err != nil {
		t.Fatalf("legacy base override request failed: %v", err)
	}
	if got := client.BaseURL; got != legacy.URL {
		t.Fatalf("effective legacy OpenRouter base override = %q, want %q", got, legacy.URL)
	}
}

func TestNewLLMClientUsesConfiguredProvider(t *testing.T) {
	cfg := &config.Config{LLM: config.LLM{
		Provider: "not-registered",
		APIKey:   "test-key",
	}}

	client := newLLMClient(cfg, "")
	if _, err := client.CompleteJSON(context.Background(), "return ok", "payload"); err == nil ||
		!strings.Contains(err.Error(), "not-registered") {
		t.Fatalf("configured provider was not selected, got error %v", err)
	}
}
