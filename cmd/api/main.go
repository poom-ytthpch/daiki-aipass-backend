package main

import (
	"bufio"
	"bytes"
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
	HermesBase, HermesKey                 string
	HermesEnabled                         bool
	HermesFallbackToLiteLLM               bool
	DatabaseURL, RedisAddr                string
	AllowedOrigins                        []string
	AdminEmail                            string
	AppBaseURL                            string
	GoogleClientID, GoogleClientSecret    string
	GmailOAuthRedirectURI                 string
	TokenEncryptionKey                    string
	LocalLLMBase                          string
	PendingChatTokenLimit                 int64
	PendingChatRequestsPerHour            int
	PendingChatMinIntervalSeconds         int
	PendingChatMaxCompletionTokens        int
	SearXNGBase                           string
	WebResearchMaxResults                 int
	WebResearchFetchPages                 int
}

type app struct {
	cfg           config
	verifier      *oidc.IDTokenVerifier
	http          *http.Client
	inferenceHTTP *http.Client
	redis         *redis.Client
	db            *pgxpool.Pool
	store         *store.Store
	router        *inference.Router
	queue         *inference.Queue
}

type claims struct {
	Sub               string `json:"sub"`
	AuthorizedParty   string `json:"azp"`
	Audience          any    `json:"aud"`
	Name              string `json:"name"`
	Email             string `json:"email"`
	EmailVerified     bool   `json:"email_verified"`
	PreferredUsername string `json:"preferred_username"`
	IdentityProvider  string `json:"identity_provider"`
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
		HermesBase: strings.TrimRight(strings.TrimSpace(os.Getenv("HERMES_BASE_URL")), "/"), HermesKey: strings.TrimSpace(os.Getenv("HERMES_API_KEY")), HermesEnabled: strings.EqualFold(getenv("HERMES_ENABLED", "false"), "true"), HermesFallbackToLiteLLM: strings.EqualFold(getenv("HERMES_FALLBACK_TO_LITELLM", "true"), "true"),
		DatabaseURL: os.Getenv("DATABASE_URL"), RedisAddr: getenv("REDIS_ADDR", "daiki-redis.daiki-ai-passport.svc.cluster.local:6379"),
		AllowedOrigins: splitCSV(getenv("ALLOWED_ORIGINS", "https://ai.infra.local")), AdminEmail: strings.ToLower(strings.TrimSpace(os.Getenv("ADMIN_EMAIL"))), AppBaseURL: strings.TrimRight(getenv("APP_BASE_URL", "https://daiki-aipass.matchchemical.co"), "/"), GoogleClientID: strings.TrimSpace(os.Getenv("GOOGLE_CLIENT_ID")), GoogleClientSecret: strings.TrimSpace(os.Getenv("GOOGLE_CLIENT_SECRET")), GmailOAuthRedirectURI: getenv("GMAIL_OAUTH_REDIRECT_URI", "https://daiki-aipass.matchchemical.co/api/admin/integrations/gmail/callback"), TokenEncryptionKey: strings.TrimSpace(os.Getenv("TOKEN_ENCRYPTION_KEY")), LocalLLMBase: getenv("LOCAL_LLM_BASE_URL", "http://10.90.0.11:8000/v1"),
		PendingChatTokenLimit: int64(getenvInt("PENDING_CHAT_TOKEN_LIMIT", 8000)), PendingChatRequestsPerHour: getenvInt("PENDING_CHAT_REQUESTS_PER_HOUR", 10),
		PendingChatMinIntervalSeconds: getenvInt("PENDING_CHAT_MIN_INTERVAL_SECONDS", 30), PendingChatMaxCompletionTokens: getenvInt("PENDING_CHAT_MAX_COMPLETION_TOKENS", 512),
		SearXNGBase: getenv("SEARXNG_BASE_URL", "http://searxng:8080"), WebResearchMaxResults: getenvInt("WEB_RESEARCH_MAX_RESULTS", 5), WebResearchFetchPages: getenvInt("WEB_RESEARCH_FETCH_PAGES", 3),
	}
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	cfg := loadConfig()
	keySet := oidc.NewRemoteKeySet(context.Background(), cfg.JWKSURL)
	a := &app{cfg: cfg, verifier: oidc.NewVerifier(cfg.PublicIssuer, keySet, &oidc.Config{SkipClientIDCheck: true}), http: &http.Client{Timeout: 15 * time.Second}, inferenceHTTP: &http.Client{Timeout: time.Duration(getenvInt("INFERENCE_HTTP_TIMEOUT_SECONDS", 300)) * time.Second}, router: inference.NewRouter(getenv("MODEL_FAST", "qwen-local"), getenv("MODEL_BALANCED", "qwen-local"), getenv("MODEL_DEEP", "qwen-local"), getenv("MODEL_VISION", "qwen-local"))}
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
			if err := a.store.RecoverInterruptedChatRuns(ctx); err != nil {
				slog.Warn("chat run recovery failed", "error", err)
			}
			go func() {
				timer := time.NewTimer(2 * time.Second)
				defer timer.Stop()
				select {
				case <-ctx.Done():
					return
				case <-timer.C:
				}
				reconcileCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
				defer cancel()
				a.reconcileProviderModels(reconcileCtx)
			}()
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
		r.Post("/auth/password-reset", a.passwordReset)
		r.Post("/auth/password-reset/confirm", a.passwordResetConfirm)
		r.Get("/guest/policy", a.guestPolicyPublic)
		r.Get("/guest/capabilities", a.guestCapabilities)
		r.Post("/guest/chat", a.guestChat)
		r.Post("/guest/chat/stream", a.guestChatStream)
		r.Get("/guest/attachments", a.guestListAttachments)
		r.Post("/guest/attachments", a.guestUploadAttachment)
		r.Get("/guest/attachments/{id}", a.guestDownloadAttachment)
		r.Delete("/guest/attachments/{id}", a.guestDeleteAttachment)
		r.Post("/guest/generate/file", a.guestGenerateFile)
		r.Post("/guest/generate/image", a.guestGenerateImage)
		r.Group(func(r chi.Router) {
			r.Use(a.auth)
			r.Get("/me", a.me)
			r.Get("/capabilities", a.smartCapabilities)
			r.Get("/attachments", a.listAttachments)
			r.Post("/attachments", a.uploadAttachment)
			r.Get("/attachments/{id}", a.downloadAttachment)
			r.Delete("/attachments/{id}", a.deleteAttachment)
			r.Get("/chat-sessions", a.listChatSessions)
			r.Post("/chat-sessions", a.createChatSession)
			r.Get("/chat-sessions/{id}", a.getChatSession)
			r.Patch("/chat-sessions/{id}", a.updateChatSession)
			r.Delete("/chat-sessions/{id}", a.deleteChatSession)
			r.Post("/chat-sessions/{id}/messages", a.addChatMessage)
			r.Patch("/chat-sessions/{id}/messages/{messageID}", a.editChatMessage)
			r.Get("/chat-sessions/{id}/runs/latest", a.latestChatRun)
			r.Get("/chat-runs/{runID}", a.getChatRun)
			r.Patch("/chat-runs/{runID}", a.controlChatRun)
			r.Route("/admin", func(r chi.Router) {
				r.Use(a.adminOnly)
				r.Get("/summary", a.adminSummary)
				r.Get("/model-providers", a.adminModelProviders)
				r.Post("/model-providers", a.adminSaveModelProvider)
				r.Put("/model-providers/{id}", a.adminSaveModelProvider)
				r.Delete("/model-providers/{id}", a.adminDeleteModelProvider)
				r.Post("/model-providers/{id}/test", a.adminTestModelProvider)
				r.Get("/model-providers/{id}/discover", a.adminDiscoverProviderModels)
				r.Post("/model-providers/{id}/models", a.adminRegisterProviderModel)
				r.Put("/provider-models/{id}", a.adminUpdateProviderModel)
				r.Delete("/provider-models/{id}", a.adminDeleteProviderModel)
				r.Get("/model-aliases", a.adminModelAliases)
				r.Put("/model-aliases/{alias}", a.adminSetModelAlias)
				r.Get("/integrations/gmail", a.gmailIntegrationStatus)
				r.Post("/integrations/gmail/authorize", a.gmailAuthorize)
				r.Post("/integrations/gmail/callback", a.gmailCallback)
				r.Post("/integrations/gmail/test", a.gmailTest)
				r.Delete("/integrations/gmail", a.gmailDisconnect)
				r.Get("/usage", a.adminUsage)
				r.Get("/queues", a.adminQueues)
				r.Get("/users", a.adminUsers)
				r.Get("/users/{subject}", a.adminUserDetail)
				r.Post("/users", a.createUser)
				r.Patch("/users/{subject}/status", a.adminUserStatus)
				r.Put("/users/{subject}/roles", a.adminUserRoles)
				r.Get("/users/{subject}/quota", a.adminUserQuota)
				r.Put("/users/{subject}/quota", a.adminUserQuota)
				r.Post("/users/{subject}/quota-reset", a.adminResetUserQuota)
				r.Post("/users/{subject}/quota-reset-grants", a.adminGrantUserQuotaResets)
				r.Get("/api-keys", a.adminAPIKeys)
				r.Post("/api-keys", a.createAPIKey)
				r.Delete("/api-keys/{id}", a.revokeAPIKey)
				r.Get("/api-keys/{id}/quota", a.adminAPIKeyQuota)
				r.Put("/api-keys/{id}/quota", a.adminAPIKeyQuota)
				r.Get("/token-policy", a.adminSystemQuota)
				r.Put("/token-policy", a.adminSystemQuota)
				r.Get("/guest-policy", a.adminGuestPolicy)
				r.Put("/guest-policy", a.adminGuestPolicy)
				r.Get("/guests/{guestSubject}", a.adminGuestDetail)
				r.Post("/guests/{guestSubject}/quota-reset", a.adminResetGuestQuota)
				r.Get("/audit", a.adminAudit)
			})
			r.Group(func(r chi.Router) {
				r.Use(a.chatAccess)
				r.Use(requirePrincipalScope("inference"))
				r.Get("/usage", a.usage)
				r.Get("/quota-resets", a.quotaResets)
				r.Post("/quota-resets/use", a.useQuotaReset)
				r.Post("/chat", a.chat)
				r.Post("/chat/stream", a.chatStream)
				r.Post("/chat-sessions/{id}/runs", a.startChatRun)
			})
			r.Group(func(r chi.Router) {
				r.Use(a.approvalRequired)
				r.Use(requirePrincipalScope("inference"))
				r.Get("/models", a.models)
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
			ctx = context.WithValue(ctx, principalKey, principal{AuthKind: "api_key", APIKeyID: key.ID, Scopes: append([]string{}, key.Scopes...)})
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
		if !tokenForClient(c, a.cfg.ClientID) {
			writeJSON(w, 401, map[string]string{"error": "token client mismatch"})
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
		if a.isConfiguredAdmin(c) && u.Status != "approved" {
			if approved, approveErr := a.store.SetUserStatus(r.Context(), "system:admin-email", c.Sub, "approved"); approveErr == nil {
				u = approved
			} else {
				slog.Warn("configured admin auto-approval failed", "error", approveErr)
			}
		}
		ctx := context.WithValue(r.Context(), claimsKey, c)
		ctx = context.WithValue(ctx, appUserKey, u)
		ctx = context.WithValue(ctx, principalKey, principal{AuthKind: "oidc"})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
func tokenForClient(c claims, client string) bool {
	if client == "" {
		return false
	}
	if c.AuthorizedParty == client {
		return true
	}
	switch aud := c.Audience.(type) {
	case string:
		return aud == client
	case []any:
		for _, v := range aud {
			if s, ok := v.(string); ok && s == client {
				return true
			}
		}
	}
	return false
}
func current(r *http.Request) claims { c, _ := r.Context().Value(claimsKey).(claims); return c }
func roles(c claims, client string) []string {
	out := append([]string{}, c.RealmAccess.Roles...)
	if x, ok := c.ResourceAccess[client]; ok {
		out = append(out, x.Roles...)
	}
	return out
}
func (a *app) isConfiguredAdmin(c claims) bool {
	return a.cfg.AdminEmail != "" && c.EmailVerified && strings.EqualFold(strings.TrimSpace(c.Email), a.cfg.AdminEmail)
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
		if !hasRole(current(r), a.cfg.ClientID, "ai-admin", "admin") && !a.isConfiguredAdmin(current(r)) {
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
	if a.redis == nil {
		services["redis"] = "not-configured"
		status = 503
	} else if err := a.redis.Ping(r.Context()).Err(); err != nil {
		services["redis"] = "down"
		status = 503
	} else {
		services["redis"] = "ok"
	}
	if a.db == nil {
		services["postgres"] = "not-configured"
		status = 503
	} else if err := a.db.Ping(r.Context()); err != nil {
		services["postgres"] = "down"
		status = 503
	} else {
		services["postgres"] = "ok"
	}
	services["local_llm"] = "optional"
	writeJSON(w, status, map[string]any{"status": map[bool]string{true: "ready", false: "degraded"}[status == 200], "services": services})
}
func (a *app) me(w http.ResponseWriter, r *http.Request) {
	c := current(r)
	u, _ := currentUser(r)
	out := map[string]any{"sub": c.Sub, "name": first(c.Name, c.PreferredUsername, c.Email, c.Sub), "email": c.Email, "roles": roles(c, a.cfg.ClientID), "status": u.Status, "authProvider": u.AuthProvider, "createdAt": u.CreatedAt, "approvedAt": u.ApprovedAt, "authKind": currentPrincipal(r).AuthKind, "apiKeyId": currentPrincipal(r).APIKeyID, "isAdmin": a.isConfiguredAdmin(c) || hasRole(c, a.cfg.ClientID, "ai-admin", "admin")}
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
	responseLanguage := responseLanguagePreference{}
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid body"})
		return
	}
	body, commandSelection, err := applyChatCommands(body, false)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	body, researchMeta, researchErr := a.enrichChatWithResearch(r.Context(), body)
	if researchErr == nil {
		body, responseLanguage, err = applyResponseLanguage(body)
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": "unable to apply response language"})
			return
		}
	} else {
		responseLanguage = responseLanguagePreference{}
	}
	chatRunID := strings.TrimSpace(r.Header.Get("x-daiki-chat-run-id"))
	if chatRunID != "" && a.store != nil {
		_ = a.store.UpdateChatRunActivity(context.Background(), chatRunID, map[string]any{
			"phase":    "thinking",
			"research": safeRunResearchActivity(researchMeta),
		})
	}
	if researchErr != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": researchErr.Error(), "research": researchMeta})
		return
	}
	body, thinkingProfile, err := applyThinkingMode(body)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "unable to apply thinking mode"})
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
	body, attachments, err := a.expandChatAttachments(r, body)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	body, commandSelection, err = applyAutomaticAttachmentSkills(body, attachments, commandSelection, false)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "unable to apply attachment skills"})
		return
	}
	skills := selectSmartSkills(body)
	// Hermes owns agent skills/tooling. Keep the legacy small-model instruction only
	// for direct-LiteLLM compatibility mode; injecting it into Hermes both wastes
	// tokens and incorrectly tells 20B/27B models that they are a 4B model.
	if !a.cfg.HermesEnabled {
		body, err = applySmartSkills(body, skills)
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": "unable to prepare smart skills"})
			return
		}
	}
	tokenEstimate := estimateTokens(body, thinkingProfile)
	route, upstreamBody, err := a.router.RouteChat(body)
	if err == nil && a.store != nil && route.ResolvedAlias != "" {
		if alias, aliasErr := a.store.ModelAlias(r.Context(), route.ResolvedAlias); aliasErr == nil && alias.LiteLLMModelName != "" {
			var dynamicPayload map[string]any
			if json.Unmarshal(upstreamBody, &dynamicPayload) == nil {
				dynamicPayload["model"] = alias.LiteLLMModelName
				if rewritten, marshalErr := json.Marshal(dynamicPayload); marshalErr == nil {
					upstreamBody = rewritten
					route.PhysicalModel = alias.LiteLLMModelName
				}
			}
		}
	}
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
	w.Header().Set("x-daiki-request-id", requestID)
	if chatRunID != "" && a.store != nil {
		_ = a.store.UpdateChatRunActivity(context.Background(), chatRunID, map[string]any{"requestId": requestID})
	}
	c := current(r)
	if err := a.reserveQuota(r.Context(), requestID, decision, reserved); err != nil {
		status := http.StatusServiceUnavailable
		code := "quota_service_unavailable"
		payload := map[string]any{"error": code, "quota": decision}
		if strings.Contains(err.Error(), "exhausted") {
			status = http.StatusTooManyRequests
			code = "quota_exhausted"
			payload["error"] = code
			if seconds := quotaRetryAfterSeconds(decision); seconds > 0 {
				w.Header().Set("Retry-After", strconv.Itoa(seconds))
				payload["retryAfterSeconds"] = seconds
			}
			if a.redis != nil {
				ttl := time.Hour
				if decision.ResetAt != nil {
					if until := time.Until(*decision.ResetAt); until > 0 {
						ttl = until
					}
				}
				_ = a.redis.Set(r.Context(), "quota:blocked:"+decision.CounterKey, "1", ttl).Err()
			}
		}
		writeJSON(w, status, payload)
		return
	}
	if a.redis != nil {
		_ = a.redis.Del(r.Context(), "quota:blocked:"+decision.CounterKey).Err()
	}
	if err := a.store.StartUsageForPrincipal(r.Context(), requestID, c.Sub, currentPrincipal(r).APIKeyID, route.Alias, string(route.Workload), reserved); err != nil {
		a.releaseReservation(r.Context(), requestID, decision, reserved)
		writeJSON(w, 503, map[string]string{"error": "usage ledger unavailable"})
		return
	}
	requestMeta := usageRequestMetadata(body, currentPrincipal(r).AuthKind, currentPrincipal(r).APIKeyID)
	skillIDs := make([]string, 0, len(skills))
	for _, skill := range skills {
		skillIDs = append(skillIDs, skill.ID)
	}
	requestMeta["skills"] = skillIDs
	if len(attachments) > 0 {
		requestMeta["attachments"] = attachments
	}
	requestMeta["resolvedAlias"] = route.ResolvedAlias
	requestMeta["physicalModel"] = route.PhysicalModel
	requestMeta["workload"] = string(route.Workload)
	requestMeta["research"] = researchMeta
	requestMeta["commands"] = commandSelection
	if responseLanguage.Code != "" {
		requestMeta["responseLanguage"] = responseLanguage
	}
	requestMeta["thinking"] = map[string]any{"mode": thinkingProfile.Mode, "reasoningBudget": thinkingProfile.ReasoningBudget, "maxCompletionTokens": thinkingProfile.MaxCompletionTokens, "estimate": tokenEstimate}
	_ = a.store.MergeUsageMetadata(r.Context(), requestID, requestMeta)
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
	toolUsage := store.Usage{}
	toolNames := []string{}
	if !a.cfg.HermesEnabled && !researchMeta.Used && shouldEnableSmartTools(body) {
		plannedBody, usedTools, plannerUsage, planErr := a.runSmartToolLoop(r.Context(), c.Sub, upstreamBody)
		toolUsage = plannerUsage
		toolNames = usedTools
		if plannerUsage.TotalTokens > 0 || len(toolNames) > 0 {
			_ = a.store.MergeUsageMetadata(r.Context(), requestID, map[string]any{"tools": toolNames, "toolPlannerTokens": plannerUsage.TotalTokens})
		}
		if planErr != nil {
			slog.Warn("smart tool planning skipped", "request_id", requestID, "error", planErr)
		} else {
			upstreamBody = plannedBody
		}
	}
	if stream {
		upstreamBody = ensureStreamUsage(upstreamBody)
	}
	if researchMeta.Used {
		w.Header().Set("x-daiki-research-used", "true")
		w.Header().Set("x-daiki-research-sources", strconv.Itoa(len(researchMeta.Sources)))
		w.Header().Set("x-daiki-research-mode", researchMeta.Mode)
	} else {
		w.Header().Set("x-daiki-research-used", "false")
		w.Header().Set("x-daiki-research-sources", "0")
		w.Header().Set("x-daiki-research-mode", researchMeta.Mode)
	}
	w.Header().Set("x-daiki-model-alias", route.Alias)
	if commandSelection.Mode != "" {
		w.Header().Set("x-daiki-command-mode", commandSelection.Mode)
	}
	if len(commandSelection.Skills) > 0 {
		w.Header().Set("x-daiki-command-skills", strings.Join(commandSelection.Skills, ","))
	}
	if len(commandSelection.AutoSkills) > 0 {
		w.Header().Set("x-daiki-auto-skills", strings.Join(commandSelection.AutoSkills, ","))
	}
	w.Header().Set("x-daiki-skills", strings.Join(skillIDs, ","))
	if len(toolNames) > 0 {
		w.Header().Set("x-daiki-tools", strings.Join(toolNames, ","))
	}
	w.Header().Set("x-daiki-workload", string(route.Workload))
	if responseLanguage.Code != "" {
		w.Header().Set("x-daiki-response-language", responseLanguage.Code)
	}
	w.Header().Set("x-daiki-thinking-mode", thinkingProfile.Mode)
	w.Header().Set("x-daiki-token-estimate-input", fmt.Sprint(tokenEstimate.InputTokens))
	w.Header().Set("x-daiki-token-estimate-thinking", fmt.Sprint(tokenEstimate.ThinkingBudget))
	w.Header().Set("x-daiki-token-estimate-output", fmt.Sprint(tokenEstimate.VisibleBudget))
	w.Header().Set("x-daiki-token-estimate-total", fmt.Sprint(tokenEstimate.TotalBudget))
	w.Header().Set("x-daiki-queue-wait-ms", fmt.Sprint(ticket.AcquiredAt.Sub(ticket.EnqueuedAt).Milliseconds()))
	hermesProfile := chooseHermesProfile(upstreamBody, researchMeta, route, skills, commandSelection)
	upstreamURL, upstreamKey, upstreamName := a.authenticatedHermesUpstream(path, hermesProfile)
	w.Header().Set("x-daiki-hermes-profile", hermesProfile)
	makeUpstreamRequest := func(payload []byte) (*http.Request, error) {
		req, buildErr := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, strings.NewReader(string(payload)))
		if buildErr != nil {
			return nil, buildErr
		}
		req.Header.Set("content-type", "application/json")
		if upstreamKey != "" {
			req.Header.Set("authorization", "Bearer "+upstreamKey)
		}
		req.Header.Set("x-daiki-request-id", requestID)
		req.Header.Set("x-daiki-principal", currentPrincipal(r).AuthKind)
		if upstreamName == "hermes" {
			baseKey := hermesSessionKey(r)
			if baseKey != "" {
				baseKey += ":p:" + hermesProfile
			}
			applyHermesSessionScope(req, baseKey, payload)
		}
		return req, nil
	}
	w.Header().Set("x-daiki-inference-upstream", upstreamName)
	resp, recoveredBody, recoveredModel, recovery, err := a.doModelRequestWithRecovery(r.Context(), upstreamBody, route.PhysicalModel, hermesProfile, makeUpstreamRequest)
	upstreamBody = recoveredBody
	if recoveredModel != "" {
		route.PhysicalModel = recoveredModel
	}
	w.Header().Set("x-daiki-model-physical", route.PhysicalModel)
	w.Header().Set("x-daiki-retry-attempts", strconv.Itoa(max(0, recovery.Attempts-1)))
	w.Header().Set("x-daiki-admission-wait-ms", strconv.FormatInt(recovery.AdmissionWaitMS, 10))
	w.Header().Set("x-daiki-admission-tokens", strconv.Itoa(recovery.AdmissionTokens))
	if recovery.RequestedReasoningEffort != "" {
		w.Header().Set("x-daiki-reasoning-requested", recovery.RequestedReasoningEffort)
	}
	if recovery.EffectiveReasoningEffort != "" {
		w.Header().Set("x-daiki-reasoning-effective", recovery.EffectiveReasoningEffort)
	}
	w.Header().Set("x-daiki-reasoning-native", strconv.FormatBool(recovery.NativeReasoning))
	if recovery.ReasoningModel != "" {
		w.Header().Set("x-daiki-reasoning-model", recovery.ReasoningModel)
	}
	if recovery.AdmissionSpillover {
		w.Header().Set("x-daiki-admission-spillover", "true")
	}
	if recovery.ContextTrimmed {
		w.Header().Set("x-daiki-context-trimmed", "true")
	}
	if recovery.FallbackTo != "" {
		w.Header().Set("x-daiki-fallback-model", recovery.FallbackTo)
	}
	_ = a.store.MergeUsageMetadata(r.Context(), requestID, recoveryMetadata(recovery))
	statusCode := 0
	if resp != nil {
		statusCode = resp.StatusCode
	}
	if shouldFallbackFromHermes(upstreamName, a.cfg.HermesFallbackToLiteLLM, statusCode, err) {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		fallbackURL, fallbackKey, _ := a.liteLLMUpstream(path)
		fallbackReq, fallbackReqErr := http.NewRequestWithContext(r.Context(), r.Method, fallbackURL, strings.NewReader(string(upstreamBody)))
		if fallbackReqErr == nil {
			fallbackReq.Header.Set("content-type", "application/json")
			if fallbackKey != "" {
				fallbackReq.Header.Set("authorization", "Bearer "+fallbackKey)
			}
			fallbackReq.Header.Set("x-daiki-request-id", requestID)
			fallbackReq.Header.Set("x-daiki-principal", currentPrincipal(r).AuthKind)
			resp, err = a.inferenceHTTP.Do(fallbackReq)
			if err == nil {
				upstreamName = "litellm-fallback"
				w.Header().Set("x-daiki-inference-upstream", upstreamName)
				_ = a.store.MergeUsageMetadata(r.Context(), requestID, map[string]any{"inferenceUpstream": upstreamName, "hermesFallback": true})
			}
		}
	}
	if err != nil {
		a.releaseReservation(r.Context(), requestID, decision, reserved)
		status := "failed"
		if r.Context().Err() != nil {
			status = "cancelled"
		}
		_ = a.store.FinishUsage(context.Background(), requestID, status, toolUsage)
		writeJSON(w, 502, map[string]string{"error": upstreamName + " unavailable"})
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
		capture := &cappedBuffer{max: storedPayloadLimit}
		usage, copyErr := copySSEWithUsage(io.MultiWriter(w, capture), resp.Body)
		actual := usage.TotalTokens
		status := "completed"
		if copyErr != nil || r.Context().Err() != nil {
			status = "cancelled"
			usage = store.Usage{}
			slog.Warn("chat stream interrupted", "request_id", requestID, "copy_error", copyErr, "context_error", r.Context().Err(), "research_used", researchMeta.Used, "research_sources", len(researchMeta.Sources), "model", route.PhysicalModel)
		} else if resp.StatusCode >= 400 {
			status = "failed"
			usage = store.Usage{}
		} else if actual == 0 {
			// Compatibility fallback for upstreams that do not emit an OpenAI-style
			// final usage chunk even after stream_options.include_usage is requested.
			actual = reserved
			usage.TotalTokens = actual
		}
		usage = addUsage(usage, toolUsage)
		_ = a.store.MergeUsageMetadata(context.Background(), requestID, usageResponseMetadata(capture.Bytes()))
		_ = a.store.FinishUsage(context.Background(), requestID, status, usage)
		a.releaseReservation(context.Background(), requestID, decision, reserved)
		return
	}

	responseBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if readErr != nil {
		a.releaseReservation(r.Context(), requestID, decision, reserved)
		_ = a.store.FinishUsage(r.Context(), requestID, "failed", toolUsage)
		writeJSON(w, 502, map[string]string{"error": "invalid LiteLLM response"})
		return
	}
	usage := parseUsagePayload(responseBody)
	responseBody = sanitizeReasoningJSON(responseBody)
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
	usage = addUsage(usage, toolUsage)
	_ = a.store.MergeUsageMetadata(r.Context(), requestID, usageResponseMetadata(responseBody))
	_ = a.store.FinishUsage(r.Context(), requestID, ledgerStatus, usage)
	a.releaseReservation(r.Context(), requestID, decision, reserved)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(responseBody)
}
func ensureStreamUsage(body []byte) []byte {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return body
	}
	payload["stream"] = true
	streamOptions, _ := payload["stream_options"].(map[string]any)
	if streamOptions == nil {
		streamOptions = map[string]any{}
	}
	streamOptions["include_usage"] = true
	payload["stream_options"] = streamOptions
	out, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return out
}

func stripPrivateReasoning(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, value := range x {
			normalized := strings.ToLower(strings.ReplaceAll(k, "-", "_"))
			switch normalized {
			case "reasoning_content", "reasoning_text", "chain_of_thought":
				continue
			}
			out[k] = stripPrivateReasoning(value)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i := range x {
			out[i] = stripPrivateReasoning(x[i])
		}
		return out
	default:
		return v
	}
}

func sanitizeReasoningJSON(data []byte) []byte {
	if !bytes.Contains(data, []byte("reasoning_content")) && !bytes.Contains(data, []byte("reasoning_text")) && !bytes.Contains(data, []byte("chain_of_thought")) {
		return data
	}
	var payload any
	if json.Unmarshal(data, &payload) != nil {
		return data
	}
	out, err := json.Marshal(stripPrivateReasoning(payload))
	if err != nil {
		return data
	}
	return out
}

func copySSEWithUsage(dst io.Writer, src io.Reader) (store.Usage, error) {
	reader := bufio.NewReaderSize(src, 64<<10)
	var usage store.Usage
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			outLine := line
			trimmed := strings.TrimSpace(string(line))
			if strings.HasPrefix(trimmed, "data:") {
				data := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
				if data != "" && data != "[DONE]" {
					parsed := parseUsagePayload([]byte(data))
					if parsed.TotalTokens > 0 || parsed.InputTokens > 0 || parsed.OutputTokens > 0 {
						usage = parsed
					}
					sanitized := sanitizeReasoningJSON([]byte(data))
					if !bytes.Equal(sanitized, []byte(data)) {
						ending := "\n"
						if bytes.HasSuffix(line, []byte("\r\n")) {
							ending = "\r\n"
						}
						outLine = []byte("data: " + string(sanitized) + ending)
					}
				}
			}
			if _, writeErr := dst.Write(outLine); writeErr != nil {
				return usage, writeErr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return usage, nil
			}
			return usage, err
		}
	}
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
	if decision.WindowStart != nil {
		start = *decision.WindowStart
	} else if decision.Interval == "lifetime" {
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
func (a *app) keycloakJSON(ctx context.Context, method, path string, body any) (*http.Response, error) {
	tok, err := a.kcAdminToken(ctx)
	if err != nil {
		return nil, err
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = strings.NewReader(string(encoded))
	}
	u := fmt.Sprintf("%s/admin/realms/%s%s", strings.TrimRight(a.cfg.KeycloakBase, "/"), a.cfg.KeycloakRealm, path)
	req, err := http.NewRequestWithContext(ctx, method, u, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("authorization", "Bearer "+tok)
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	return a.http.Do(req)
}

func (a *app) syncKeycloakUsers(ctx context.Context) error {
	if a.store == nil {
		return errors.New("identity store unavailable")
	}
	resp, err := a.keycloakJSON(ctx, http.MethodGet, "/users?max=500", nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("keycloak user listing status %d", resp.StatusCode)
	}
	var users []struct {
		ID        string `json:"id"`
		Username  string `json:"username"`
		Email     string `json:"email"`
		FirstName string `json:"firstName"`
		LastName  string `json:"lastName"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&users); err != nil {
		return err
	}
	for _, u := range users {
		if strings.TrimSpace(u.ID) == "" {
			continue
		}
		email := strings.TrimSpace(first(u.Email, u.Username))
		name := strings.TrimSpace(strings.TrimSpace(u.FirstName) + " " + strings.TrimSpace(u.LastName))
		if name == "" {
			name = first(u.Username, email, u.ID)
		}
		if _, err := a.store.UpsertIdentity(ctx, u.ID, email, name, "keycloak"); err != nil {
			return err
		}
	}
	return nil
}
func (a *app) setKeycloakRealmRoles(ctx context.Context, userID string, desired []string) error {
	managed := map[string]bool{"ai-user": true, "ai-admin": true}
	roleByName := map[string]map[string]any{}
	for name := range managed {
		resp, err := a.keycloakJSON(ctx, http.MethodGet, "/roles/"+url.PathEscape(name), nil)
		if err != nil {
			return err
		}
		if resp.StatusCode >= 300 {
			_ = resp.Body.Close()
			return fmt.Errorf("keycloak role %s unavailable", name)
		}
		var rep map[string]any
		err = json.NewDecoder(resp.Body).Decode(&rep)
		_ = resp.Body.Close()
		if err != nil {
			return err
		}
		roleByName[name] = rep
	}
	currentResp, err := a.keycloakJSON(ctx, http.MethodGet, "/users/"+url.PathEscape(userID)+"/role-mappings/realm", nil)
	if err != nil {
		return err
	}
	var current []map[string]any
	if currentResp.StatusCode < 300 {
		_ = json.NewDecoder(currentResp.Body).Decode(&current)
	}
	_ = currentResp.Body.Close()
	var remove []map[string]any
	for _, rep := range current {
		if name, _ := rep["name"].(string); managed[name] {
			remove = append(remove, rep)
		}
	}
	if len(remove) > 0 {
		resp, err := a.keycloakJSON(ctx, http.MethodDelete, "/users/"+url.PathEscape(userID)+"/role-mappings/realm", remove)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode >= 300 {
			return fmt.Errorf("keycloak role removal status %d", resp.StatusCode)
		}
	}
	var add []map[string]any
	seen := map[string]bool{}
	for _, name := range desired {
		name = strings.TrimSpace(name)
		if !managed[name] || seen[name] {
			continue
		}
		seen[name] = true
		add = append(add, roleByName[name])
	}
	if len(add) > 0 {
		resp, err := a.keycloakJSON(ctx, http.MethodPost, "/users/"+url.PathEscape(userID)+"/role-mappings/realm", add)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode >= 300 {
			return fmt.Errorf("keycloak role assignment status %d", resp.StatusCode)
		}
	}
	return nil
}

func generatedPassword() (string, error) {
	buf := make([]byte, 18)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "Dk!" + base64.RawURLEncoding.EncodeToString(buf), nil
}

func (a *app) createUser(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email       string   `json:"email"`
		DisplayName string   `json:"displayName"`
		Password    string   `json:"password"`
		Status      string   `json:"status"`
		Roles       []string `json:"roles"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in) != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid body"})
		return
	}
	in.Email = strings.ToLower(strings.TrimSpace(in.Email))
	if in.Email == "" || !strings.Contains(in.Email, "@") {
		writeJSON(w, 400, map[string]string{"error": "valid email is required"})
		return
	}
	if strings.TrimSpace(in.DisplayName) == "" {
		in.DisplayName = strings.SplitN(in.Email, "@", 2)[0]
	}
	if in.Status == "" {
		in.Status = "pending"
	}
	if len(in.Roles) == 0 {
		in.Roles = []string{"ai-user"}
	}
	password := strings.TrimSpace(in.Password)
	generated := false
	if password == "" {
		var err error
		password, err = generatedPassword()
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": "temporary password generation failed"})
			return
		}
		generated = true
	}
	resp, err := a.keycloakJSON(r.Context(), http.MethodPost, "/users", map[string]any{"username": in.Email, "email": in.Email, "firstName": in.DisplayName, "enabled": true, "emailVerified": true})
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": "keycloak unavailable"})
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusConflict {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "user already exists"})
		return
	}
	if resp.StatusCode >= 300 {
		writeJSON(w, 502, map[string]any{"error": "keycloak user creation failed", "status": resp.StatusCode})
		return
	}
	location := resp.Header.Get("Location")
	userID := strings.TrimSpace(location[strings.LastIndex(location, "/")+1:])
	if userID == "" {
		writeJSON(w, 502, map[string]string{"error": "keycloak did not return user id"})
		return
	}
	resetResp, err := a.keycloakJSON(r.Context(), http.MethodPut, "/users/"+url.PathEscape(userID)+"/reset-password", map[string]any{"type": "password", "value": password, "temporary": generated})
	if err != nil || resetResp.StatusCode >= 300 {
		if resetResp != nil {
			_ = resetResp.Body.Close()
		}
		writeJSON(w, 502, map[string]string{"error": "keycloak password setup failed"})
		return
	}
	_ = resetResp.Body.Close()
	if err := a.setKeycloakRealmRoles(r.Context(), userID, in.Roles); err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	u, err := a.store.UpsertManagedUser(r.Context(), current(r).Sub, userID, in.Email, in.DisplayName, in.Roles, in.Status)
	if err != nil {
		writeJSON(w, 503, map[string]string{"error": "application user creation failed"})
		return
	}
	out := map[string]any{"user": u}
	if generated {
		out["temporaryPassword"] = password
	}
	writeJSON(w, http.StatusCreated, out)
}

func apiKeyHash(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
func newHermesSessionID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("api_%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
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
	} else if resp.StatusCode >= 400 {
		s = "degraded"
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
	services := []map[string]any{probe(ctx, a.http, "Keycloak", strings.TrimRight(a.cfg.KeycloakBase, "/")+"/realms/"+a.cfg.KeycloakRealm+"/.well-known/openid-configuration"), probe(ctx, a.http, "LiteLLM", strings.TrimRight(a.cfg.LiteLLMBase, "/")+"/health/liveliness"), probe(ctx, a.http, "Local LLM", strings.TrimRight(a.cfg.LocalLLMBase, "/")+"/models")}
	stats, err := a.store.AdminStats(ctx)
	if err != nil {
		writeJSON(w, 503, map[string]string{"error": "admin summary unavailable"})
		return
	}
	writeJSON(w, 200, map[string]any{"services": services, "localLlmRequired": false, "users": stats.Users, "pending": stats.Pending, "approved": stats.Approved, "suspended": stats.Suspended, "rejected": stats.Rejected, "totalTokens": stats.TotalTokens, "requests": stats.Requests})
}
func (a *app) adminUsage(w http.ResponseWriter, r *http.Request) {
	rows, err := a.store.AdminUserUsage(r.Context())
	if err != nil {
		writeJSON(w, 503, map[string]string{"error": "admin usage unavailable"})
		return
	}
	writeJSON(w, 200, rows)
}
