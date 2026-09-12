package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"
)

type liveRouteQualityCase struct {
	Name     string
	Category string
	Points   int
	Prompt   string
	Score    func(string) bool
}

type liveRouteBenchmarkResult struct {
	Route         string
	Model         string
	ThinkingMode  string
	Score         int
	MaxScore      int
	Passed        int
	TotalCases    int
	Duration      time.Duration
	Failures      []string
	CategoryScore map[string]int
	CategoryMax   map[string]int
}

func compactBenchmarkOrder(answer string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || strings.ContainsRune("-→>`|,.;:()[]{}", r) {
			return -1
		}
		return unicode.ToUpper(r)
	}, answer)
}

func compactBenchmarkNumber(answer string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || r == ',' {
			return -1
		}
		return r
	}, answer)
}

func routeQualityCases() []liveRouteQualityCase {
	return []liveRouteQualityCase{
		{
			Name: "fractional_capacity", Category: "quantitative", Points: 8,
			Prompt: "A tank is 3/5 full. After adding exactly 24 liters it is 3/4 full. What is the tank's total capacity in liters? Give the final numeric answer and a concise verification.",
			Score: func(answer string) bool {
				return regexp.MustCompile(`(?i)(^|[^0-9])160(?:\.0+)?([^0-9]|$)`).MatchString(answer)
			},
		},
		{
			Name: "thai_discount_vat", Category: "quantitative-thai", Points: 8,
			Prompt: "สินค้าราคา 1,250 บาท ลดราคา 20% ก่อน แล้วคิด VAT 7% จากราคาหลังหักส่วนลด ราคาสุทธิที่ต้องจ่ายเท่าไร? แสดงคำตอบเป็นบาทและตรวจเลขสั้น ๆ",
			Score: func(answer string) bool {
				n := compactBenchmarkNumber(answer)
				return regexp.MustCompile(`(?i)(^|[^0-9])1070(?:\.0+)?([^0-9]|$)`).MatchString(n)
			},
		},
		{
			Name: "constraint_ordering_count", Category: "logic", Points: 8,
			Prompt: "Five tasks A, B, C, D, E must each appear exactly once. Constraints: A is before C; B is after D; E is immediately before C; D is not first. How many valid complete task orders exist? List every valid order and give the count.",
			Score: func(answer string) bool {
				n := compactBenchmarkOrder(answer)
				return strings.Contains(n, "ADBEC") && strings.Contains(n, "ADECB") && strings.Contains(n, "AECDB") && regexp.MustCompile(`(?i)(count|total|exactly|answer|valid|ทั้งหมด|จำนวน)[^0-9]{0,24}3`).MatchString(answer)
			},
		},
		{
			Name: "constraint_intersection", Category: "decision", Points: 8,
			Prompt: "Choose one server plan. Requirements: latency <=35 ms, capacity >=1500 req/s, monthly cost <=$100. Plan A: 30 ms, 1000 req/s, $80. Plan B: 20 ms, 2000 req/s, $120. Plan C: 45 ms, 3000 req/s, $90. Which plan satisfies every requirement? Check every constraint before answering.",
			Score: func(answer string) bool {
				return liveBenchmarkContainsAll(answer,
					[]string{"none", "no plan", "no option", "ไม่มี"},
					[]string{"1000", "1500", "capacity"},
					[]string{"120", "budget", "cost"},
					[]string{"45", "latency"},
				)
			},
		},
		{
			Name: "concurrent_transfer_bug", Category: "coding", Points: 10,
			Prompt: "Review this pseudocode for correctness under concurrent requests:\n\nasync transfer(from,to,amount) { fromBal=await get(from); if (fromBal<amount) throw; await set(from,fromBal-amount); toBal=await get(to); await set(to,toBal+amount); }\n\nIdentify the most important correctness bug and the production-safe class of fix. Do not focus on style.",
			Score: func(answer string) bool {
				return liveBenchmarkContainsAll(answer,
					[]string{"race", "concurrent", "lost update", "atomicity"},
					[]string{"transaction", "atomic", "lock", "serializable", "compare-and-swap", "cas"},
				)
			},
		},
		{
			Name: "bayes_missing_prevalence", Category: "uncertainty", Points: 10,
			Prompt: "A diagnostic test has 95% sensitivity and 90% specificity. A patient's result is positive. What is the probability that the patient actually has the disease? Give a numeric probability if it is determined by the information provided; otherwise explain exactly what additional quantity is required.",
			Score: func(answer string) bool {
				return liveBenchmarkContainsAll(answer,
					[]string{"prevalence", "base rate", "prior probability", "prior", "อัตราความชุก"},
					[]string{"cannot", "not enough", "insufficient", "not determined", "need", "required", "ไม่สามารถ", "ข้อมูลไม่พอ", "ต้องทราบ"},
				)
			},
		},
		{
			Name: "sql_not_in_null", Category: "data", Points: 8,
			Prompt: "A SQL table t has non-NULL integer ids 1, 2, 3. What rows are returned by `SELECT id FROM t WHERE id NOT IN (1, NULL);` under standard SQL three-valued logic? Explain the NULL behavior briefly.",
			Score: func(answer string) bool {
				return liveBenchmarkContainsAll(answer,
					[]string{"no rows", "zero rows", "none", "empty", "0 rows", "ไม่มีแถว"},
					[]string{"unknown", "three-valued", "null"},
				)
			},
		},
		{
			Name: "javascript_event_loop", Category: "coding", Points: 8,
			Prompt: "For standard modern JavaScript, what is the console output order?\nconsole.log('A');\nPromise.resolve().then(()=>console.log('B'));\nsetTimeout(()=>console.log('C'),0);\nconsole.log('D');\nInclude one line exactly in the form `ORDER: A,D,B,C`.",
			Score: func(answer string) bool {
				return regexp.MustCompile(`(?i)ORDER\s*:\s*A\s*,\s*D\s*,\s*B\s*,\s*C`).MatchString(answer)
			},
		},
		{
			Name: "tcp_message_boundaries", Category: "systems", Points: 8,
			Prompt: "Does TCP preserve application message boundaries? If a sender calls send() twice, can the receiver rely on two recv() calls matching those sends? State the correct model and one production framing technique.",
			Score: func(answer string) bool {
				return liveBenchmarkContainsAll(answer,
					[]string{"byte stream", "stream of bytes", "no message boundaries", "does not preserve", "ไม่รักษาขอบเขต"},
					[]string{"framing", "length prefix", "length-prefixed", "delimiter", "header"},
				)
			},
		},
		{
			Name: "logical_contradiction", Category: "logic", Points: 8,
			Prompt: "Premises: (1) every A is a B, (2) no B is a C, (3) object x is both an A and a C. Are the premises jointly satisfiable? Answer yes/no and explain in one or two sentences.",
			Score: func(answer string) bool {
				return liveBenchmarkContainsAll(answer,
					[]string{"no", "not satisfiable", "unsatisfiable", "contradiction", "inconsistent", "impossible", "ไม่"},
					[]string{"b", "c"},
				)
			},
		},
		{
			Name: "false_premise_nobel_cs", Category: "hallucination", Points: 8,
			Prompt: "Who won the 2024 Nobel Prize in Computer Science, and what work was it awarded for? Be precise; if the premise is wrong, correct it rather than inventing an award.",
			Score: func(answer string) bool {
				a := strings.ToLower(answer)
				return (strings.Contains(a, "no nobel prize in computer science") || strings.Contains(a, "no nobel prize category") || strings.Contains(a, "does not have a computer science") || strings.Contains(a, "ไม่มีรางวัลโนเบลสาขาวิทยาการคอมพิวเตอร์") || strings.Contains(a, "ไม่มีสาขาวิทยาการคอมพิวเตอร์")) && !strings.Contains(a, "2024 nobel prize in computer science was awarded")
			},
		},
		{
			Name: "strict_json_instruction", Category: "instruction", Points: 8,
			Prompt: "Convert 7.5 kilometers to meters. Return ONLY a JSON object with exactly two keys: `result` (number) and `unit` (string). The `unit` value must be exactly `m`. Do not use Markdown or add explanation.",
			Score: func(answer string) bool {
				var obj map[string]any
				if json.Unmarshal([]byte(strings.TrimSpace(answer)), &obj) != nil || len(obj) != 2 {
					return false
				}
				result, ok := obj["result"].(float64)
				unit, ok2 := obj["unit"].(string)
				return ok && ok2 && result == 7500 && strings.EqualFold(strings.TrimSpace(unit), "m")
			},
		},
	}
}

func routeBenchmarkModels() map[string]string {
	return map[string]string{
		"fast":     envOr("DAIKI_LIVE_ROUTE_FAST_MODEL", "groq-groq-compound-mini"),
		"balanced": envOr("DAIKI_LIVE_ROUTE_BALANCED_MODEL", "groq-groq-compound"),
		"deep":     envOr("DAIKI_LIVE_ROUTE_DEEP_MODEL", "groq-qwen-qwen3.8-27b"),
	}
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func intEnvOr(key string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func csvSelection(key string) map[string]bool {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return nil
	}
	out := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		if v := strings.TrimSpace(part); v != "" {
			out[v] = true
		}
	}
	return out
}

func selectedRouteCases(all []liveRouteQualityCase) []liveRouteQualityCase {
	selected := csvSelection("DAIKI_LIVE_ROUTE_CASES")
	if len(selected) == 0 {
		return all
	}
	out := make([]liveRouteQualityCase, 0, len(all))
	for _, tc := range all {
		if selected[tc.Name] {
			out = append(out, tc)
		}
	}
	return out
}

func routeBenchmarkThinkingMode(route string) string {
	defaults := map[string]string{"fast": "low", "balanced": "medium", "deep": "high"}
	if global := strings.TrimSpace(os.Getenv("DAIKI_LIVE_ROUTE_THINKING_MODE")); global != "" {
		return global
	}
	return envOr("DAIKI_LIVE_ROUTE_"+strings.ToUpper(route)+"_THINKING_MODE", defaults[route])
}

func routeBenchmarkMaxTokens(route string) int {
	if global := intEnvOr("DAIKI_LIVE_ROUTE_MAX_TOKENS", 0); global > 0 {
		return global
	}
	defaults := map[string]int{"fast": 1200, "balanced": 1800, "deep": 8192}
	return intEnvOr("DAIKI_LIVE_ROUTE_"+strings.ToUpper(route)+"_MAX_TOKENS", defaults[route])
}

func TestLiveRouteAnswerQualityBenchmark(t *testing.T) {
	if os.Getenv("DAIKI_LIVE_ROUTE_QUALITY_BENCHMARK") != "1" {
		t.Skip("set DAIKI_LIVE_ROUTE_QUALITY_BENCHMARK=1 to run provider-backed Fast/Balanced/Deep quality benchmark")
	}
	endpoint := strings.TrimSpace(os.Getenv("DAIKI_LIVE_REASONING_URL"))
	apiKey := strings.TrimSpace(os.Getenv("DAIKI_LIVE_REASONING_API_KEY"))
	if endpoint == "" || apiKey == "" {
		t.Fatal("DAIKI_LIVE_REASONING_URL and DAIKI_LIVE_REASONING_API_KEY are required")
	}

	pauseMS := intEnvOr("DAIKI_LIVE_ROUTE_PAUSE_MS", 2200)
	client := &http.Client{Timeout: 90 * time.Second}
	cases := selectedRouteCases(routeQualityCases())
	if len(cases) == 0 {
		t.Fatal("DAIKI_LIVE_ROUTE_CASES did not match any benchmark case")
	}
	models := routeBenchmarkModels()
	routes := []string{"fast", "balanced", "deep"}
	if only := csvSelection("DAIKI_LIVE_ROUTE_ONLY"); len(only) > 0 {
		filtered := make([]string, 0, len(routes))
		for _, route := range routes {
			if only[route] {
				filtered = append(filtered, route)
			}
		}
		routes = filtered
	}
	if len(routes) == 0 {
		t.Fatal("DAIKI_LIVE_ROUTE_ONLY did not match fast, balanced, or deep")
	}
	results := make(map[string]liveRouteBenchmarkResult, len(routes))

	for _, route := range routes {
		model := models[route]
		thinkingMode := routeBenchmarkThinkingMode(route)
		maxTokens := routeBenchmarkMaxTokens(route)
		maxScore := 0
		categoryMax := map[string]int{}
		for _, tc := range cases {
			maxScore += tc.Points
			categoryMax[tc.Category] += tc.Points
		}
		result := liveRouteBenchmarkResult{Route: route, Model: model, ThinkingMode: thinkingMode, MaxScore: maxScore, TotalCases: len(cases), CategoryScore: map[string]int{}, CategoryMax: categoryMax}
		start := time.Now()
		t.Run(route, func(t *testing.T) {
			for _, tc := range cases {
				caseStart := time.Now()
				ctx, cancel := context.WithTimeout(t.Context(), 85*time.Second)
				answer, err := runLiveReasoningCompletion(ctx, client, endpoint, apiKey, model, thinkingMode, tc.Prompt, int64(maxTokens))
				cancel()
				elapsed := time.Since(caseStart)
				if err != nil {
					result.Failures = append(result.Failures, tc.Name+":provider_error")
					t.Logf("ROUTE_CASE route=%s model=%s case=%s category=%s points=0/%d latency_ms=%d error=%v", route, model, tc.Name, tc.Category, tc.Points, elapsed.Milliseconds(), err)
				} else {
					passed := tc.Score(answer)
					if passed {
						result.Score += tc.Points
						result.Passed++
						result.CategoryScore[tc.Category] += tc.Points
					} else {
						result.Failures = append(result.Failures, tc.Name)
					}
					t.Logf("ROUTE_CASE route=%s model=%s case=%s category=%s points=%d/%d latency_ms=%d", route, model, tc.Name, tc.Category, map[bool]int{true: tc.Points, false: 0}[passed], tc.Points, elapsed.Milliseconds())
					if !passed {
						t.Logf("ROUTE_FAILURE route=%s case=%s answer=%q", route, tc.Name, answer)
					}
				}
				if pauseMS > 0 {
					time.Sleep(time.Duration(pauseMS) * time.Millisecond)
				}
			}
		})
		result.Duration = time.Since(start)
		results[route] = result
		categories := make([]string, 0, len(result.CategoryMax))
		for category := range result.CategoryMax {
			categories = append(categories, category)
		}
		sort.Strings(categories)
		for _, category := range categories {
			t.Logf("ROUTE_CATEGORY_SCORE route=%s model=%s category=%s score=%d/%d", route, model, category, result.CategoryScore[category], result.CategoryMax[category])
		}
		t.Logf("ROUTE_QUALITY_SCORE route=%s model=%s thinking=%s max_tokens=%d score=%d/%d passed=%d/%d duration_ms=%d failures=%v", route, model, thinkingMode, maxTokens, result.Score, result.MaxScore, result.Passed, result.TotalCases, result.Duration.Milliseconds(), result.Failures)
	}

	if len(cases) != len(routeQualityCases()) || len(routes) != 3 {
		return
	}
	floors := map[string]int{
		"fast":     intEnvOr("DAIKI_LIVE_ROUTE_FAST_FLOOR", 80),
		"balanced": intEnvOr("DAIKI_LIVE_ROUTE_BALANCED_FLOOR", 90),
		"deep":     intEnvOr("DAIKI_LIVE_ROUTE_DEEP_FLOOR", 90),
	}
	var failures []string
	for _, route := range routes {
		if results[route].Score < floors[route] {
			failures = append(failures, fmt.Sprintf("%s=%d<%d", route, results[route].Score, floors[route]))
		}
	}
	// Fast is allowed to trade some quality for latency, but the higher-quality
	// routes must not materially regress below it on the same gold-answer set.
	if results["balanced"].Score+5 < results["fast"].Score {
		failures = append(failures, fmt.Sprintf("balanced_regressed fast=%d balanced=%d", results["fast"].Score, results["balanced"].Score))
	}
	if results["deep"].Score+5 < results["balanced"].Score {
		failures = append(failures, fmt.Sprintf("deep_regressed balanced=%d deep=%d", results["balanced"].Score, results["deep"].Score))
	}
	for _, route := range routes {
		for _, failedCase := range results[route].Failures {
			if strings.HasPrefix(failedCase, "false_premise_nobel_cs") {
				failures = append(failures, route+"_hallucination_guard_failed")
			}
		}
	}
	if len(failures) > 0 {
		sort.Strings(failures)
		t.Fatalf("route answer quality below acceptance: %s", strings.Join(failures, ", "))
	}
}

func TestRouteQualityScorerNormalizesUnicodeNumberSpacing(t *testing.T) {
	var thaiCase *liveRouteQualityCase
	for i := range routeQualityCases() {
		if routeQualityCases()[i].Name == "thai_discount_vat" {
			tc := routeQualityCases()[i]
			thaiCase = &tc
			break
		}
	}
	if thaiCase == nil {
		t.Fatal("thai_discount_vat benchmark case missing")
	}
	for _, answer := range []string{"1,070 บาท", "1\u202f070 บาท", "1 070 บาท", "1070.00 บาท"} {
		if !thaiCase.Score(answer) {
			t.Fatalf("expected scorer to accept %q", answer)
		}
	}
}

func TestRouteBenchmarkPolicyDefaults(t *testing.T) {
	for _, key := range []string{
		"DAIKI_LIVE_ROUTE_THINKING_MODE",
		"DAIKI_LIVE_ROUTE_FAST_THINKING_MODE", "DAIKI_LIVE_ROUTE_BALANCED_THINKING_MODE", "DAIKI_LIVE_ROUTE_DEEP_THINKING_MODE",
		"DAIKI_LIVE_ROUTE_MAX_TOKENS",
		"DAIKI_LIVE_ROUTE_FAST_MAX_TOKENS", "DAIKI_LIVE_ROUTE_BALANCED_MAX_TOKENS", "DAIKI_LIVE_ROUTE_DEEP_MAX_TOKENS",
	} {
		t.Setenv(key, "")
	}
	wantMode := map[string]string{"fast": "low", "balanced": "medium", "deep": "high"}
	wantTokens := map[string]int{"fast": 1200, "balanced": 1800, "deep": 8192}
	for _, route := range []string{"fast", "balanced", "deep"} {
		if got := routeBenchmarkThinkingMode(route); got != wantMode[route] {
			t.Fatalf("%s thinking mode=%q want %q", route, got, wantMode[route])
		}
		if got := routeBenchmarkMaxTokens(route); got != wantTokens[route] {
			t.Fatalf("%s max tokens=%d want %d", route, got, wantTokens[route])
		}
	}
}
