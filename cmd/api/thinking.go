package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

type thinkingProfile struct {
	Mode                  string `json:"mode"`
	Label                 string `json:"label"`
	ReasoningBudget       int64  `json:"reasoningBudget"`
	MaxCompletionTokens   int64  `json:"maxCompletionTokens"`
	NativeReasoningEffort string `json:"-"`
	Instruction           string `json:"-"`
}

var thinkingProfiles = map[string]thinkingProfile{
	"off": {
		Mode: "off", Label: "Off", ReasoningBudget: 0, MaxCompletionTokens: 1024, NativeReasoningEffort: "none",
		Instruction: "Answer directly. Do not spend extra tokens on deliberate reasoning unless needed for correctness.",
	},
	"low": {
		Mode: "low", Label: "Low", ReasoningBudget: 384, MaxCompletionTokens: 1536, NativeReasoningEffort: "low",
		Instruction: "Use a small internal reasoning budget. Check the most important assumption once, then answer concisely.",
	},
	"medium": {
		Mode: "medium", Label: "Medium", ReasoningBudget: 768, MaxCompletionTokens: 2560, NativeReasoningEffort: "medium",
		Instruction: "Use moderate internal reasoning. Break the task into a few verifiable steps, check key facts and then answer.",
	},
	"high": {
		Mode: "high", Label: "High", ReasoningBudget: 1536, MaxCompletionTokens: 4096, NativeReasoningEffort: "high",
		Instruction: "Use a larger internal reasoning budget. Examine alternatives and edge cases, verify important facts and calculations, then give only the useful conclusion and supporting rationale. Never reveal private chain-of-thought.",
	},
}

func normalizeThinkingMode(v any) string {
	mode := strings.ToLower(strings.TrimSpace(fmt.Sprint(v)))
	if _, ok := thinkingProfiles[mode]; ok {
		return mode
	}
	return "medium"
}

func thinkingProfileFor(mode string) thinkingProfile {
	if p, ok := thinkingProfiles[normalizeThinkingMode(mode)]; ok {
		return p
	}
	return thinkingProfiles["medium"]
}

// applyThinkingMode converts the Daiki-only thinkingMode field into an
// OpenAI-compatible request. max_completion_tokens is a combined completion
// ceiling: hidden reasoning + visible answer. Actual reasoning tokens are read
// from usage.completion_tokens_details.reasoning_tokens after inference.
func applyThinkingMode(body []byte) ([]byte, thinkingProfile, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, thinkingProfile{}, err
	}
	mode := normalizeThinkingMode(payload["thinkingMode"])
	profile := thinkingProfileFor(mode)
	delete(payload, "thinkingMode")
	// Hermes API Server consumes per-request reasoning from model_options. Keep
	// provider wire fields out of the top-level request here; the runtime adapter
	// applies only fields supported by the resolved physical model.
	delete(payload, "reasoning_effort")
	delete(payload, "reasoning_format")
	delete(payload, "include_reasoning")
	modelOptions, _ := payload["model_options"].(map[string]any)
	if modelOptions == nil {
		modelOptions = map[string]any{}
	}
	modelOptions["reasoning"] = map[string]any{
		"enabled": profile.Mode != "off",
		"effort":  profile.NativeReasoningEffort,
	}
	payload["model_options"] = modelOptions
	if existing, ok := payload["max_completion_tokens"].(float64); !ok || int64(existing) <= 0 || int64(existing) > profile.MaxCompletionTokens {
		payload["max_completion_tokens"] = profile.MaxCompletionTokens
	}
	delete(payload, "max_tokens")

	messages, _ := payload["messages"].([]any)
	instruction := "THINKING MODE: " + strings.ToUpper(profile.Mode) + ". " + profile.Instruction + " The provider receives the matching native reasoning effort when supported. Keep hidden reasoning private and return only the useful answer/rationale."
	if len(messages) > 0 {
		if first, ok := messages[0].(map[string]any); ok && strings.EqualFold(strings.TrimSpace(fmt.Sprint(first["role"])), "system") {
			first["content"] = strings.TrimSpace(fmt.Sprint(first["content"])) + "\n\n" + instruction
			messages[0] = first
		} else {
			messages = append([]any{map[string]any{"role": "system", "content": instruction}}, messages...)
		}
	} else {
		messages = []any{map[string]any{"role": "system", "content": instruction}}
	}
	payload["messages"] = messages
	out, err := json.Marshal(payload)
	return out, profile, err
}

type tokenEstimate struct {
	InputTokens      int64 `json:"inputTokens"`
	ThinkingBudget   int64 `json:"thinkingBudget"`
	VisibleBudget    int64 `json:"visibleBudget"`
	CompletionBudget int64 `json:"completionBudget"`
	TotalBudget      int64 `json:"totalBudget"`
}

func estimateTokens(body []byte, profile thinkingProfile) tokenEstimate {
	// UTF-8 bytes/4 is deliberately conservative for English while naturally
	// charging more for Thai and other multibyte scripts. Provider-reported
	// usage remains the durable source of truth after completion.
	input := int64(len(body)/4 + 256)
	if input > 16000 {
		input = 16000
	}
	completion := profile.MaxCompletionTokens
	var payload struct {
		MaxCompletionTokens int64 `json:"max_completion_tokens"`
		MaxTokens           int64 `json:"max_tokens"`
	}
	if json.Unmarshal(body, &payload) == nil {
		if payload.MaxCompletionTokens > 0 {
			completion = payload.MaxCompletionTokens
		} else if payload.MaxTokens > 0 {
			completion = payload.MaxTokens
		}
	}
	if completion <= 0 {
		completion = profile.MaxCompletionTokens
	}
	thinking := profile.ReasoningBudget
	if thinking > completion {
		thinking = completion
	}
	visible := completion - thinking
	if visible < 0 {
		visible = 0
	}
	return tokenEstimate{
		InputTokens:      input,
		ThinkingBudget:   thinking,
		VisibleBudget:    visible,
		CompletionBudget: completion,
		TotalBudget:      input + completion,
	}
}
