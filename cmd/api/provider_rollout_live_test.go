package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/poom-ytthpch/daiki-ai-passport-backend/internal/store"
)

type liveRolloutSpec struct {
	Route    string
	Upstream string
	Name     string
	Fallback string
}

func findLiveProviderByKind(t *testing.T, a *app, kind string) store.ModelProvider {
	t.Helper()
	providers, err := a.store.ModelProviders(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range providers {
		if p.Enabled && connectedProviderKind(p) == kind {
			return p
		}
	}
	t.Fatalf("enabled %s provider not found", kind)
	return store.ModelProvider{}
}

func liveRolloutModelID() (string, error) {
	tok, err := randomURLToken(9)
	if err != nil {
		return "", err
	}
	return "pmodel_" + tok, nil
}

func liteLLMModelID(result map[string]any) string {
	if v, ok := result["model_id"].(string); ok && v != "" {
		return v
	}
	if mi, ok := result["model_info"].(map[string]any); ok {
		if v, ok := mi["id"].(string); ok && v != "" {
			return v
		}
	}
	if v, ok := result["id"].(string); ok {
		return v
	}
	return ""
}

func ensureLiveProviderModel(t *testing.T, a *app, p store.ModelProvider, spec liveRolloutSpec) store.ProviderModel {
	t.Helper()
	models, err := a.store.ProviderModels(t.Context(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	var rec store.ProviderModel
	for _, m := range models {
		if m.UpstreamModel == spec.Upstream {
			rec = m
			break
		}
	}
	if rec.ID == "" {
		id, err := liveRolloutModelID()
		if err != nil {
			t.Fatal(err)
		}
		rec, err = a.store.UpsertProviderModel(t.Context(), store.ProviderModel{
			ID:               id,
			ProviderID:       p.ID,
			UpstreamModel:    spec.Upstream,
			LiteLLMModelName: spec.Name,
			Status:           "pending",
		})
		if err != nil {
			t.Fatal(err)
		}
	} else if rec.LiteLLMModelName != spec.Name {
		rec.LiteLLMModelName = spec.Name
		rec, err = a.store.UpsertProviderModel(t.Context(), rec)
		if err != nil {
			t.Fatal(err)
		}
	}

	params, err := a.providerLiteLLMParams(p, rec)
	if err != nil {
		t.Fatal(err)
	}
	if rec.LiteLLMModelID == "" {
		result, _, err := a.litellmAdmin(t.Context(), http.MethodPost, "/model/new", map[string]any{
			"model_name":     spec.Name,
			"litellm_params": params,
			"model_info":     providerLiteLLMModelInfo(rec, ""),
		})
		if err != nil {
			t.Fatal(err)
		}
		rec.LiteLLMModelID = liteLLMModelID(result)
		if rec.LiteLLMModelID == "" {
			t.Fatalf("LiteLLM did not return model id for %s: %#v", spec.Name, result)
		}
		if err := a.store.SetProviderModelState(t.Context(), rec.ID, "active", rec.LiteLLMModelID, ""); err != nil {
			t.Fatal(err)
		}
	} else {
		if _, _, err := a.litellmAdmin(t.Context(), http.MethodPost, "/model/update", map[string]any{
			"model_info":     providerLiteLLMModelInfo(rec, rec.LiteLLMModelID),
			"litellm_params": params,
		}); err != nil {
			t.Fatal(err)
		}
		if err := a.store.SetProviderModelState(t.Context(), rec.ID, "active", "", ""); err != nil {
			t.Fatal(err)
		}
	}

	rec.Status = "active"
	rec.TimeoutSeconds = 180
	rec.StreamTimeoutSeconds = 180
	rec.MaxRetries = 2
	rec.ProviderMaxRetries = 0
	rec.RetryBackoffMS = 750
	rec.ContextStrategy = "adaptive"
	rec.FallbackModelName = spec.Fallback
	rec, err = a.store.UpdateProviderModelRuntime(t.Context(), "live-provider-rollout", rec)
	if err != nil {
		t.Fatal(err)
	}
	params, err = a.providerLiteLLMParams(p, rec)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.litellmAdmin(t.Context(), http.MethodPost, "/model/update", map[string]any{
		"model_info":     providerLiteLLMModelInfo(rec, rec.LiteLLMModelID),
		"litellm_params": params,
	}); err != nil {
		t.Fatal(err)
	}
	return rec
}

func ensureLiveRuntimeFallback(t *testing.T, a *app, modelName, fallback string) store.ProviderModel {
	t.Helper()
	rec, err := a.store.ProviderModelByLiteLLMName(t.Context(), modelName)
	if err != nil {
		t.Fatalf("runtime model %s not found: %v", modelName, err)
	}
	rec.ContextStrategy = "adaptive"
	rec.FallbackModelName = fallback
	rec.MaxRetries = 2
	rec.ProviderMaxRetries = 0
	if rec.RetryBackoffMS <= 0 {
		rec.RetryBackoffMS = 750
	}
	rec, err = a.store.UpdateProviderModelRuntime(t.Context(), "live-provider-rollout", rec)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := a.store.ModelProvider(t.Context(), rec.ProviderID)
	if err != nil {
		t.Fatal(err)
	}
	params, err := a.providerLiteLLMParams(provider, rec)
	if err != nil {
		t.Fatal(err)
	}
	if rec.LiteLLMModelID != "" {
		if _, _, err := a.litellmAdmin(t.Context(), http.MethodPost, "/model/update", map[string]any{
			"model_info":     providerLiteLLMModelInfo(rec, rec.LiteLLMModelID),
			"litellm_params": params,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return rec
}

func liveLiteLLMRequestFactory(a *app) inferenceRequestFactory {
	return func(body []byte) (*http.Request, error) {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, strings.TrimRight(a.cfg.LiteLLMBase, "/")+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("authorization", "Bearer "+a.cfg.LiteLLMKey)
		req.Header.Set("content-type", "application/json")
		return req, nil
	}
}

func smokeLiveDaikiRecovery(t *testing.T, a *app, model, wantFinal string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"model": model,
		"messages": []map[string]any{
			{"role": "system", "content": thinkingInstruction(thinkingProfileFor("high"), "general")},
			{"role": "user", "content": "Reply with exactly OK"},
		},
		"model_options": map[string]any{"reasoning": map[string]any{"enabled": true, "effort": "high"}},
		"temperature":   0,
		"max_tokens":    64,
		"stream":        false,
	})
	ctx, cancel := context.WithTimeout(t.Context(), 75*time.Second)
	defer cancel()
	resp, _, finalModel, meta, err := a.doModelRequestWithRecovery(ctx, body, model, "user", liveLiteLLMRequestFactory(a))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		t.Fatalf("Daiki recovery smoke HTTP %d final=%s meta=%#v body=%s", resp.StatusCode, finalModel, meta, raw)
	}
	if wantFinal != "" && finalModel != wantFinal {
		t.Fatalf("Daiki recovery final model=%s want=%s meta=%#v", finalModel, wantFinal, meta)
	}
	if meta.ObservedHTTPStatus == http.StatusTooManyRequests && finalModel == model {
		t.Fatalf("rate-limited model did not fall back: final=%s meta=%#v", finalModel, meta)
	}
	if !bytes.Contains(bytes.ToUpper(raw), []byte("OK")) {
		t.Fatalf("Daiki recovery smoke unexpected response: %s", raw)
	}
	t.Logf("ROLLOUT_RECOVERY start=%s final=%s attempts=%d fallback_from=%s fallback_to=%s status=%d", model, finalModel, meta.Attempts, meta.FallbackFrom, meta.FallbackTo, meta.ObservedHTTPStatus)
}

func smokeLiveLiteLLMModel(t *testing.T, a *app, model string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"model":       model,
		"messages":    []map[string]any{{"role": "user", "content": "Reply with exactly OK"}},
		"temperature": 0,
		"max_tokens":  32,
		"stream":      false,
	})
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(a.cfg.LiteLLMBase, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("authorization", "Bearer "+a.cfg.LiteLLMKey)
	req.Header.Set("content-type", "application/json")
	resp, err := a.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		t.Fatalf("LiteLLM smoke %s HTTP %d: %s", model, resp.StatusCode, raw)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Choices) == 0 || !strings.Contains(strings.ToUpper(strings.TrimSpace(out.Choices[0].Message.Content)), "OK") {
		t.Fatalf("LiteLLM smoke %s unexpected response: %s", model, raw)
	}
}

func TestLiveRolloutSelectedGeminiRoutes(t *testing.T) {
	if os.Getenv("DAIKI_LIVE_PROVIDER_ROLLOUT") != "1" {
		t.Skip("set DAIKI_LIVE_PROVIDER_ROLLOUT=1 to register and optionally apply selected Gemini routes")
	}
	cfg := loadConfig()
	if cfg.DatabaseURL == "" || cfg.TokenEncryptionKey == "" || cfg.LiteLLMKey == "" || cfg.LiteLLMBase == "" {
		t.Fatal("DATABASE_URL, TOKEN_ENCRYPTION_KEY, LITELLM_MASTER_KEY and LITELLM_BASE_URL are required")
	}
	db, err := pgxpool.New(t.Context(), cfg.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	client := &http.Client{Timeout: 60 * time.Second}
	a := &app{cfg: cfg, db: db, store: store.New(db), http: client, inferenceHTTP: client}
	provider := findLiveProviderByKind(t, a, "gemini")
	discovered, latency, err := a.discoverProvider(t.Context(), provider)
	if err != nil {
		t.Fatal(err)
	}
	available := map[string]bool{}
	for _, m := range discovered {
		available[m.ID] = true
	}
	specs := []liveRolloutSpec{
		{Route: "fast", Upstream: "gemini-3.5-flash-lite", Name: "gemini-gemini-3.5-flash-lite", Fallback: "groq-groq-compound-mini"},
		{Route: "deep", Upstream: "gemini-3.7-flash", Name: "gemini-gemini-3.7-flash", Fallback: "groq-groq-compound"},
	}
	for _, spec := range specs {
		if !available[spec.Upstream] {
			t.Fatalf("selected model %s not present in Gemini discovery", spec.Upstream)
		}
	}
	t.Logf("ROLLOUT_DISCOVERY provider=%s models=%d latency_ms=%d", provider.Name, len(discovered), latency.Milliseconds())

	registered := map[string]store.ProviderModel{}
	for _, spec := range specs {
		if _, ok := registered[spec.Name]; ok {
			continue
		}
		rec := ensureLiveProviderModel(t, a, provider, spec)
		registered[spec.Name] = rec
		if spec.Route == "fast" {
			smokeLiveLiteLLMModel(t, a, spec.Name)
		}
		t.Logf("ROLLOUT_MODEL name=%s upstream=%s fallback=%s status=%s", rec.LiteLLMModelName, rec.UpstreamModel, rec.FallbackModelName, rec.Status)
	}

	mini := ensureLiveRuntimeFallback(t, a, "groq-groq-compound-mini", "groq-openai-gpt-oss-20b")
	compound := ensureLiveRuntimeFallback(t, a, "groq-groq-compound", "groq-openai-gpt-oss-20b")
	t.Logf("ROLLOUT_FALLBACK model=%s fallback=%s", mini.LiteLLMModelName, mini.FallbackModelName)
	t.Logf("ROLLOUT_FALLBACK model=%s fallback=%s", compound.LiteLLMModelName, compound.FallbackModelName)
	// Gemini 3.7 may already be at its small free daily quota. Exercise the
	// Daiki recovery path so a live 429 proves the request can still complete
	// through Compound instead of making rollout depend on remaining quota.
	smokeLiveDaikiRecovery(t, a, "gemini-gemini-3.7-flash", "")

	if os.Getenv("DAIKI_LIVE_PROVIDER_ROLLOUT_APPLY_ALIASES") != "1" {
		t.Log("ROLLOUT_ALIASES skipped; set DAIKI_LIVE_PROVIDER_ROLLOUT_APPLY_ALIASES=1 to apply")
		return
	}
	aliasPlan := map[string]string{
		"fast":     "gemini-gemini-3.5-flash-lite",
		"balanced": "groq-groq-compound",
		"deep":     "gemini-gemini-3.7-flash",
	}
	for _, route := range []string{"fast", "balanced", "deep"} {
		model := aliasPlan[route]
		if _, err := a.store.SetModelAlias(t.Context(), route, model, "live-provider-rollout"); err != nil {
			t.Fatal(err)
		}
		t.Logf("ROLLOUT_ALIAS route=%s model=%s", route, model)
	}
	aliases, err := a.store.ModelAliases(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, alias := range aliases {
		if alias.Alias == "fast" || alias.Alias == "balanced" || alias.Alias == "deep" {
			t.Logf("ROLLOUT_FINAL_ALIAS route=%s model=%s", alias.Alias, alias.LiteLLMModelName)
		}
	}
}

func TestLiveRolloutSpecsAreIntentional(t *testing.T) {
	if "gemini-gemini-3.7-flash" == "gemini-gemini-3.8-flash" {
		t.Fatal(fmt.Errorf("deep rollout must not silently select Gemini 3.8 Flash"))
	}
}
