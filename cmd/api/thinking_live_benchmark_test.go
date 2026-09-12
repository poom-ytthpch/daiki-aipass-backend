package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

type liveReasoningBenchmarkCase struct {
	Name   string
	Prompt string
	Points int
	Score  func(string) bool
}

func liveBenchmarkContainsAll(answer string, groups ...[]string) bool {
	answer = strings.ToLower(answer)
	for _, group := range groups {
		matched := false
		for _, needle := range group {
			if strings.Contains(answer, strings.ToLower(needle)) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func liveReasoningBenchmarkCases() []liveReasoningBenchmarkCase {
	return []liveReasoningBenchmarkCase{
		{
			Name: "fractional_capacity", Points: 15,
			Prompt: "A tank is 3/5 full. After adding exactly 24 liters it is 3/4 full. What is the tank's total capacity in liters? Give the final numeric answer and a concise verification.",
			Score: func(answer string) bool {
				return regexp.MustCompile(`(?i)(^|[^0-9])160(?:\.0+)?([^0-9]|$)`).MatchString(answer)
			},
		},
		{
			Name: "constraint_ordering_count", Points: 15,
			Prompt: "Five tasks A, B, C, D, E must each appear exactly once. Constraints: A is before C; B is after D; E is immediately before C; D is not first. How many valid complete task orders exist? List every valid order and give the count.",
			Score: func(answer string) bool {
				n := strings.NewReplacer(" ", "", "-", "", ">", "", "→", "").Replace(strings.ToUpper(answer))
				return strings.Contains(n, "ADBEC") && strings.Contains(n, "ADECB") && strings.Contains(n, "AECDB") && regexp.MustCompile(`(?i)(count|total|exactly|answer|valid)[^0-9]{0,20}3`).MatchString(answer)
			},
		},
		{
			Name: "constraint_intersection", Points: 20,
			Prompt: "Choose one server plan. Requirements: latency <=35 ms, capacity >=1500 req/s, monthly cost <=$100. Plan A: 30 ms, 1000 req/s, $80. Plan B: 20 ms, 2000 req/s, $120. Plan C: 45 ms, 3000 req/s, $90. Which plan satisfies every requirement? Check every constraint before answering.",
			Score: func(answer string) bool {
				return liveBenchmarkContainsAll(answer,
					[]string{"none", "no plan", "no option", "ไม่มี"},
					[]string{"capacity", "1000", "1500"},
					[]string{"budget", "$120", "120"},
					[]string{"latency", "45 ms", "45ms"},
				)
			},
		},
		{
			Name: "concurrent_transfer_bug", Points: 25,
			Prompt: "Review this pseudocode for correctness under concurrent requests:\n\nasync transfer(from,to,amount) { fromBal=await get(from); if (fromBal<amount) throw; await set(from,fromBal-amount); toBal=await get(to); await set(to,toBal+amount); }\n\nIdentify the most important correctness bug and the production-safe class of fix. Do not focus on style.",
			Score: func(answer string) bool {
				return liveBenchmarkContainsAll(answer,
					[]string{"race", "concurrent", "lost update", "atomicity"},
					[]string{"transaction", "atomic", "lock", "serializable", "compare-and-swap", "cas"},
					[]string{"balance", "update", "transfer"},
				)
			},
		},
		{
			Name: "bayes_missing_prevalence", Points: 25,
			Prompt: "A diagnostic test has 95% sensitivity and 90% specificity. A patient's result is positive. What is the probability that the patient actually has the disease? Give a numeric probability if it is determined by the information provided; otherwise explain exactly what additional quantity is required.",
			Score: func(answer string) bool {
				return liveBenchmarkContainsAll(answer,
					[]string{"prevalence", "base rate", "prior probability", "prior"},
					[]string{"cannot", "not enough", "insufficient", "not determined", "need", "required", "ไม่สามารถ", "ข้อมูลไม่พอ"},
				)
			},
		},
	}
}

func runLiveReasoningCompletion(ctx context.Context, client *http.Client, endpoint, apiKey, model, mode, prompt string, benchmarkMaxTokens int64) (string, error) {
	profile := thinkingProfileFor(mode)
	completionCap := profile.MaxCompletionTokens
	if benchmarkMaxTokens > 0 && benchmarkMaxTokens < completionCap {
		completionCap = benchmarkMaxTokens
	}
	requestBody := map[string]any{
		"model": model,
		"messages": []map[string]any{
			{"role": "system", "content": thinkingInstruction(profile, thinkingTaskClass([]byte(fmt.Sprintf(`{"messages":[{"role":"user","content":%q}]}`, prompt))))},
			{"role": "user", "content": prompt},
		},
		"temperature": 0,
		"stream":      false,
	}
	tokenField := strings.TrimSpace(os.Getenv("DAIKI_LIVE_REASONING_TOKEN_FIELD"))
	if tokenField != "max_tokens" {
		tokenField = "max_completion_tokens"
	}
	requestBody[tokenField] = completionCap
	if os.Getenv("DAIKI_LIVE_REASONING_NATIVE_REASONING") == "1" && mode != "off" {
		requestBody["reasoning_effort"] = mode
	}
	raw, err := json.Marshal(requestBody)
	if err != nil {
		return "", err
	}
	var lastErr error
	for attempt := 0; attempt < 7; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
		if err != nil {
			return "", err
		}
		req.Header.Set("content-type", "application/json")
		req.Header.Set("authorization", "Bearer "+apiKey)
		resp, err := client.Do(req)
		if err != nil {
			return "", err
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		_ = resp.Body.Close()
		if readErr != nil {
			return "", readErr
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			var out struct {
				Choices []struct {
					Message struct {
						Content string `json:"content"`
					} `json:"message"`
				} `json:"choices"`
			}
			if err := json.Unmarshal(body, &out); err != nil {
				return "", err
			}
			if len(out.Choices) == 0 || strings.TrimSpace(out.Choices[0].Message.Content) == "" {
				return "", fmt.Errorf("provider returned no answer")
			}
			return strings.TrimSpace(out.Choices[0].Message.Content), nil
		}
		lastErr = fmt.Errorf("provider status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		if resp.StatusCode != http.StatusTooManyRequests || attempt == 6 {
			return "", lastErr
		}
		wait := 3 * time.Second
		if match := regexp.MustCompile(`(?i)try again in\s+([0-9.]+)s`).FindStringSubmatch(string(body)); len(match) == 2 {
			if parsed, parseErr := time.ParseDuration(match[1] + "s"); parseErr == nil && parsed > wait {
				wait = parsed
			}
		}
		wait += 1200 * time.Millisecond
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(wait):
		}
	}
	return "", lastErr
}

func TestLiveGroqCompoundThinkingQualityBenchmark(t *testing.T) {
	if os.Getenv("DAIKI_LIVE_REASONING_BENCHMARK") != "1" {
		t.Skip("set DAIKI_LIVE_REASONING_BENCHMARK=1 to run provider-backed reasoning benchmark")
	}
	endpoint := strings.TrimSpace(os.Getenv("DAIKI_LIVE_REASONING_URL"))
	apiKey := strings.TrimSpace(os.Getenv("DAIKI_LIVE_REASONING_API_KEY"))
	model := strings.TrimSpace(os.Getenv("DAIKI_LIVE_REASONING_MODEL"))
	if endpoint == "" || apiKey == "" {
		t.Fatal("DAIKI_LIVE_REASONING_URL and DAIKI_LIVE_REASONING_API_KEY are required")
	}
	if model == "" {
		model = "groq-groq-compound"
	}
	client := &http.Client{Timeout: 75 * time.Second}
	cases := liveReasoningBenchmarkCases()
	modes := []string{"off", "low", "medium", "high"}
	scores := map[string]int{}
	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			score := 0
			for _, tc := range cases {
				ctx, cancel := context.WithTimeout(t.Context(), 70*time.Second)
				answer, err := runLiveReasoningCompletion(ctx, client, endpoint, apiKey, model, mode, tc.Prompt, 1200)
				cancel()
				if err != nil {
					t.Fatalf("%s: %v", tc.Name, err)
				}
				passed := tc.Score(answer)
				if passed {
					score += tc.Points
				}
				t.Logf("mode=%s case=%s points=%d/%d", mode, tc.Name, map[bool]int{true: tc.Points, false: 0}[passed], tc.Points)
				if !passed {
					t.Logf("mode=%s case=%s answer=%q", mode, tc.Name, answer)
				}
				time.Sleep(1500 * time.Millisecond)
			}
			scores[mode] = score
			t.Logf("LIVE_REASONING_SCORE mode=%s score=%d/100 policyScore=%d", mode, score, thinkingPolicyScore(thinkingProfileFor(mode)))
		})
	}
	if scores["high"] < scores["low"] {
		t.Fatalf("high thinking regressed below low: low=%d high=%d scores=%v", scores["low"], scores["high"], scores)
	}
	if scores["medium"] < 70 || scores["high"] < 80 {
		t.Fatalf("reasoning quality below acceptance floor: scores=%v", scores)
	}
}
