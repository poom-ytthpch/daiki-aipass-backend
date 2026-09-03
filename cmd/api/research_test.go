package main

import (
	"encoding/json"
	"net"
	"testing"
)

func TestNormalizeResearchMode(t *testing.T) {
	cases := map[any]string{
		"web":      "web",
		"research": "web",
		"off":      "off",
		"auto":     "auto",
		"":         "auto",
		nil:        "auto",
	}
	for in, want := range cases {
		if got := normalizeResearchMode(in); got != want {
			t.Fatalf("normalizeResearchMode(%v)=%q want %q", in, got, want)
		}
	}
}

func TestAutoResearchSignals(t *testing.T) {
	for _, q := range []string{
		"What is the latest LiteLLM release?",
		"ช่วยค้นราคา GPU ล่าสุดให้หน่อย",
		"compare current vLLM and Ollama",
	} {
		if !shouldAutoResearch(q) {
			t.Fatalf("expected research for %q", q)
		}
	}
	for _, q := range []string{"write a haiku", "2+2 เท่ากับเท่าไหร่", "explain a binary tree"} {
		if shouldAutoResearch(q) {
			t.Fatalf("did not expect research for %q", q)
		}
	}
}

func TestLastUserText(t *testing.T) {
	payload := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": "rules"},
		map[string]any{"role": "assistant", "content": "hello"},
		map[string]any{"role": "user", "content": "latest qwen release"},
	}}
	if got := lastUserText(payload); got != "latest qwen release" {
		t.Fatalf("got %q", got)
	}
}

func TestResearchOffStillAddsSafeReasoningInstruction(t *testing.T) {
	a := &app{cfg: config{}}
	body, meta, err := a.enrichChatWithResearch(t.Context(), []byte(`{"model":"auto","researchMode":"off","messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if meta.Mode != "off" || meta.Used {
		t.Fatalf("unexpected metadata %#v", meta)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if _, exists := payload["researchMode"]; exists {
		t.Fatal("researchMode must not be forwarded to LiteLLM")
	}
	messages, _ := payload["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("expected system + user messages, got %#v", messages)
	}
	first, _ := messages[0].(map[string]any)
	if first["role"] != "system" {
		t.Fatalf("expected system instruction, got %#v", first)
	}
}

func TestPublicIPGuard(t *testing.T) {
	blocked := []string{"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.1.1", "169.254.169.254", "0.0.0.0", "::1"}
	for _, raw := range blocked {
		if publicIP(net.ParseIP(raw)) {
			t.Fatalf("unsafe address allowed: %s", raw)
		}
	}
	for _, raw := range []string{"1.1.1.1", "8.8.8.8", "2606:4700:4700::1111"} {
		if !publicIP(net.ParseIP(raw)) {
			t.Fatalf("public address blocked: %s", raw)
		}
	}
}
