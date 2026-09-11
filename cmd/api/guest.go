package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
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
	payload := map[string]any{
		"enabled": p.Enabled, "model": "fast", "quotaMode": p.QuotaMode, "tokenLimit": p.TokenLimit, "intervalKind": p.IntervalKind,
		"requestsPerHour": p.RequestsPerHour, "minIntervalSeconds": p.MinIntervalSeconds, "maxCompletionTokens": p.MaxCompletionTokens,
		"allowUploads": p.AllowUploads, "allowImageGeneration": p.AllowImageGeneration, "allowFileGeneration": p.AllowFileGeneration,
		"maxUploadBytes": p.MaxUploadBytes, "maxImageUploadBytes": p.MaxImageUploadBytes, "maxUploadsPerHour": p.MaxUploadsPerHour, "maxStoredFiles": p.MaxStoredFiles,
		"maxStoredBytes": p.MaxStoredBytes, "maxAttachmentsPerMessage": p.MaxAttachmentsPerMessage, "attachmentRetentionHours": p.AttachmentRetentionHours,
		"imageGenerationsPerDay": p.ImageGenerationsPerDay, "fileGenerationsPerDay": p.FileGenerationsPerDay,
		"maxGeneratedFileBytes": p.MaxGeneratedFileBytes, "maxGeneratedImageBytes": p.MaxGeneratedImageBytes,
	}
	identity := guestIdentityForRequest(r)
	if decision, _, quotaErr := a.guestQuota(r.Context(), identity.Subject, p); quotaErr == nil {
		payload["quota"] = decision
	}
	if retryAfter, rateErr := a.guestRateRetryAfter(r.Context(), guestRateSubject(identity), p); rateErr == nil {
		payload["rateRetryAfterSeconds"] = max(0, int(retryAfter.Seconds()))
	}
	writeJSON(w, http.StatusOK, payload)
}

func (a *app) adminGuestPolicy(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		p, configured, err := a.store.GuestAccessPolicy(r.Context())
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "guest policy unavailable"})
			return
		}
		u, requests, _ := a.store.GuestUsageTotals(r.Context())
		devices, _ := a.store.GuestUsageBreakdown(r.Context(), 500)
		writeJSON(w, http.StatusOK, map[string]any{"policy": p, "configured": configured, "usage": u, "requests": requests, "devices": devices})
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

type guestIdentity struct {
	Subject       string
	DeviceID      string
	DeviceName    string
	UserAgentHash string
}

func cleanGuestHeader(value string, maxLen int) string {
	value = strings.TrimSpace(value)
	if len(value) > maxLen {
		value = value[:maxLen]
	}
	return strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, value)
}
func guestIdentityForRequest(r *http.Request) guestIdentity {
	deviceID := cleanGuestHeader(r.Header.Get("X-Daiki-Guest-Device-ID"), 128)
	if deviceID == "" {
		deviceID = "anonymous"
	}
	deviceName := cleanGuestHeader(r.Header.Get("X-Daiki-Guest-Device-Name"), 120)
	ua := sha256.Sum256([]byte(r.UserAgent()))
	return guestIdentity{Subject: guestSubject(r), DeviceID: deviceID, DeviceName: deviceName, UserAgentHash: hex.EncodeToString(ua[:8])}
}
func guestRateSubject(id guestIdentity) string {
	// Token quota intentionally stays network-scoped so changing browser/device metadata
	// cannot create a fresh allowance. Request cooldowns are device-scoped, however, so
	// unrelated Guest users behind the same office/home/carrier NAT do not throttle one
	// another. The raw device identifier is not persisted in the Redis limiter key.
	sum := sha256.Sum256([]byte(id.Subject + "\x00" + id.DeviceID))
	return "guest-rate:" + hex.EncodeToString(sum[:12])
}
func (a *app) recordGuestIdentity(ctx context.Context, id guestIdentity) {
	if a.store != nil {
		_ = a.store.UpsertGuestDevice(ctx, id.Subject, id.DeviceID, id.DeviceName, id.UserAgentHash)
	}
}

const guestContinuitySystemPrompt = `You are Daiki in Guest mode. Use the conversation messages supplied in this request as authoritative context. Resolve short follow-ups, pronouns, and references from the immediately preceding user/assistant turns instead of asking what topic the user means when the topic is already present. Do not claim memory beyond the supplied messages. Guest mode has no unrestricted agent tools. Only Daiki-owned, server-whitelisted command modes/skills and supplied attachment/web evidence may be used for this request.`

func restrictGuestChat(body []byte, p store.GuestAccessPolicy) ([]byte, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("invalid chat payload")
	}
	rawMessages, ok := payload["messages"].([]any)
	if !ok || len(rawMessages) == 0 {
		return nil, fmt.Errorf("guest chat requires messages")
	}
	messages := make([]any, 0, len(rawMessages)+1)
	// User-provided system/developer roles remain forbidden. This bounded Daiki-owned
	// instruction exists only to make frontend-supplied conversation history reliably
	// resolve short follow-ups without enabling any Guest tools or persistence.
	messages = append(messages, map[string]any{"role": "system", "content": guestContinuitySystemPrompt})
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
		"researchMode":          "off",
		"max_completion_tokens": p.MaxCompletionTokens,
	}
	region := normalizeResearchRegion(payload["researchRegion"])
	if region != "" {
		clean["researchRegion"] = region
	}
	if locale := normalizeResearchLocale(payload["researchLocale"]); locale != "" {
		clean["researchLocale"] = locale
	}
	clean["researchScope"] = normalizeResearchScope(payload["researchScope"], region)
	clean["researchDepth"] = normalizeResearchDepth(payload["researchDepth"])
	if focus := clipText(strings.TrimSpace(fmt.Sprint(payload["researchFocus"])), 240); focus != "" {
		clean["researchFocus"] = focus
	}
	if stream, ok := payload["stream"].(bool); ok {
		clean["stream"] = stream
	}
	if commandMode, ok := payload["commandMode"].(string); ok && strings.TrimSpace(commandMode) != "" {
		clean["commandMode"] = commandMode
	}
	if commandSkills, ok := payload["commandSkills"].([]any); ok {
		clean["commandSkills"] = commandSkills
	}
	if ids, ok := payload["attachmentIds"].([]any); ok {
		limit := 10
		if p.MaxAttachmentsPerMessage > 0 && p.MaxAttachmentsPerMessage < limit {
			limit = p.MaxAttachmentsPerMessage
		}
		if len(ids) > limit {
			return nil, fmt.Errorf("guest chat supports at most %d attachments", limit)
		}
		clean["attachmentIds"] = ids
	}
	return json.Marshal(clean)
}

func (a *app) guestRateRetryAfter(ctx context.Context, subject string, p store.GuestAccessPolicy) (time.Duration, error) {
	if p.RequestsPerHour == 0 && p.MinIntervalSeconds == 0 {
		return 0, nil
	}
	if a.redis == nil {
		return 0, fmt.Errorf("guest rate limiter unavailable")
	}
	now := time.Now().UTC()
	hourKey := "guest-chat:hour:" + subject
	lastKey := "guest-chat:last:" + subject
	script := `local now=tonumber(ARGV[1]); local minGap=tonumber(ARGV[2]); local hourly=tonumber(ARGV[3]); local wait=0; local last=tonumber(redis.call('GET',KEYS[2]) or '0'); if minGap>0 and last>0 and now-last<minGap then wait=minGap-(now-last) end; local count=tonumber(redis.call('GET',KEYS[1]) or '0'); if hourly>0 and count>=hourly then local ttl=redis.call('TTL',KEYS[1]); if ttl<1 then ttl=3600 end; if ttl>wait then wait=ttl end end; return wait`
	result, err := a.redis.Eval(ctx, script, []string{hourKey, lastKey}, now.Unix(), p.MinIntervalSeconds, p.RequestsPerHour).Int64()
	if err != nil {
		return 0, err
	}
	if result <= 0 {
		return 0, nil
	}
	return time.Duration(result) * time.Second, nil
}
func (a *app) enforceGuestRate(ctx context.Context, subject string, p store.GuestAccessPolicy) (time.Duration, error) {
	// Zero explicitly means unlimited for each limiter. Avoid requiring Redis when
	// both dimensions are unlimited so an admin policy change takes effect on the
	// very next request without a restart.
	if p.RequestsPerHour == 0 && p.MinIntervalSeconds == 0 {
		return 0, nil
	}
	if a.redis == nil {
		return 0, fmt.Errorf("guest rate limiter unavailable")
	}
	now := time.Now().UTC()
	hourKey := "guest-chat:hour:" + subject
	lastKey := "guest-chat:last:" + subject
	script := `local now=tonumber(ARGV[1]); local minGap=tonumber(ARGV[2]); local hourly=tonumber(ARGV[3]); local last=tonumber(redis.call('GET',KEYS[2]) or '0'); if minGap>0 and last>0 and now-last<minGap then return -(minGap-(now-last)) end; local count=tonumber(redis.call('GET',KEYS[1]) or '0'); if hourly>0 and count>=hourly then local ttl=redis.call('TTL',KEYS[1]); if ttl<1 then ttl=3600 end; return -ttl end; if hourly>0 then count=redis.call('INCR',KEYS[1]); if count==1 then redis.call('EXPIRE',KEYS[1],3600) end end; if minGap>0 then redis.call('SET',KEYS[2],now,'EX',3600) end; return count`
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
	policy := store.Policy{ScopeType: "system", ScopeID: "guest", QuotaMode: p.QuotaMode, TokenLimit: &limit, IntervalKind: p.IntervalKind, IntervalSeconds: p.IntervalSeconds, AllowedModels: json.RawMessage(`["fast","vision"]`)}
	if p.QuotaMode == "unlimited" {
		policy.TokenLimit = nil
		return quotaDecision{Mode: "unlimited", Interval: p.IntervalKind, CounterKey: subject}, policy, nil
	}
	start, reset := quotaWindow(policy, time.Now().UTC())
	if lastReset, resetErr := a.store.GuestLastReset(ctx, subject); resetErr == nil && lastReset != nil && lastReset.After(start) {
		start = *lastReset
	}
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
	return strings.HasPrefix(upstreamName, "hermes") && enabled && (err != nil || statusCode >= 500)
}

func (a *app) authenticatedHermesUpstream(path, profile string) (string, string, string) {
	if a.cfg.HermesEnabled && a.cfg.HermesBase != "" {
		profile = strings.Trim(strings.TrimSpace(profile), "/")
		if profile == "" {
			profile = "user"
		}
		return strings.TrimRight(a.cfg.HermesBase, "/") + "/p/" + profile + path, a.cfg.HermesKey, "hermes"
	}
	return a.liteLLMUpstream(path)
}
func (a *app) inferenceUpstreamForRequest(r *http.Request, path string) (string, string, string) {
	// Compatibility helper: authenticated chat defaults to the slim user profile.
	return a.authenticatedHermesUpstream(path, "user")
}
func (a *app) guestHermesUpstream(path, profile string) (string, string, string) {
	if a.cfg.HermesEnabled && a.cfg.HermesBase != "" {
		switch profile {
		case "guest-skills", "vision":
		default:
			profile = "guest"
		}
		return strings.TrimRight(a.cfg.HermesBase, "/") + "/p/" + profile + path, a.cfg.HermesKey, "hermes-" + profile
	}
	return a.liteLLMUpstream(path)
}

func hermesRequestModel(payload []byte) string {
	var body map[string]any
	if json.Unmarshal(payload, &body) != nil {
		return ""
	}
	model, _ := body["model"].(string)
	return strings.TrimSpace(model)
}
func hermesModelScopedSessionKey(base string, payload []byte) string {
	if base == "" {
		return ""
	}
	model := hermesRequestModel(payload)
	if model == "" {
		return base
	}
	sum := sha256.Sum256([]byte(model))
	return base + ":m:" + hex.EncodeToString(sum[:8])
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
	key := "daiki:" + hex.EncodeToString(sum[:16])
	if chatSession := strings.TrimSpace(r.Header.Get("x-daiki-chat-session-id")); chatSession != "" {
		conversationSum := sha256.Sum256([]byte(chatSession))
		key += ":c:" + hex.EncodeToString(conversationSum[:12])
	}
	return key
}

func (a *app) guestChat(w http.ResponseWriter, r *http.Request) { a.proxyGuestInference(w, r, false) }
func (a *app) guestChatStream(w http.ResponseWriter, r *http.Request) {
	a.proxyGuestInference(w, r, true)
}

func guestResearchSourcesHeader(meta researchMetadata) string {
	if len(meta.Sources) == 0 {
		return ""
	}
	limit := len(meta.Sources)
	if limit > 8 {
		limit = 8
	}
	for limit > 0 {
		rows := make([]map[string]any, 0, limit)
		for _, source := range meta.Sources[:limit] {
			rows = append(rows, map[string]any{"index": source.Index, "title": clipText(source.Title, 180), "url": source.URL, "engine": source.Engine, "region": source.Region, "authority": source.Authority, "sourceType": source.SourceType, "platform": source.Platform, "qualityScore": source.QualityScore, "relevanceScore": source.RelevanceScore, "freshnessScore": source.FreshnessScore})
		}
		raw, err := json.Marshal(rows)
		if err != nil {
			return ""
		}
		encoded := base64.StdEncoding.EncodeToString(raw)
		if len(encoded) <= 6000 {
			return encoded
		}
		limit--
	}
	return ""
}

func guestActivityText(value string, maxBytes int) string {
	value = strings.TrimSpace(value)
	if maxBytes <= 0 || len(value) <= maxBytes {
		return value
	}
	return strings.TrimSpace(value[:maxBytes]) + "…"
}
func guestResponseText(body []byte) string {
	if text := chatCompletionText(body); text != "" {
		return guestActivityText(text, 64<<10)
	}
	var out strings.Builder
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		raw := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(raw) == 0 || bytes.Equal(raw, []byte("[DONE]")) {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content any `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal(raw, &chunk) != nil || len(chunk.Choices) == 0 {
			continue
		}
		switch content := chunk.Choices[0].Delta.Content.(type) {
		case string:
			out.WriteString(content)
		case []any:
			for _, itemRaw := range content {
				item, _ := itemRaw.(map[string]any)
				if text, _ := item["text"].(string); text != "" {
					out.WriteString(text)
				}
			}
		}
		if out.Len() >= 64<<10 {
			break
		}
	}
	return guestActivityText(out.String(), 64<<10)
}
func guestModelAlias(route inference.Route) string {
	if route.ResolvedAlias == "vision" || route.Workload == inference.WorkloadVision {
		return "vision"
	}
	return "fast"
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
	identity := guestIdentityForRequest(r)
	subject := identity.Subject
	a.recordGuestIdentity(r.Context(), identity)
	// Parse and validate the user-controlled payload and attachments before consuming
	// Guest admission. Research is deliberately performed only after quota/rate checks
	// so /deep-search cannot become an unmetered public-web resource bypass.
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
	body, commandSelection, err := applyChatCommands(body, true)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	guestPrompt := ""
	var guestPromptPayload map[string]any
	if json.Unmarshal(body, &guestPromptPayload) == nil {
		guestPrompt = guestActivityText(lastUserText(guestPromptPayload), 32<<10)
	}
	body, attachments, err := a.expandGuestChatAttachments(r.Context(), identity, body, p)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	body, commandSelection, err = applyAutomaticAttachmentSkills(body, attachments, commandSelection, true)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unable to apply attachment skills"})
		return
	}
	decision, _, err := a.guestQuota(r.Context(), subject, p)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "guest quota unavailable"})
		return
	}
	if decision.Remaining != nil && *decision.Remaining <= 0 {
		payload := map[string]any{"error": "guest_quota_exhausted", "quota": decision}
		if seconds := quotaRetryAfterSeconds(decision); seconds > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(seconds))
			payload["retryAfterSeconds"] = seconds
		}
		writeJSON(w, http.StatusTooManyRequests, payload)
		return
	}
	if retry, rateErr := a.enforceGuestRate(r.Context(), guestRateSubject(identity), p); rateErr != nil {
		if retry > 0 {
			seconds := max(1, int(retry.Seconds()))
			w.Header().Set("Retry-After", strconv.Itoa(seconds))
			writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "guest_rate_limited", "retryAfterSeconds": seconds})
		} else {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "guest_rate_limiter_unavailable"})
		}
		return
	}
	body, researchMeta, researchErr := a.enrichChatWithResearch(r.Context(), body)
	if researchErr != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": researchErr.Error(), "research": researchMeta})
		return
	}
	body, responseLanguage, err := applyResponseLanguage(body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unable to apply response language"})
		return
	}
	route, upstreamBody, err := a.router.RouteChat(body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	guestAlias := guestModelAlias(route)
	// Guest text is Fast-only, while image understanding uses the dedicated Vision
	// route. Both are still policy-controlled Guest capabilities and go through Hermes.
	if alias, aliasErr := a.store.ModelAlias(r.Context(), guestAlias); aliasErr == nil && alias.LiteLLMModelName != "" {
		var payload map[string]any
		if json.Unmarshal(upstreamBody, &payload) == nil {
			payload["model"] = alias.LiteLLMModelName
			upstreamBody, _ = json.Marshal(payload)
			route.PhysicalModel = alias.LiteLLMModelName
		}
	}
	reserved := reservationTokens(body)
	requestID := middleware.GetReqID(r.Context())
	if requestID == "" {
		requestID = fmt.Sprintf("guest-%d", time.Now().UnixNano())
	}
	if err := a.reserveQuota(r.Context(), requestID, decision, reserved); err != nil {
		status, code := http.StatusServiceUnavailable, "guest_quota_service_unavailable"
		payload := map[string]any{"error": code, "quota": decision}
		if strings.Contains(err.Error(), "exhausted") {
			status, code = http.StatusTooManyRequests, "guest_quota_exhausted"
			payload["error"] = code
			if seconds := quotaRetryAfterSeconds(decision); seconds > 0 {
				w.Header().Set("Retry-After", strconv.Itoa(seconds))
				payload["retryAfterSeconds"] = seconds
			}
		}
		writeJSON(w, status, payload)
		return
	}
	if err := a.store.StartUsageForPrincipal(r.Context(), requestID, subject, "", guestAlias, string(route.Workload), reserved); err != nil {
		a.releaseReservation(r.Context(), requestID, decision, reserved)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "guest usage ledger unavailable"})
		return
	}
	guestMeta := map[string]any{
		"authKind": "guest", "resolvedAlias": guestAlias, "physicalModel": route.PhysicalModel,
		"guestNetworkId": identity.Subject, "guestDeviceId": identity.DeviceID, "guestDeviceName": identity.DeviceName,
		"guestPrompt": guestPrompt, "attachments": attachments, "commands": commandSelection, "research": safeRunResearchActivity(researchMeta),
	}
	if responseLanguage.Code != "" {
		guestMeta["responseLanguage"] = responseLanguage
	}
	_ = a.store.MergeUsageMetadata(r.Context(), requestID, guestMeta)
	ticket, err := a.queue.Acquire(r.Context(), requestID, subject, route.Workload, route.Priority)
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
	bufferResearchStream := stream && researchMeta.Used
	if bufferResearchStream {
		upstreamBody = forceChatNonStream(upstreamBody)
	} else if stream {
		upstreamBody = ensureStreamUsage(upstreamBody)
	}
	guestProfile := "guest"
	if route.Workload == inference.WorkloadVision {
		guestProfile = "vision"
	} else if commandSelectionNeedsSkillsProfile(commandSelection) {
		guestProfile = "guest-skills"
	}
	upstreamURL, upstreamKey, upstreamName := a.guestHermesUpstream("/v1/chat/completions", guestProfile)
	makeGuestRequest := func(payload []byte) (*http.Request, error) {
		req, buildErr := http.NewRequestWithContext(r.Context(), http.MethodPost, upstreamURL, strings.NewReader(string(payload)))
		if buildErr != nil {
			return nil, buildErr
		}
		req.Header.Set("content-type", "application/json")
		if upstreamKey != "" {
			req.Header.Set("authorization", "Bearer "+upstreamKey)
		}
		req.Header.Set("x-daiki-request-id", requestID)
		req.Header.Set("x-daiki-principal", "guest")
		if strings.HasPrefix(upstreamName, "hermes") {
			baseKey := "daiki-guest:" + strings.TrimPrefix(identity.Subject, "guest:") + ":" + identity.DeviceID
			applyHermesSessionScope(req, baseKey, payload)
		}
		return req, nil
	}
	resp, recoveredBody, recoveredModel, recovery, err := a.doModelRequestWithRecovery(r.Context(), upstreamBody, route.PhysicalModel, guestProfile, makeGuestRequest)
	upstreamBody = recoveredBody
	if recoveredModel != "" {
		route.PhysicalModel = recoveredModel
	}
	_ = a.store.MergeUsageMetadata(r.Context(), requestID, recoveryMetadata(recovery))
	statusCode := 0
	if resp != nil {
		statusCode = resp.StatusCode
	}
	if shouldFallbackFromHermes(upstreamName, a.cfg.HermesFallbackToLiteLLM, statusCode, err) {
		fallbackURL, fallbackKey, fallbackName := a.liteLLMUpstream("/v1/chat/completions")
		fallbackReq, buildErr := http.NewRequestWithContext(r.Context(), http.MethodPost, fallbackURL, strings.NewReader(string(upstreamBody)))
		if buildErr == nil {
			fallbackReq.Header.Set("content-type", "application/json")
			if fallbackKey != "" {
				fallbackReq.Header.Set("authorization", "Bearer "+fallbackKey)
			}
			resp, err = a.inferenceHTTP.Do(fallbackReq)
			if err == nil {
				upstreamName = fallbackName
			}
		}
	}
	if err != nil {
		a.releaseReservation(r.Context(), requestID, decision, reserved)
		_ = a.store.FinishUsage(context.Background(), requestID, "failed", store.Usage{})
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": upstreamName + " unavailable"})
		return
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		rawRateBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		_ = resp.Body.Close()
		friendly := friendlyRateLimitError(rawRateBody, recovery)
		resp.Body = io.NopCloser(bytes.NewReader(friendly))
		resp.ContentLength = int64(len(friendly))
		resp.Header.Set("content-type", "application/json")
	}
	defer resp.Body.Close()
	w.Header().Set("x-daiki-access-mode", "guest-fast")
	if commandSelection.Mode != "" {
		w.Header().Set("x-daiki-command-mode", commandSelection.Mode)
	}
	if len(commandSelection.Skills) > 0 {
		w.Header().Set("x-daiki-command-skills", strings.Join(commandSelection.Skills, ","))
	}
	if len(commandSelection.AutoSkills) > 0 {
		w.Header().Set("x-daiki-auto-skills", strings.Join(commandSelection.AutoSkills, ","))
	}
	if researchMeta.Used {
		w.Header().Set("x-daiki-research-used", "true")
		w.Header().Set("x-daiki-research-sources", strconv.Itoa(len(researchMeta.Sources)))
		w.Header().Set("x-daiki-research-quality", strconv.Itoa(researchMeta.QualityScore))
		w.Header().Set("x-daiki-research-quality-grade", researchMeta.QualityGrade)
		if encodedSources := guestResearchSourcesHeader(researchMeta); encodedSources != "" {
			w.Header().Set("x-daiki-research-sources-json", encodedSources)
		}
	} else {
		w.Header().Set("x-daiki-research-used", "false")
		w.Header().Set("x-daiki-research-sources", "0")
	}
	w.Header().Set("x-daiki-research-mode", researchMeta.Mode)
	w.Header().Set("x-daiki-model-alias", guestAlias)
	if responseLanguage.Code != "" {
		w.Header().Set("x-daiki-response-language", responseLanguage.Code)
	}
	w.Header().Set("x-daiki-inference-upstream", upstreamName)
	w.Header().Set("x-daiki-hermes-profile", guestProfile)
	w.Header().Set("x-daiki-model-physical", route.PhysicalModel)
	w.Header().Set("x-daiki-retry-attempts", strconv.Itoa(max(0, recovery.Attempts-1)))
	w.Header().Set("x-daiki-admission-wait-ms", strconv.FormatInt(recovery.AdmissionWaitMS, 10))
	w.Header().Set("x-daiki-admission-tokens", strconv.Itoa(recovery.AdmissionTokens))
	if recovery.AdmissionSpillover {
		w.Header().Set("x-daiki-admission-spillover", "true")
	}
	if recovery.ContextTrimmed {
		w.Header().Set("x-daiki-context-trimmed", "true")
	}
	if recovery.FallbackTo != "" {
		w.Header().Set("x-daiki-fallback-model", recovery.FallbackTo)
	}
	if decision.Remaining != nil {
		w.Header().Set("x-daiki-quota-remaining", fmt.Sprint(*decision.Remaining))
	}
	if stream && !bufferResearchStream {
		if v := resp.Header.Get("content-type"); v != "" {
			w.Header().Set("content-type", v)
		}
		w.Header().Set("x-accel-buffering", "no")
		w.WriteHeader(resp.StatusCode)
		capture := &cappedBuffer{max: storedPayloadLimit}
		usage, copyErr := copySSEWithUsage(io.MultiWriter(w, capture), resp.Body)
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
		responseMeta := usageResponseMetadata(capture.Bytes())
		responseMeta["guestResponse"] = guestResponseText(capture.Bytes())
		_ = a.store.MergeUsageMetadata(context.Background(), requestID, responseMeta)
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
	grounding := researchGroundingResult{Body: responseBody}
	if resp.StatusCode < http.StatusBadRequest && researchMeta.Used {
		grounding = a.enforceResearchGrounding(r.Context(), upstreamBody, responseBody, researchMeta, route.PhysicalModel, guestProfile, makeGuestRequest)
		responseBody = grounding.Body
		if grounding.Retried {
			w.Header().Set("x-daiki-grounding-retry", "true")
			w.Header().Set("x-daiki-grounding-violations", strconv.Itoa(len(grounding.Violations)))
		}
		if grounding.Fallback {
			w.Header().Set("x-daiki-grounding-fallback", "true")
		}
	}
	usage := addUsage(parseUsagePayload(responseBody), grounding.ExtraUsage)
	status := "completed"
	if resp.StatusCode >= 400 {
		status = "failed"
		usage = store.Usage{}
	} else if usage.TotalTokens == 0 {
		usage.TotalTokens = reserved
	}
	responseMeta := usageResponseMetadata(responseBody)
	responseMeta["guestResponse"] = guestResponseText(responseBody)
	_ = a.store.MergeUsageMetadata(r.Context(), requestID, responseMeta)
	_ = a.store.FinishUsage(r.Context(), requestID, status, usage)
	a.releaseReservation(r.Context(), requestID, decision, reserved)
	if bufferResearchStream {
		w.Header().Set("content-type", "text/event-stream")
		w.Header().Set("cache-control", "no-cache")
		w.Header().Set("x-accel-buffering", "no")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(chatCompletionToSSE(responseBody))
		return
	}
	if v := resp.Header.Get("content-type"); v != "" {
		w.Header().Set("content-type", v)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(responseBody)
}
