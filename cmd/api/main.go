package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/poom-ytthpch/daiki-ai-passport-backend/internal/inference"
	"github.com/poom-ytthpch/daiki-ai-passport-backend/internal/store"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
)

type config struct {
	Addr, PublicIssuer, JWKSURL, ClientID string
	KeycloakBase, KeycloakRealm           string
	KeycloakAdminUser, KeycloakAdminPass  string
	LiteLLMBase, LiteLLMKey               string
	DatabaseURL, RedisAddr                string
	AllowedOrigins                        []string
	LocalLLMBase                          string
	PendingChatTokenLimit                 int64
	PendingChatRequestsPerHour            int
	PendingChatMinIntervalSeconds         int
	PendingChatMaxCompletionTokens        int
}

type app struct {
	cfg      config
	verifier *oidc.IDTokenVerifier
	http     *http.Client
	redis    *redis.Client
	db       *pgxpool.Pool
	store    *store.Store
	router   *inference.Router
	queue    *inference.Queue
}

type claims struct {
	Sub               string `json:"sub"`
	Name              string `json:"name"`
	Email             string `json:"email"`
	PreferredUsername string `json:"preferred_username"`
	RealmAccess       struct {
		Roles []string `json:"roles"`
	} `json:"realm_access"`
	ResourceAccess map[string]struct {
		Roles []string `json:"roles"`
	} `json:"resource_access"`
}

type ctxKey string

const claimsKey ctxKey = "claims"

func getenv(k, d string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return d
}
func splitCSV(v string) []string {
	var out []string
	for _, x := range strings.Split(v, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}
func getenvInt(k string, d int) int {
	v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(k)))
	if err != nil || v <= 0 {
		return d
	}
	return v
}

func loadConfig() config {
	return config{
		Addr: getenv("ADDR", ":8080"), PublicIssuer: getenv("OIDC_ISSUER", "https://auth.ai.infra.local/realms/daiki"),
		JWKSURL: getenv("OIDC_JWKS_URL", "http://keycloak.daiki-ai-passport.svc.cluster.local:8080/realms/daiki/protocol/openid-connect/certs"), ClientID: getenv("OIDC_CLIENT_ID", "daiki-web"),
		KeycloakBase: getenv("KEYCLOAK_BASE_URL", "http://keycloak.daiki-ai-passport.svc.cluster.local:8080"), KeycloakRealm: getenv("KEYCLOAK_REALM", "daiki"),
		KeycloakAdminUser: getenv("KEYCLOAK_ADMIN_USER", "admin"), KeycloakAdminPass: os.Getenv("KEYCLOAK_ADMIN_PASSWORD"),
		LiteLLMBase: getenv("LITELLM_BASE_URL", "http://litellm.daiki-ai-passport.svc.cluster.local:4000"), LiteLLMKey: os.Getenv("LITELLM_MASTER_KEY"),
		DatabaseURL: os.Getenv("DATABASE_URL"), RedisAddr: getenv("REDIS_ADDR", "daiki-redis.daiki-ai-passport.svc.cluster.local:6379"),
		AllowedOrigins: splitCSV(getenv("ALLOWED_ORIGINS", "https://ai.infra.local")), LocalLLMBase: getenv("LOCAL_LLM_BASE_URL", "http://10.90.0.11:8000/v1"),
		PendingChatTokenLimit: int64(getenvInt("PENDING_CHAT_TOKEN_LIMIT", 8000)), PendingChatRequestsPerHour: getenvInt("PENDING_CHAT_REQUESTS_PER_HOUR", 10),
		PendingChatMinIntervalSeconds: getenvInt("PENDING_CHAT_MIN_INTERVAL_SECONDS", 30), PendingChatMaxCompletionTokens: getenvInt("PENDING_CHAT_MAX_COMPLETION_TOKENS", 512),
	}
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	cfg := loadConfig()
	keySet := oidc.NewRemoteKeySet(context.Background(), cfg.JWKSURL)
	a := &app{cfg: cfg, verifier: oidc.NewVerifier(cfg.PublicIssuer, keySet, &oidc.Config{ClientID: cfg.ClientID}), http: &http.Client{Timeout: 15 * time.Second}, router: inference.NewRouter(getenv("MODEL_FAST", "qwen-local"), getenv("MODEL_BALANCED", "qwen-local"), getenv("MODEL_DEEP", "qwen-local"), getenv("MODEL_VISION", "qwen-local"))}
	if cfg.RedisAddr != "" {
		a.redis = redis.NewClient(&redis.Options{Addr: cfg.RedisAddr})
		a.queue = inference.NewQueue(a.redis, inference.QueueConfig{
			GlobalLimit: getenvInt("INFERENCE_GLOBAL_CONCURRENCY", 4), PerPrincipalLimit: 1,
			WorkloadLimits: map[inference.Workload]int{
				inference.WorkloadFast:      getenvInt("INFERENCE_FAST_CONCURRENCY", 4),
				inference.WorkloadDeep:      getenvInt("INFERENCE_DEEP_CONCURRENCY", 1),
				inference.WorkloadVision:    getenvInt("INFERENCE_VISION_CONCURRENCY", 1),
				inference.WorkloadImage:     getenvInt("INFERENCE_IMAGE_CONCURRENCY", 1),
				inference.WorkloadEmbedding: getenvInt("INFERENCE_EMBEDDING_CONCURRENCY", 2),
				inference.WorkloadBatch:     getenvInt("INFERENCE_BATCH_CONCURRENCY", 1),
			},
			LeaseTTL: time.Duration(getenvInt("INFERENCE_LEASE_SECONDS", 30)) * time.Second,
			MaxWait:  time.Duration(getenvInt("INFERENCE_MAX_WAIT_SECONDS", 120)) * time.Second,
		})
	}
	if cfg.DatabaseURL != "" {
		if db, err := pgxpool.New(ctx, cfg.DatabaseURL); err == nil {
			a.db = db
			a.store = store.New(db)
			if err := a.store.Migrate(ctx); err != nil {
				panic(fmt.Errorf("database schema migration failed: %w", err))
			}
			defer db.Close()
		} else {
			slog.Warn("postgres disabled", "error", err)
		}
	}

	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Recoverer, middleware.Timeout(6*time.Minute))
	r.Use(cors.Handler(cors.Options{AllowedOrigins: cfg.AllowedOrigins, AllowedMethods: []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"}, AllowedHeaders: []string{"Accept", "Authorization", "Content-Type"}, AllowCredentials: true, MaxAge: 300}))
	r.Get("/metrics", promhttp.Handler().ServeHTTP)
	r.Route("/v1", func(r chi.Router) {
		r.Get("/health/live", a.live)
		r.Get("/health/ready", a.ready)
		r.Group(func(r chi.Router) {
			r.Use(a.auth)
			r.Get("/me", a.me)
			r.Route("/admin", func(r chi.Router) {
				r.Use(a.adminOnly)
				r.Get("/summary", a.adminSummary)
				r.Get("/queues", a.adminQueues)
				r.Get("/users", a.adminUsers)
				r.Post("/users", a.createUser)
				r.Patch("/users/{subject}/status", a.adminUserStatus)
				r.Get("/users/{subject}/quota", a.adminUserQuota)
				r.Put("/users/{subject}/quota", a.adminUserQuota)
				r.Get("/api-keys", a.adminAPIKeys)
				r.Post("/api-keys", a.createAPIKey)
				r.Delete("/api-keys/{id}", a.revokeAPIKey)
				r.Get("/api-keys/{id}/quota", a.adminAPIKeyQuota)
				r.Put("/api-keys/{id}/quota", a.adminAPIKeyQuota)
			})
			r.Group(func(r chi.Router) {
				r.Use(a.chatAccess)
				r.Use(requirePrincipalScope("inference"))
				r.Post("/chat", a.chat)
				r.Post("/chat/stream", a.chatStream)
			})
			r.Group(func(r chi.Router) {
				r.Use(a.approvalRequired)
				r.Use(requirePrincipalScope("inference"))
				r.Get("/models", a.models)
				r.Get("/usage", a.usage)
			})
		})
	})
	srv := &http.Server{Addr: cfg.Addr, Handler: r, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	slog.Info("daiki backend listening", "addr", cfg.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		panic(err)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(strings.ToLower(h), "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}
func (a *app) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := bearer(r)
		if tok == "" {
			writeJSON(w, 401, map[string]string{"error": "missing bearer token"})
			return
		}
		if strings.HasPrefix(tok, "dk_") {
			if a.store == nil {
				writeJSON(w, 503, map[string]string{"error": "identity store unavailable"})
				return
			}
			key, u, err := a.store.AuthenticateAPIKey(r.Context(), apiKeyHash(tok))
			if err != nil {
				writeJSON(w, 401, map[string]string{"error": "invalid api key"})
				return
			}
			c := claims{Sub: u.Subject, Name: u.DisplayName, Email: u.Email, PreferredUsername: u.Email}
			c.RealmAccess.Roles = append([]string{}, u.Roles...)
			ctx := context.WithValue(r.Context(), claimsKey, c)
			ctx = context.WithValue(ctx, appUserKey, u)
			ctx = context.WithValue(ctx, principalKey, principal{AuthKind: "api_key", APIKeyID: key.ID})
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		id, err := a.verifier.Verify(r.Context(), tok)
		if err != nil {
			writeJSON(w, 401, map[string]string{"error": "invalid token"})
			return
		}
		var c claims
		if err = id.Claims(&c); err != nil {
			writeJSON(w, 401, map[string]string{"error": "invalid claims"})
			return
		}
		if a.store == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "identity store unavailable"})
			return
		}
		u, err := a.store.UpsertLogin(r.Context(), c.Sub, c.Email, first(c.Name, c.PreferredUsername, c.Email, c.Sub), identityProvider(c), roles(c, a.cfg.ClientID))
		if err != nil {
			slog.Error("application user sync failed", "error", err)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "identity store unavailable"})
			return
		}
		ctx := context.WithValue(r.Context(), claimsKey, c)
		ctx = context.WithValue(ctx, appUserKey, u)
		ctx = context.WithValue(ctx, principalKey, principal{AuthKind: "oidc"})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
func current(r *http.Request) claims { c, _ := r.Context().Value(claimsKey).(claims); return c }
func roles(c claims, client string) []string {
	out := append([]string{}, c.RealmAccess.Roles...)
	if x, ok := c.ResourceAccess[client]; ok {
		out = append(out, x.Roles...)
	}
	return out
}
func hasRole(c claims, client string, want ...string) bool {
	for _, r := range roles(c, client) {
		for _, w := range want {
			if r == w {
				return true
			}
		}
	}
	return false
}
func (a *app) adminOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, ok := currentUser(r)
		if !ok || u.Status != "approved" {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "admin_access_requires_approval", "status": u.Status})
			return
		}
		if currentPrincipal(r).AuthKind == "api_key" {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin API requires interactive OIDC"})
			return
		}
		if !hasRole(current(r), a.cfg.ClientID, "ai-admin", "admin") {
			writeJSON(w, 403, map[string]string{"error": "admin role required"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *app) live(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"service": "daiki-ai-passport-backend", "status": "ok"})
}
func (a *app) ready(w http.ResponseWriter, r *http.Request) {
	services := map[string]string{}
	status := 200
	if a.redis != nil {
		if err := a.redis.Ping(r.Context()).Err(); err != nil {
			services["redis"] = "down"
			status = 503
		} else {
			services["redis"] = "ok"
		}
	}
	if a.db != nil {
		if err := a.db.Ping(r.Context()); err != nil {
			services["postgres"] = "down"
			status = 503
		} else {
			services["postgres"] = "ok"
		}
	}
	services["local_llm"] = "optional"
	writeJSON(w, status, map[string]any{"status": map[bool]string{true: "ready", false: "degraded"}[status == 200], "services": services})
}
func (a *app) me(w http.ResponseWriter, r *http.Request) {
	c := current(r)
	u, _ := currentUser(r)
	out := map[string]any{"sub": c.Sub, "name": first(c.Name, c.PreferredUsername, c.Email, c.Sub), "email": c.Email, "roles": roles(c, a.cfg.ClientID), "status": u.Status, "authProvider": u.AuthProvider, "createdAt": u.CreatedAt, "approvedAt": u.ApprovedAt, "authKind": currentPrincipal(r).AuthKind, "apiKeyId": currentPrincipal(r).APIKeyID}
	if u.Status == "pending" {
		out["pendingChatPolicy"] = map[string]any{"model": "fast", "tokenLimitPerDay": a.cfg.PendingChatTokenLimit, "requestsPerHour": a.cfg.PendingChatRequestsPerHour, "minIntervalSeconds": a.cfg.PendingChatMinIntervalSeconds, "maxCompletionTokens": a.cfg.PendingChatMaxCompletionTokens, "textOnly": true}
	}
	writeJSON(w, 200, out)
}
func first(v ...string) string {
	for _, x := range v {
		if x != "" {
			return x
		}
	}
	return ""
}

func (a *app) proxyLiteLLM(w http.ResponseWriter, r *http.Request, path string, stream bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid body"})
		return
	}
	if u, ok := currentUser(r); ok && u.Status == "pending" {
		body, err = a.restrictPendingChat(body)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		retryAfter, rateErr := a.enforcePendingChatRate(r.Context(), current(r).Sub)
		if rateErr != nil {
			if retryAfter > 0 {
				w.Header().Set("Retry-After", strconv.Itoa(max(1, int(retryAfter.Seconds()))))
				writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "pending_chat_rate_limited", "retryAfterSeconds": max(1, int(retryAfter.Seconds()))})
			} else {
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "pending_chat_rate_limiter_unavailable"})
			}
			return
		}
		w.Header().Set("x-daiki-access-mode", "pending-chat")
	}
	route, upstreamBody, err := a.router.RouteChat(body)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	decision, policy, err := a.quotaFor(r)
	if err != nil {
		writeJSON(w, 503, map[string]string{"error": "quota service unavailable"})
		return
	}
	if !policyAllowsModel(policy, route.Alias) {
		writeJSON(w, 403, map[string]any{"error": "model_not_allowed", "model": route.Alias})
		return
	}
	reserved := reservationTokens(body)
	requestID := middleware.GetReqID(r.Context())
	if requestID == "" {
		requestID = fmt.Sprintf("req-%d", time.Now().UnixNano())
	}
	c := current(r)
	if err := a.reserveQuota(r.Context(), requestID, decision, reserved); err != nil {
		status := http.StatusServiceUnavailable
		code := "quota_service_unavailable"
		if strings.Contains(err.Error(), "exhausted") {
			status = http.StatusTooManyRequests
			code = "quota_exhausted"
		}
		writeJSON(w, status, map[string]any{"error": code, "quota": decision})
		return
	}
	if err := a.store.StartUsageForPrincipal(r.Context(), requestID, c.Sub, currentPrincipal(r).APIKeyID, route.Alias, string(route.Workload), reserved); err != nil {
		a.releaseReservation(r.Context(), requestID, decision, reserved)
		writeJSON(w, 503, map[string]string{"error": "usage ledger unavailable"})
		return
	}
	principalID := "user:" + c.Sub
	if currentPrincipal(r).APIKeyID != "" {
		principalID = "api_key:" + currentPrincipal(r).APIKeyID
	}
	if a.queue == nil {
		a.releaseReservation(r.Context(), requestID, decision, reserved)
		_ = a.store.FinishUsage(r.Context(), requestID, "failed", store.Usage{})
		writeJSON(w, 503, map[string]string{"error": "inference queue unavailable"})
		return
	}
	ticket, err := a.queue.Acquire(r.Context(), requestID, principalID, route.Workload, route.Priority)
	if err != nil {
		a.releaseReservation(context.Background(), requestID, decision, reserved)
		status := "failed"
		code := "queue_unavailable"
		httpStatus := http.StatusServiceUnavailable
		if errors.Is(err, inference.ErrQueueTimeout) {
			status = "cancelled"
			code = "queue_timeout"
			httpStatus = http.StatusGatewayTimeout
		}
		if r.Context().Err() != nil {
			status = "cancelled"
			_ = a.store.FinishUsage(context.Background(), requestID, status, store.Usage{})
			return
		}
		_ = a.store.FinishUsage(context.Background(), requestID, status, store.Usage{})
		writeJSON(w, httpStatus, map[string]string{"error": code})
		return
	}
	defer ticket.Release(context.Background())
	w.Header().Set("x-daiki-model-alias", route.Alias)
	w.Header().Set("x-daiki-workload", string(route.Workload))
	w.Header().Set("x-daiki-queue-wait-ms", fmt.Sprint(ticket.AcquiredAt.Sub(ticket.EnqueuedAt).Milliseconds()))
	req, err := http.NewRequestWithContext(r.Context(), r.Method, strings.TrimRight(a.cfg.LiteLLMBase, "/")+path, strings.NewReader(string(upstreamBody)))
	if err != nil {
		a.releaseReservation(r.Context(), requestID, decision, reserved)
		_ = a.store.FinishUsage(r.Context(), requestID, "failed", store.Usage{})
		writeJSON(w, 500, map[string]string{"error": "request construction failed"})
		return
	}
	req.Header.Set("content-type", "application/json")
	if a.cfg.LiteLLMKey != "" {
		req.Header.Set("authorization", "Bearer "+a.cfg.LiteLLMKey)
	}
	resp, err := a.http.Do(req)
	if err != nil {
		a.releaseReservation(r.Context(), requestID, decision, reserved)
		status := "failed"
		if r.Context().Err() != nil {
			status = "cancelled"
		}
		_ = a.store.FinishUsage(context.Background(), requestID, status, store.Usage{})
		writeJSON(w, 502, map[string]string{"error": "LiteLLM unavailable"})
		return
	}
	defer func() { _ = resp.Body.Close() }()
	for _, h := range []string{"content-type", "cache-control"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.Header().Set("x-daiki-quota-mode", decision.Mode)
	if decision.Remaining != nil {
		w.Header().Set("x-daiki-quota-remaining", fmt.Sprint(*decision.Remaining))
	}
	if decision.ResetAt != nil {
		w.Header().Set("x-daiki-quota-reset", decision.ResetAt.Format(time.RFC3339))
	}

	if stream {
		w.Header().Set("x-accel-buffering", "no")
		w.WriteHeader(resp.StatusCode)
		_, copyErr := io.Copy(w, resp.Body)
		actual := reserved // bounded estimate until stream usage chunks are normalized.
		status := "completed"
		if copyErr != nil || r.Context().Err() != nil {
			status = "cancelled"
			actual = 0
		}
		_ = a.store.FinishUsage(context.Background(), requestID, status, store.Usage{TotalTokens: actual})
		a.releaseReservation(context.Background(), requestID, decision, reserved)
		return
	}

	responseBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if readErr != nil {
		a.releaseReservation(r.Context(), requestID, decision, reserved)
		_ = a.store.FinishUsage(r.Context(), requestID, "failed", store.Usage{})
		writeJSON(w, 502, map[string]string{"error": "invalid LiteLLM response"})
		return
	}
	usage := parseUsagePayload(responseBody)
	actual := usage.TotalTokens
	if actual == 0 && resp.StatusCode < 400 {
		actual = reserved
		usage.TotalTokens = actual
	}
	if resp.StatusCode >= 400 {
		usage = store.Usage{}
	}
	ledgerStatus := "completed"
	if resp.StatusCode >= 400 {
		ledgerStatus = "failed"
	}
	_ = a.store.FinishUsage(r.Context(), requestID, ledgerStatus, usage)
	a.releaseReservation(r.Context(), requestID, decision, reserved)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(responseBody)
}
func (a *app) chat(w http.ResponseWriter, r *http.Request) {
	a.proxyLiteLLM(w, r, "/v1/chat/completions", false)
}
func (a *app) chatStream(w http.ResponseWriter, r *http.Request) {
	a.proxyLiteLLM(w, r, "/v1/chat/completions", true)
}
func (a *app) models(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"object": "list", "data": a.router.Aliases()})
}
func (a *app) usage(w http.ResponseWriter, r *http.Request) {
	decision, policy, err := a.quotaFor(r)
	if err != nil {
		writeJSON(w, 503, map[string]string{"error": "usage unavailable"})
		return
	}
	start, _ := quotaWindow(policy, time.Now().UTC())
	if decision.Interval == "lifetime" {
		start = time.Unix(0, 0).UTC()
	}
	var u store.Usage
	if policy.ScopeType == "api_key" && currentPrincipal(r).APIKeyID != "" {
		u, err = a.store.UsageSummaryForAPIKey(r.Context(), currentPrincipal(r).APIKeyID, start)
	} else {
		u, err = a.store.UsageSummaryForUser(r.Context(), current(r).Sub, start)
	}
	if err != nil {
		writeJSON(w, 503, map[string]string{"error": "usage unavailable"})
		return
	}
	decision.Used = u.TotalTokens
	if decision.Limit != nil {
		rem := *decision.Limit - u.TotalTokens
		if rem < 0 {
			rem = 0
		}
		decision.Remaining = &rem
	}
	writeJSON(w, 200, map[string]any{"usage": u, "quota": decision, "source": "postgres-ledger+redis-reservations"})
}

func (a *app) kcAdminToken(ctx context.Context) (string, error) {
	form := url.Values{"grant_type": {"password"}, "client_id": {"admin-cli"}, "username": {a.cfg.KeycloakAdminUser}, "password": {a.cfg.KeycloakAdminPass}}
	u := strings.TrimRight(a.cfg.KeycloakBase, "/") + "/realms/master/protocol/openid-connect/token"
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	resp, err := a.http.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	var x struct {
		AccessToken string `json:"access_token"`
	}
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("keycloak admin token status %d", resp.StatusCode)
	}
	if err = json.NewDecoder(resp.Body).Decode(&x); err != nil {
		return "", err
	}
	return x.AccessToken, nil
}
func (a *app) kc(w http.ResponseWriter, r *http.Request, path string) {
	tok, err := a.kcAdminToken(r.Context())
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 2<<20))
	u := fmt.Sprintf("%s/admin/realms/%s%s", strings.TrimRight(a.cfg.KeycloakBase, "/"), a.cfg.KeycloakRealm, path)
	req, _ := http.NewRequestWithContext(r.Context(), r.Method, u, strings.NewReader(string(body)))
	req.Header.Set("authorization", "Bearer "+tok)
	req.Header.Set("content-type", "application/json")
	resp, err := a.http.Do(req)
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": "keycloak unavailable"})
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if ct := resp.Header.Get("content-type"); ct != "" {
		w.Header().Set("content-type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
func (a *app) createUser(w http.ResponseWriter, r *http.Request) { a.kc(w, r, "/users") }

func apiKeyHash(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
func newAPIKeySecret() (id, raw, prefix, hash string, err error) {
	buf := make([]byte, 32)
	if _, err = rand.Read(buf); err != nil {
		return
	}
	idbuf := make([]byte, 12)
	if _, err = rand.Read(idbuf); err != nil {
		return
	}
	raw = "dk_" + base64.RawURLEncoding.EncodeToString(buf)
	id = "key_" + base64.RawURLEncoding.EncodeToString(idbuf)
	prefix = raw
	if len(prefix) > 12 {
		prefix = prefix[:12]
	}
	hash = apiKeyHash(raw)
	return
}
func probe(ctx context.Context, c *http.Client, name, u string) map[string]any {
	if u == "" {
		return map[string]any{"name": name, "status": "not-configured"}
	}
	start := time.Now()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	resp, err := c.Do(req)
	if err != nil {
		return map[string]any{"name": name, "status": "down"}
	}
	_ = resp.Body.Close()
	s := "ready"
	if resp.StatusCode >= 500 {
		s = "down"
	}
	return map[string]any{"name": name, "status": s, "latencyMs": time.Since(start).Milliseconds()}
}
func (a *app) adminQueues(w http.ResponseWriter, r *http.Request) {
	if a.queue == nil {
		writeJSON(w, 503, map[string]string{"error": "queue unavailable"})
		return
	}
	snap, err := a.queue.Snapshot(r.Context())
	if err != nil {
		writeJSON(w, 503, map[string]string{"error": "queue unavailable"})
		return
	}
	writeJSON(w, 200, snap)
}
func (a *app) adminSummary(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	services := []map[string]any{probe(ctx, a.http, "Keycloak", strings.TrimRight(a.cfg.KeycloakBase, "/")+"/health/ready"), probe(ctx, a.http, "LiteLLM", strings.TrimRight(a.cfg.LiteLLMBase, "/")+"/health/liveliness"), probe(ctx, a.http, "Local LLM", strings.TrimRight(a.cfg.LocalLLMBase, "/")+"/models")}
	writeJSON(w, 200, map[string]any{"services": services, "localLlmRequired": false})
}
