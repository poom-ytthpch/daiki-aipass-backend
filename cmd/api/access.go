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

type quotaLimitDecision struct {
	ID            string     `json:"id"`
	Primary       bool       `json:"primary,omitempty"`
	Limit         int64      `json:"limit"`
	Used          int64      `json:"used"`
	Remaining     int64      `json:"remaining"`
	ResetAt       *time.Time `json:"resetAt,omitempty"`
	Interval      string     `json:"interval"`
	IntervalCount int        `json:"intervalCount,omitempty"`
	WindowStart   time.Time  `json:"windowStart"`
}

type quotaDecision struct {
	CounterKey      string                  `json:"-"`
	Mode            string                  `json:"mode"`
	Limit           *int64                  `json:"limit,omitempty"`
	Used            int64                   `json:"used"`
	Remaining       *int64                  `json:"remaining,omitempty"`
	ResetAt         *time.Time              `json:"resetAt,omitempty"`
	Interval        string                  `json:"interval"`
	IntervalCount   int                     `json:"intervalCount,omitempty"`
	WindowStart     *time.Time              `json:"windowStart,omitempty"`
	Blocked         bool                    `json:"blocked"`
	BlockingLimitID string                  `json:"blockingLimitId,omitempty"`
	Limits          []quotaLimitDecision    `json:"limits,omitempty"`
	ResetCredits    *store.QuotaResetStatus `json:"resetCredits,omitempty"`
}

type quotaSpec struct {
	ID              string
	Primary         bool
	Limit           int64
	IntervalKind    string
	IntervalCount   int
	IntervalSeconds *int64
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

func quotaWindowSpec(kind string, count int, seconds *int64, now time.Time) (time.Time, *time.Time) {
	var start time.Time
	var reset *time.Time
	if count <= 0 {
		count = 1
	}
	switch kind {
	case "hour":
		span := time.Duration(count) * time.Hour
		start = now.Truncate(span)
		r := start.Add(span)
		reset = &r
	case "day":
		start = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		r := start.AddDate(0, 0, count)
		reset = &r
	case "week":
		d := (int(now.Weekday()) + 6) % 7
		base := now.AddDate(0, 0, -d)
		start = time.Date(base.Year(), base.Month(), base.Day(), 0, 0, 0, 0, time.UTC)
		r := start.AddDate(0, 0, 7*count)
		reset = &r
	case "month":
		start = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		r := start.AddDate(0, count, 0)
		reset = &r
	case "rolling", "custom":
		windowSeconds := int64(3600)
		if seconds != nil {
			windowSeconds = *seconds
		}
		start = now.Add(-time.Duration(windowSeconds) * time.Second)
		r := now.Add(time.Duration(windowSeconds) * time.Second)
		reset = &r
	default:
		start = time.Unix(0, 0).UTC()
	}
	return start, reset
}

func quotaWindow(p store.Policy, now time.Time) (time.Time, *time.Time) {
	return quotaWindowSpec(p.IntervalKind, p.IntervalCount, p.IntervalSeconds, now)
}

func policyQuotaSpecs(p store.Policy) ([]quotaSpec, error) {
	if p.QuotaMode == "unlimited" || p.TokenLimit == nil {
		return nil, nil
	}
	specs := []quotaSpec{{ID: "primary", Primary: true, Limit: *p.TokenLimit, IntervalKind: p.IntervalKind, IntervalCount: p.IntervalCount, IntervalSeconds: p.IntervalSeconds}}
	if len(p.ParallelLimits) == 0 {
		return specs, nil
	}
	var parallel []store.QuotaLimit
	if err := json.Unmarshal(p.ParallelLimits, &parallel); err != nil {
		return nil, fmt.Errorf("invalid parallel quota limits: %w", err)
	}
	for i, limit := range parallel {
		id := strings.TrimSpace(limit.ID)
		if id == "" {
			id = fmt.Sprintf("parallel-%d", i+1)
		}
		specs = append(specs, quotaSpec{ID: id, Limit: limit.TokenLimit, IntervalKind: limit.IntervalKind, IntervalCount: limit.IntervalCount, IntervalSeconds: limit.IntervalSeconds})
	}
	return specs, nil
}

func (a *app) clearUserQuotaRuntimeState(ctx context.Context, subject string) {
	if a.redis == nil || strings.TrimSpace(subject) == "" {
		return
	}
	for _, pattern := range []string{"quota:pending:user:" + subject + ":*", "quota:reservation:user:" + subject + ":*"} {
		var cursor uint64
		for {
			keys, next, err := a.redis.Scan(ctx, cursor, pattern, 100).Result()
			if err != nil {
				break
			}
			if len(keys) > 0 {
				_ = a.redis.Del(ctx, keys...).Err()
			}
			cursor = next
			if cursor == 0 {
				break
			}
		}
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

func (a *app) evaluateQuota(ctx context.Context, subject, apiKeyID string, p store.Policy, countAsAPIKey bool) (quotaDecision, store.Usage, error) {
	if p.QuotaMode == "" {
		p.QuotaMode = "unlimited"
	}
	if p.IntervalKind == "" {
		p.IntervalKind = "lifetime"
	}
	if p.IntervalCount <= 0 {
		p.IntervalCount = 1
	}
	counterKey := "user:" + subject
	if countAsAPIKey && apiKeyID != "" {
		counterKey = "api_key:" + apiKeyID
	}
	d := quotaDecision{Mode: p.QuotaMode, Limit: p.TokenLimit, Interval: p.IntervalKind, IntervalCount: p.IntervalCount, CounterKey: counterKey}
	if p.QuotaMode == "unlimited" || p.TokenLimit == nil {
		return d, store.Usage{}, nil
	}
	specs, err := policyQuotaSpecs(p)
	if err != nil {
		return quotaDecision{}, store.Usage{}, err
	}
	now := time.Now().UTC()
	var resetStatus store.QuotaResetStatus
	if !countAsAPIKey {
		resetStatus, err = a.store.QuotaResetStatus(ctx, subject)
		if err != nil {
			return quotaDecision{}, store.Usage{}, err
		}
		d.ResetCredits = &resetStatus
	}
	var primaryUsage store.Usage
	var blockingReset *time.Time
	blockingNever := false
	for _, spec := range specs {
		start, reset := quotaWindowSpec(spec.IntervalKind, spec.IntervalCount, spec.IntervalSeconds, now)
		if !countAsAPIKey && resetStatus.LastResetAt != nil && resetStatus.LastResetAt.After(start) && !resetStatus.LastResetAt.After(now) {
			start = *resetStatus.LastResetAt
		}
		var usage store.Usage
		if countAsAPIKey {
			usage, err = a.store.UsageSummaryForAPIKey(ctx, apiKeyID, start)
		} else {
			usage, err = a.store.UsageSummaryForUser(ctx, subject, start)
		}
		if err != nil {
			return quotaDecision{}, store.Usage{}, err
		}
		remaining := spec.Limit - usage.TotalTokens
		if remaining < 0 {
			remaining = 0
		}
		limit := quotaLimitDecision{ID: spec.ID, Primary: spec.Primary, Limit: spec.Limit, Used: usage.TotalTokens, Remaining: remaining, ResetAt: reset, Interval: spec.IntervalKind, IntervalCount: max(1, spec.IntervalCount), WindowStart: start}
		d.Limits = append(d.Limits, limit)
		if spec.Primary {
			primaryUsage = usage
			d.Used = usage.TotalTokens
			d.Remaining = &remaining
			d.ResetAt = reset
			d.WindowStart = &start
		}
		if remaining <= 0 {
			d.Blocked = true
			// A request becomes usable only after every exhausted window resets.
			// A lifetime blocker therefore wins; otherwise use the latest reset.
			if reset == nil {
				blockingNever = true
				d.BlockingLimitID = spec.ID
			} else if !blockingNever && (blockingReset == nil || reset.After(*blockingReset)) {
				t := *reset
				blockingReset = &t
				d.BlockingLimitID = spec.ID
			}
		}
	}
	if d.Blocked {
		if blockingNever {
			d.ResetAt = nil
		} else {
			d.ResetAt = blockingReset
		}
	}
	return d, primaryUsage, nil
}

func (a *app) userQuotaSnapshot(ctx context.Context, subject string) (quotaDecision, store.Policy, store.Usage, error) {
	p, _, err := a.effectiveUserQuota(ctx, subject)
	if err != nil {
		return quotaDecision{}, store.Policy{}, store.Usage{}, err
	}
	d, usage, err := a.evaluateQuota(ctx, subject, "", p, false)
	return d, p, usage, err
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
		d, _, err := a.evaluateQuota(r.Context(), c.Sub, "", p, false)
		return d, p, err
	}
	p, _, err := a.store.PolicyForPrincipal(r.Context(), c.Sub, principal.APIKeyID, roles(c, a.cfg.ClientID))
	if err != nil {
		return quotaDecision{}, p, err
	}
	countAsAPIKey := p.ScopeType == "api_key" && principal.APIKeyID != ""
	d, _, err := a.evaluateQuota(r.Context(), c.Sub, principal.APIKeyID, p, countAsAPIKey)
	return d, p, err
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

func quotaRuntimeKeys(d quotaDecision, requestID string) ([]string, []any) {
	if len(d.Limits) == 0 {
		return nil, nil
	}
	keys := make([]string, 0, len(d.Limits)*2)
	args := make([]any, 0, 2+len(d.Limits)*2)
	args = append(args, len(d.Limits))
	for _, limit := range d.Limits {
		keys = append(keys, fmt.Sprintf("quota:pending:%s:%s:%d", d.CounterKey, limit.ID, limit.WindowStart.Unix()))
	}
	for _, limit := range d.Limits {
		keys = append(keys, fmt.Sprintf("quota:reservation:%s:%s:%s", d.CounterKey, limit.ID, requestID))
	}
	return keys, args
}

func (a *app) reserveQuota(ctx context.Context, requestID string, d quotaDecision, amount int64) error {
	if d.Mode == "unlimited" || d.Limit == nil {
		return nil
	}
	if d.Blocked {
		return fmt.Errorf("quota exhausted")
	}
	if a.redis == nil {
		return nil
	}
	keys, args := quotaRuntimeKeys(d, requestID)
	if len(d.Limits) == 0 {
		return nil
	}
	args = append(args, amount)
	for _, limit := range d.Limits {
		args = append(args, limit.Limit, limit.Used)
	}
	// All configured windows are checked atomically. Every successful request
	// consumes the same eventual token usage from every window, e.g. 10K/hour
	// and 10M/week. The reservation is capped to each window's remaining
	// headroom so a large max-output estimate cannot make a freshly reset small
	// quota unusable.
	script := `local n=tonumber(ARGV[1]); local amount=tonumber(ARGV[2]); for i=1,n do local lim=tonumber(ARGV[2*i+1]); local durable=tonumber(ARGV[2*i+2]); local pending=tonumber(redis.call('GET',KEYS[i]) or '0'); if lim-durable-pending<=0 then return -1 end end; for i=1,n do local lim=tonumber(ARGV[2*i+1]); local durable=tonumber(ARGV[2*i+2]); local pending=tonumber(redis.call('GET',KEYS[i]) or '0'); local reserve=math.min(amount,lim-durable-pending); redis.call('INCRBY',KEYS[i],reserve); redis.call('EXPIRE',KEYS[i],3600); redis.call('SET',KEYS[n+i],reserve,'EX',3600); end; return 1`
	v, err := a.redis.Eval(ctx, script, keys, args...).Int64()
	if err != nil {
		return err
	}
	if v < 0 {
		return fmt.Errorf("quota exhausted")
	}
	return nil
}

func (a *app) releaseReservation(ctx context.Context, requestID string, d quotaDecision, reserved int64) {
	if d.Mode == "unlimited" || d.Limit == nil || a.redis == nil || len(d.Limits) == 0 {
		return
	}
	keys, _ := quotaRuntimeKeys(d, requestID)
	args := []any{len(d.Limits)}
	// Read the exact amount reserved for every parallel window. This prevents a
	// capped short-window reservation from subtracting too much from another
	// concurrent request when it is released.
	script := `local n=tonumber(ARGV[1]); for i=1,n do local pending=tonumber(redis.call('GET',KEYS[i]) or '0'); local amount=tonumber(redis.call('GET',KEYS[n+i]) or '0'); local next=pending-amount; if next<=0 then redis.call('DEL',KEYS[i]) else redis.call('SET',KEYS[i],next,'EX',3600) end; redis.call('DEL',KEYS[n+i]); end; return 1`
	_, _ = a.redis.Eval(ctx, script, keys, args...).Result()
	_ = reserved // kept in the signature for existing callers and compatibility.
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
		writeJSON(w, 200, map[string]any{"policy": override, "override": override, "effective": effective, "configured": configured, "usage": usage, "remaining": remaining, "resetAt": decision.ResetAt, "windowStart": decision.WindowStart, "resetCredits": decision.ResetCredits, "blocked": decision.Blocked, "blockingLimitId": decision.BlockingLimitID, "limits": decision.Limits})
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
	if decision.Mode != "limited" || decision.Limit == nil || (!decision.Blocked && blockedCount == 0) {
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
