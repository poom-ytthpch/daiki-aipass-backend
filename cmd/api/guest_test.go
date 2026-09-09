package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/poom-ytthpch/daiki-ai-passport-backend/internal/store"
	"github.com/redis/go-redis/v9"
)

func TestRestrictGuestChatForcesFastAndStripsCapabilities(t *testing.T) {
	p := store.DefaultGuestAccessPolicy()
	p.MaxCompletionTokens = 321
	body, err := restrictGuestChat([]byte(`{"model":"deep","max_tokens":9000,"research":"web","web_search":true,"tools":[{"type":"function"}],"tool_choice":"auto","attachments":["a1"],"messages":[{"role":"user","content":"hello"}]}`), p)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["model"] != "fast" {
		t.Fatalf("guest route must be fast: %#v", payload)
	}
	if payload["max_completion_tokens"] != float64(321) {
		t.Fatalf("unexpected output cap: %#v", payload)
	}
	for _, key := range []string{"max_tokens", "research", "web_search", "tools", "tool_choice", "attachments"} {
		if _, exists := payload[key]; exists {
			t.Fatalf("guest payload must strip %s: %#v", key, payload)
		}
	}
}

func TestRestrictGuestChatRejectsImages(t *testing.T) {
	p := store.DefaultGuestAccessPolicy()
	_, err := restrictGuestChat([]byte(`{"model":"fast","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,x"}}]}]}`), p)
	if err == nil {
		t.Fatal("guest image input must be rejected")
	}
}

func TestGuestSubjectIsHashedAndStable(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/guest/chat", strings.NewReader(`{}`))
	r.RemoteAddr = "203.0.113.7:54321"
	r.Header.Set("User-Agent", "guest-test")
	got := guestSubject(r)
	if !strings.HasPrefix(got, "guest:") || strings.Contains(got, "203.0.113.7") {
		t.Fatalf("guest identity must be pseudonymous: %q", got)
	}
	if got != guestSubject(r) {
		t.Fatal("guest fingerprint must be stable for identical request identity")
	}
}

func TestInferenceUpstreamUsesHermesWhenEnabled(t *testing.T) {
	a := &app{cfg: config{LiteLLMBase: "http://litellm:4000", LiteLLMKey: "lite", HermesBase: "http://hermes:8000", HermesKey: "hermes-key", HermesEnabled: true}}
	url, key, name := a.inferenceUpstream("/v1/chat/completions")
	if url != "http://hermes:8000/v1/chat/completions" || key != "hermes-key" || name != "hermes" {
		t.Fatalf("unexpected Hermes upstream: %q %q %q", url, key, name)
	}
	a.cfg.HermesEnabled = false
	url, key, name = a.inferenceUpstream("/v1/chat/completions")
	if url != "http://litellm:4000/v1/chat/completions" || key != "lite" || name != "litellm" {
		t.Fatalf("unexpected LiteLLM fallback: %q %q %q", url, key, name)
	}
}

func TestGuestRateLimit(t *testing.T) {
	addr := os.Getenv("DAIKI_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("DAIKI_TEST_REDIS_ADDR not set")
	}
	client := redis.NewClient(&redis.Options{Addr: addr, DB: 14})
	t.Cleanup(func() { _ = client.FlushDB(context.Background()).Err(); _ = client.Close() })
	a := &app{redis: client}
	p := store.DefaultGuestAccessPolicy()
	p.RequestsPerHour = 2
	p.MinIntervalSeconds = 1
	if retry, err := a.enforceGuestRate(context.Background(), "guest:test", p); err != nil || retry != 0 {
		t.Fatalf("first request failed retry=%v err=%v", retry, err)
	}
	if retry, err := a.enforceGuestRate(context.Background(), "guest:test", p); err == nil || retry <= 0 {
		t.Fatalf("cooldown should reject retry=%v err=%v", retry, err)
	}
	time.Sleep(1100 * time.Millisecond)
	if retry, err := a.enforceGuestRate(context.Background(), "guest:test", p); err != nil || retry != 0 {
		t.Fatalf("second request failed retry=%v err=%v", retry, err)
	}
}

func TestRestrictedAuthenticatedRequestBypassesHermes(t *testing.T) {
	a := &app{cfg: config{LiteLLMBase: "http://litellm:4000", LiteLLMKey: "lite", HermesBase: "http://hermes:8642", HermesKey: "hermes", HermesEnabled: true}}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat", nil)
	r = r.WithContext(context.WithValue(r.Context(), appUserKey, store.User{Subject: "pending-user", Status: "pending"}))
	url, key, name := a.inferenceUpstreamForRequest(r, "/v1/chat/completions")
	if url != "http://litellm:4000/v1/chat/completions" || key != "lite" || name != "litellm" {
		t.Fatalf("pending user must bypass Hermes: %q %q %q", url, key, name)
	}
	r = r.WithContext(context.WithValue(r.Context(), appUserKey, store.User{Subject: "approved-user", Status: "approved"}))
	url, key, name = a.inferenceUpstreamForRequest(r, "/v1/chat/completions")
	if url != "http://hermes:8642/v1/chat/completions" || key != "hermes" || name != "hermes" {
		t.Fatalf("approved user should use Hermes: %q %q %q", url, key, name)
	}
}

func TestHermesFallbackDecision(t *testing.T) {
	if !shouldFallbackFromHermes("hermes", true, 503, nil) {
		t.Fatal("Hermes 5xx should fallback")
	}
	if !shouldFallbackFromHermes("hermes", true, 0, context.DeadlineExceeded) {
		t.Fatal("Hermes transport error should fallback")
	}
	if shouldFallbackFromHermes("hermes", true, 429, nil) {
		t.Fatal("Hermes 429 must preserve backpressure")
	}
	if shouldFallbackFromHermes("hermes", false, 503, nil) {
		t.Fatal("disabled fallback must remain disabled")
	}
	if shouldFallbackFromHermes("litellm", true, 503, nil) {
		t.Fatal("LiteLLM errors are not Hermes fallback candidates")
	}
}
