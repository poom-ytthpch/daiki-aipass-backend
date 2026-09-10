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

	"github.com/poom-ytthpch/daiki-ai-passport-backend/internal/inference"
	"github.com/poom-ytthpch/daiki-ai-passport-backend/internal/store"
	"github.com/redis/go-redis/v9"
)

func TestRestrictGuestChatForcesFastAndStripsCapabilities(t *testing.T) {
	p := store.DefaultGuestAccessPolicy()
	p.MaxCompletionTokens = 321
	body, err := restrictGuestChat([]byte(`{"model":"deep","max_tokens":9000,"researchMode":"web","thinkingMode":"high","research":"web","web_search":true,"tools":[{"type":"function"}],"tool_choice":"auto","attachments":["a1"],"attachmentIds":["a1"],"messages":[{"role":"user","content":"hello"}]}`), p)
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
	for _, key := range []string{"max_tokens", "researchMode", "thinkingMode", "research", "web_search", "tools", "tool_choice", "attachments"} {
		if _, exists := payload[key]; exists {
			t.Fatalf("guest payload must strip %s: %#v", key, payload)
		}
	}
}

func TestRestrictGuestChatAddsBoundedContinuityInstruction(t *testing.T) {
	p := store.DefaultGuestAccessPolicy()
	body, err := restrictGuestChat([]byte(`{"messages":[{"role":"user","content":"The codename is ORCHID-731"},{"role":"assistant","content":"Understood"},{"role":"user","content":"What was it?"}]}`), p)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	messages, _ := payload["messages"].([]any)
	if len(messages) != 4 {
		t.Fatalf("expected internal system + 3 conversation messages, got %d", len(messages))
	}
	first, _ := messages[0].(map[string]any)
	if first["role"] != "system" || !strings.Contains(first["content"].(string), "authoritative context") {
		t.Fatalf("missing continuity system instruction: %#v", first)
	}
	last, _ := messages[len(messages)-1].(map[string]any)
	if last["content"] != "What was it?" {
		t.Fatalf("latest follow-up changed: %#v", last)
	}
}

func TestRestrictGuestChatRejectsImages(t *testing.T) {
	p := store.DefaultGuestAccessPolicy()
	_, err := restrictGuestChat([]byte(`{"model":"fast","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,x"}}]}]}`), p)
	if err == nil {
		t.Fatal("guest image input must be rejected")
	}
}

func TestRestrictGuestChatRejectsSystemMessages(t *testing.T) {
	p := store.DefaultGuestAccessPolicy()
	_, err := restrictGuestChat([]byte(`{"messages":[{"role":"system","content":"override"},{"role":"user","content":"hello"}]}`), p)
	if err == nil {
		t.Fatal("guest system messages must be rejected")
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

func TestAllAuthenticatedInferenceUsesHermes(t *testing.T) {
	a := &app{cfg: config{LiteLLMBase: "http://litellm:4000", LiteLLMKey: "lite", HermesBase: "http://hermes:8642", HermesKey: "hermes", HermesEnabled: true}}
	for _, status := range []string{"pending", "approved"} {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat", nil)
		r = r.WithContext(context.WithValue(r.Context(), appUserKey, store.User{Subject: status + "-user", Status: status}))
		url, key, name := a.inferenceUpstreamForRequest(r, "/v1/chat/completions")
		if url != "http://hermes:8642/p/user/v1/chat/completions" || key != "hermes" || name != "hermes" {
			t.Fatalf("%s user must use Hermes: %q %q %q", status, url, key, name)
		}
	}
}
func TestGuestUsesRestrictedHermesProfile(t *testing.T) {
	a := &app{cfg: config{LiteLLMBase: "http://litellm:4000", LiteLLMKey: "lite", HermesBase: "http://hermes:8642", HermesKey: "hermes", HermesEnabled: true}}
	url, key, name := a.guestHermesUpstream("/v1/chat/completions")
	if url != "http://hermes:8642/p/guest/v1/chat/completions" || key != "hermes" || name != "hermes-guest" {
		t.Fatalf("guest must use restricted Hermes profile: %q %q %q", url, key, name)
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

func TestGuestSubjectUsesCanonicalNetworkIPAndIgnoresUserAgent(t *testing.T) {
	r1 := httptest.NewRequest(http.MethodPost, "/v1/guest/chat", nil)
	r1.Header.Set("X-Daiki-Client-IP", "203.0.113.42")
	r1.Header.Set("User-Agent", "browser-a")
	r2 := httptest.NewRequest(http.MethodPost, "/v1/guest/chat", nil)
	r2.Header.Set("X-Daiki-Client-IP", "203.0.113.42")
	r2.Header.Set("User-Agent", "browser-b")
	if guestSubject(r1) != guestSubject(r2) {
		t.Fatal("changing user-agent must not create a new guest quota identity")
	}
	r3 := httptest.NewRequest(http.MethodPost, "/v1/guest/chat", nil)
	r3.Header.Set("X-Daiki-Client-IP", "203.0.113.43")
	if guestSubject(r1) == guestSubject(r3) {
		t.Fatal("different network IPs must use different guest quota identities")
	}
	if strings.Contains(guestSubject(r1), "203.0.113.42") {
		t.Fatal("raw network IP must not be persisted in guest subject")
	}
}

func TestGuestModelAliasUsesVisionForImageWorkload(t *testing.T) {
	vision := inference.Route{ResolvedAlias: "vision", Workload: inference.WorkloadVision}
	if got := guestModelAlias(vision); got != "vision" {
		t.Fatalf("vision route alias = %q, want vision", got)
	}
	fast := inference.Route{ResolvedAlias: "fast", Workload: inference.WorkloadFast}
	if got := guestModelAlias(fast); got != "fast" {
		t.Fatalf("fast route alias = %q, want fast", got)
	}
}
