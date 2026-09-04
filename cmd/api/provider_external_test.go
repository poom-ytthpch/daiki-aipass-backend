package main

import (
	"testing"

	"github.com/poom-ytthpch/daiki-ai-passport-backend/internal/store"
)

func TestExternalProviderAliases(t *testing.T) {
	cases := map[string]string{
		"openai":     "openai-compatible",
		"groq":       "openai-compatible",
		"openrouter": "openai-compatible",
		"together":   "openai-compatible",
		"fireworks":  "openai-compatible",
		"deepinfra":  "openai-compatible",
		"xai":        "openai-compatible",
		"mistral":    "openai-compatible",
		"cerebras":   "openai-compatible",
		"anthropic":  "anthropic",
		"gemini":     "gemini",
		"vllm":       "vllm",
		"lmstudio":   "lmstudio",
		"ollama":     "ollama",
	}
	for input, want := range cases {
		if got := normalizeProviderType(input); got != want {
			t.Fatalf("normalizeProviderType(%q)=%q want %q", input, got, want)
		}
	}
	if got := normalizeProviderType("unknown-provider"); got != "" {
		t.Fatalf("unknown provider should be rejected, got %q", got)
	}
}

func TestHostedProviderDefaults(t *testing.T) {
	cases := []string{"openai", "groq", "openrouter", "together", "fireworks", "deepinfra", "xai", "mistral", "cerebras", "anthropic", "gemini"}
	for _, provider := range cases {
		if got := providerTypeDefaultBase(provider); got == "" {
			t.Fatalf("expected default base for %s", provider)
		}
	}
}

func TestExternalProviderLiteLLMMapping(t *testing.T) {
	generic := store.ModelProvider{ProviderType: "openai-compatible", BaseURL: "https://api.groq.com/openai/v1"}
	if got := providerLiteLLMBase(generic); got != "https://api.groq.com/openai/v1" {
		t.Fatalf("generic base changed unexpectedly: %q", got)
	}
	if got := providerLiteLLMModel(generic, "qwen/qwen3-32b"); got != "openai/qwen/qwen3-32b" {
		t.Fatalf("generic model mapping=%q", got)
	}

	anthropic := store.ModelProvider{ProviderType: "anthropic", BaseURL: "https://api.anthropic.com"}
	if got := providerDiscoveryURL(anthropic); got != "https://api.anthropic.com/v1/models" {
		t.Fatalf("anthropic discovery=%q", got)
	}
	if got := providerLiteLLMModel(anthropic, "claude-sonnet-4-5"); got != "anthropic/claude-sonnet-4-5" {
		t.Fatalf("anthropic model mapping=%q", got)
	}

	gemini := store.ModelProvider{ProviderType: "gemini", BaseURL: "https://generativelanguage.googleapis.com/v1beta"}
	if got := providerDiscoveryURL(gemini); got != "https://generativelanguage.googleapis.com/v1beta/models" {
		t.Fatalf("gemini discovery=%q", got)
	}
	if got := providerLiteLLMModel(gemini, "models/gemini-2.5-flash"); got != "gemini/gemini-2.5-flash" {
		t.Fatalf("gemini model mapping=%q", got)
	}
}
