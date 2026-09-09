package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/poom-ytthpch/daiki-ai-passport-backend/internal/inference"
	"github.com/poom-ytthpch/daiki-ai-passport-backend/internal/store"
)

func (a *app) guestPolicyPublic(w http.ResponseWriter, r *http.Request) {
	p, _, err := a.store.GuestAccessPolicy(r.Context())
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "guest policy unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": p.Enabled, "model": "fast", "tokenLimit": p.TokenLimit, "intervalKind": p.IntervalKind, "requestsPerHour": p.RequestsPerHour, "minIntervalSeconds": p.MinIntervalSeconds, "maxCompletionTokens": p.MaxCompletionTokens})
}

func (a *app) adminGuestPolicy(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		p, configured, err := a.store.GuestAccessPolicy(r.Context())
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "guest policy unavailable"})
			return
		}
		u, requests, _ := a.store.GuestUsageTotals(r.Context())
		writeJSON(w, http.StatusOK, map[string]any{"policy": p, "configured": configured, "usage": u, "requests": requests})
		return
	}
	var p store.GuestAccessPolicy
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&p) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	out, err := a.store.UpsertGuestAccessPolicy(r.Context(), current(r).Sub, p)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func guestClientIP(r *http.Request) string {
	// Public Guest traffic is terminated by the Next.js frontend, which resolves
	// the network address at the server boundary and forwards only this single
	// canonical value to the internal backend service.
	if v := strings.TrimSpace(r.Header.Get("X-Daiki-Client-IP")); v != "" {
		return v
	}
	if v := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); v != "" {
		return v
	}
	if v := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); v != "" {
		return strings.TrimSpace(strings.Split(v, ",")[0])
	}
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err == nil && host != "" {
		return host
	}
	return strings.TrimSpace(r.RemoteAddr)
}

func guestSubject(r *http.Request) string {
	// IP-only identity is intentionally strict: changing browser/User-Agent must
	// not create a fresh Guest quota bucket. Raw addresses are never persisted.
	raw := guestClientIP(r)
	sum := sha256.Sum256([]byte(raw))
	return "guest:" + hex.EncodeToString(sum[:12])
}

func restrictGuestChat(body []byte, p store.GuestAccessPolicy) ([]byte, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("invalid chat payload")
	}
	rawMessages, ok := payload["messages"].([]any)
	if !ok || len(rawMessages) == 0 {
		return nil, fmt.Errorf("guest chat requires messages")
	}
	messages := make([]any, 0, len(rawMessages))
	for _, raw := range rawMessages {
		message, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("guest chat supports text only")
		}
		role, _ := message["role"].(string)
		if role != "user" && role != "assistant" {
			return nil, fmt.Errorf("guest chat supports user and assistant messages only")
		}
		content, ok := message["content"].(string)
		if !ok {
			return nil, fmt.Errorf("guest chat supports text only")
		}
		messages = append(messages, map[string]any{"role": role, "content": content})
	}
	clean := map[string]any{
		"model":                 "fast",
		"messages":              messages,
		"max_completion_tokens": p.MaxCompletionTokens,
	}
	if stream, ok := payload["stream"].(bool); ok {
		clean["stream"] = stream
	}
	return json.Marshal(clean)
}

func (a *app) enforceGuestRate(ctx context.Context, subject string, p store.GuestAccessPolicy) (time.Duration, error) {
	if a.redis == nil {
		return 0, fmt.Errorf("guest rate limiter unavailable")
	}
	now := time.Now().UTC()
	hourKey := "guest-chat:hour:" + subject
	lastKey := "guest-chat:last:" + subject
	script := `local now=tonumber(ARGV[1]); local minGap=tonumber(ARGV[2]); local hourly=tonumber(ARGV[3]); local last=tonumber(redis.call('GET',KEYS[2]) or '0'); if last>0 and now-last<minGap then return -(minGap-(now-last)) end; local count=tonumber(redis.call('GET',KEYS[1]) or '0'); if count>=hourly then local ttl=redis.call('TTL',KEYS[1]); if ttl<1 then ttl=3600 end; return -ttl end; count=redis.call('INCR',KEYS[1]); if count==1 then redis.call('EXPIRE',KEYS[1],3600) end; redis.call('SET',KEYS[2],now,'EX',3600); return count`
	result, err := a.redis.Eval(ctx, script, []string{hourKey, lastKey}, now.Unix(), p.MinIntervalSeconds, p.RequestsPerHour).Int64()
	if err != nil {
		return 0, err
	}
	if result < 0 {
		return time.Duration(-result) * time.Second, fmt.Errorf("guest rate limit exceeded")
	}
	return 0, nil
}

func (a *app) guestQuota(ctx context.Context, subject string, p store.GuestAccessPolicy) (quotaDecision, store.Policy, error) {
	limit := p.TokenLimit
	policy := store.Policy{ScopeType: "system", ScopeID: "guest", QuotaMode: "limited", TokenLimit: &limit, IntervalKind: p.IntervalKind, IntervalSeconds: p.IntervalSeconds, AllowedModels: json.RawMessage(`["fast"]`)}
	start, reset := quotaWindow(policy, time.Now().UTC())
	u, err := a.store.GuestUsageSummary(ctx, subject, start)
	if err != nil {
		return quotaDecision{}, policy, err
	}
	remaining := limit - u.TotalTokens
	if remaining < 0 {
		remaining = 0
	}
	return quotaDecision{Mode: "limited", Limit: &limit, Used: u.TotalTokens, Remaining: &remaining, ResetAt: reset, Interval: p.IntervalKind, CounterKey: subject}, policy, nil
}

func (a *app) liteLLMUpstream(path string) (string, string, string) {
	return strings.TrimRight(a.cfg.LiteLLMBase, "/") + path, a.cfg.LiteLLMKey, "litellm"
}

func (a *app) inferenceUpstream(path string) (string, string, string) {
	if a.cfg.HermesEnabled && a.cfg.HermesBase != "" {
		return strings.TrimRight(a.cfg.HermesBase, "/") + path, a.cfg.HermesKey, "hermes"
	}
	return a.liteLLMUpstream(path)
}

func shouldFallbackFromHermes(upstreamName string, enabled bool, statusCode int, err error) bool {
	return upstreamName == "hermes" && enabled && (err != nil || statusCode >= 500)
}

func (a *app) inferenceUpstreamForRequest(r *http.Request, path string) (string, string, string) {
	// Restricted authenticated accounts must never inherit the Hermes API profile's
	// agent toolsets. Approved users/API keys may use Hermes; pending users stay on
	// the same Fast-only direct LiteLLM path as before.
	if u, ok := currentUser(r); ok && u.Status != "approved" {
		return a.liteLLMUpstream(path)
	}
	return a.inferenceUpstream(path)
}

func hermesSessionKey(r *http.Request) string {
	raw := ""
	if u, ok := currentUser(r); ok {
		raw = u.Subject
	}
	if raw == "" {
		p := currentPrincipal(r)
		raw = p.AuthKind + ":" + p.APIKeyID
	}
	if raw == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(raw))
	return "daiki:" + hex.EncodeToString(sum[:16])
}

func (a *app) guestChat(w http.ResponseWriter, r *http.Request) { a.proxyGuestInference(w, r, false) }
func (a *app) guestChatStream(w http.ResponseWriter, r *http.Request) {
	a.proxyGuestInference(w, r, true)
}

func (a *app) proxyGuestInference(w http.ResponseWriter, r *http.Request, stream bool) {
	if a.store == nil || a.queue == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "guest chat unavailable"})
		return
	}
	p, _, err := a.store.GuestAccessPolicy(r.Context())
	if err != nil || !p.Enabled {
		status, code := http.StatusServiceUnavailable, "guest_chat_unavailable"
		if err == nil && !p.Enabled {
			status, code = http.StatusForbidden, "guest_chat_disabled"
		}
		writeJSON(w, status, map[string]string{"error": code})
		return
	}
	subject := guestSubject(r)
	if retry, rateErr := a.enforceGuestRate(r.Context(), subject, p); rateErr != nil {
		if retry > 0 {
			seconds := max(1, int(retry.Seconds()))
			w.Header().Set("Retry-After", strconv.Itoa(seconds))
			writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "guest_rate_limited", "retryAfterSeconds": seconds})
		} else {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "guest_rate_limiter_unavailable"})
		}
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	body, err = restrictGuestChat(body, p)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	route, upstreamBody, err := a.router.RouteChat(body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if alias, aliasErr := a.store.ModelAlias(r.Context(), "fast"); aliasErr == nil && alias.LiteLLMModelName != "" {
		var payload map[string]any
		if json.Unmarshal(upstreamBody, &payload) == nil {
			payload["model"] = alias.LiteLLMModelName
			upstreamBody, _ = json.Marshal(payload)
			route.PhysicalModel = alias.LiteLLMModelName
		}
	}
	decision, _, err := a.guestQuota(r.Context(), subject, p)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "guest quota unavailable"})
		return
	}
	reserved := reservationTokens(body)
	requestID := middleware.GetReqID(r.Context())
	if requestID == "" {
		requestID = fmt.Sprintf("guest-%d", time.Now().UnixNano())
	}
	if err := a.reserveQuota(r.Context(), requestID, decision, reserved); err != nil {
		status, code := http.StatusServiceUnavailable, "guest_quota_service_unavailable"
		if strings.Contains(err.Error(), "exhausted") {
			status, code = http.StatusTooManyRequests, "guest_quota_exhausted"
		}
		writeJSON(w, status, map[string]any{"error": code, "quota": decision})
		return
	}
	if err := a.store.StartUsageForPrincipal(r.Context(), requestID, subject, "", "fast", string(inference.WorkloadFast), reserved); err != nil {
		a.releaseReservation(r.Context(), requestID, decision, reserved)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "guest usage ledger unavailable"})
		return
	}
	_ = a.store.MergeUsageMetadata(r.Context(), requestID, map[string]any{"authKind": "guest", "resolvedAlias": "fast", "physicalModel": route.PhysicalModel})
	ticket, err := a.queue.Acquire(r.Context(), requestID, subject, inference.WorkloadFast, route.Priority)
	if err != nil {
		a.releaseReservation(context.Background(), requestID, decision, reserved)
		_ = a.store.FinishUsage(context.Background(), requestID, "failed", store.Usage{})
		status := http.StatusServiceUnavailable
		if errors.Is(err, inference.ErrQueueTimeout) {
			status = http.StatusGatewayTimeout
		}
		writeJSON(w, status, map[string]string{"error": "guest_queue_unavailable"})
		return
	}
	defer ticket.Release(context.Background())
	if stream {
		upstreamBody = ensureStreamUsage(upstreamBody)
	}
	upstreamURL, upstreamKey, upstreamName := a.liteLLMUpstream("/v1/chat/completions")
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstreamURL, strings.NewReader(string(upstreamBody)))
	if err != nil {
		a.releaseReservation(r.Context(), requestID, decision, reserved)
		_ = a.store.FinishUsage(r.Context(), requestID, "failed", store.Usage{})
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "request construction failed"})
		return
	}
	req.Header.Set("content-type", "application/json")
	if upstreamKey != "" {
		req.Header.Set("authorization", "Bearer "+upstreamKey)
	}
	req.Header.Set("x-daiki-request-id", requestID)
	req.Header.Set("x-daiki-principal", "guest")
	resp, err := a.inferenceHTTP.Do(req)
	if err != nil {
		a.releaseReservation(r.Context(), requestID, decision, reserved)
		_ = a.store.FinishUsage(context.Background(), requestID, "failed", store.Usage{})
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": upstreamName + " unavailable"})
		return
	}
	defer resp.Body.Close()
	w.Header().Set("x-daiki-access-mode", "guest-fast")
	w.Header().Set("x-daiki-model-alias", "fast")
	w.Header().Set("x-daiki-inference-upstream", upstreamName)
	if decision.Remaining != nil {
		w.Header().Set("x-daiki-quota-remaining", fmt.Sprint(*decision.Remaining))
	}
	if stream {
		if v := resp.Header.Get("content-type"); v != "" {
			w.Header().Set("content-type", v)
		}
		w.Header().Set("x-accel-buffering", "no")
		w.WriteHeader(resp.StatusCode)
		usage, copyErr := copySSEWithUsage(w, resp.Body)
		status := "completed"
		if copyErr != nil || r.Context().Err() != nil {
			status = "cancelled"
			usage = store.Usage{}
		} else if resp.StatusCode >= 400 {
			status = "failed"
			usage = store.Usage{}
		} else if usage.TotalTokens == 0 {
			usage.TotalTokens = reserved
		}
		_ = a.store.FinishUsage(context.Background(), requestID, status, usage)
		a.releaseReservation(context.Background(), requestID, decision, reserved)
		return
	}
	responseBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if readErr != nil {
		a.releaseReservation(r.Context(), requestID, decision, reserved)
		_ = a.store.FinishUsage(r.Context(), requestID, "failed", store.Usage{})
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "invalid inference response"})
		return
	}
	usage := parseUsagePayload(responseBody)
	status := "completed"
	if resp.StatusCode >= 400 {
		status = "failed"
		usage = store.Usage{}
	} else if usage.TotalTokens == 0 {
		usage.TotalTokens = reserved
	}
	_ = a.store.FinishUsage(r.Context(), requestID, status, usage)
	a.releaseReservation(r.Context(), requestID, decision, reserved)
	if v := resp.Header.Get("content-type"); v != "" {
		w.Header().Set("content-type", v)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(responseBody)
}
