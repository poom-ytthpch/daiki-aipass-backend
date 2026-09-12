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
	AnalysisPasses        int    `json:"analysisPasses"`
	VerificationPasses    int    `json:"verificationPasses"`
	AlternativePaths      int    `json:"alternativePaths"`
	ConstraintAudit       bool   `json:"constraintAudit"`
	CounterexampleAudit   bool   `json:"counterexampleAudit"`
	UncertaintyAudit      bool   `json:"uncertaintyAudit"`
	TaskAdaptation        bool   `json:"taskAdaptation"`
	Instruction           string `json:"-"`
}

var thinkingProfiles = map[string]thinkingProfile{
	"off": {
		Mode: "off", Label: "Off", ReasoningBudget: 0, MaxCompletionTokens: 2048, NativeReasoningEffort: "none",
		Instruction: "Answer directly and prioritize correctness. Do not perform an extended deliberation workflow unless it is necessary to avoid an obvious mistake.",
	},
	"low": {
		Mode: "low", Label: "Low", ReasoningBudget: 1024, MaxCompletionTokens: 4096, NativeReasoningEffort: "low",
		AnalysisPasses: 1, VerificationPasses: 1, AlternativePaths: 1, ConstraintAudit: true, TaskAdaptation: true,
		Instruction: "Use one deliberate solve pass plus one sanity check. Identify the user's goal and hard constraints, solve the task directly, then verify the highest-risk assumption, calculation, or factual dependency once before answering. State material uncertainty instead of guessing.",
	},
	"medium": {
		Mode: "medium", Label: "Medium", ReasoningBudget: 2048, MaxCompletionTokens: 6144, NativeReasoningEffort: "medium",
		AnalysisPasses: 2, VerificationPasses: 2, AlternativePaths: 2, ConstraintAudit: true, UncertaintyAudit: true, TaskAdaptation: true,
		Instruction: "Use a structured internal workflow: frame the goal, constraints, and unknowns; decompose the task into verifiable parts; solve them; independently re-check important calculations or factual dependencies; compare at least one plausible alternative when it could change the answer; reconcile contradictions before answering. State material uncertainty instead of filling gaps with guesses.",
	},
	"high": {
		Mode: "high", Label: "High", ReasoningBudget: 4096, MaxCompletionTokens: 8192, NativeReasoningEffort: "high",
		AnalysisPasses: 3, VerificationPasses: 3, AlternativePaths: 3, ConstraintAudit: true, CounterexampleAudit: true, UncertaintyAudit: true, TaskAdaptation: true,
		Instruction: "Use a robust internal workflow: define the task and acceptance criteria; separate known facts, assumptions, and unknowns; generate multiple candidate approaches when meaningful; solve using the strongest candidate; adversarially challenge it with edge cases, counterexamples, failure modes, and conflicting evidence; independently verify critical calculations and constraints; reconcile any inconsistency; then synthesize the best answer with calibrated uncertainty. Repeat a verification pass when a critical inconsistency remains.",
	},
}

func thinkingPolicyScore(profile thinkingProfile) int {
	score := 10
	score += min(profile.AnalysisPasses, 3) * 10
	score += min(profile.VerificationPasses, 3) * 10
	score += min(profile.AlternativePaths, 3) * 5
	if profile.ConstraintAudit {
		score += 5
	}
	if profile.CounterexampleAudit {
		score += 5
	}
	if profile.UncertaintyAudit {
		score += 5
	}
	if profile.TaskAdaptation {
		score += 5
	}
	return min(score, 100)
}

func thinkingContainsAny(text string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

func thinkingTaskClass(body []byte) string {
	return thinkingTaskClassFromText(latestUserText(body))
}

func thinkingTaskClassFromText(text string) string {
	text = strings.ToLower(strings.TrimSpace(text))
	if text == "" {
		return "general"
	}
	if thinkingContainsAny(text, "debug", "bug", "stack trace", "exception", "typescript", "javascript", "nestjs", "next.js", "nextjs", "react", "sql", "function", "code", "โค้ด", "บั๊ก", "แก้ error", "แก้ไข error") {
		return "coding"
	}
	if thinkingContainsAny(text, "calculate", "calculation", "equation", "probability", "percent", "percentage", "ratio", "formula", "คำนวณ", "สมการ", "เปอร์เซ็นต์", "ความน่าจะเป็น") {
		return "quantitative"
	}
	if thinkingContainsAny(text, "compare", "comparison", "choose", "recommend", "tradeoff", "pros and cons", "decision", "เปรียบเทียบ", "เลือก", "แนะนำ", "ข้อดีข้อเสีย", "ตัดสินใจ") {
		return "decision"
	}
	if thinkingContainsAny(text, "research", "latest", "current", "source", "evidence", "citation", "specification", "specs", "ราคา", "ล่าสุด", "ปัจจุบัน", "แหล่งข้อมูล", "หลักฐาน", "สเปก", "วิจัย", "ค้นข้อมูล") {
		return "factual"
	}
	return "general"
}

func thinkingTaskInstruction(taskClass string) string {
	switch taskClass {
	case "coding":
		return "TASK-SPECIFIC CHECK: trace the relevant input/state/control flow before proposing a fix; preserve existing contracts; identify likely regression surfaces; and include the smallest useful validation or test evidence."
	case "quantitative":
		return "TASK-SPECIFIC CHECK: compute carefully, track units and boundary conditions, then independently recompute or cross-check the result before presenting it."
	case "decision":
		return "TASK-SPECIFIC CHECK: identify decision criteria and hard constraints, compare realistic alternatives on the same criteria, surface material tradeoffs, and test whether the recommendation changes under a plausible counterfactual."
	case "factual":
		return "TASK-SPECIFIC CHECK: separate evidence from inference, prefer supplied or tool-grounded evidence for freshness-sensitive claims, check source authority and recency, and never invent a source, citation, measurement, or missing fact."
	default:
		return "TASK-SPECIFIC CHECK: make the user's requirements explicit, check for contradictions or missing constraints, and verify the conclusion against the original request before answering."
	}
}

func thinkingInstruction(profile thinkingProfile, taskClass string) string {
	parts := []string{
		"THINKING MODE: " + strings.ToUpper(profile.Mode) + ".",
		profile.Instruction,
	}
	if profile.TaskAdaptation {
		parts = append(parts, thinkingTaskInstruction(taskClass))
	}
	parts = append(parts,
		"Treat the workflow as private scratch work: never reveal hidden chain-of-thought, private deliberation, or internal token-by-token reasoning. Do not spend the visible answer enumerating exhaustive scratch steps unless the user explicitly asks for them. Preserve enough completion budget to always deliver the final answer.",
		"OUTPUT CONTRACT: explicit user constraints on the final response take priority over explanatory prose. If the user requests ONLY JSON, exact keys, an exact line, no Markdown, a fixed schema, or another unambiguous format, obey it literally; do not add code fences, commentary, preambles, postambles, or clarification questions that violate the requested format.",
		"Lead with the answer and keep supporting rationale concise. Before finishing, verify that the final answer actually contains every requested result and satisfies the requested format; if the completion budget is becoming tight, stop expanding the scratch analysis and produce the verified final answer immediately.",
		"The provider receives the matching native reasoning effort when supported; otherwise these reasoning and verification gates remain mandatory prompt-guided behavior.",
	)
	return strings.Join(parts, " ")
}

func thinkingMetadata(profile thinkingProfile, body []byte) map[string]any {
	return map[string]any{
		"mode":                profile.Mode,
		"policyScore":         thinkingPolicyScore(profile),
		"taskClass":           thinkingTaskClass(body),
		"reasoningBudget":     profile.ReasoningBudget,
		"maxCompletionTokens": profile.MaxCompletionTokens,
		"analysisPasses":      profile.AnalysisPasses,
		"verificationPasses":  profile.VerificationPasses,
		"alternativePaths":    profile.AlternativePaths,
		"constraintAudit":     profile.ConstraintAudit,
		"counterexampleAudit": profile.CounterexampleAudit,
		"uncertaintyAudit":    profile.UncertaintyAudit,
		"taskAdaptation":      profile.TaskAdaptation,
	}
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
	taskClass := thinkingTaskClass(body)
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
	instruction := thinkingInstruction(profile, taskClass)
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
