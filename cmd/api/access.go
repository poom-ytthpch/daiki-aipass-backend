package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/poom-ytthpch/daiki-ai-passport-backend/internal/store"
)

const appUserKey ctxKey = "app-user"
const principalKey ctxKey = "principal"

type principal struct {
	AuthKind string
	APIKeyID string
	Scopes   []string
}

type quotaDecision struct {
	CounterKey   string                  `json:"-"`
	Mode         string                  `json:"mode"`
	Limit        *int64                  `json:"limit,omitempty"`
	Used         int64                   `json:"used"`
	Remaining    *int64                  `json:"remaining,omitempty"`
	ResetAt      *time.Time              `json:"resetAt,omitempty"`
	Interval     string                  `json:"interval"`
	WindowStart  *time.Time              `json:"windowStart,omitempty"`
	ResetCredits *store.QuotaResetStatus `json:"resetCredits,omitempty"`
}

func currentUser(r *http.Request) (store.User, bool) {
	u, ok := r.Context().Value(appUserKey).(store.User)
	return u, ok
}
func currentPrincipal(r *http.Request) principal {
	p, _ := r.Context().Value(principalKey).(principal)
	return p
}

func hasPrincipalScope(r *http.Request, want string) bool {
	p := currentPrincipal(r)
	if p.AuthKind != "api_key" {
		return true
	}
	for _, scope := range p.Scopes {
		if scope == "*" || scope == want {
			return true
		}
	}
	return false
}

func requirePrincipalScope(scope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !hasPrincipalScope(r, scope) {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "api key scope required", "scope": scope})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func identityProvider(c claims) string {
	if provider := strings.ToLower(strings.TrimSpace(c.IdentityProvider)); provider != "" {
		return provider
	}
	// Native Keycloak username/password sessions do not carry the broker session
	// note, so absence of identity_provider means the built-in email/password flow.
	return "email"
}

func (a *app) approvalRequired(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, ok := currentUser(r)
		if !ok {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "application identity unavailable"})
			return
		}
		if u.Status != "approved" {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "access_not_approved", "status": u.Status})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func quotaWindow(p store.Policy, now time.Time) (time.Time, *time.Time) {
	var start time.Time
	var reset *time.Time
	switch p.IntervalKind {
	case "hour":
		start = now.Truncate(time.Hour)
		r := start.Add(time.Hour)
		reset = &r
	case "day":
		start = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		r := start.AddDate(0, 0, 1)
		reset = &r
	case "week":
		d := (int(now.Weekday()) + 6) % 7
		base := now.AddDate(0, 0, -d)
		start = time.Date(base.Year(), base.Month(), base.Day(), 0, 0, 0, 0, time.UTC)
		r := start.AddDate(0, 0, 7)
		reset = &r
	case "month":
		start = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		r := start.AddDate(0, 1, 0)
		reset = &r
	case "rolling", "custom":
		seconds := int64(3600)
		if p.IntervalSeconds != nil {
			seconds = *p.IntervalSeconds
		}
		start = now.Add(-time.Duration(seconds) * time.Second)
		r := now.Add(time.Duration(seconds) * time.Second)
		reset = &r
	default:
		start = time.Unix(0, 0).UTC()
	}
	return start, reset
}

func (a *app) userQuotaWindow(ctx context.Context, subject string, p store.Policy, now time.Time) (time.Time, *time.Time, store.QuotaResetStatus, error) {
	start, reset := quotaWindow(p, now)
	status, err := a.store.QuotaResetStatus(ctx, subject)
	if err != nil {
		return start, reset, status, err
	}
	if status.LastResetAt != nil && status.LastResetAt.After(start) && !status.LastResetAt.After(now) {
		start = *status.LastResetAt
	}
	return start, reset, status, nil
}

func (a *app) clearUserQuotaRuntimeState(ctx context.Context, subject string) {
	if a.redis == nil || strings.TrimSpace(subject) == "" {
		return
	}
	_ = a.redis.Del(ctx,
		"quota:pending:user:"+subject,
		"quota:blocked:user:"+subject,
		"pending-chat:hour:"+subject,
		"pending-chat:last:"+subject,
	).Err()
}

func (a *app) effectiveUserQuota(ctx context.Context, subject string) (store.Policy, bool, error) {
	override, configured, err := a.store.UserPolicyOverride(ctx, subject)
	if err != nil {
		return store.Policy{}, false, err
	}
	if configured {
		return override, true, nil
	}
	u, err := a.store.User(ctx, subject)
	if err != nil {
		return store.Policy{}, false, err
	}
	if u.Status == "pending" {
		return a.pendingQuotaPolicy(), false, nil
	}
	p, _, err := a.store.PolicyForUser(ctx, subject, u.Roles)
	return p, false, err
}

func (a *app) userQuotaSnapshot(ctx context.Context, subject string) (quotaDecision, store.Policy, store.Usage, error) {
	p, _, err := a.effectiveUserQuota(ctx, subject)
	if err != nil {
		return quotaDecision{}, store.Policy{}, store.Usage{}, err
	}
	if p.QuotaMode == "" {
		p.QuotaMode = "unlimited"
	}
	if p.IntervalKind == "" {
		p.IntervalKind = "lifetime"
	}
	d := quotaDecision{Mode: p.QuotaMode, Limit: p.TokenLimit, Interval: p.IntervalKind, CounterKey: "user:" + subject}
	start, reset, resetStatus, err := a.userQuotaWindow(ctx, subject, p, time.Now().UTC())
	if err != nil {
		return quotaDecision{}, p, store.Usage{}, err
	}
	u, err := a.store.UsageSummaryForUser(ctx, subject, start)
	if err != nil {
		return quotaDecision{}, p, store.Usage{}, err
	}
	d.Used = u.TotalTokens
	d.ResetAt = reset
	d.WindowStart = &start
	d.ResetCredits = &resetStatus
	if p.QuotaMode != "unlimited" && p.TokenLimit != nil {
		remaining := *p.TokenLimit - u.TotalTokens
		if remaining < 0 {
			remaining = 0
		}
		d.Remaining = &remaining
	}
	return d, p, u, nil
}

func policyAllowsModel(p store.Policy, alias string) bool {
	if len(p.AllowedModels) == 0 {
		return true
	}
	var allowed []string
	if json.Unmarshal(p.AllowedModels, &allowed) != nil || len(allowed) == 0 {
		return true
	}
	for _, model := range allowed {
		if model == "*" || model == alias {
			return true
		}
	}
	return false
}

func (a *app) quotaFor(r *http.Request) (quotaDecision, store.Policy, error) {
	c := current(r)
	principal := currentPrincipal(r)
	if u, ok := currentUser(r); ok && u.Status == "pending" {
		p, configured, err := a.store.UserPolicyOverride(r.Context(), c.Sub)
		if err != nil {
			return quotaDecision{}, p, err
		}
		if !configured {
			p = a.pendingQuotaPolicy()
		}
		if p.QuotaMode == "" {
			p.QuotaMode = "unlimited"
		}
		if p.IntervalKind == "" {
			p.IntervalKind = "lifetime"
		}
		d := quotaDecision{Mode: p.QuotaMode, Limit: p.TokenLimit, Interval: p.IntervalKind, CounterKey: "user:" + c.Sub}
		if p.QuotaMode == "unlimited" || p.TokenLimit == nil {
			return d, p, nil
		}
		start, reset, resetStatus, err := a.userQuotaWindow(r.Context(), c.Sub, p, time.Now().UTC())
		if err != nil {
			return quotaDecision{}, p, err
		}
		uUsage, err := a.store.UsageSummaryForUser(r.Context(), c.Sub, start)
		if err != nil {
			return quotaDecision{}, p, err
		}
		d.Used = uUsage.TotalTokens
		remaining := *p.TokenLimit - uUsage.TotalTokens
		if remaining < 0 {
			remaining = 0
		}
		d.Remaining = &remaining
		d.ResetAt = reset
		d.WindowStart = &start
		d.ResetCredits = &resetStatus
		return d, p, nil
	}
	p, _, err := a.store.PolicyForPrincipal(r.Context(), c.Sub, principal.APIKeyID, roles(c, a.cfg.ClientID))
	if err != nil {
		return quotaDecision{}, p, err
	}
	if p.QuotaMode == "" {
		p.QuotaMode = "unlimited"
	}
	if p.IntervalKind == "" {
		p.IntervalKind = "lifetime"
	}
	counterKey := "user:" + c.Sub
	if p.ScopeType == "api_key" && principal.APIKeyID != "" {
		counterKey = "api_key:" + principal.APIKeyID
	}
	d := quotaDecision{Mode: p.QuotaMode, Limit: p.TokenLimit, Interval: p.IntervalKind, CounterKey: counterKey}
	if p.QuotaMode == "unlimited" || p.TokenLimit == nil {
		return d, p, nil
	}
	start, reset := quotaWindow(p, time.Now().UTC())
	var resetStatus store.QuotaResetStatus
	var u store.Usage
	if p.ScopeType == "api_key" && principal.APIKeyID != "" {
		u, err = a.store.UsageSummaryForAPIKey(r.Context(), principal.APIKeyID, start)
	} else {
		start, reset, resetStatus, err = a.userQuotaWindow(r.Context(), c.Sub, p, time.Now().UTC())
		if err == nil {
			u, err = a.store.UsageSummaryForUser(r.Context(), c.Sub, start)
		}
	}
	if err != nil {
		return quotaDecision{}, p, err
	}
	d.Used = u.TotalTokens
	remaining := *p.TokenLimit - u.TotalTokens
	if remaining < 0 {
		remaining = 0
	}
	d.Remaining = &remaining
	d.ResetAt = reset
	d.WindowStart = &start
	if p.ScopeType != "api_key" || principal.APIKeyID == "" {
		d.ResetCredits = &resetStatus
	}
	return d, p, nil
}

func reservationTokens(body []byte) int64 {
	var payload struct {
		MaxTokens           int64 `json:"max_tokens"`
		MaxCompletionTokens int64 `json:"max_completion_tokens"`
	}
	_ = json.Unmarshal(body, &payload)
	out := payload.MaxCompletionTokens
	if out <= 0 {
		out = payload.MaxTokens
	}
	if out <= 0 {
		out = 2048
	}
	// Conservative prompt estimate until tokenizer-aware routing is introduced.
	prompt := int64(len(body)/4 + 256)
	if prompt > 16000 {
		prompt = 16000
	}
	return prompt + out
}

func (a *app) reserveQuota(ctx context.Context, requestID string, d quotaDecision, amount int64) error {
	if d.Mode == "unlimited" || d.Limit == nil || a.redis == nil {
		return nil
	}
	key := "quota:pending:" + d.CounterKey
	reservationKey := "quota:reservation:" + requestID
	// Redis tracks only in-flight reservations. Durable completed usage stays in
	// PostgreSQL, so fixed and rolling windows can always be rebuilt accurately.
	script := `local pending=tonumber(redis.call('GET',KEYS[1]) or '0'); local amount=tonumber(ARGV[1]); local lim=tonumber(ARGV[2]); local durable=tonumber(ARGV[3]); if durable+pending+amount>lim then return -1 end; redis.call('INCRBY',KEYS[1],amount); redis.call('EXPIRE',KEYS[1],3600); redis.call('SET',KEYS[2],amount,'EX',3600); return pending+amount`
	v, err := a.redis.Eval(ctx, script, []string{key, reservationKey}, amount, *d.Limit, d.Used).Int64()
	if err != nil {
		return err
	}
	if v < 0 {
		return fmt.Errorf("quota exhausted")
	}
	return nil
}

func (a *app) releaseReservation(ctx context.Context, requestID string, d quotaDecision, reserved int64) {
	if d.Mode == "unlimited" || d.Limit == nil || a.redis == nil {
		return
	}
	key := "quota:pending:" + d.CounterKey
	script := `local pending=tonumber(redis.call('GET',KEYS[1]) or '0'); local amount=tonumber(ARGV[1]); local next=pending-amount; if next<=0 then redis.call('DEL',KEYS[1]) else redis.call('SET',KEYS[1],next,'EX',3600) end; redis.call('DEL',KEYS[2]); return math.max(next,0)`
	_, _ = a.redis.Eval(ctx, script, []string{key, "quota:reservation:" + requestID}, reserved).Result()
}

func parseUsagePayload(body []byte) store.Usage {
	var x struct {
		Usage struct {
			PromptTokens            int64 `json:"prompt_tokens"`
			CompletionTokens        int64 `json:"completion_tokens"`
			TotalTokens             int64 `json:"total_tokens"`
			CompletionTokensDetails struct {
				ReasoningTokens int64 `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &x) != nil {
		return store.Usage{}
	}
	return store.Usage{InputTokens: x.Usage.PromptTokens, OutputTokens: x.Usage.CompletionTokens, TotalTokens: x.Usage.TotalTokens}
}

func (a *app) adminUsers(w http.ResponseWriter, r *http.Request) {
	if err := a.syncKeycloakUsers(r.Context()); err != nil {
		slog.Warn("keycloak user reconciliation failed", "error", err)
	}
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	users, err := a.store.Users(r.Context(), status)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "users unavailable"})
		return
	}
	writeJSON(w, 200, users)
}

func (a *app) adminUserStatus(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Status string `json:"status"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in) != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid body"})
		return
	}
	u, err := a.store.SetUserStatus(r.Context(), current(r).Sub, chi.URLParam(r, "subject"), in.Status)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, u)
}

func (a *app) adminUserRoles(w http.ResponseWriter, r *http.Request) {
	subject := chi.URLParam(r, "subject")
	if subject == "" {
		writeJSON(w, 400, map[string]string{"error": "missing subject"})
		return
	}
	var in struct {
		Roles []string `json:"roles"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in) != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid body"})
		return
	}
	allowed := map[string]bool{"ai-user": true, "ai-admin": true}
	clean := make([]string, 0, len(in.Roles))
	seen := map[string]bool{}
	for _, role := range in.Roles {
		role = strings.TrimSpace(role)
		if !allowed[role] {
			writeJSON(w, 400, map[string]string{"error": "unsupported role " + role})
			return
		}
		if !seen[role] {
			seen[role] = true
			clean = append(clean, role)
		}
	}
	if err := a.setKeycloakRealmRoles(r.Context(), subject, clean); err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	u, err := a.store.SetUserRoles(r.Context(), current(r).Sub, subject, clean)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, u)
}

func (a *app) adminSystemQuota(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		p, found, err := a.store.PolicyForPrincipal(r.Context(), "", "", nil)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": "system token policy unavailable"})
			return
		}
		writeJSON(w, 200, map[string]any{"policy": p, "configured": found})
		return
	}
	var p store.Policy
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&p) != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid body"})
		return
	}
	out, err := a.store.UpsertPolicy(r.Context(), current(r).Sub, "system", "default", p)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, out)
}

func (a *app) adminAudit(w http.ResponseWriter, r *http.Request) {
	rows, err := a.store.AuditLog(r.Context(), 200)
	if err != nil {
		writeJSON(w, 503, map[string]string{"error": "audit log unavailable"})
		return
	}
	writeJSON(w, 200, rows)
}

func (a *app) adminUserQuota(w http.ResponseWriter, r *http.Request) {
	subject := chi.URLParam(r, "subject")
	if subject == "" {
		writeJSON(w, 400, map[string]string{"error": "missing subject"})
		return
	}
	if r.Method == http.MethodGet {
		override, configured, err := a.store.UserPolicyOverride(r.Context(), subject)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": "quota unavailable"})
			return
		}
		var effective store.Policy
		if configured {
			effective = override
		} else if u, userErr := a.store.User(r.Context(), subject); userErr == nil && u.Status == "pending" {
			effective = a.pendingQuotaPolicy()
		} else {
			effective, _, err = a.store.PolicyForUser(r.Context(), subject, nil)
			if err != nil {
				writeJSON(w, 500, map[string]string{"error": "quota unavailable"})
				return
			}
		}
		decision, _, usage, snapErr := a.userQuotaSnapshot(r.Context(), subject)
		if snapErr != nil {
			writeJSON(w, 500, map[string]string{"error": "quota usage unavailable"})
			return
		}
		remaining := int64(0)
		if decision.Remaining != nil {
			remaining = *decision.Remaining
		}
		writeJSON(w, 200, map[string]any{"policy": override, "override": override, "effective": effective, "configured": configured, "usage": usage, "remaining": remaining, "resetAt": decision.ResetAt, "windowStart": decision.WindowStart, "resetCredits": decision.ResetCredits})
		return
	}
	var p store.Policy
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&p) != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid body"})
		return
	}
	out, err := a.store.UpsertUserPolicy(r.Context(), current(r).Sub, subject, p)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, out)
}

func (a *app) adminAPIKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := a.store.APIKeys(r.Context())
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "api keys unavailable"})
		return
	}
	writeJSON(w, 200, keys)
}

func (a *app) createAPIKey(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name         string     `json:"name"`
		OwnerSubject string     `json:"ownerSubject"`
		Scopes       []string   `json:"scopes"`
		ExpiresAt    *time.Time `json:"expiresAt"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in) != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid body"})
		return
	}
	actor := current(r).Sub
	if strings.TrimSpace(in.OwnerSubject) == "" {
		in.OwnerSubject = actor
	}
	if strings.TrimSpace(in.Name) == "" {
		in.Name = "developer-key"
	}
	if len(in.Scopes) == 0 {
		in.Scopes = []string{"inference"}
	}
	id, raw, prefix, hash, err := newAPIKeySecret()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "key generation failed"})
		return
	}
	key, err := a.store.CreateAPIKey(r.Context(), actor, store.APIKey{ID: id, OwnerSubject: in.OwnerSubject, Name: in.Name, KeyPrefix: prefix, Scopes: in.Scopes, ExpiresAt: in.ExpiresAt}, hash)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "api key creation failed"})
		return
	}
	writeJSON(w, 201, map[string]any{"apiKey": key, "key": raw})
}

func (a *app) revokeAPIKey(w http.ResponseWriter, r *http.Request) {
	if err := a.store.RevokeAPIKey(r.Context(), current(r).Sub, chi.URLParam(r, "id")); err != nil {
		writeJSON(w, 404, map[string]string{"error": "api key not found"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *app) adminAPIKeyQuota(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeJSON(w, 400, map[string]string{"error": "missing api key id"})
		return
	}
	if r.Method == http.MethodGet {
		p, found, err := a.store.PolicyForPrincipal(r.Context(), "", id, nil)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": "quota unavailable"})
			return
		}
		writeJSON(w, 200, map[string]any{"policy": p, "configured": found})
		return
	}
	var p store.Policy
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&p) != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid body"})
		return
	}
	out, err := a.store.UpsertPolicy(r.Context(), current(r).Sub, "api_key", id, p)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, out)
}

func (a *app) chatAccess(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, ok := currentUser(r)
		if !ok {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "application identity unavailable"})
			return
		}
		switch u.Status {
		case "approved":
			next.ServeHTTP(w, r)
		case "pending":
			if currentPrincipal(r).AuthKind != "oidc" {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "pending_chat_requires_interactive_login"})
				return
			}
			next.ServeHTTP(w, r)
		case "suspended", "rejected":
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "access_not_approved", "status": u.Status})
		default:
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "access_not_approved", "status": u.Status})
		}
	})
}

func (a *app) enforcePendingChatRate(ctx context.Context, subject string) (time.Duration, error) {
	if a.redis == nil {
		return 0, fmt.Errorf("pending chat rate limiter unavailable")
	}
	now := time.Now().UTC()
	hourKey := "pending-chat:hour:" + subject
	lastKey := "pending-chat:last:" + subject
	script := `local now=tonumber(ARGV[1]); local minGap=tonumber(ARGV[2]); local hourly=tonumber(ARGV[3]); local last=tonumber(redis.call('GET',KEYS[2]) or '0'); if last>0 and now-last<minGap then return -(minGap-(now-last)) end; local count=tonumber(redis.call('GET',KEYS[1]) or '0'); if count>=hourly then local ttl=redis.call('TTL',KEYS[1]); if ttl<1 then ttl=3600 end; return -ttl end; count=redis.call('INCR',KEYS[1]); if count==1 then redis.call('EXPIRE',KEYS[1],3600) end; redis.call('SET',KEYS[2],now,'EX',3600); return count`
	result, err := a.redis.Eval(ctx, script, []string{hourKey, lastKey}, now.Unix(), a.cfg.PendingChatMinIntervalSeconds, a.cfg.PendingChatRequestsPerHour).Int64()
	if err != nil {
		return 0, err
	}
	if result < 0 {
		return time.Duration(-result) * time.Second, fmt.Errorf("pending chat rate limit exceeded")
	}
	return 0, nil
}

func (a *app) restrictPendingChat(body []byte) ([]byte, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("invalid chat payload")
	}
	encoded := string(body)
	if strings.Contains(encoded, `"image_url"`) || strings.Contains(encoded, `"input_image"`) {
		return nil, fmt.Errorf("pending accounts support text chat only")
	}
	payload["model"] = "fast"
	delete(payload, "max_tokens")
	payload["max_completion_tokens"] = a.cfg.PendingChatMaxCompletionTokens
	return json.Marshal(payload)
}

func (a *app) pendingQuotaPolicy() store.Policy {
	limit := a.cfg.PendingChatTokenLimit
	return store.Policy{
		ScopeType:     "user",
		ScopeID:       "pending-chat",
		QuotaMode:     "limited",
		TokenLimit:    &limit,
		IntervalKind:  "day",
		AllowedModels: json.RawMessage(`["fast"]`),
	}
}

func (a *app) adminUserDetail(w http.ResponseWriter, r *http.Request) {
	subject := strings.TrimSpace(chi.URLParam(r, "subject"))
	if subject == "" {
		writeJSON(w, 400, map[string]string{"error": "missing subject"})
		return
	}
	period := strings.TrimSpace(r.URL.Query().Get("period"))
	if period == "" {
		period = "month"
	}
	now := time.Now().UTC()
	var seriesSince time.Time
	switch period {
	case "day":
		seriesSince = now.Add(-24 * time.Hour)
	case "month":
		seriesSince = now.AddDate(0, -1, 0)
	case "year":
		seriesSince = now.AddDate(-1, 0, 0)
	default:
		writeJSON(w, 400, map[string]string{"error": "period must be day, month or year"})
		return
	}
	u, err := a.store.User(r.Context(), subject)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "user not found"})
		return
	}
	keys, err := a.store.APIKeysForUser(r.Context(), subject)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "api keys unavailable"})
		return
	}
	connections, err := a.store.OAuthConnectionsForUser(r.Context(), subject)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "connections unavailable"})
		return
	}
	attachments, err := a.store.UserAttachments(r.Context(), subject, 200)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "attachments unavailable"})
		return
	}
	caps, err := a.store.CapabilitiesForUser(r.Context(), subject)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "capabilities unavailable"})
		return
	}
	activity, err := a.store.RecentActivityForUser(r.Context(), subject, 100)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "activity unavailable"})
		return
	}
	series, err := a.store.UsageSeriesForUser(r.Context(), subject, period, "Asia/Bangkok", seriesSince)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "usage series unavailable"})
		return
	}
	dayUsage, dayReq, err := a.store.UsageTotalsForUser(r.Context(), subject, now.Add(-24*time.Hour))
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "daily usage unavailable"})
		return
	}
	monthUsage, monthReq, err := a.store.UsageTotalsForUser(r.Context(), subject, now.AddDate(0, -1, 0))
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "monthly usage unavailable"})
		return
	}
	yearUsage, yearReq, err := a.store.UsageTotalsForUser(r.Context(), subject, now.AddDate(-1, 0, 0))
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "yearly usage unavailable"})
		return
	}
	allUsage, allReq, err := a.store.UsageTotalsForUser(r.Context(), subject, time.Unix(0, 0).UTC())
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "lifetime usage unavailable"})
		return
	}
	policy, configured, err := a.store.PolicyForUser(r.Context(), subject, u.Roles)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "quota unavailable"})
		return
	}
	activeKeys := 0
	for _, k := range keys {
		if k.Status == "active" {
			activeKeys++
		}
	}
	writeJSON(w, 200, map[string]any{
		"user":          u,
		"apiKeys":       keys,
		"apiKeySummary": map[string]any{"total": len(keys), "active": activeKeys, "revoked": len(keys) - activeKeys},
		"connections":   connections,
		"attachments":   attachments,
		"capabilities":  caps,
		"quota":         map[string]any{"policy": policy, "configured": configured},
		"usage": map[string]any{
			"day":      map[string]any{"tokens": dayUsage, "requests": dayReq},
			"month":    map[string]any{"tokens": monthUsage, "requests": monthReq},
			"year":     map[string]any{"tokens": yearUsage, "requests": yearReq},
			"lifetime": map[string]any{"tokens": allUsage, "requests": allReq},
			"period":   period,
			"series":   series,
		},
		"activity":   activity,
		"dataPolicy": map[string]any{"requestResponseSnapshots": true, "snapshotLimitBytes": storedPayloadLimit, "secretsRedacted": true, "rawApiKeysStored": false, "rawOAuthRefreshTokensExposed": false},
	})
}

func (a *app) quotaResets(w http.ResponseWriter, r *http.Request) {
	status, err := a.store.QuotaResetStatus(r.Context(), current(r).Sub)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "quota reset status unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (a *app) useQuotaReset(w http.ResponseWriter, r *http.Request) {
	decision, _, err := a.quotaFor(r)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "quota unavailable"})
		return
	}
	blockedCount := int64(0)
	if a.redis != nil {
		blockedCount, _ = a.redis.Exists(r.Context(), "quota:blocked:"+decision.CounterKey).Result()
	}
	if decision.Mode != "limited" || decision.Limit == nil || decision.Remaining == nil || (*decision.Remaining > 0 && blockedCount == 0) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "quota_not_exhausted", "quota": decision})
		return
	}
	event, err := a.store.UseQuotaResetCredit(r.Context(), current(r).Sub)
	if err != nil {
		if err == pgx.ErrNoRows {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "no_quota_resets_available"})
		} else {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "unable to use quota reset"})
		}
		return
	}
	a.clearUserQuotaRuntimeState(r.Context(), current(r).Sub)
	decision, _, usage, snapErr := a.userQuotaSnapshot(r.Context(), current(r).Sub)
	if snapErr != nil {
		writeJSON(w, http.StatusOK, map[string]any{"reset": event})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reset": event, "usage": usage, "quota": decision})
}

func (a *app) adminGrantUserQuotaResets(w http.ResponseWriter, r *http.Request) {
	subject := strings.TrimSpace(chi.URLParam(r, "subject"))
	var in struct {
		Count     int       `json:"count"`
		ExpiresAt time.Time `json:"expiresAt"`
		Note      string    `json:"note"`
	}
	if subject == "" || json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid reset grant"})
		return
	}
	grant, err := a.store.GrantQuotaResets(r.Context(), current(r).Sub, subject, in.Count, in.ExpiresAt, strings.TrimSpace(in.Note))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, grant)
}

func (a *app) adminResetUserQuota(w http.ResponseWriter, r *http.Request) {
	subject := strings.TrimSpace(chi.URLParam(r, "subject"))
	if subject == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing subject"})
		return
	}
	var in struct {
		Note string `json:"note"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in)
	event, err := a.store.AdminResetQuota(r.Context(), current(r).Sub, subject, strings.TrimSpace(in.Note))
	if err != nil {
		if err == pgx.ErrNoRows {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "user not found"})
		} else {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "unable to reset quota"})
		}
		return
	}
	a.clearUserQuotaRuntimeState(r.Context(), subject)
	decision, _, usage, snapErr := a.userQuotaSnapshot(r.Context(), subject)
	if snapErr != nil {
		writeJSON(w, http.StatusOK, map[string]any{"reset": event})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reset": event, "usage": usage, "quota": decision})
}
