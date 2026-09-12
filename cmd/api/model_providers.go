package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/poom-ytthpch/daiki-ai-passport-backend/internal/store"
)

type providerInput struct {
	Name         string `json:"name"`
	ProviderType string `json:"providerType"`
	BaseURL      string `json:"baseUrl"`
	APIKey       string `json:"apiKey"`
	Enabled      *bool  `json:"enabled,omitempty"`
}

type discoveredModel struct {
	ID string `json:"id"`
}

func normalizeProviderType(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	switch v {
	case "vllm", "lmstudio", "ollama", "anthropic", "gemini":
		return v
	case "openai-compatible", "openai_compatible", "openai", "groq", "openrouter", "together", "together-ai", "fireworks", "deepinfra", "xai", "mistral", "cerebras", "opencode", "opencodezen", "opencode-zen", "zen", "custom":
		return "openai-compatible"
	default:
		return ""
	}
}

func providerTypeDefaultBase(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "openai":
		return "https://api.openai.com/v1"
	case "groq":
		return "https://api.groq.com/openai/v1"
	case "openrouter":
		return "https://openrouter.ai/api/v1"
	case "together", "together-ai":
		return "https://api.together.xyz/v1"
	case "fireworks":
		return "https://api.fireworks.ai/inference/v1"
	case "deepinfra":
		return "https://api.deepinfra.com/v1/openai"
	case "xai":
		return "https://api.x.ai/v1"
	case "mistral":
		return "https://api.mistral.ai/v1"
	case "cerebras":
		return "https://api.cerebras.ai/v1"
	case "opencode", "opencodezen", "opencode-zen", "zen":
		return "https://opencode.ai/zen/v1"
	case "anthropic":
		return "https://api.anthropic.com"
	case "gemini":
		return "https://generativelanguage.googleapis.com/v1beta"
	default:
		return ""
	}
}

func normalizeProviderBase(providerType, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("base URL is required")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return "", errors.New("base URL must be an http(s) URL without credentials")
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || host == "metadata.google.internal" || host == "metadata.google" {
		return "", errors.New("loopback and metadata endpoints are not allowed")
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
			return "", errors.New("loopback, link-local and multicast endpoints are not allowed")
		}
	}
	u.RawQuery = ""
	u.Fragment = ""
	u.Path = strings.TrimRight(u.Path, "/")
	if providerType == "ollama" && strings.HasSuffix(u.Path, "/v1") {
		u.Path = strings.TrimSuffix(u.Path, "/v1")
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func providerDiscoveryURL(p store.ModelProvider) string {
	base := strings.TrimRight(p.BaseURL, "/")
	switch p.ProviderType {
	case "ollama":
		return base + "/api/tags"
	case "anthropic":
		if strings.HasSuffix(base, "/v1") {
			return base + "/models"
		}
		return base + "/v1/models"
	case "gemini", "openai-compatible":
		return providerLiteLLMBase(p) + "/models"
	default:
		if strings.HasSuffix(base, "/v1") {
			return base + "/models"
		}
		return base + "/v1/models"
	}
}
func providerLiteLLMBase(p store.ModelProvider) string {
	base := strings.TrimRight(p.BaseURL, "/")
	switch p.ProviderType {
	case "ollama":
		return strings.TrimSuffix(base, "/v1")
	case "openai-compatible":
		u, err := url.Parse(base)
		if err == nil && strings.Trim(u.Path, "/") != "" {
			return base
		}
		return base + "/v1"
	case "gemini", "anthropic":
		return base
	default:
		if strings.HasSuffix(base, "/v1") {
			return base
		}
		return base + "/v1"
	}
}
func providerLiteLLMModel(p store.ModelProvider, upstream string) string {
	upstream = strings.TrimPrefix(strings.TrimSpace(upstream), "models/")
	switch p.ProviderType {
	case "ollama":
		return "ollama/" + upstream
	case "anthropic":
		return "anthropic/" + upstream
	case "gemini":
		return "gemini/" + upstream
	default:
		return "openai/" + upstream
	}
}

func (a *app) providerAPIKey(p store.ModelProvider) (string, error) {
	if p.EncryptedAPIKey == "" {
		return "", nil
	}
	return a.decryptScopedSecret("model-provider:"+p.ID+":api-key", p.EncryptedAPIKey)
}

func providerIPAllowed(ip net.IP) bool {
	return ip != nil && !ip.IsLoopback() && !ip.IsUnspecified() && !ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() && !ip.IsMulticast()
}

func safeProviderHTTPClient(ctx context.Context, rawURL string) (*http.Client, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	host := u.Hostname()
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil || len(ips) == 0 {
		return nil, fmt.Errorf("provider host resolution failed")
	}
	for _, ip := range ips {
		if !providerIPAllowed(ip) {
			return nil, fmt.Errorf("provider host resolves to a blocked address")
		}
	}
	selected := ips[0].String()
	dialer := &net.Dialer{Timeout: 8 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{Proxy: nil, DialContext: func(dialCtx context.Context, network, address string) (net.Conn, error) {
		h, p, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		if !strings.EqualFold(h, host) {
			return nil, fmt.Errorf("provider redirect host is not allowed")
		}
		return dialer.DialContext(dialCtx, network, net.JoinHostPort(selected, p))
	}}
	return &http.Client{Timeout: 10 * time.Second, Transport: transport, CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return errors.New("provider redirects are not allowed")
	}}, nil
}

func (a *app) discoverProvider(ctx context.Context, p store.ModelProvider) ([]discoveredModel, time.Duration, error) {
	if _, err := normalizeProviderBase(p.ProviderType, p.BaseURL); err != nil {
		return nil, 0, err
	}
	apiKey, err := a.providerAPIKey(p)
	if err != nil {
		return nil, 0, err
	}
	client, err := safeProviderHTTPClient(ctx, p.BaseURL)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, providerDiscoveryURL(p), nil)
	if err != nil {
		return nil, 0, err
	}
	if apiKey != "" {
		switch p.ProviderType {
		case "anthropic":
			req.Header.Set("x-api-key", apiKey)
			req.Header.Set("anthropic-version", "2023-06-01")
		case "gemini":
			req.Header.Set("x-goog-api-key", apiKey)
		default:
			req.Header.Set("authorization", "Bearer "+apiKey)
		}
	}
	started := time.Now()
	resp, err := client.Do(req)
	latency := time.Since(started)
	if err != nil {
		return nil, latency, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, latency, fmt.Errorf("provider returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	out := []discoveredModel{}
	if p.ProviderType == "ollama" {
		var payload struct {
			Models []struct {
				Name  string `json:"name"`
				Model string `json:"model"`
			} `json:"models"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&payload); err != nil {
			return nil, latency, err
		}
		for _, m := range payload.Models {
			id := first(m.Model, m.Name)
			if id != "" {
				out = append(out, discoveredModel{ID: id})
			}
		}
	} else if p.ProviderType == "gemini" {
		var payload struct {
			Models []struct {
				Name string `json:"name"`
			} `json:"models"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&payload); err != nil {
			return nil, latency, err
		}
		for _, m := range payload.Models {
			id := strings.TrimPrefix(strings.TrimSpace(m.Name), "models/")
			if id != "" {
				out = append(out, discoveredModel{ID: id})
			}
		}
	} else {
		var payload struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&payload); err != nil {
			return nil, latency, err
		}
		for _, m := range payload.Data {
			if m.ID != "" {
				out = append(out, discoveredModel{ID: m.ID})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, latency, nil
}

func providerPublic(p store.ModelProvider) store.ModelProvider { p.EncryptedAPIKey = ""; return p }

func (a *app) adminModelProviders(w http.ResponseWriter, r *http.Request) {
	providers, err := a.store.ModelProviders(r.Context())
	if err != nil {
		writeJSON(w, 503, map[string]string{"error": "provider registry unavailable"})
		return
	}
	models, err := a.store.ProviderModels(r.Context(), "")
	if err != nil {
		writeJSON(w, 503, map[string]string{"error": "provider model registry unavailable"})
		return
	}
	aliases, err := a.store.ModelAliases(r.Context())
	if err != nil {
		writeJSON(w, 503, map[string]string{"error": "alias registry unavailable"})
		return
	}
	for i := range providers {
		providers[i] = providerPublic(providers[i])
	}
	writeJSON(w, 200, map[string]any{"providers": providers, "models": models, "aliases": aliases})
}

func (a *app) adminSaveModelProvider(w http.ResponseWriter, r *http.Request) {
	var in providerInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10)).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid provider payload"})
		return
	}
	rawType := strings.ToLower(strings.TrimSpace(in.ProviderType))
	typ := normalizeProviderType(rawType)
	if typ == "" {
		writeJSON(w, 400, map[string]string{"error": "providerType must be vllm, lmstudio, ollama, openai-compatible, anthropic, gemini, or a supported hosted provider preset"})
		return
	}
	rawBase := strings.TrimSpace(in.BaseURL)
	if rawBase == "" {
		rawBase = providerTypeDefaultBase(rawType)
	}
	base, err := normalizeProviderBase(typ, rawBase)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	name := strings.TrimSpace(in.Name)
	if name == "" || len(name) > 80 {
		writeJSON(w, 400, map[string]string{"error": "name is required and must be <= 80 characters"})
		return
	}
	id := chi.URLParam(r, "id")
	if id == "" {
		tok, e := randomURLToken(9)
		if e != nil {
			writeJSON(w, 500, map[string]string{"error": "unable to create provider id"})
			return
		}
		id = "prov_" + tok
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	encrypted := ""
	if strings.TrimSpace(in.APIKey) != "" {
		encrypted, err = a.encryptScopedSecret("model-provider:"+id+":api-key", strings.TrimSpace(in.APIKey))
		if err != nil {
			writeJSON(w, 503, map[string]string{"error": "provider secret encryption unavailable"})
			return
		}
	}
	saved, err := a.store.UpsertModelProvider(r.Context(), store.ModelProvider{ID: id, Name: name, ProviderType: typ, BaseURL: base, EncryptedAPIKey: encrypted, Enabled: enabled})
	if err != nil {
		writeJSON(w, 503, map[string]string{"error": "unable to save provider"})
		return
	}
	models, latency, testErr := a.discoverProvider(r.Context(), saved)
	status, msg := "ok", fmt.Sprintf("Connected · %d models · %dms", len(models), latency.Milliseconds())
	if testErr != nil {
		status = "error"
		msg = testErr.Error()
	}
	_ = a.store.SetProviderTest(r.Context(), id, status, msg)
	saved.LastTestStatus = status
	saved.LastTestMessage = msg
	syncStatus := "not-needed"
	if testErr == nil {
		if syncErr := a.syncProviderModels(r.Context(), saved); syncErr != nil {
			syncStatus = "error"
			msg += " · LiteLLM sync failed: " + syncErr.Error()
		} else {
			syncStatus = "ok"
		}
	}
	writeJSON(w, 200, map[string]any{"provider": providerPublic(saved), "models": models, "test": map[string]any{"status": status, "message": msg}, "syncStatus": syncStatus})
}

func (a *app) adminTestModelProvider(w http.ResponseWriter, r *http.Request) {
	p, err := a.store.ModelProvider(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "provider not found"})
		return
	}
	models, latency, testErr := a.discoverProvider(r.Context(), p)
	if testErr != nil {
		_ = a.store.SetProviderTest(r.Context(), p.ID, "error", testErr.Error())
		writeJSON(w, 502, map[string]any{"ok": false, "error": testErr.Error(), "latencyMs": latency.Milliseconds()})
		return
	}
	msg := fmt.Sprintf("Connected · %d models · %dms", len(models), latency.Milliseconds())
	_ = a.store.SetProviderTest(r.Context(), p.ID, "ok", msg)
	writeJSON(w, 200, map[string]any{"ok": true, "models": models, "latencyMs": latency.Milliseconds()})
}
func (a *app) adminDiscoverProviderModels(w http.ResponseWriter, r *http.Request) {
	a.adminTestModelProvider(w, r)
}

func (a *app) litellmAdmin(ctx context.Context, method, path string, payload any) (map[string]any, int, error) {
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, 0, err
		}
		body = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(a.cfg.LiteLLMBase, "/")+path, body)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("authorization", "Bearer "+a.cfg.LiteLLMKey)
	if payload != nil {
		req.Header.Set("content-type", "application/json")
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	if resp.StatusCode >= 300 {
		return out, resp.StatusCode, fmt.Errorf("LiteLLM HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return out, resp.StatusCode, nil
}

func (a *app) providerLiteLLMParams(p store.ModelProvider, m store.ProviderModel) (map[string]any, error) {
	providerKey, err := a.providerAPIKey(p)
	if err != nil {
		return nil, err
	}
	timeout := m.TimeoutSeconds
	if timeout <= 0 {
		timeout = 300
	}
	params := map[string]any{"model": providerLiteLLMModel(p, m.UpstreamModel), "timeout": timeout}
	if m.StreamTimeoutSeconds > 0 {
		params["stream_timeout"] = m.StreamTimeoutSeconds
	}
	if m.ProviderMaxRetries >= 0 {
		params["max_retries"] = m.ProviderMaxRetries
	}
	if m.TPMLimit > 0 {
		params["tpm"] = m.TPMLimit
	}
	if m.ITPMLimit > 0 {
		params["itpm"] = m.ITPMLimit
	}
	if m.OTPMLimit > 0 {
		params["otpm"] = m.OTPMLimit
	}
	if m.RPMLimit > 0 {
		params["rpm"] = m.RPMLimit
	}
	if p.ProviderType != "anthropic" && p.ProviderType != "gemini" {
		params["api_base"] = providerLiteLLMBase(p)
	}
	if providerKey != "" {
		params["api_key"] = providerKey
	} else if p.ProviderType != "ollama" {
		params["api_key"] = "not-needed"
	}
	return params, nil
}

func (a *app) syncProviderModels(ctx context.Context, p store.ModelProvider) error {
	models, err := a.store.ProviderModels(ctx, p.ID)
	if err != nil {
		return err
	}
	for _, m := range models {
		if m.Status != "active" || m.LiteLLMModelID == "" {
			continue
		}
		params, err := a.providerLiteLLMParams(p, m)
		if err != nil {
			return err
		}
		_, _, err = a.litellmAdmin(ctx, http.MethodPost, "/model/update", map[string]any{"model_info": providerLiteLLMModelInfo(m, m.LiteLLMModelID), "litellm_params": params})
		if err != nil {
			_ = a.store.SetProviderModelState(ctx, m.ID, "error", "", err.Error())
			return fmt.Errorf("update %s: %w", m.LiteLLMModelName, err)
		}
		_ = a.store.SetProviderModelState(ctx, m.ID, "active", "", "")
		a.clearModelCircuit(ctx, m.LiteLLMModelName)
	}
	return nil
}

func providerLiteLLMModelInfo(m store.ProviderModel, id string) map[string]any {
	info := map[string]any{"provider": "daiki", "provider_id": m.ProviderID}
	if id != "" {
		info["id"] = id
	}
	if m.MaxInputTokens > 0 {
		info["max_input_tokens"] = m.MaxInputTokens
	}
	if m.MaxOutputTokens > 0 {
		info["max_output_tokens"] = m.MaxOutputTokens
	}
	info["daiki_context_strategy"] = m.ContextStrategy
	info["daiki_context_target_tokens"] = m.ContextTargetTokens
	info["daiki_agent_overhead_tokens"] = m.AgentOverheadTokens
	info["daiki_research_overhead_tokens"] = m.ResearchOverheadTokens
	info["daiki_fallback_model"] = m.FallbackModelName
	return info
}

func (a *app) adminUpdateProviderModel(w http.ResponseWriter, r *http.Request) {
	currentModel, err := a.store.ProviderModel(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "model not found"})
		return
	}
	var in struct {
		MaxInputTokens         int    `json:"maxInputTokens"`
		MaxOutputTokens        int    `json:"maxOutputTokens"`
		TPMLimit               int    `json:"tpmLimit"`
		ITPMLimit              int    `json:"itpmLimit"`
		OTPMLimit              int    `json:"otpmLimit"`
		RPMLimit               int    `json:"rpmLimit"`
		RPDLimit               int    `json:"rpdLimit"`
		TimeoutSeconds         int    `json:"timeoutSeconds"`
		StreamTimeoutSeconds   int    `json:"streamTimeoutSeconds"`
		MaxRetries             int    `json:"maxRetries"`
		ProviderMaxRetries     int    `json:"providerMaxRetries"`
		RetryBackoffMS         int    `json:"retryBackoffMs"`
		ContextStrategy        string `json:"contextStrategy"`
		ContextTargetTokens    int    `json:"contextTargetTokens"`
		AgentOverheadTokens    int    `json:"agentOverheadTokens"`
		ResearchOverheadTokens int    `json:"researchOverheadTokens"`
		FallbackModelName      string `json:"fallbackModelName"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in) != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid model settings"})
		return
	}
	currentModel.MaxInputTokens = in.MaxInputTokens
	currentModel.MaxOutputTokens = in.MaxOutputTokens
	currentModel.TPMLimit = in.TPMLimit
	currentModel.ITPMLimit = in.ITPMLimit
	currentModel.OTPMLimit = in.OTPMLimit
	currentModel.RPMLimit = in.RPMLimit
	currentModel.RPDLimit = in.RPDLimit
	currentModel.TimeoutSeconds = in.TimeoutSeconds
	currentModel.StreamTimeoutSeconds = in.StreamTimeoutSeconds
	currentModel.MaxRetries = in.MaxRetries
	currentModel.ProviderMaxRetries = in.ProviderMaxRetries
	currentModel.RetryBackoffMS = in.RetryBackoffMS
	currentModel.ContextStrategy = strings.TrimSpace(in.ContextStrategy)
	currentModel.ContextTargetTokens = in.ContextTargetTokens
	currentModel.AgentOverheadTokens = in.AgentOverheadTokens
	currentModel.ResearchOverheadTokens = in.ResearchOverheadTokens
	currentModel.FallbackModelName = strings.TrimSpace(in.FallbackModelName)
	if currentModel.TimeoutSeconds == 0 {
		currentModel.TimeoutSeconds = 300
	}
	if currentModel.StreamTimeoutSeconds == 0 {
		currentModel.StreamTimeoutSeconds = currentModel.TimeoutSeconds
	}
	if currentModel.ContextStrategy == "" {
		currentModel.ContextStrategy = "adaptive"
	}
	if currentModel.FallbackModelName != "" {
		if currentModel.FallbackModelName == currentModel.LiteLLMModelName {
			writeJSON(w, 400, map[string]string{"error": "fallback model cannot be the same model"})
			return
		}
		fallback, ferr := a.store.ProviderModelByLiteLLMName(r.Context(), currentModel.FallbackModelName)
		if ferr != nil || fallback.Status != "active" {
			writeJSON(w, 400, map[string]string{"error": "fallbackModelName must be an active registered model"})
			return
		}
	}
	updated, err := a.store.UpdateProviderModelRuntime(r.Context(), current(r).Sub, currentModel)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	p, err := a.store.ModelProvider(r.Context(), updated.ProviderID)
	if err != nil {
		writeJSON(w, 503, map[string]string{"error": "provider unavailable"})
		return
	}
	params, err := a.providerLiteLLMParams(p, updated)
	if err != nil {
		writeJSON(w, 503, map[string]string{"error": "provider secret unavailable"})
		return
	}
	if updated.LiteLLMModelID != "" {
		if _, _, err = a.litellmAdmin(r.Context(), http.MethodPost, "/model/update", map[string]any{"model_info": providerLiteLLMModelInfo(updated, updated.LiteLLMModelID), "litellm_params": params}); err != nil {
			_ = a.store.SetProviderModelState(r.Context(), updated.ID, "error", "", err.Error())
			writeJSON(w, 502, map[string]any{"error": "LiteLLM model update failed", "detail": err.Error(), "model": updated})
			return
		}
	}
	_ = a.store.SetProviderModelState(r.Context(), updated.ID, "active", "", "")
	updated.Status = "active"
	updated.LastError = ""
	a.clearModelCircuit(r.Context(), updated.LiteLLMModelName)
	writeJSON(w, 200, map[string]any{"model": updated})
}

func (a *app) reconcileProviderModels(ctx context.Context) {
	if a.store == nil {
		return
	}
	providers, err := a.store.ModelProviders(ctx)
	if err != nil {
		slog.Warn("provider runtime reconciliation skipped", "error", err)
		return
	}
	for _, p := range providers {
		if !p.Enabled {
			continue
		}
		if err := a.syncProviderModels(ctx, p); err != nil {
			slog.Warn("provider runtime reconciliation failed", "provider", p.ID, "error", err)
		}
	}
}

func (a *app) adminRegisterProviderModel(w http.ResponseWriter, r *http.Request) {
	p, err := a.store.ModelProvider(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "provider not found"})
		return
	}
	var in struct {
		UpstreamModel    string `json:"upstreamModel"`
		LiteLLMModelName string `json:"litellmModelName"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid model payload"})
		return
	}
	upstream := strings.TrimSpace(in.UpstreamModel)
	if upstream == "" || len(upstream) > 180 {
		writeJSON(w, 400, map[string]string{"error": "upstreamModel is required"})
		return
	}
	name := strings.TrimSpace(in.LiteLLMModelName)
	if name == "" {
		name = p.ID + ":" + strings.ReplaceAll(upstream, " ", "-")
	}
	if len(name) > 220 {
		writeJSON(w, 400, map[string]string{"error": "litellmModelName too long"})
		return
	}
	idToken, e := randomURLToken(9)
	if e != nil {
		writeJSON(w, 500, map[string]string{"error": "unable to create model id"})
		return
	}
	modelID := "pmodel_" + idToken
	rec, err := a.store.UpsertProviderModel(r.Context(), store.ProviderModel{ID: modelID, ProviderID: p.ID, UpstreamModel: upstream, LiteLLMModelName: name, Status: "pending"})
	if err != nil {
		writeJSON(w, 409, map[string]string{"error": "model already registered or name conflicts"})
		return
	}
	params, paramErr := a.providerLiteLLMParams(p, rec)
	if paramErr != nil {
		writeJSON(w, 503, map[string]string{"error": "provider secret unavailable"})
		return
	}
	result, _, regErr := a.litellmAdmin(r.Context(), http.MethodPost, "/model/new", map[string]any{"model_name": name, "litellm_params": params, "model_info": providerLiteLLMModelInfo(rec, "")})
	if regErr != nil {
		_ = a.store.SetProviderModelState(r.Context(), rec.ID, "error", "", regErr.Error())
		writeJSON(w, 502, map[string]any{"error": "LiteLLM registration failed", "detail": regErr.Error(), "model": rec})
		return
	}
	litellmID := ""
	if v, ok := result["model_id"].(string); ok {
		litellmID = v
	}
	if litellmID == "" {
		if mi, ok := result["model_info"].(map[string]any); ok {
			if v, ok := mi["id"].(string); ok {
				litellmID = v
			}
		}
	}
	if litellmID == "" {
		if v, ok := result["id"].(string); ok {
			litellmID = v
		}
	}
	_ = a.store.SetProviderModelState(r.Context(), rec.ID, "active", litellmID, "")
	rec.Status = "active"
	rec.LiteLLMModelID = litellmID
	writeJSON(w, 200, map[string]any{"model": rec, "litellm": result})
}

func (a *app) adminDeleteProviderModel(w http.ResponseWriter, r *http.Request) {
	m, err := a.store.ProviderModel(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "model not found"})
		return
	}
	if m.LiteLLMModelID != "" {
		if _, _, err := a.litellmAdmin(r.Context(), http.MethodPost, "/model/delete", map[string]any{"id": m.LiteLLMModelID}); err != nil {
			writeJSON(w, 502, map[string]string{"error": "LiteLLM delete failed", "detail": err.Error()})
			return
		}
	}
	_ = a.store.DeleteAliasesForModel(r.Context(), m.LiteLLMModelName)
	if err := a.store.DeleteProviderModel(r.Context(), m.ID); err != nil {
		writeJSON(w, 503, map[string]string{"error": "unable to delete model"})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
func (a *app) adminDeleteModelProvider(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	models, _ := a.store.ProviderModels(r.Context(), id)
	for _, m := range models {
		if m.LiteLLMModelID != "" {
			if _, _, err := a.litellmAdmin(r.Context(), http.MethodPost, "/model/delete", map[string]any{"id": m.LiteLLMModelID}); err != nil {
				writeJSON(w, 502, map[string]string{"error": "remove provider models from LiteLLM first", "detail": err.Error()})
				return
			}
		}
		_ = a.store.DeleteAliasesForModel(r.Context(), m.LiteLLMModelName)
	}
	if err := a.store.DeleteModelProvider(r.Context(), id); err != nil {
		writeJSON(w, 503, map[string]string{"error": "unable to delete provider"})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
func (a *app) adminModelAliases(w http.ResponseWriter, r *http.Request) {
	rows, err := a.store.ModelAliases(r.Context())
	if err != nil {
		writeJSON(w, 503, map[string]string{"error": "alias registry unavailable"})
		return
	}
	writeJSON(w, 200, rows)
}
func (a *app) adminSetModelAlias(w http.ResponseWriter, r *http.Request) {
	alias := strings.ToLower(strings.TrimSpace(chi.URLParam(r, "alias")))
	if alias != "fast" && alias != "balanced" && alias != "deep" && alias != "vision" {
		writeJSON(w, 400, map[string]string{"error": "unsupported alias"})
		return
	}
	var in struct {
		LiteLLMModelName string `json:"litellmModelName"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10)).Decode(&in) != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid alias payload"})
		return
	}
	model := strings.TrimSpace(in.LiteLLMModelName)
	models, err := a.store.ProviderModels(r.Context(), "")
	if err != nil {
		writeJSON(w, 503, map[string]string{"error": "model registry unavailable"})
		return
	}
	found := false
	for _, m := range models {
		if m.LiteLLMModelName == model && m.Status == "active" {
			found = true
			break
		}
	}
	if !found {
		writeJSON(w, 400, map[string]string{"error": "alias target must be an active registered model"})
		return
	}
	saved, err := a.store.SetModelAlias(r.Context(), alias, model, current(r).Sub)
	if err != nil {
		writeJSON(w, 503, map[string]string{"error": "unable to save alias"})
		return
	}
	writeJSON(w, 200, saved)
}
