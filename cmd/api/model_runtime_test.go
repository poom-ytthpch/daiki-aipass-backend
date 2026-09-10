package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
