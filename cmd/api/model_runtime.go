package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/poom-ytthpch/daiki-ai-passport-backend/internal/store"
)

type modelRecoveryMeta struct {
	Attempts              int    `json:"attempts"`
	ContextTrimmed        bool   `json:"contextTrimmed"`
	OriginalInputTokens   int    `json:"originalInputTokens"`
	FinalInputTokens      int    `json:"finalInputTokens"`
	ObservedRateLimit     int    `json:"observedRateLimit,omitempty"`
	ObservedRequested     int    `json:"observedRequested,omitempty"`
	ObservedRateKind      string `json:"observedRateKind,omitempty"`
	ObservedHTTPStatus    int    `json:"observedHttpStatus,omitempty"`
	FailureKind           string `json:"failureKind,omitempty"`
	CircuitBypass         bool   `json:"circuitBypass,omitempty"`
	CircuitModel          string `json:"circuitModel,omitempty"`
	FallbackFrom          string `json:"fallbackFrom,omitempty"`
	PreflightFallback     bool   `json:"preflightFallback,omitempty"`
	PredictedInputTokens  int    `json:"predictedInputTokens,omitempty"`
	FallbackTo            string `json:"fallbackTo,omitempty"`
	FinalModel            string `json:"finalModel"`
	RuntimeProfile        string `json:"runtimeProfile,omitempty"`
	AppliedOverheadTokens int    `json:"appliedOverheadTokens,omitempty"`
	AdmissionWaitMS       int64  `json:"admissionWaitMs,omitempty"`
	AdmissionSpillover    bool   `json:"admissionSpillover,omitempty"`
	AdmissionTokens       int    `json:"admissionTokens,omitempty"`
}

type rateLimitObservation struct {
	Kind      string
	Limit     int
	Requested int
}

var providerLimitPattern = regexp.MustCompile(`(?i)limit\s+([0-9][0-9,]*)\s*,\s*requested\s+([0-9][0-9,]*)`)
var providerHTTPStatusPattern = regexp.MustCompile(`(?i)\bHTTP\s+([1-5][0-9]{2})\b`)
var providerJSONCodePattern = regexp.MustCompile(`(?i)"code"\s*:\s*"?([1-5][0-9]{2})"?`)

func recoverableProviderStatus(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusNotFound || status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500
}

func providerFailureStatus(raw []byte) int {
	text := string(raw)
	for _, re := range []*regexp.Regexp{providerHTTPStatusPattern, providerJSONCodePattern} {
		if m := re.FindStringSubmatch(text); len(m) == 2 {
			status, _ := strconv.Atoi(m[1])
			if recoverableProviderStatus(status) {
				return status
			}
		}
	}
	lower := strings.ToLower(text)
	if strings.Contains(lower, "authenticationerror") || strings.Contains(lower, "authentication error") || strings.Contains(lower, "user not found") {
		return http.StatusUnauthorized
	}
	if strings.Contains(lower, "ratelimiterror") || strings.Contains(lower, "rate limit") {
		return http.StatusTooManyRequests
	}
	return 0
}

func failureKindForStatus(status int) string {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return "provider_auth"
	case http.StatusNotFound:
		return "provider_model"
	case http.StatusRequestTimeout:
		return "provider_timeout"
	case http.StatusTooManyRequests:
		return "provider_rate_limit"
	default:
		if status >= 500 {
			return "provider_upstream"
		}
	}
	return "provider_error"
}

func parsePositiveInt(s string) int {
	s = strings.ReplaceAll(strings.TrimSpace(s), ",", "")
	v, _ := strconv.Atoi(s)
	if v < 0 {
		return 0
	}
	return v
}

func parseRateLimitObservation(status int, body []byte) rateLimitObservation {
	if status != http.StatusTooManyRequests {
		return rateLimitObservation{}
	}
	text := strings.ToLower(string(body))
	obs := rateLimitObservation{}
	switch {
	case strings.Contains(text, "input tokens per minute") || strings.Contains(text, "itpm"):
		obs.Kind = "itpm"
	case strings.Contains(text, "output tokens per minute") || strings.Contains(text, "otpm"):
		obs.Kind = "otpm"
	case strings.Contains(text, "tokens per minute") || strings.Contains(text, "tpm"):
		obs.Kind = "tpm"
	case strings.Contains(text, "requests per minute") || strings.Contains(text, "rpm"):
		obs.Kind = "rpm"
	default:
		obs.Kind = "rate_limit"
	}
	if m := providerLimitPattern.FindStringSubmatch(string(body)); len(m) == 3 {
		obs.Limit, obs.Requested = parsePositiveInt(m[1]), parsePositiveInt(m[2])
	}
	return obs
}

func estimateMessageTokens(v any) int {
	switch x := v.(type) {
	case string:
		return max(1, (len([]byte(x))+3)/4)
	case []any:
		n := 0
		for _, item := range x {
			n += estimateMessageTokens(item)
		}
		return n
	case map[string]any:
		typ, _ := x["type"].(string)
		if typ == "image_url" || typ == "input_image" || typ == "image" {
			return 512
		}
		n := 0
		for k, item := range x {
			if k == "image_url" || k == "url" {
				continue
			}
			n += estimateMessageTokens(item)
		}
		return n
	default:
		return 0
	}
}

func estimateChatInputTokens(body []byte) int {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return max(1, len(body)/4)
	}
	messages, _ := payload["messages"].([]any)
	total := 64
	for _, raw := range messages {
		total += 8 + estimateMessageTokens(raw)
	}
	return max(1, total)
}

func compactText(v string, budgetTokens int) string {
	if budgetTokens <= 0 {
		return ""
	}
	maxBytes := budgetTokens * 4
	if len(v) <= maxBytes {
		return v
	}
	if maxBytes < 160 {
		return v[len(v)-maxBytes:]
	}
	head := maxBytes * 3 / 5
	tail := maxBytes - head - len("\n…[context compacted]…\n")
	if tail < 32 {
		tail = 32
	}
	return v[:head] + "\n…[context compacted]…\n" + v[len(v)-tail:]
}

func compactMessage(raw any, budgetTokens int) any {
	m, ok := raw.(map[string]any)
	if !ok {
		return raw
	}
	copyMap := make(map[string]any, len(m))
	for k, v := range m {
		copyMap[k] = v
	}
	switch c := copyMap["content"].(type) {
	case string:
		copyMap["content"] = compactText(c, budgetTokens)
	case []any:
		remaining := budgetTokens
		out := make([]any, 0, len(c))
		for _, part := range c {
			pm, _ := part.(map[string]any)
			if pm != nil {
				typ, _ := pm["type"].(string)
				if typ == "image_url" || typ == "input_image" || typ == "image" {
					out = append(out, part)
					remaining -= 512
					continue
				}
				if text, ok := pm["text"].(string); ok {
					cp := make(map[string]any, len(pm))
					for k, v := range pm {
						cp[k] = v
					}
					cp["text"] = compactText(text, max(0, remaining))
					remaining -= estimateMessageTokens(cp["text"])
					out = append(out, cp)
					continue
				}
			}
			out = append(out, part)
			remaining -= estimateMessageTokens(part)
		}
		copyMap["content"] = out
	}
	return copyMap
}

func trimChatContext(body []byte, target int) ([]byte, int, int, error) {
	if target <= 0 {
		return body, estimateChatInputTokens(body), estimateChatInputTokens(body), nil
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, 0, 0, err
	}
	messages, ok := payload["messages"].([]any)
	if !ok || len(messages) == 0 {
		return body, estimateChatInputTokens(body), estimateChatInputTokens(body), nil
	}
	original := estimateChatInputTokens(body)
	if original <= target {
		return body, original, original, nil
	}
	budget := max(256, target-96)
	selected := make([]any, 0, len(messages))
	used := 0
	for i := len(messages) - 1; i >= 0; i-- {
		cost := 8 + estimateMessageTokens(messages[i])
		if len(selected) == 0 || used+cost <= budget {
			selected = append(selected, messages[i])
			used += cost
			continue
		}
		// Preserve a leading system/developer instruction if it fits after compaction.
		if i == 0 {
			if m, ok := messages[i].(map[string]any); ok {
				role, _ := m["role"].(string)
				if role == "system" || role == "developer" {
					selected = append(selected, compactMessage(messages[i], max(64, budget-used)))
					used = budget
				}
			}
		}
	}
	for i, j := 0, len(selected)-1; i < j; i, j = i+1, j-1 {
		selected[i], selected[j] = selected[j], selected[i]
	}
	payload["messages"] = selected
	out, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, 0, err
	}
	final := estimateChatInputTokens(out)
	if final > target && len(selected) > 0 {
		// The newest turn itself is larger than the target. Compact it rather than failing repeatedly.
		last := len(selected) - 1
		selected[last] = compactMessage(selected[last], max(128, target-192))
		payload["messages"] = selected
		out, err = json.Marshal(payload)
		if err != nil {
			return nil, 0, 0, err
		}
		final = estimateChatInputTokens(out)
	}
	return out, original, final, nil
}

func applyModelOutputCap(body []byte, maxOutput int) []byte {
	if maxOutput <= 0 {
		return body
	}
	var p map[string]any
	if json.Unmarshal(body, &p) != nil {
		return body
	}
	cur := maxOutput
	for _, key := range []string{"max_completion_tokens", "max_tokens"} {
		if v, ok := p[key].(float64); ok && int(v) > 0 && int(v) < cur {
			cur = int(v)
		}
		delete(p, "max_tokens")
	}
	p["max_completion_tokens"] = cur
	out, err := json.Marshal(p)
	if err != nil {
		return body
	}
	return out
}

func setRequestModel(body []byte, model string) []byte {
	if strings.TrimSpace(model) == "" {
		return body
	}
	var p map[string]any
	if json.Unmarshal(body, &p) != nil {
		return body
	}
	p["model"] = model
	out, err := json.Marshal(p)
	if err != nil {
		return body
	}
	return out
}

func runtimeContextTarget(m store.ProviderModel, observed int, factor float64) int {
	candidates := []int{}
	if m.ContextTargetTokens > 0 {
		candidates = append(candidates, m.ContextTargetTokens)
	}
	if m.MaxInputTokens > 0 {
		candidates = append(candidates, m.MaxInputTokens)
	}
	if m.ITPMLimit > 0 {
		candidates = append(candidates, int(float64(m.ITPMLimit)*0.78))
	}
	if observed > 0 {
		candidates = append(candidates, int(float64(observed)*0.78))
	}
	if len(candidates) == 0 {
		return 0
	}
	target := candidates[0]
	for _, v := range candidates[1:] {
		if v > 0 && v < target {
			target = v
		}
	}
	if factor > 0 && factor < 1 {
		target = int(float64(target) * factor)
	}
	return max(256, target)
}

func defaultRuntimeModel(name string) store.ProviderModel {
	return store.ProviderModel{LiteLLMModelName: name, TimeoutSeconds: 300, StreamTimeoutSeconds: 300, MaxRetries: 2, ProviderMaxRetries: 0, RetryBackoffMS: 500, ContextStrategy: "adaptive"}
}

func (a *app) runtimeModel(ctx context.Context, name string) store.ProviderModel {
	if a.store == nil || strings.TrimSpace(name) == "" {
		return defaultRuntimeModel(name)
	}
	m, err := a.store.ProviderModelByLiteLLMName(ctx, name)
	if err != nil {
		return defaultRuntimeModel(name)
	}
	if m.TimeoutSeconds <= 0 {
		m.TimeoutSeconds = 300
	}
	if m.StreamTimeoutSeconds <= 0 {
		m.StreamTimeoutSeconds = m.TimeoutSeconds
	}
	if m.ContextStrategy == "" {
		m.ContextStrategy = "adaptive"
	}
	return m
}

type replayReadCloser struct {
	io.Reader
	closer io.Closer
}

func (r *replayReadCloser) Close() error {
	if r.closer != nil {
		return r.closer.Close()
	}
	return nil
}

func responseLooksLikeRateLimitFailure(raw []byte) bool {
	text := strings.ToLower(string(raw))
	return strings.Contains(text, "http 429") &&
		(strings.Contains(text, "ratelimit") || strings.Contains(text, "rate limit")) &&
		(strings.Contains(text, "limit") || strings.Contains(text, "requested"))
}

func sseEventHasVisibleContent(raw []byte) bool {
	var event map[string]any
	if json.Unmarshal(raw, &event) != nil {
		return false
	}
	choices, _ := event["choices"].([]any)
	for _, item := range choices {
		choice, _ := item.(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		if text, _ := delta["content"].(string); strings.TrimSpace(text) != "" {
			return true
		}
		message, _ := choice["message"].(map[string]any)
		if text, _ := message["content"].(string); strings.TrimSpace(text) != "" {
			return true
		}
	}
	return false
}

// inspectHermesSoftFailure keeps the upstream response private until Hermes has
// produced a real assistant token. Hermes may answer HTTP 200 and only later emit
// an SSE `finish_reason:error` carrying the provider's 429. If that happens before
// visible content, Daiki can still recover without leaking the failed attempt to
// the browser.
func inspectHermesSoftFailure(resp *http.Response) ([]byte, int, error) {
	if resp == nil || resp.Body == nil || resp.StatusCode != http.StatusOK {
		return nil, 0, nil
	}
	contentType := strings.ToLower(resp.Header.Get("content-type"))
	if strings.Contains(contentType, "text/event-stream") {
		original := resp.Body
		reader := bufio.NewReader(original)
		var captured bytes.Buffer
		for captured.Len() < 512<<10 {
			line, err := reader.ReadString('\n')
			captured.WriteString(line)
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "data:") {
				payload := bytes.TrimSpace([]byte(strings.TrimPrefix(trimmed, "data:")))
				if bytes.Equal(payload, []byte("[DONE]")) {
					resp.Body = &replayReadCloser{Reader: io.MultiReader(bytes.NewReader(captured.Bytes()), reader), closer: original}
					return nil, 0, nil
				}
				// Once a real token exists, the response is committed; replay everything
				// captured so the browser receives the role/keepalive/content events intact.
				if sseEventHasVisibleContent(payload) {
					resp.Body = &replayReadCloser{Reader: io.MultiReader(bytes.NewReader(captured.Bytes()), reader), closer: original}
					return nil, 0, nil
				}
				if status := providerFailureStatus(payload); status != 0 {
					_ = original.Close()
					return append([]byte(nil), payload...), status, nil
				}
			}
			if err != nil {
				if err == io.EOF {
					resp.Body = io.NopCloser(bytes.NewReader(captured.Bytes()))
					_ = original.Close()
					return nil, 0, nil
				}
				_ = original.Close()
				return nil, 0, err
			}
		}
		resp.Body = &replayReadCloser{Reader: io.MultiReader(bytes.NewReader(captured.Bytes()), reader), closer: original}
		return nil, 0, nil
	}
	if strings.Contains(contentType, "application/json") || resp.ContentLength >= 0 && resp.ContentLength <= 2<<20 {
		original := resp.Body
		raw, err := io.ReadAll(io.LimitReader(original, 2<<20))
		if err != nil {
			_ = original.Close()
			return nil, 0, err
		}
		if status := providerFailureStatus(raw); status != 0 {
			_ = original.Close()
			return raw, status, nil
		}
		resp.Body = &replayReadCloser{Reader: io.MultiReader(bytes.NewReader(raw), original), closer: original}
	}
	return nil, 0, nil
}

func runtimeAgentOverhead(m store.ProviderModel, profile string) int {
	switch strings.ToLower(strings.TrimSpace(profile)) {
	case "research", "guest":
		if m.ResearchOverheadTokens > 0 {
			return m.ResearchOverheadTokens
		}
	}
	return m.AgentOverheadTokens
}
func runtimeTokenLimit(m store.ProviderModel) int {
	limit := m.TPMLimit
	if limit <= 0 || (m.ITPMLimit > 0 && m.ITPMLimit < limit) {
		limit = m.ITPMLimit
	}
	return limit
}

func requestCompletionReserve(body []byte, m store.ProviderModel) int {
	var payload struct {
		MaxTokens           int `json:"max_tokens"`
		MaxCompletionTokens int `json:"max_completion_tokens"`
	}
	_ = json.Unmarshal(body, &payload)
	out := payload.MaxCompletionTokens
	if out <= 0 {
		out = payload.MaxTokens
	}
	if out <= 0 {
		out = m.MaxOutputTokens
	}
	if out <= 0 {
		out = 256
	}
	// Admission protects throughput, not the user's quota. Reserving a bounded
	// completion tail tracks provider TPM closely without making a large configured
	// max_output_tokens stall every short chat turn for a full minute.
	return min(out, 256)
}

func modelAdmissionCost(body []byte, m store.ProviderModel, profile string) int {
	limit := runtimeTokenLimit(m)
	if limit <= 0 {
		return 0
	}
	cost := estimateChatInputTokens(body) + runtimeAgentOverhead(m, profile) + requestCompletionReserve(body, m)
	capacity := max(1, int(float64(limit)*0.85))
	if cost > capacity {
		cost = capacity
	}
	return max(1, cost)
}

func (a *app) tryModelAdmission(ctx context.Context, model string, body []byte, m store.ProviderModel, profile string) (bool, time.Duration, int, error) {
	if a.redis == nil {
		return true, 0, 0, nil
	}
	limit := runtimeTokenLimit(m)
	cost := modelAdmissionCost(body, m, profile)
	if limit <= 0 || cost <= 0 {
		return true, 0, 0, nil
	}
	// Keep 15% headroom for provider-side token accounting differences, title/
	// auxiliary calls and traffic that may not pass through this backend process.
	capacity := max(1, int(float64(limit)*0.85))
	refillPerMS := float64(limit) / 60000.0
	if refillPerMS <= 0 {
		return true, 0, cost, nil
	}
	key := "model:tpm:" + strings.TrimPrefix(modelCircuitKey(model), "model:circuit:")
	script := `local now=tonumber(ARGV[1]); local capacity=tonumber(ARGV[2]); local refill=tonumber(ARGV[3]); local cost=tonumber(ARGV[4]); local tokens=tonumber(redis.call('HGET',KEYS[1],'tokens')); local last=tonumber(redis.call('HGET',KEYS[1],'last')); if not tokens or not last then tokens=capacity; last=now end; tokens=math.min(capacity,tokens); if now>last then tokens=math.min(capacity,tokens+((now-last)*refill)); last=now end; if tokens>=cost then tokens=tokens-cost; redis.call('HSET',KEYS[1],'tokens',tokens,'last',last); redis.call('PEXPIRE',KEYS[1],120000); return 0 end; local need=cost-tokens; local wait=math.ceil(need/refill); redis.call('HSET',KEYS[1],'tokens',tokens,'last',last); redis.call('PEXPIRE',KEYS[1],120000); return wait`
	nowMS := time.Now().UnixMilli()
	waitMS, err := a.redis.Eval(ctx, script, []string{key}, nowMS, capacity, refillPerMS, cost).Int64()
	if err != nil {
		// Admission is an availability optimization. Redis queue/quota failures are
		// handled elsewhere; fail open rather than taking inference down here.
		return true, 0, cost, nil
	}
	if waitMS <= 0 {
		return true, 0, cost, nil
	}
	return false, time.Duration(waitMS) * time.Millisecond, cost, nil
}

func shouldSpillModelAdmission(primaryWait, fallbackWait time.Duration) bool {
	const spillThreshold = 2 * time.Second
	const meaningfulGain = 500 * time.Millisecond
	if primaryWait < spillThreshold {
		return false
	}
	return fallbackWait <= 0 || fallbackWait+meaningfulGain < primaryWait
}

func (a *app) waitForModelAdmission(ctx context.Context, model string, body []byte, m store.ProviderModel, profile string) (time.Duration, int, error) {
	started := time.Now()
	for {
		admitted, waitHint, cost, err := a.tryModelAdmission(ctx, model, body, m, profile)
		if err != nil {
			return time.Since(started), cost, err
		}
		if admitted {
			return time.Since(started), cost, nil
		}
		select {
		case <-ctx.Done():
			return time.Since(started), cost, ctx.Err()
		case <-time.After(min(waitHint, 15*time.Second)):
		}
	}
}

func runtimeSafeInputBudget(m store.ProviderModel, overhead int) int {
	limit := m.ITPMLimit
	if limit <= 0 || (m.TPMLimit > 0 && m.TPMLimit < limit) {
		limit = m.TPMLimit
	}
	if limit <= 0 {
		return 0
	}
	budget := int(float64(limit) * 0.90)
	if m.TPMLimit > 0 && m.MaxOutputTokens > 0 {
		reserve := min(m.MaxOutputTokens, max(128, int(float64(m.TPMLimit)*0.20)))
		budget -= reserve
	}
	messageBudget := budget - overhead
	if messageBudget <= 0 {
		return -1
	}
	return max(256, messageBudget)
}
func runtimeMessageTarget(m store.ProviderModel, observed int, factor float64, overhead int) int {
	target := runtimeContextTarget(m, observed, factor)
	limit := m.ITPMLimit
	if limit <= 0 || (m.TPMLimit > 0 && m.TPMLimit < limit) {
		limit = m.TPMLimit
	}
	if observed > 0 && (limit == 0 || observed < limit) {
		limit = observed
	}
	if limit > 0 {
		messageBudget := int(float64(limit)*0.90) - overhead
		if m.TPMLimit > 0 && m.MaxOutputTokens > 0 {
			messageBudget -= min(m.MaxOutputTokens, max(128, int(float64(m.TPMLimit)*0.20)))
		}
		if messageBudget <= 0 {
			return 0
		}
		if target == 0 || messageBudget < target {
			target = messageBudget
		}
	}
	if target <= 0 {
		return 0
	}
	return max(256, target)
}
func shouldPreflightFallback(m store.ProviderModel, body []byte, overhead int) bool {
	budget := runtimeSafeInputBudget(m, overhead)
	if strings.TrimSpace(m.FallbackModelName) == "" || !(m.ContextStrategy == "adaptive" || m.ContextStrategy == "fallback") {
		return false
	}
	if budget < 0 {
		return true
	}
	if budget == 0 {
		return false
	}
	predictedMessages := estimateChatInputTokens(body)
	return predictedMessages > budget
}

type inferenceRequestFactory func(body []byte) (*http.Request, error)

func modelCircuitKey(model string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(model)))
	return "model:circuit:" + hex.EncodeToString(sum[:12])
}

func modelCircuitTTL(status int, kind string) time.Duration {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return 10 * time.Minute
	case http.StatusRequestTimeout:
		return 45 * time.Second
	}
	if status >= 500 {
		return 45 * time.Second
	}
	if kind == "hermes_transport" {
		return 45 * time.Second
	}
	return 30 * time.Second
}

func (a *app) modelCircuitOpen(ctx context.Context, model string) bool {
	if a == nil || a.redis == nil || strings.TrimSpace(model) == "" {
		return false
	}
	n, err := a.redis.Exists(ctx, modelCircuitKey(model)).Result()
	return err == nil && n > 0
}

func (a *app) openModelCircuit(ctx context.Context, model string, status int, kind string) {
	if a == nil || a.redis == nil || strings.TrimSpace(model) == "" || status == http.StatusTooManyRequests {
		return
	}
	_ = a.redis.Set(ctx, modelCircuitKey(model), first(kind, failureKindForStatus(status)), modelCircuitTTL(status, kind)).Err()
}

func (a *app) clearModelCircuit(ctx context.Context, model string) {
	if a == nil || a.redis == nil || strings.TrimSpace(model) == "" {
		return
	}
	_ = a.redis.Del(ctx, modelCircuitKey(model)).Err()
}

func chatPayloadHasImage(body []byte) bool {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return bytes.Contains(body, []byte(`"image_url"`)) || bytes.Contains(body, []byte(`"input_image"`))
	}
	var walk func(any) bool
	walk = func(v any) bool {
		switch x := v.(type) {
		case []any:
			for _, item := range x {
				if walk(item) {
					return true
				}
			}
		case map[string]any:
			if typ, _ := x["type"].(string); typ == "image_url" || typ == "input_image" || typ == "image" {
				return true
			}
			for key, item := range x {
				if key == "image_url" || key == "input_image" {
					return true
				}
				if walk(item) {
					return true
				}
			}
		}
		return false
	}
	return walk(payload["messages"])
}

func modelCanFallback(m store.ProviderModel, currentModel, strategy string) bool {
	return (strategy == "adaptive" || strategy == "fallback") && strings.TrimSpace(m.FallbackModelName) != "" && m.FallbackModelName != currentModel
}

func applyFallbackModel(ctx context.Context, a *app, body []byte, currentModel, profile string, m store.ProviderModel, meta *modelRecoveryMeta) ([]byte, string, store.ProviderModel, int, string) {
	if meta.FallbackFrom == "" {
		meta.FallbackFrom = currentModel
	}
	meta.FallbackTo = m.FallbackModelName
	currentModel = m.FallbackModelName
	body = setRequestModel(body, currentModel)
	m = a.runtimeModel(ctx, currentModel)
	overhead := runtimeAgentOverhead(m, profile)
	meta.AppliedOverheadTokens = overhead
	meta.FinalModel = currentModel
	body = applyModelOutputCap(body, m.MaxOutputTokens)
	strategy := m.ContextStrategy
	if strategy == "" {
		strategy = "adaptive"
	}
	return body, currentModel, m, overhead, strategy
}

func (a *app) doModelRequestWithRecovery(ctx context.Context, body []byte, model, profile string, makeReq inferenceRequestFactory) (*http.Response, []byte, string, modelRecoveryMeta, error) {
	m := a.runtimeModel(ctx, model)
	overhead := runtimeAgentOverhead(m, profile)
	meta := modelRecoveryMeta{Attempts: 0, OriginalInputTokens: estimateChatInputTokens(body), FinalModel: model, RuntimeProfile: profile, AppliedOverheadTokens: overhead}
	meta.PredictedInputTokens = meta.OriginalInputTokens + overhead
	currentModel := model
	// Never downgrade an image request to a text-only model fallback. The dedicated
	// Vision alias must resolve to a model that can actually inspect pixels; a clear
	// vision failure is safer than a plausible answer produced without seeing the image.
	allowModelFallback := !chatPayloadHasImage(body)
	strategy := m.ContextStrategy
	if strategy == "" {
		strategy = "adaptive"
	}

	// A recent deterministic provider failure opens a short shared circuit. Keep
	// the configured alias intact, but route new requests straight to its model-level
	// fallback until the circuit expires or an admin save/sync clears it.
	if allowModelFallback && a.modelCircuitOpen(ctx, currentModel) && modelCanFallback(m, currentModel, strategy) {
		meta.PreflightFallback = true
		meta.CircuitBypass = true
		meta.CircuitModel = currentModel
		body, currentModel, m, overhead, strategy = applyFallbackModel(ctx, a, body, currentModel, profile, m, &meta)
	}

	// A model whose Hermes/tool overhead already consumes its provider token budget cannot
	// succeed even with an empty conversation. Route around it before spending a
	// provider attempt or exposing an SSE stream.
	if allowModelFallback && !meta.PreflightFallback && shouldPreflightFallback(m, body, overhead) {
		meta.PreflightFallback = true
		meta.FallbackFrom = currentModel
		meta.FallbackTo = m.FallbackModelName
		currentModel = m.FallbackModelName
		body = setRequestModel(body, currentModel)
		m = a.runtimeModel(ctx, currentModel)
		overhead = runtimeAgentOverhead(m, profile)
		meta.AppliedOverheadTokens = overhead
		strategy = m.ContextStrategy
		if strategy == "" {
			strategy = "adaptive"
		}
	}

	body = applyModelOutputCap(body, m.MaxOutputTokens)
	if strategy == "adaptive" || strategy == "trim" {
		if target := runtimeMessageTarget(m, 0, 1, overhead); target > 0 {
			trimmed, orig, final, err := trimChatContext(body, target)
			if err == nil {
				body = trimmed
				if !meta.PreflightFallback {
					meta.OriginalInputTokens = orig
				}
				meta.FinalInputTokens = final
				meta.ContextTrimmed = final < orig
			}
		}
	}
	if meta.FinalInputTokens == 0 {
		meta.FinalInputTokens = estimateChatInputTokens(body)
	}
	meta.FinalModel = currentModel
	maxRetries := m.MaxRetries
	if maxRetries < 0 {
		maxRetries = 0
	}
	if maxRetries > 4 {
		maxRetries = 4
	}

	for attempt := 0; ; attempt++ {
		meta.Attempts++
		admitted, primaryWait, admissionTokens, admissionErr := a.tryModelAdmission(ctx, currentModel, body, m, profile)
		if admissionErr != nil {
			return nil, body, currentModel, meta, admissionErr
		}
		if !admitted && allowModelFallback && modelCanFallback(m, currentModel, strategy) && primaryWait >= 2*time.Second {
			fallbackModel := m.FallbackModelName
			fallbackRuntime := a.runtimeModel(ctx, fallbackModel)
			fallbackAdmitted, fallbackWait, fallbackTokens, _ := a.tryModelAdmission(ctx, fallbackModel, body, fallbackRuntime, profile)
			if fallbackAdmitted || shouldSpillModelAdmission(primaryWait, fallbackWait) {
				body, currentModel, m, overhead, strategy = applyFallbackModel(ctx, a, body, currentModel, profile, m, &meta)
				meta.AdmissionSpillover = true
				admitted = fallbackAdmitted
				admissionTokens = fallbackTokens
			}
		}
		if !admitted {
			waited, tokens, waitErr := a.waitForModelAdmission(ctx, currentModel, body, m, profile)
			meta.AdmissionWaitMS += waited.Milliseconds()
			meta.AdmissionTokens = tokens
			if waitErr != nil {
				return nil, body, currentModel, meta, waitErr
			}
		} else {
			meta.AdmissionTokens = admissionTokens
		}
		req, err := makeReq(body)
		if err != nil {
			return nil, body, currentModel, meta, err
		}
		resp, err := a.inferenceHTTP.Do(req)
		if err != nil {
			meta.FailureKind = "hermes_transport"
			a.openModelCircuit(ctx, currentModel, 0, meta.FailureKind)
			if allowModelFallback && attempt < maxRetries && strategy != "reject" && modelCanFallback(m, currentModel, strategy) {
				body, currentModel, m, overhead, strategy = applyFallbackModel(ctx, a, body, currentModel, profile, m, &meta)
				continue
			}
			return resp, body, currentModel, meta, err
		}

		var errBody []byte
		statusForRecovery := resp.StatusCode
		if resp.StatusCode == http.StatusOK {
			softBody, softStatus, inspectErr := inspectHermesSoftFailure(resp)
			if inspectErr != nil {
				return resp, body, currentModel, meta, inspectErr
			}
			if softStatus != 0 {
				errBody = softBody
				statusForRecovery = softStatus
				resp.StatusCode = softStatus
				resp.Status = fmt.Sprintf("%d %s", softStatus, http.StatusText(softStatus))
			}
		}
		if !recoverableProviderStatus(statusForRecovery) {
			if statusForRecovery < 400 {
				a.clearModelCircuit(ctx, currentModel)
			}
			meta.FinalInputTokens = estimateChatInputTokens(body)
			meta.FinalModel = currentModel
			return resp, body, currentModel, meta, nil
		}
		if len(errBody) == 0 {
			errBody, _ = io.ReadAll(io.LimitReader(resp.Body, 2<<20))
			_ = resp.Body.Close()
		}
		meta.ObservedHTTPStatus = statusForRecovery
		meta.FailureKind = failureKindForStatus(statusForRecovery)
		a.openModelCircuit(ctx, currentModel, statusForRecovery, meta.FailureKind)
		obs := parseRateLimitObservation(statusForRecovery, errBody)
		if statusForRecovery == http.StatusTooManyRequests {
			meta.ObservedRateKind, meta.ObservedRateLimit, meta.ObservedRequested = obs.Kind, obs.Limit, obs.Requested
		}
		if attempt >= maxRetries || strategy == "reject" {
			resp.Body = io.NopCloser(bytes.NewReader(errBody))
			resp.Header.Set("content-type", "application/json")
			return resp, body, currentModel, meta, nil
		}
		recovered := false
		if statusForRecovery == http.StatusTooManyRequests && (strategy == "adaptive" || strategy == "trim") && (obs.Kind == "itpm" || obs.Kind == "tpm" || obs.Requested > obs.Limit && obs.Limit > 0) {
			target := runtimeMessageTarget(m, obs.Limit, 1-float64(attempt)*0.18, overhead)
			if target == 0 && obs.Limit > 0 && overhead < int(float64(obs.Limit)*0.90) {
				target = max(256, int(float64(obs.Limit)*0.72)-overhead)
			}
			if target > 0 {
				before := estimateChatInputTokens(body)
				trimmed, orig, final, trimErr := trimChatContext(body, target)
				if trimErr == nil && final < before {
					body = trimmed
					if meta.OriginalInputTokens == 0 {
						meta.OriginalInputTokens = orig
					}
					meta.FinalInputTokens = final
					meta.ContextTrimmed = true
					recovered = true
				}
			}
		}
		if allowModelFallback && !recovered && modelCanFallback(m, currentModel, strategy) {
			body, currentModel, m, overhead, strategy = applyFallbackModel(ctx, a, body, currentModel, profile, m, &meta)
			recovered = true
		}
		if !recovered {
			if m.RetryBackoffMS <= 0 {
				resp.Body = io.NopCloser(bytes.NewReader(errBody))
				resp.Header.Set("content-type", "application/json")
				return resp, body, currentModel, meta, nil
			}
			select {
			case <-ctx.Done():
				return nil, body, currentModel, meta, ctx.Err()
			case <-time.After(time.Duration(min(m.RetryBackoffMS*(attempt+1), 3000)) * time.Millisecond):
			}
		}
	}
}

func recoveryMetadata(meta modelRecoveryMeta) map[string]any {
	return map[string]any{"modelRecovery": meta}
}

func friendlyRateLimitError(body []byte, meta modelRecoveryMeta) []byte {
	obs := parseRateLimitObservation(http.StatusTooManyRequests, body)
	message := "Model provider is temporarily rate limited. Daiki retried automatically; please continue in a moment."
	if obs.Kind == "itpm" && obs.Limit > 0 {
		message = fmt.Sprintf("This model's input limit is %d tokens/minute. Daiki compacted the conversation and retried automatically, but the provider is still rate limited. Your chat is preserved; send the next message normally.", obs.Limit)
	}
	out, _ := json.Marshal(map[string]any{"error": map[string]any{"code": "provider_rate_limited", "message": message}, "retryable": true, "recovery": meta})
	return out
}
