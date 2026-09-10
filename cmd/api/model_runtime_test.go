package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/poom-ytthpch/daiki-ai-passport-backend/internal/store"
)

func TestParseRateLimitObservationITPM(t *testing.T) {
	body := []byte(`API call failed after 3 retries: HTTP 429: Request too large for model on input tokens per minute (ITPM): Limit 7000, Requested 11705, please reduce your message size`)
	got := parseRateLimitObservation(429, body)
	if got.Kind != "itpm" || got.Limit != 7000 || got.Requested != 11705 {
		t.Fatalf("unexpected observation: %#v", got)
	}
}

func TestTrimChatContextKeepsNewestTurn(t *testing.T) {
	messages := []map[string]any{{"role": "user", "content": "old " + strings.Repeat("a", 12000)}, {"role": "assistant", "content": strings.Repeat("b", 12000)}, {"role": "user", "content": "LATEST QUESTION"}}
	body, _ := json.Marshal(map[string]any{"model": "m", "messages": messages})
	trimmed, orig, final, err := trimChatContext(body, 1500)
	if err != nil {
		t.Fatal(err)
	}
	if final >= orig || final > 1700 {
		t.Fatalf("context was not compacted enough orig=%d final=%d", orig, final)
	}
	if !strings.Contains(string(trimmed), "LATEST QUESTION") {
		t.Fatalf("newest turn was lost: %s", trimmed)
	}
}

func TestModelRequestRecoversITPMBeforeReturning(t *testing.T) {
	calls := 0
	var secondTokens int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		b, _ := io.ReadAll(r.Body)
		if calls == 1 {
			w.WriteHeader(429)
			_, _ = w.Write([]byte(`RateLimitError input tokens per minute (ITPM): Limit 7000, Requested 11705`))
			return
		}
		secondTokens = estimateChatInputTokens(b)
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"total_tokens":10}}`))
	}))
	defer srv.Close()
	payload, _ := json.Marshal(map[string]any{"model": "qwen", "messages": []map[string]any{{"role": "user", "content": strings.Repeat("x", 46000)}, {"role": "user", "content": "keep this"}}})
	a := &app{inferenceHTTP: srv.Client()}
	makeReq := func(body []byte) (*http.Request, error) {
		return http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, strings.NewReader(string(body)))
	}
	resp, _, _, meta, err := a.doModelRequestWithRecovery(context.Background(), payload, "qwen", "user", makeReq)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || calls != 2 {
		t.Fatalf("status=%d calls=%d meta=%#v", resp.StatusCode, calls, meta)
	}
	if !meta.ContextTrimmed || secondTokens >= 7000 {
		t.Fatalf("retry did not compact below limit tokens=%d meta=%#v", secondTokens, meta)
	}
}

func TestPreflightFallbackIncludesHermesOverhead(t *testing.T) {
	body, _ := json.Marshal(map[string]any{"model": "groq-qwen-qwen3.8-27b", "messages": []map[string]any{{"role": "user", "content": "hello"}}})
	m := store.ProviderModel{
		LiteLLMModelName:    "groq-qwen-qwen3.8-27b",
		ITPMLimit:           7000,
		AgentOverheadTokens: 7600,
		ContextStrategy:     "adaptive",
		FallbackModelName:   "groq-openai-gpt-oss-20b",
	}
	overhead := runtimeAgentOverhead(m, "user")
	if !shouldPreflightFallback(m, body, overhead) {
		t.Fatalf("expected preflight fallback: estimated=%d overhead=%d budget=%d", estimateChatInputTokens(body), overhead, runtimeSafeInputBudget(m, overhead))
	}
}

func TestResearchProfileUsesResearchOverhead(t *testing.T) {
	body, _ := json.Marshal(map[string]any{"model": "qwen", "messages": []map[string]any{{"role": "user", "content": strings.Repeat("research ", 700)}}})
	m := store.ProviderModel{ITPMLimit: 7000, AgentOverheadTokens: 5300, ResearchOverheadTokens: 800, ContextStrategy: "adaptive", FallbackModelName: "fallback"}
	userOverhead := runtimeAgentOverhead(m, "user")
	researchOverhead := runtimeAgentOverhead(m, "research")
	if userOverhead != 5300 || researchOverhead != 800 {
		t.Fatalf("unexpected overheads user=%d research=%d", userOverhead, researchOverhead)
	}
	if !shouldPreflightFallback(m, body, userOverhead) {
		t.Fatal("user profile should fallback with the larger agent overhead")
	}
	if shouldPreflightFallback(m, body, researchOverhead) {
		t.Fatal("research profile should retain headroom for the same payload")
	}
}

func TestModelRequestRecoversHermesSSESoft429BeforeVisibleToken(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("content-type", "text/event-stream")
		if calls == 1 {
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n")
			_, _ = io.WriteString(w, ": keepalive\n\n")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"error\"}],\"usage\":{\"total_tokens\":0},\"error\":{\"message\":\"HTTP 429: litellm.RateLimitError input tokens per minute (ITPM): Limit 7000, Requested 10157\",\"type\":\"agent_error\"},\"hermes\":{\"failed\":true}}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"RECOVERED\"},\"finish_reason\":null}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	a := &app{inferenceHTTP: srv.Client()}
	payload, _ := json.Marshal(map[string]any{"model": "qwen", "messages": []map[string]any{{"role": "user", "content": "search overdrive.qd.je"}}, "stream": true})
	makeReq := func(body []byte) (*http.Request, error) {
		return http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, strings.NewReader(string(body)))
	}
	resp, _, _, meta, err := a.doModelRequestWithRecovery(context.Background(), payload, "qwen", "user", makeReq)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if calls != 2 {
		t.Fatalf("expected internal recovery before returning stream, calls=%d meta=%#v", calls, meta)
	}
	if strings.Contains(string(raw), "10157") || strings.Contains(string(raw), "RateLimitError") {
		t.Fatalf("failed Hermes attempt leaked to client stream: %s", raw)
	}
	if !strings.Contains(string(raw), "RECOVERED") {
		t.Fatalf("recovered stream missing content: %s", raw)
	}
}

func TestHermesModelScopedSessionKeyChangesWithFallbackModel(t *testing.T) {
	base := "daiki:user"
	a := hermesModelScopedSessionKey(base, []byte(`{"model":"groq-qwen-qwen3.8-27b"}`))
	b := hermesModelScopedSessionKey(base, []byte(`{"model":"groq-openai-gpt-oss-20b"}`))
	if a == b || a == base || b == base {
		t.Fatalf("expected model-scoped keys, got %q %q", a, b)
	}
	if a != hermesModelScopedSessionKey(base, []byte(`{"model":"groq-qwen-qwen3.8-27b"}`)) {
		t.Fatal("same model must produce stable session key")
	}
}

func TestProviderFailureStatusDetectsHermesSoft401(t *testing.T) {
	raw := []byte(`{"choices":[{"delta":{},"finish_reason":"error"}],"error":{"message":"HTTP 401: litellm.AuthenticationError: OpenAIException - User not found"}}`)
	if got := providerFailureStatus(raw); got != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", got)
	}
	if failureKindForStatus(http.StatusUnauthorized) != "provider_auth" {
		t.Fatal("expected provider_auth failure kind")
	}
}

func TestInspectHermesSoftFailureDetectsNonStreaming401(t *testing.T) {
	body := `{"id":"x","choices":[{"message":{"role":"assistant","content":"API call failed after 1 retries: HTTP 401: litellm.AuthenticationError - User not found"}}]}`
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body))}
	raw, status, err := inspectHermesSoftFailure(resp)
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusUnauthorized {
		t.Fatalf("expected soft 401, got %d body=%s", status, raw)
	}
}

func TestInspectHermesSoftFailureDetectsStreaming401BeforeToken(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"error\"}],\"error\":{\"message\":\"HTTP 401: litellm.AuthenticationError - User not found\"}}\n\n"
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body)), ContentLength: -1}
	raw, status, err := inspectHermesSoftFailure(resp)
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusUnauthorized {
		t.Fatalf("expected streaming soft 401, got %d body=%s", status, raw)
	}
}

func TestModelCanFallbackOnAdaptiveProviderFailure(t *testing.T) {
	m := store.ProviderModel{FallbackModelName: "groq-openai-gpt-oss-20b"}
	if !modelCanFallback(m, "broken-model", "adaptive") {
		t.Fatal("adaptive model should fallback")
	}
	if modelCanFallback(m, "broken-model", "trim") {
		t.Fatal("trim-only model must not switch providers")
	}
	if modelCanFallback(m, "groq-openai-gpt-oss-20b", "adaptive") {
		t.Fatal("fallback loop must be rejected")
	}
}

func TestModelCircuitKeyIsStableAndOpaque(t *testing.T) {
	model := "daiki-aipass-openrouter-google-gemma-4-26b-a4b-it-free"
	a := modelCircuitKey(model)
	b := modelCircuitKey(model)
	if a != b || !strings.HasPrefix(a, "model:circuit:") {
		t.Fatalf("unexpected circuit key: %q %q", a, b)
	}
	if strings.Contains(a, "gemma") || strings.Contains(a, "openrouter") {
		t.Fatalf("circuit key leaks model identity: %q", a)
	}
}

func TestModelCircuitTTLByFailureClass(t *testing.T) {
	if got := modelCircuitTTL(http.StatusUnauthorized, "provider_auth"); got != 10*time.Minute {
		t.Fatalf("auth circuit ttl=%s", got)
	}
	if got := modelCircuitTTL(http.StatusBadGateway, "provider_upstream"); got != 45*time.Second {
		t.Fatalf("upstream circuit ttl=%s", got)
	}
	if got := modelCircuitTTL(0, "hermes_transport"); got != 45*time.Second {
		t.Fatalf("transport circuit ttl=%s", got)
	}
}

func TestChatPayloadHasImagePreventsSemanticFallback(t *testing.T) {
	text := []byte(`{"model":"fast","messages":[{"role":"user","content":"hello"}]}`)
	image := []byte(`{"model":"vision","messages":[{"role":"user","content":[{"type":"text","text":"read it"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`)
	if chatPayloadHasImage(text) {
		t.Fatal("text payload must not be treated as vision")
	}
	if !chatPayloadHasImage(image) {
		t.Fatal("image payload must be detected")
	}
}

func TestModelAdmissionCostUsesProfileOverheadAndBoundedOutput(t *testing.T) {
	m := store.ProviderModel{TPMLimit: 8000, AgentOverheadTokens: 1200, ResearchOverheadTokens: 300, MaxOutputTokens: 4096}
	body := []byte(`{"messages":[{"role":"user","content":"hello"}],"max_completion_tokens":4096}`)
	user := modelAdmissionCost(body, m, "user")
	research := modelAdmissionCost(body, m, "research")
	if user <= research {
		t.Fatalf("user cost %d must include higher agent overhead than research %d", user, research)
	}
	if user > int(float64(m.TPMLimit)*0.85) {
		t.Fatalf("admission cost must be capped to bucket capacity: %d", user)
	}
}

func TestRuntimeTokenLimitUsesTighterInputOrTotalLimit(t *testing.T) {
	if got := runtimeTokenLimit(store.ProviderModel{TPMLimit: 8000, ITPMLimit: 7000}); got != 7000 {
		t.Fatalf("got %d", got)
	}
	if got := runtimeTokenLimit(store.ProviderModel{TPMLimit: 8000}); got != 8000 {
		t.Fatalf("got %d", got)
	}
}

func TestShouldSpillModelAdmissionPrefersMeaningfulCapacityGain(t *testing.T) {
	if shouldSpillModelAdmission(1500*time.Millisecond, 0) {
		t.Fatal("short primary wait should stay on primary")
	}
	if !shouldSpillModelAdmission(3*time.Second, 0) {
		t.Fatal("ready fallback should absorb a long primary wait")
	}
	if !shouldSpillModelAdmission(5*time.Second, 2*time.Second) {
		t.Fatal("materially faster fallback should be selected")
	}
	if shouldSpillModelAdmission(3*time.Second, 2800*time.Millisecond) {
		t.Fatal("minor wait difference should not churn models")
	}
}

func TestRuntimeAgentOverheadUsesLeanVisionBudget(t *testing.T) {
	m := store.ProviderModel{AgentOverheadTokens: 3800, ResearchOverheadTokens: 800}
	if got := runtimeAgentOverhead(m, "vision"); got != 800 {
		t.Fatalf("vision overhead=%d want 800", got)
	}
}
