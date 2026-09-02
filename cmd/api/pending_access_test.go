package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/poom-ytthpch/daiki-ai-passport-backend/internal/store"
	"github.com/redis/go-redis/v9"
)

func pendingRequest(status, authKind string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat", nil)
	ctx := context.WithValue(r.Context(), appUserKey, store.User{Subject: "pending-user", Status: status})
	ctx = context.WithValue(ctx, principalKey, principal{AuthKind: authKind})
	ctx = context.WithValue(ctx, claimsKey, claims{Sub: "pending-user"})
	return r.WithContext(ctx)
}

func TestChatAccessPendingAllowsOnlyInteractiveOIDC(t *testing.T) {
	a := &app{}
	h := a.chatAccess(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, pendingRequest("pending", "oidc"))
	if w.Code != http.StatusNoContent {
		t.Fatalf("pending OIDC chat denied: %d", w.Code)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, pendingRequest("pending", "api_key"))
	if w.Code != http.StatusForbidden {
		t.Fatalf("pending API key chat must be denied: %d", w.Code)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, pendingRequest("suspended", "oidc"))
	if w.Code != http.StatusForbidden {
		t.Fatalf("suspended chat must be denied: %d", w.Code)
	}
}

func TestRestrictPendingChatForcesFastTextAndOutputCap(t *testing.T) {
	a := &app{cfg: config{PendingChatMaxCompletionTokens: 512}}
	body, err := a.restrictPendingChat([]byte(`{"model":"deep","max_tokens":9000,"messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["model"] != "fast" {
		t.Fatalf("pending route must be fast: %#v", payload)
	}
	if payload["max_completion_tokens"] != float64(512) {
		t.Fatalf("unexpected output cap: %#v", payload)
	}
	if _, exists := payload["max_tokens"]; exists {
		t.Fatal("max_tokens must be replaced by controlled output cap")
	}
	if _, err := a.restrictPendingChat([]byte(`{"model":"auto","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,x"}}]}]}`)); err == nil {
		t.Fatal("pending image input must be rejected")
	}
}

func TestPendingQuotaPolicyIsSmallDailyFastOnly(t *testing.T) {
	a := &app{cfg: config{PendingChatTokenLimit: 8000}}
	p := a.pendingQuotaPolicy()
	if p.QuotaMode != "limited" || p.TokenLimit == nil || *p.TokenLimit != 8000 || p.IntervalKind != "day" {
		t.Fatalf("unexpected pending quota: %#v", p)
	}
	if !policyAllowsModel(p, "fast") || policyAllowsModel(p, "deep") {
		t.Fatal("pending policy must allow fast only")
	}
}

func TestPendingChatRateLimit(t *testing.T) {
	addr := os.Getenv("DAIKI_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("DAIKI_TEST_REDIS_ADDR not set")
	}
	client := redis.NewClient(&redis.Options{Addr: addr, DB: 15})
	t.Cleanup(func() { _ = client.FlushDB(context.Background()).Err(); _ = client.Close() })
	a := &app{redis: client, cfg: config{PendingChatRequestsPerHour: 2, PendingChatMinIntervalSeconds: 1}}
	if retry, err := a.enforcePendingChatRate(context.Background(), "u1"); err != nil || retry != 0 {
		t.Fatalf("first request failed retry=%v err=%v", retry, err)
	}
	if retry, err := a.enforcePendingChatRate(context.Background(), "u1"); err == nil || retry <= 0 {
		t.Fatalf("cooldown should reject retry=%v err=%v", retry, err)
	}
	time.Sleep(1100 * time.Millisecond)
	if retry, err := a.enforcePendingChatRate(context.Background(), "u1"); err != nil || retry != 0 {
		t.Fatalf("second request failed retry=%v err=%v", retry, err)
	}
	time.Sleep(1100 * time.Millisecond)
	if retry, err := a.enforcePendingChatRate(context.Background(), "u1"); err == nil || retry < time.Minute {
		t.Fatalf("hourly rate should reject retry=%v err=%v", retry, err)
	}
}
