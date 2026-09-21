package config

import (
	"os"
	"strings"
	"testing"
)

func clearLLMEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"PIKOPOD_LLM_KEY",
		"OPENROUTER_API_KEY",
		"OPENAI_API_KEY",
		"ANTHROPIC_API_KEY",
		"PIKOPOD_OPENROUTER_KEY",
		"PIKOPOD_OPENROUTER_MODEL",
		"OPENROUTER_MODEL",
	} {
		t.Setenv(name, "")
	}
}

func TestLLMProviderDefaultsAndLegacyKeys(t *testing.T) {
	clearLLMEnv(t)

	path := writeCfg(t, "upstreams: {}\nllm:\n  openrouter_key: legacy-file\n")
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLM.Provider != "openrouter" {
		t.Fatalf("default provider = %q, want openrouter", cfg.LLM.Provider)
	}
	if cfg.LLM.APIKey != "legacy-file" || cfg.LLM.OpenRouterKey != "legacy-file" {
		t.Fatalf("legacy file key did not resolve: %+v", cfg.LLM)
	}

	clearLLMEnv(t)
	t.Setenv("PIKOPOD_OPENROUTER_KEY", "legacy-env")
	cfg, err = Load(writeCfg(t, "upstreams: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLM.APIKey != "legacy-env" {
		t.Fatalf("legacy env key = %q, want legacy-env", cfg.LLM.APIKey)
	}
}

func TestLLMKeyResolutionPrecedence(t *testing.T) {
	clearLLMEnv(t)
	t.Setenv("PIKOPOD_LLM_KEY", "generic-env")
	t.Setenv("OPENROUTER_API_KEY", "provider-env")
	t.Setenv("PIKOPOD_OPENROUTER_KEY", "legacy-env")

	path := writeCfg(t, "upstreams: {}\nllm:\n  api_key: file-key\n  openrouter_key: legacy-file\n")
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLM.APIKey != "file-key" {
		t.Fatalf("llm.api_key must win, got %q", cfg.LLM.APIKey)
	}

	clearLLMEnv(t)
	t.Setenv("PIKOPOD_LLM_KEY", "generic-env")
	t.Setenv("OPENROUTER_API_KEY", "provider-env")
	t.Setenv("PIKOPOD_OPENROUTER_KEY", "legacy-env")
	cfg, err = Load(writeCfg(t, "upstreams: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLM.APIKey != "generic-env" {
		t.Fatalf("PIKOPOD_LLM_KEY must beat provider env, got %q", cfg.LLM.APIKey)
	}

	clearLLMEnv(t)
	t.Setenv("OPENROUTER_API_KEY", "provider-env")
	t.Setenv("PIKOPOD_OPENROUTER_KEY", "legacy-env")
	cfg, err = Load(writeCfg(t, "upstreams: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLM.APIKey != "provider-env" {
		t.Fatalf("provider env must beat legacy env, got %q", cfg.LLM.APIKey)
	}
}

func TestOpenAIKeyResolutionPrecedence(t *testing.T) {
	clearLLMEnv(t)
	t.Setenv("PIKOPOD_LLM_KEY", "generic-env")
	t.Setenv("OPENAI_API_KEY", "provider-env")

	path := writeCfg(t, "upstreams: {}\nllm:\n  provider: openai\n  api_key: file-key\n")
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLM.APIKey != "file-key" {
		t.Fatalf("llm.api_key must win for OpenAI, got %q", cfg.LLM.APIKey)
	}

	clearLLMEnv(t)
	t.Setenv("PIKOPOD_LLM_KEY", "generic-env")
	t.Setenv("OPENAI_API_KEY", "provider-env")
	cfg, err = Load(writeCfg(t, "upstreams: {}\nllm:\n  provider: openai\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLM.APIKey != "generic-env" {
		t.Fatalf("PIKOPOD_LLM_KEY must beat OPENAI_API_KEY, got %q", cfg.LLM.APIKey)
	}

	clearLLMEnv(t)
	t.Setenv("OPENAI_API_KEY", "provider-env")
	cfg, err = Load(writeCfg(t, "upstreams: {}\nllm:\n  provider: openai\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLM.APIKey != "provider-env" {
		t.Fatalf("OPENAI_API_KEY = %q, want provider-env", cfg.LLM.APIKey)
	}
}

func TestUnknownLLMProviderNamesValidChoices(t *testing.T) {
	clearLLMEnv(t)
	_, err := Load(writeCfg(t, "upstreams: {}\nllm:\n  provider: made-up\n"))
	if err == nil {
		t.Fatal("unknown provider must be rejected")
	}
	if !strings.Contains(err.Error(), "unknown llm provider") || !strings.Contains(err.Error(), "openrouter") {
		t.Fatalf("unknown provider error must name valid providers: %v", err)
	}
}
