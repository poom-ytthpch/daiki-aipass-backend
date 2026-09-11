package main

import (
	"encoding/json"
	"strings"
	"testing"
)

type thinkingBenchmarkDimensions struct {
	PolicyScore        int
	AnalysisPasses     int
	VerificationPasses int
	AlternativePaths   int
	ConstraintAudit    bool
	Counterexample     bool
	UncertaintyAudit   bool
	TaskAdaptation     bool
}

func benchmarkThinkingProfile(profile thinkingProfile) thinkingBenchmarkDimensions {
	return thinkingBenchmarkDimensions{
		PolicyScore:        thinkingPolicyScore(profile),
		AnalysisPasses:     profile.AnalysisPasses,
		VerificationPasses: profile.VerificationPasses,
		AlternativePaths:   profile.AlternativePaths,
		ConstraintAudit:    profile.ConstraintAudit,
		Counterexample:     profile.CounterexampleAudit,
		UncertaintyAudit:   profile.UncertaintyAudit,
		TaskAdaptation:     profile.TaskAdaptation,
	}
}

func systemInstructionFromThinkingBody(t *testing.T, body []byte) string {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	messages, _ := payload["messages"].([]any)
	if len(messages) == 0 {
		t.Fatal("thinking policy did not inject a system instruction")
	}
	first, _ := messages[0].(map[string]any)
	if !strings.EqualFold(strings.TrimSpace(stringValue(first["role"])), "system") {
		t.Fatalf("first message is not system: %#v", first)
	}
	return strings.TrimSpace(stringValue(first["content"]))
}

func stringValue(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func TestThinkingPolicyBenchmarkScoreScalesWithSelectedLevel(t *testing.T) {
	cases := []struct {
		mode                    string
		wantScore               int
		wantReasoning           int64
		wantCompletion          int64
		wantAnalysis            int
		wantVerification        int
		wantAlternatives        int
		wantCounterexampleAudit bool
	}{
		{mode: "off", wantScore: 10, wantReasoning: 0, wantCompletion: 2048},
		{mode: "low", wantScore: 45, wantReasoning: 1024, wantCompletion: 4096, wantAnalysis: 1, wantVerification: 1, wantAlternatives: 1},
		{mode: "medium", wantScore: 75, wantReasoning: 2048, wantCompletion: 6144, wantAnalysis: 2, wantVerification: 2, wantAlternatives: 2},
		{mode: "high", wantScore: 100, wantReasoning: 4096, wantCompletion: 8192, wantAnalysis: 3, wantVerification: 3, wantAlternatives: 3, wantCounterexampleAudit: true},
	}
	previousScore := -1
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			profile := thinkingProfileFor(tc.mode)
			score := benchmarkThinkingProfile(profile)
			t.Logf("thinking=%s policyScore=%d analysis=%d verify=%d alternatives=%d constraints=%t counterexample=%t uncertainty=%t taskAdaptation=%t reasoningBudget=%d completionBudget=%d",
				tc.mode, score.PolicyScore, score.AnalysisPasses, score.VerificationPasses, score.AlternativePaths, score.ConstraintAudit, score.Counterexample, score.UncertaintyAudit, score.TaskAdaptation, profile.ReasoningBudget, profile.MaxCompletionTokens)
			if score.PolicyScore != tc.wantScore {
				t.Fatalf("policy score=%d want %d", score.PolicyScore, tc.wantScore)
			}
			if profile.ReasoningBudget != tc.wantReasoning || profile.MaxCompletionTokens != tc.wantCompletion {
				t.Fatalf("budget reasoning/completion=%d/%d want %d/%d", profile.ReasoningBudget, profile.MaxCompletionTokens, tc.wantReasoning, tc.wantCompletion)
			}
			if score.AnalysisPasses != tc.wantAnalysis || score.VerificationPasses != tc.wantVerification || score.AlternativePaths != tc.wantAlternatives {
				t.Fatalf("policy dimensions=%#v", score)
			}
			if score.Counterexample != tc.wantCounterexampleAudit {
				t.Fatalf("counterexample audit=%t want %t", score.Counterexample, tc.wantCounterexampleAudit)
			}
		})
		if tc.wantScore <= previousScore {
			t.Fatalf("benchmark configuration is not strictly increasing: previous=%d current=%d", previousScore, tc.wantScore)
		}
		previousScore = tc.wantScore
	}
}

func TestThinkingTaskAdaptationBenchmark(t *testing.T) {
	cases := []struct {
		name       string
		prompt     string
		wantClass  string
		wantMarker string
	}{
		{name: "coding", prompt: "ช่วย debug TypeScript function นี้และหา regression ให้หน่อย", wantClass: "coding", wantMarker: "trace the relevant input/state/control flow"},
		{name: "quantitative", prompt: "คำนวณ 17.5% ของ 12,480 และตรวจคำตอบซ้ำ", wantClass: "quantitative", wantMarker: "track units and boundary conditions"},
		{name: "decision", prompt: "เปรียบเทียบสองทางเลือกนี้แล้วแนะนำว่าควรเลือกอะไร", wantClass: "decision", wantMarker: "compare realistic alternatives on the same criteria"},
		{name: "factual", prompt: "research latest specification and source evidence for this product", wantClass: "factual", wantMarker: "separate evidence from inference"},
		{name: "general", prompt: "ช่วยจัดแผนงานนี้ให้อ่านง่ายและครบข้อกำหนด", wantClass: "general", wantMarker: "make the user's requirements explicit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(map[string]any{
				"model":        "groq-groq-compound",
				"thinkingMode": "high",
				"messages":     []map[string]any{{"role": "user", "content": tc.prompt}},
			})
			if got := thinkingTaskClass(raw); got != tc.wantClass {
				t.Fatalf("task class=%q want %q", got, tc.wantClass)
			}
			prepared, _, err := applyThinkingMode(raw)
			if err != nil {
				t.Fatal(err)
			}
			instruction := systemInstructionFromThinkingBody(t, prepared)
			if !strings.Contains(instruction, tc.wantMarker) {
				t.Fatalf("instruction missing task-adaptive marker %q: %s", tc.wantMarker, instruction)
			}
		})
	}
}

func TestGroqCompoundHighKeepsPromptGuidedReasoningWhenNativeEffortIsUnavailable(t *testing.T) {
	raw := []byte(`{"model":"groq-groq-compound","thinkingMode":"high","messages":[{"role":"user","content":"Compare three deployment designs, find failure modes, and recommend the safest one."}]}`)
	prepared, profile, err := applyThinkingMode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if profile.Mode != "high" || thinkingPolicyScore(profile) != 100 {
		t.Fatalf("unexpected high profile %#v score=%d", profile, thinkingPolicyScore(profile))
	}
	adapted, effective, native, err := applyReasoningForModel(prepared, "groq-groq-compound", "high")
	if err != nil {
		t.Fatal(err)
	}
	if native || effective != "" {
		t.Fatalf("Groq Compound must use prompt-guided reasoning unless native support is explicitly added, effective=%q native=%t", effective, native)
	}
	instruction := systemInstructionFromThinkingBody(t, adapted)
	for _, marker := range []string{"multiple candidate approaches", "counterexamples", "failure modes", "independently verify", "never reveal hidden chain-of-thought"} {
		if !strings.Contains(instruction, marker) {
			t.Fatalf("high compound policy missing %q: %s", marker, instruction)
		}
	}
	var payload map[string]any
	if err := json.Unmarshal(adapted, &payload); err != nil {
		t.Fatal(err)
	}
	if _, exists := payload["model_options"]; exists {
		t.Fatalf("unsupported native reasoning options must be removed for compound: %#v", payload["model_options"])
	}
	if payload["max_completion_tokens"] != float64(8192) {
		t.Fatalf("compound high completion budget=%v want 8192", payload["max_completion_tokens"])
	}
}

func TestThinkingHighPolicyIsMateriallyStrongerThanLow(t *testing.T) {
	prompt := `{"model":"groq-groq-compound","messages":[{"role":"user","content":"Compare two architecture options and verify edge cases."}],"thinkingMode":"low"}`
	lowBody, low, err := applyThinkingMode([]byte(prompt))
	if err != nil {
		t.Fatal(err)
	}
	highBody, high, err := applyThinkingMode([]byte(strings.Replace(prompt, `"thinkingMode":"low"`, `"thinkingMode":"high"`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	lowInstruction := systemInstructionFromThinkingBody(t, lowBody)
	highInstruction := systemInstructionFromThinkingBody(t, highBody)
	if thinkingPolicyScore(high)-thinkingPolicyScore(low) < 50 {
		t.Fatalf("high policy score gain too small: low=%d high=%d", thinkingPolicyScore(low), thinkingPolicyScore(high))
	}
	if strings.Contains(lowInstruction, "adversarially challenge") {
		t.Fatalf("low mode unexpectedly received high adversarial workflow: %s", lowInstruction)
	}
	for _, marker := range []string{"adversarially challenge", "counterexamples", "conflicting evidence", "Repeat a verification pass"} {
		if !strings.Contains(highInstruction, marker) {
			t.Fatalf("high mode missing %q", marker)
		}
	}
}
