package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/poom-ytthpch/daiki-ai-passport-backend/internal/store"
)

func TestRolesAndAdmin(t *testing.T) {
	var c claims
	c.RealmAccess.Roles = []string{"ai-user"}
	c.ResourceAccess = map[string]struct {
		Roles []string `json:"roles"`
	}{"daiki-web": {Roles: []string{"ai-admin"}}}
	if !hasRole(c, "daiki-web", "ai-admin") {
		t.Fatal("expected ai-admin role")
	}
	if hasRole(c, "daiki-web", "missing") {
		t.Fatal("unexpected role")
	}
}

func TestFirst(t *testing.T) {
	if got := first("", "Daiki", "fallback"); got != "Daiki" {
		t.Fatalf("got %q", got)
	}
}

func TestReservationTokensUsesRequestedCompletionBudget(t *testing.T) {
	got := reservationTokens([]byte(`{"messages":[{"role":"user","content":"hello"}],"max_tokens":512}`))
	if got < 512 {
		t.Fatalf("reservation %d must include output budget", got)
	}
}

func TestQuotaWindowDay(t *testing.T) {
	now := time.Date(2026, 9, 2, 10, 30, 0, 0, time.UTC)
	start, reset := quotaWindow(store.Policy{IntervalKind: "day"}, now)
	if start.Hour() != 0 || reset == nil || !reset.Equal(time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("unexpected window %v %v", start, reset)
	}
}

func TestAPIKeySecretIsHashedAndPrefixed(t *testing.T) {
	id, raw, prefix, hash, err := newAPIKeySecret()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(id, "key_") || !strings.HasPrefix(raw, "dk_") {
		t.Fatalf("unexpected key format id=%q raw-prefix=%q", id, raw[:3])
	}
	if prefix == raw || len(prefix) > 12 {
		t.Fatalf("prefix must be bounded and non-secret: %q", prefix)
	}
	if hash != apiKeyHash(raw) || strings.Contains(hash, raw) {
		t.Fatal("API key hash mismatch")
	}
}

func TestPrincipalScope(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/v1/chat", nil)
	ctx := context.WithValue(r.Context(), principalKey, principal{AuthKind: "api_key", Scopes: []string{"inference"}})
	if !hasPrincipalScope(r.WithContext(ctx), "inference") {
		t.Fatal("expected inference scope")
	}
	if hasPrincipalScope(r.WithContext(ctx), "admin") {
		t.Fatal("unexpected admin scope")
	}
}

func TestEnsureStreamUsageRequestsFinalUsageChunk(t *testing.T) {
	body := ensureStreamUsage([]byte(`{"model":"physical-fast","messages":[]}`))
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["stream"] != true {
		t.Fatalf("stream must be enabled: %#v", payload)
	}
	opts, ok := payload["stream_options"].(map[string]any)
	if !ok || opts["include_usage"] != true {
		t.Fatalf("stream usage must be requested: %#v", payload)
	}
}

func TestCopySSEWithUsagePreservesStreamAndExtractsTokens(t *testing.T) {
	input := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":3,\"total_tokens\":15}}\n\n" +
		"data: [DONE]\n\n"
	var out bytes.Buffer
	usage, err := copySSEWithUsage(&out, strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != input {
		t.Fatalf("stream was modified:\n%s", out.String())
	}
	if usage.InputTokens != 12 || usage.OutputTokens != 3 || usage.TotalTokens != 15 {
		t.Fatalf("unexpected usage: %#v", usage)
	}
}

func TestIdentityProviderAttribution(t *testing.T) {
	if got := identityProvider(claims{IdentityProvider: "Google"}); got != "google" {
		t.Fatalf("expected google provider, got %q", got)
	}
	if got := identityProvider(claims{}); got != "email" {
		t.Fatalf("expected native email provider, got %q", got)
	}
}

func TestConfiguredAdminEmail(t *testing.T) {
	a := &app{cfg: config{AdminEmail: "admin@example.com"}}
	if !a.isConfiguredAdmin(claims{Email: "Admin@Example.com", EmailVerified: true}) {
		t.Fatal("configured admin email should match case-insensitively")
	}
	if a.isConfiguredAdmin(claims{Email: "user@example.com", EmailVerified: true}) {
		t.Fatal("non-admin email must not be treated as configured admin")
	}
	if a.isConfiguredAdmin(claims{Email: "admin@example.com", EmailVerified: false}) {
		t.Fatal("unverified email must never receive configured-admin privileges")
	}
}

func TestAdminOnlyAcceptsConfiguredAdminEmail(t *testing.T) {
	a := &app{cfg: config{AdminEmail: "admin@example.com", ClientID: "daiki-web"}}
	r := httptest.NewRequest(http.MethodGet, "/v1/admin/summary", nil)
	var c claims
	c.Email = "admin@example.com"
	c.EmailVerified = true
	ctx := context.WithValue(r.Context(), claimsKey, c)
	ctx = context.WithValue(ctx, appUserKey, store.User{Status: "approved"})
	ctx = context.WithValue(ctx, principalKey, principal{AuthKind: "oidc"})
	w := httptest.NewRecorder()
	a.adminOnly(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })).ServeHTTP(w, r.WithContext(ctx))
	if w.Code != http.StatusNoContent {
		t.Fatalf("configured admin should pass adminOnly, got %d", w.Code)
	}
}

func TestPasswordResetHidesInvalidEmail(t *testing.T) {
	a := &app{}
	r := httptest.NewRequest(http.MethodPost, "/v1/auth/password-reset", strings.NewReader(`{"email":"not-an-email"}`))
	r.Header.Set("content-type", "application/json")
	w := httptest.NewRecorder()
	a.passwordReset(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("expected generic 200 response, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "If an account exists") {
		t.Fatalf("expected enumeration-safe generic response: %s", w.Body.String())
	}
}

func TestGmailRefreshTokenEncryptionRoundTrip(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	a := &app{cfg: config{TokenEncryptionKey: base64.StdEncoding.EncodeToString(key)}}
	enc, err := a.encryptSecret("refresh-token-secret")
	if err != nil {
		t.Fatal(err)
	}
	if enc == "refresh-token-secret" || strings.Contains(enc, "refresh-token-secret") {
		t.Fatal("encrypted token leaked plaintext")
	}
	got, err := a.decryptSecret(enc)
	if err != nil {
		t.Fatal(err)
	}
	if got != "refresh-token-secret" {
		t.Fatalf("got %q", got)
	}
}

func TestGmailRefreshTokenEncryptionRejectsWeakKey(t *testing.T) {
	a := &app{cfg: config{TokenEncryptionKey: base64.StdEncoding.EncodeToString([]byte("short"))}}
	if _, err := a.encryptSecret("secret"); err == nil {
		t.Fatal("expected invalid key error")
	}
}

func TestTokenForClient(t *testing.T) {
	if !tokenForClient(claims{AuthorizedParty: "daiki-web"}, "daiki-web") {
		t.Fatal("expected matching authorized party")
	}
	if !tokenForClient(claims{Audience: []any{"account", "daiki-web"}}, "daiki-web") {
		t.Fatal("expected matching audience")
	}
	if tokenForClient(claims{AuthorizedParty: "other", Audience: []any{"account"}}, "daiki-web") {
		t.Fatal("unexpected client match")
	}
}

func TestNormalizeProviderBase(t *testing.T) {
	cases := []struct {
		typ, raw, want string
		ok             bool
	}{
		{"vllm", "http://10.90.0.11:8000", "http://10.90.0.11:8000", true},
		{"lmstudio", "http://10.90.0.12:1234/v1/", "http://10.90.0.12:1234/v1", true},
		{"ollama", "http://10.90.0.13:11434/v1", "http://10.90.0.13:11434", true},
		{"vllm", "http://127.0.0.1:8000", "", false},
		{"vllm", "http://169.254.169.254/latest", "", false},
		{"vllm", "ftp://10.0.0.2", "", false},
	}
	for _, tc := range cases {
		got, err := normalizeProviderBase(tc.typ, tc.raw)
		if tc.ok && (err != nil || got != tc.want) {
			t.Fatalf("normalize %q got=%q err=%v want=%q", tc.raw, got, err, tc.want)
		}
		if !tc.ok && err == nil {
			t.Fatalf("expected %q to be rejected, got %q", tc.raw, got)
		}
	}
}

func TestProviderScopedSecretIsolation(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	a := &app{cfg: config{TokenEncryptionKey: base64.StdEncoding.EncodeToString(key)}}
	enc, err := a.encryptScopedSecret("model-provider:prov_a:api-key", "provider-secret")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := a.decryptScopedSecret("model-provider:prov_a:api-key", enc); err != nil || got != "provider-secret" {
		t.Fatalf("round trip failed got=%q err=%v", got, err)
	}
	if _, err := a.decryptScopedSecret("model-provider:prov_b:api-key", enc); err == nil {
		t.Fatal("secret must not decrypt under another provider scope")
	}
}

func TestProviderLiteLLMMapping(t *testing.T) {
	v := store.ModelProvider{ProviderType: "vllm", BaseURL: "http://10.90.0.11:8000"}
	if got := providerLiteLLMBase(v); got != "http://10.90.0.11:8000/v1" {
		t.Fatalf("unexpected vllm base %q", got)
	}
	if got := providerLiteLLMModel(v, "Qwen/Qwen3"); got != "openai/Qwen/Qwen3" {
		t.Fatalf("unexpected vllm model %q", got)
	}
	o := store.ModelProvider{ProviderType: "ollama", BaseURL: "http://10.90.0.13:11434"}
	if got := providerLiteLLMModel(o, "qwen3:8b"); got != "ollama/qwen3:8b" {
		t.Fatalf("unexpected ollama model %q", got)
	}
}

func TestProviderIPAllowed(t *testing.T) {
	allowed := []string{"10.90.0.11", "192.168.1.20", "8.8.8.8"}
	for _, raw := range allowed {
		if !providerIPAllowed(net.ParseIP(raw)) {
			t.Fatalf("expected %s to be allowed", raw)
		}
	}
	blocked := []string{"127.0.0.1", "0.0.0.0", "169.254.169.254", "224.0.0.1"}
	for _, raw := range blocked {
		if providerIPAllowed(net.ParseIP(raw)) {
			t.Fatalf("expected %s to be blocked", raw)
		}
	}
}
