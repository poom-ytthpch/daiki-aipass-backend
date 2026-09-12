package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/poom-ytthpch/daiki-ai-passport-backend/internal/store"
)

type connectedProviderCandidate struct {
	Provider string
	Route    string
	Model    string
}

type connectedProviderResult struct {
	Provider      string
	Route         string
	Model         string
	Score         int
	MaxScore      int
	Passed        int
	Total         int
	Successes     int
	ProviderError int
	LatencyTotal  time.Duration
	CategoryScore map[string]int
	CategoryMax   map[string]int
	Failures      []string
}

func connectedProviderCandidates() []connectedProviderCandidate {
	candidates := []connectedProviderCandidate{
		{Provider: "mistral", Route: "fast", Model: "ministral-8b-latest"},
		{Provider: "mistral", Route: "balanced", Model: "mistral-small-2603"},
		{Provider: "mistral", Route: "deep", Model: "mistral-medium-3-5"},
		{Provider: "gemini", Route: "fast", Model: "gemini-3.5-flash-lite"},
		{Provider: "gemini", Route: "balanced", Model: "gemini-3.7-flash"},
		{Provider: "gemini", Route: "deep", Model: "gemini-3.8-flash"},
		{Provider: "opencode", Route: "fast", Model: "nemotron-3.5-lightning-free"},
		{Provider: "opencode", Route: "balanced", Model: "deepseek-v4-flash-free"},
		{Provider: "opencode", Route: "deep", Model: "nemotron-3-ultra-free"},
	}
	for i := range candidates {
		key := "DAIKI_LIVE_PROVIDER_" + strings.ToUpper(candidates[i].Provider) + "_" + strings.ToUpper(candidates[i].Route) + "_MODEL"
		if override := strings.TrimSpace(os.Getenv(key)); override != "" {
			candidates[i].Model = override
		}
	}
	return candidates
}

func connectedProviderKind(p store.ModelProvider) string {
	base := strings.ToLower(p.BaseURL)
	name := strings.ToLower(p.Name)
	switch {
	case strings.Contains(base, "mistral.ai") || strings.Contains(name, "mistral") || strings.Contains(name, "mitral"):
		return "mistral"
	case p.ProviderType == "gemini" || strings.Contains(base, "generativelanguage.googleapis.com") || strings.Contains(name, "gemini"):
		return "gemini"
	case strings.Contains(base, "opencode.ai") || strings.Contains(name, "opencode"):
		return "opencode"
	default:
		return ""
	}
}

func connectedProviderEndpoint(kind string, p store.ModelProvider) string {
	base := strings.TrimRight(p.BaseURL, "/")
	if kind == "gemini" {
		return base + "/openai/chat/completions"
	}
	if strings.HasSuffix(base, "/v1") {
		return base + "/chat/completions"
	}
	return base + "/v1/chat/completions"
}

func connectedProviderNativeReasoning(kind, route string) bool {
	// Gemini's OpenAI compatibility layer supports reasoning_effort. For the
	// other providers we keep the benchmark portable and let the Daiki system
	// instruction control reasoning depth.
	return kind == "gemini" && route != "fast"
}

func runConnectedProviderCompletion(ctx context.Context, client *http.Client, endpoint, apiKey, model, kind, route, prompt string, maxTokens int64) (string, error) {
	mode := routeBenchmarkThinkingMode(route)
	profile := thinkingProfileFor(mode)
	completionCap := profile.MaxCompletionTokens
	if maxTokens > 0 && maxTokens < completionCap {
		completionCap = maxTokens
	}
	body := map[string]any{
		"model": model,
		"messages": []map[string]any{
			{"role": "system", "content": thinkingInstruction(profile, thinkingTaskClass([]byte(fmt.Sprintf(`{"messages":[{"role":"user","content":%q}]}`, prompt))))},
			{"role": "user", "content": prompt},
		},
		"temperature": 0,
		"max_tokens":  completionCap,
		"stream":      false,
	}
	if connectedProviderNativeReasoning(kind, route) {
		body["reasoning_effort"] = mode
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", err
	}

	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
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
		respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		_ = resp.Body.Close()
		if readErr != nil {
			return "", readErr
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			var out struct {
				Choices []struct {
					Message struct {
						Content any `json:"content"`
					} `json:"message"`
				} `json:"choices"`
			}
			if err := json.Unmarshal(respBody, &out); err != nil {
				return "", err
			}
			if len(out.Choices) == 0 {
				return "", fmt.Errorf("provider returned no choices")
			}
			answer := normalizeProviderMatrixContent(out.Choices[0].Message.Content)
			if strings.TrimSpace(answer) == "" {
				return "", fmt.Errorf("provider returned no answer")
			}
			return strings.TrimSpace(answer), nil
		}
		lastErr = fmt.Errorf("provider status %d: %s", resp.StatusCode, compactProviderError(respBody))
		retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusGatewayTimeout
		if !retryable || attempt == 3 {
			return "", lastErr
		}
		wait := time.Duration(2+attempt*3) * time.Second
		if retryAfter := strings.TrimSpace(resp.Header.Get("Retry-After")); retryAfter != "" {
			if seconds, parseErr := time.ParseDuration(retryAfter + "s"); parseErr == nil && seconds > wait {
				wait = seconds
			}
		}
		if bodyWait := providerRetryDelay(respBody); bodyWait > wait {
			wait = bodyWait
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(wait):
		}
	}
	return "", lastErr
}

func providerRetryDelay(raw []byte) time.Duration {
	lower := strings.ToLower(string(raw))
	for _, marker := range []string{"retry in ", "try again in "} {
		idx := strings.Index(lower, marker)
		if idx < 0 {
			continue
		}
		rest := strings.TrimSpace(lower[idx+len(marker):])
		if end := strings.Index(rest, "s"); end >= 0 {
			if d, err := time.ParseDuration(strings.TrimSpace(rest[:end+1])); err == nil {
				return d + 1500*time.Millisecond
			}
		}
	}
	return 0
}

func normalizeProviderMatrixContent(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []any:
		var b strings.Builder
		for _, item := range x {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if text, ok := m["text"].(string); ok {
				b.WriteString(text)
			}
		}
		return b.String()
	default:
		return ""
	}
}

func compactProviderError(raw []byte) string {
	text := strings.Join(strings.Fields(string(raw)), " ")
	if len(text) > 500 {
		text = text[:500] + "…"
	}
	return text
}

func TestLiveConnectedProviderQualityMatrix(t *testing.T) {
	if os.Getenv("DAIKI_LIVE_CONNECTED_PROVIDER_MATRIX") != "1" {
		t.Skip("set DAIKI_LIVE_CONNECTED_PROVIDER_MATRIX=1 to benchmark connected Mistral/Gemini/OpenCode providers")
	}
	if strings.TrimSpace(os.Getenv("DATABASE_URL")) == "" || strings.TrimSpace(os.Getenv("TOKEN_ENCRYPTION_KEY")) == "" {
		t.Fatal("DATABASE_URL and TOKEN_ENCRYPTION_KEY are required")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Minute)
	defer cancel()
	db, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	a := &app{
		cfg:   config{TokenEncryptionKey: strings.TrimSpace(os.Getenv("TOKEN_ENCRYPTION_KEY"))},
		http:  &http.Client{Timeout: 90 * time.Second},
		store: store.New(db),
	}
	providers, err := a.store.ModelProviders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	providerByKind := map[string]store.ModelProvider{}
	for _, p := range providers {
		if kind := connectedProviderKind(p); kind != "" && p.Enabled {
			providerByKind[kind] = p
		}
	}
	for _, kind := range []string{"mistral", "gemini", "opencode"} {
		if _, ok := providerByKind[kind]; !ok {
			t.Fatalf("connected provider %s not found", kind)
		}
	}

	available := map[string]map[string]bool{}
	keys := map[string]string{}
	for kind, p := range providerByKind {
		models, latency, err := a.discoverProvider(ctx, p)
		if err != nil {
			t.Fatalf("discover %s: %v", kind, err)
		}
		available[kind] = map[string]bool{}
		for _, m := range models {
			available[kind][m.ID] = true
		}
		key, err := a.providerAPIKey(p)
		if err != nil || key == "" {
			t.Fatalf("provider key unavailable for %s: %v", kind, err)
		}
		keys[kind] = key
		t.Logf("PROVIDER_DISCOVERY provider=%s models=%d latency_ms=%d", kind, len(models), latency.Milliseconds())
	}

	cases := routeQualityCases()
	if selected := csvSelection("DAIKI_LIVE_PROVIDER_CASES"); len(selected) > 0 {
		filtered := make([]liveRouteQualityCase, 0, len(cases))
		for _, tc := range cases {
			if selected[tc.Name] {
				filtered = append(filtered, tc)
			}
		}
		cases = filtered
		if len(cases) == 0 {
			t.Fatal("DAIKI_LIVE_PROVIDER_CASES matched no benchmark cases")
		}
	}
	pause := time.Duration(intEnvOr("DAIKI_LIVE_PROVIDER_MATRIX_PAUSE_MS", 1500)) * time.Millisecond
	client := &http.Client{Timeout: 95 * time.Second}
	providerOnly := csvSelection("DAIKI_LIVE_PROVIDER_ONLY")
	routeOnly := csvSelection("DAIKI_LIVE_PROVIDER_ROUTE_ONLY")
	candidates := make([]connectedProviderCandidate, 0, len(connectedProviderCandidates()))
	for _, candidate := range connectedProviderCandidates() {
		if len(providerOnly) > 0 && !providerOnly[candidate.Provider] {
			continue
		}
		if len(routeOnly) > 0 && !routeOnly[candidate.Route] {
			continue
		}
		candidates = append(candidates, candidate)
	}
	if len(candidates) == 0 {
		t.Fatal("provider/route filters matched no benchmark candidates")
	}
	results := make([]connectedProviderResult, 0, len(candidates))
	for _, candidate := range candidates {
		p := providerByKind[candidate.Provider]
		result := connectedProviderResult{
			Provider:      candidate.Provider,
			Route:         candidate.Route,
			Model:         candidate.Model,
			Total:         len(cases),
			CategoryScore: map[string]int{},
			CategoryMax:   map[string]int{},
		}
		for _, tc := range cases {
			result.MaxScore += tc.Points
			result.CategoryMax[tc.Category] += tc.Points
		}
		if !available[candidate.Provider][candidate.Model] {
			result.ProviderError = len(cases)
			result.Failures = append(result.Failures, "model_unavailable")
			t.Logf("PROVIDER_ROUTE_UNAVAILABLE provider=%s route=%s model=%s", candidate.Provider, candidate.Route, candidate.Model)
			results = append(results, result)
			continue
		}

		endpoint := connectedProviderEndpoint(candidate.Provider, p)
		for _, tc := range cases {
			caseCtx, caseCancel := context.WithTimeout(ctx, 90*time.Second)
			started := time.Now()
			answer, err := runConnectedProviderCompletion(caseCtx, client, endpoint, keys[candidate.Provider], candidate.Model, candidate.Provider, candidate.Route, tc.Prompt, int64(routeBenchmarkMaxTokens(candidate.Route)))
			elapsed := time.Since(started)
			caseCancel()
			result.LatencyTotal += elapsed
			if err != nil {
				result.ProviderError++
				result.Failures = append(result.Failures, tc.Name+":provider_error")
				t.Logf("PROVIDER_CASE provider=%s route=%s model=%s case=%s category=%s points=0/%d latency_ms=%d error=%v", candidate.Provider, candidate.Route, candidate.Model, tc.Name, tc.Category, tc.Points, elapsed.Milliseconds(), err)
			} else {
				result.Successes++
				passed := tc.Score(answer)
				if passed {
					result.Score += tc.Points
					result.Passed++
					result.CategoryScore[tc.Category] += tc.Points
				} else {
					result.Failures = append(result.Failures, tc.Name)
				}
				t.Logf("PROVIDER_CASE provider=%s route=%s model=%s case=%s category=%s points=%d/%d latency_ms=%d", candidate.Provider, candidate.Route, candidate.Model, tc.Name, tc.Category, map[bool]int{true: tc.Points, false: 0}[passed], tc.Points, elapsed.Milliseconds())
				if !passed {
					t.Logf("PROVIDER_FAILURE provider=%s route=%s case=%s answer=%q", candidate.Provider, candidate.Route, tc.Name, answer)
				}
			}
			if pause > 0 {
				select {
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-time.After(pause):
				}
			}
		}
		avgLatency := int64(0)
		if result.Total > 0 {
			avgLatency = result.LatencyTotal.Milliseconds() / int64(result.Total)
		}
		t.Logf("PROVIDER_ROUTE_SCORE provider=%s route=%s model=%s thinking=%s score=%d/%d passed=%d/%d request_success=%d/%d provider_errors=%d avg_latency_ms=%d failures=%v", result.Provider, result.Route, result.Model, routeBenchmarkThinkingMode(result.Route), result.Score, result.MaxScore, result.Passed, result.Total, result.Successes, result.Total, result.ProviderError, avgLatency, result.Failures)
		categories := make([]string, 0, len(result.CategoryMax))
		for category := range result.CategoryMax {
			categories = append(categories, category)
		}
		sort.Strings(categories)
		for _, category := range categories {
			t.Logf("PROVIDER_CATEGORY_SCORE provider=%s route=%s model=%s category=%s score=%d/%d", result.Provider, result.Route, result.Model, category, result.CategoryScore[category], result.CategoryMax[category])
		}
		results = append(results, result)
	}

	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Route == results[j].Route {
			if results[i].Score == results[j].Score {
				return results[i].ProviderError < results[j].ProviderError
			}
			return results[i].Score > results[j].Score
		}
		return results[i].Route < results[j].Route
	})
	for _, result := range results {
		t.Logf("PROVIDER_MATRIX_RANK route=%s provider=%s model=%s score=%d/%d errors=%d", result.Route, result.Provider, result.Model, result.Score, result.MaxScore, result.ProviderError)
	}
}
