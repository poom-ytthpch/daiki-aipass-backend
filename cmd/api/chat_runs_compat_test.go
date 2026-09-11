package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestUnexpectedBlockedToolCall(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "groq tool choice rejection",
			body: `{"error":{"message":"litellm.BadRequestError: OpenAIException - Tool choice is none, but model called a tool."}}`,
			want: true,
		},
		{
			name: "ordinary bad request",
			body: `{"error":{"message":"invalid model"}}`,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := unexpectedBlockedToolCall([]byte(tc.body)); got != tc.want {
				t.Fatalf("unexpectedBlockedToolCall()=%v want %v", got, tc.want)
			}
		})
	}
}

func TestApplyTextOnlyProviderCompatibility(t *testing.T) {
	raw := []byte(`{"model":"groq-test","tools":[{"type":"function"}],"tool_choice":"auto","parallel_tool_calls":true,"messages":[{"role":"user","content":"summarize the supplied evidence"}]}`)
	got := applyTextOnlyProviderCompatibility(raw)
	var payload map[string]any
	if err := json.Unmarshal(got, &payload); err != nil {
		t.Fatalf("compatibility payload invalid: %v", err)
	}
	for _, key := range []string{"tools", "tool_choice", "parallel_tool_calls"} {
		if _, ok := payload[key]; ok {
			t.Fatalf("compatibility payload still contains %s", key)
		}
	}
	messages, _ := payload["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("messages=%d want 2", len(messages))
	}
	first, _ := messages[0].(map[string]any)
	if first["role"] != "system" || !strings.Contains(first["content"].(string), "normal text only") {
		t.Fatalf("missing text-only provider instruction: %#v", first)
	}
	second := applyTextOnlyProviderCompatibility(got)
	var again map[string]any
	_ = json.Unmarshal(second, &again)
	againMessages, _ := again["messages"].([]any)
	if len(againMessages) != len(messages) {
		t.Fatalf("compatibility instruction duplicated: before=%d after=%d", len(messages), len(againMessages))
	}
}
