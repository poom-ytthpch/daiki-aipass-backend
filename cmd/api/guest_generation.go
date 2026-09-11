package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/poom-ytthpch/daiki-ai-passport-backend/internal/inference"
	"github.com/poom-ytthpch/daiki-ai-passport-backend/internal/store"
)

var generatedImageDataRE = regexp.MustCompile(`data:(image/[a-zA-Z0-9.+-]+);base64,([A-Za-z0-9+/=]+)`)
var errImageProviderAuth = errors.New("image provider authentication failed")

const defaultOpenRouterImageModel = "google/gemini-3.1-flash-lite-image"

type openRouterImageResponse struct {
	Data []struct {
		B64JSON   string `json:"b64_json"`
		MediaType string `json:"media_type"`
	} `json:"data"`
}

func decodeOpenRouterImageResponse(body []byte) ([]byte, string, error) {
	var payload openRouterImageResponse
	if err := json.Unmarshal(body, &payload); err != nil || len(payload.Data) == 0 {
		return nil, "", errors.New("image provider returned no image")
	}
	encoded := strings.TrimSpace(payload.Data[0].B64JSON)
	if encoded == "" {
		return nil, "", errors.New("image provider returned empty image")
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(data) == 0 {
		return nil, "", errors.New("image provider returned invalid base64")
	}
	mediaType := strings.TrimSpace(payload.Data[0].MediaType)
	if !strings.HasPrefix(mediaType, "image/") {
		mediaType = "image/png"
	}
	return data, mediaType, nil
}

func (a *app) generateGuestImageViaOpenRouter(ctx context.Context, prompt string) ([]byte, string, string, error) {
	providers, err := a.store.ModelProviders(ctx)
	if err != nil {
		return nil, "", "", err
	}
	for _, provider := range providers {
		if !provider.Enabled || !provider.HasAPIKey || !strings.Contains(strings.ToLower(provider.BaseURL), "openrouter.ai") {
			continue
		}
		key, err := a.providerAPIKey(provider)
		if err != nil || strings.TrimSpace(key) == "" {
			continue
		}
		endpoint := strings.TrimRight(provider.BaseURL, "/") + "/images"
		client, err := safeProviderHTTPClient(ctx, endpoint)
		if err != nil {
			return nil, "", "openrouter-images", err
		}
		model := strings.TrimSpace(os.Getenv("OPENROUTER_IMAGE_MODEL"))
		if model == "" {
			model = defaultOpenRouterImageModel
		}
		payload, _ := json.Marshal(map[string]any{"model": model, "prompt": strings.TrimSpace(prompt), "n": 1})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(payload)))
		if err != nil {
			return nil, "", "openrouter-images", err
		}
		req.Header.Set("content-type", "application/json")
		req.Header.Set("authorization", "Bearer "+key)
		req.Header.Set("http-referer", a.cfg.AppBaseURL)
		req.Header.Set("x-title", "Daiki AI Passport")
		resp, err := client.Do(req)
		if err != nil {
			return nil, "", "openrouter-images", err
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, "", "openrouter-images", readErr
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			detail := strings.TrimSpace(string(body))
			if len(detail) > 2048 {
				detail = detail[:2048]
			}
			if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
				_ = a.store.SetProviderTest(ctx, provider.ID, "error", "image generation provider authentication failed")
				return nil, "", "openrouter-images", fmt.Errorf("%w: status %d", errImageProviderAuth, resp.StatusCode)
			}
			return nil, "", "openrouter-images", fmt.Errorf("image provider returned status %d: %s", resp.StatusCode, detail)
		}
		data, mediaType, err := decodeOpenRouterImageResponse(body)
		return data, mediaType, "openrouter-images:" + model, err
	}
	return nil, "", "", errors.New("no OpenRouter image provider configured")
}

func (a *app) refundGuestDailyCapability(ctx context.Context, capability, subject string, limit int) {
	if limit == 0 || a.redis == nil {
		return
	}
	key := fmt.Sprintf("guest-%s:%s:%s", capability, subject, time.Now().UTC().Format("20060102"))
	if v, err := a.redis.Decr(ctx, key).Result(); err == nil && v < 0 {
		_ = a.redis.Set(ctx, key, 0, 26*time.Hour).Err()
	}
}

func (a *app) enforceGuestDailyCapability(ctx context.Context, capability, subject string, limit int) error {
	if limit == 0 {
		return nil
	}
	if a.redis == nil {
		return errors.New("guest capability limiter unavailable")
	}
	now := time.Now().UTC()
	key := fmt.Sprintf("guest-%s:%s:%s", capability, subject, now.Format("20060102"))
	v, err := a.redis.Incr(ctx, key).Result()
	if err != nil {
		return err
	}
	if v == 1 {
		_ = a.redis.Expire(ctx, key, 26*time.Hour).Err()
	}
	if v > int64(limit) {
		return fmt.Errorf("%s limit exceeded", capability)
	}
	return nil
}

func chatCompletionText(body []byte) string {
	var payload struct {
		Choices []struct {
			Message struct {
				Content any `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(body, &payload) != nil || len(payload.Choices) == 0 {
		return ""
	}
	switch content := payload.Choices[0].Message.Content.(type) {
	case string:
		return content
	case []any:
		var b strings.Builder
		for _, item := range content {
			m, _ := item.(map[string]any)
			if text, _ := m["text"].(string); text != "" {
				b.WriteString(text)
			}
		}
		return b.String()
	default:
		return ""
	}
}

func (a *app) runGuestCoreGeneration(ctx context.Context, identity guestIdentity, p store.GuestAccessPolicy, capability, profile, prompt string) ([]byte, string, error) {
	alias, err := a.store.ModelAlias(ctx, "fast")
	if err != nil || alias.LiteLLMModelName == "" {
		return nil, "", errors.New("guest fast model unavailable")
	}
	body, _ := json.Marshal(map[string]any{
		"model":                 alias.LiteLLMModelName,
		"messages":              []map[string]any{{"role": "user", "content": prompt}},
		"max_completion_tokens": p.MaxCompletionTokens,
		"stream":                false,
	})
	decision, _, err := a.guestQuota(ctx, identity.Subject, p)
	if err != nil {
		return nil, "", errors.New("guest quota unavailable")
	}
	reserved := reservationTokens(body)
	requestID := fmt.Sprintf("guest-%s-%d", capability, time.Now().UnixNano())
	if err := a.reserveQuota(ctx, requestID, decision, reserved); err != nil {
		return nil, "", err
	}
	if err := a.store.StartUsageForPrincipal(ctx, requestID, identity.Subject, "", "fast", string(inference.WorkloadFast), reserved); err != nil {
		a.releaseReservation(ctx, requestID, decision, reserved)
		return nil, "", errors.New("guest usage ledger unavailable")
	}
	_ = a.store.MergeUsageMetadata(ctx, requestID, map[string]any{
		"authKind": "guest", "guestCapability": capability, "resolvedAlias": "fast",
		"physicalModel": alias.LiteLLMModelName, "guestNetworkId": identity.Subject,
		"guestDeviceId": identity.DeviceID, "guestDeviceName": identity.DeviceName,
		"guestPrompt": guestActivityText(prompt, 32<<10),
	})
	ticket, err := a.queue.Acquire(ctx, requestID, identity.Subject, inference.WorkloadFast, 0)
	if err != nil {
		a.releaseReservation(context.Background(), requestID, decision, reserved)
		_ = a.store.FinishUsage(context.Background(), requestID, "failed", store.Usage{})
		return nil, "", err
	}
	defer ticket.Release(context.Background())
	upstreamURL := strings.TrimRight(a.cfg.HermesBase, "/") + "/p/" + profile + "/v1/chat/completions"
	upstreamName := "hermes-" + profile
	if !a.cfg.HermesEnabled || a.cfg.HermesBase == "" {
		a.releaseReservation(ctx, requestID, decision, reserved)
		_ = a.store.FinishUsage(ctx, requestID, "failed", store.Usage{})
		return nil, upstreamName, errors.New("hermes unavailable")
	}
	makeReq := func(payload []byte) (*http.Request, error) {
		req, buildErr := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, strings.NewReader(string(payload)))
		if buildErr != nil {
			return nil, buildErr
		}
		req.Header.Set("content-type", "application/json")
		if a.cfg.HermesKey != "" {
			req.Header.Set("authorization", "Bearer "+a.cfg.HermesKey)
		}
		req.Header.Set("x-daiki-request-id", requestID)
		req.Header.Set("x-daiki-principal", "guest")
		baseKey := "daiki-guest:" + strings.TrimPrefix(identity.Subject, "guest:") + ":" + identity.DeviceID + ":" + capability + ":p:" + profile
		applyHermesSessionScope(req, baseKey, payload)
		return req, nil
	}
	resp, recoveredBody, recoveredModel, recovery, err := a.doModelRequestWithRecovery(ctx, body, alias.LiteLLMModelName, profile, makeReq)
	_ = recoveredBody
	if recoveredModel != "" {
		_ = a.store.MergeUsageMetadata(ctx, requestID, map[string]any{"physicalModel": recoveredModel})
	}
	_ = a.store.MergeUsageMetadata(ctx, requestID, recoveryMetadata(recovery))
	if err != nil {
		a.releaseReservation(ctx, requestID, decision, reserved)
		_ = a.store.FinishUsage(context.Background(), requestID, "failed", store.Usage{})
		return nil, upstreamName, err
	}
	if resp == nil || resp.Body == nil {
		a.releaseReservation(ctx, requestID, decision, reserved)
		_ = a.store.FinishUsage(context.Background(), requestID, "failed", store.Usage{})
		return nil, upstreamName, errors.New("hermes returned no response")
	}
	defer resp.Body.Close()
	responseBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 12<<20))
	usage := parseUsagePayload(responseBody)
	status := "completed"
	if readErr != nil || resp.StatusCode >= 400 || providerFailureStatus(responseBody) != 0 {
		status = "failed"
		if resp.StatusCode >= 400 {
			usage = store.Usage{}
		}
	}
	if status == "completed" && usage.TotalTokens == 0 {
		usage.TotalTokens = reserved
	}
	responseMeta := usageResponseMetadata(responseBody)
	responseMeta["guestResponse"] = guestResponseText(responseBody)
	_ = a.store.MergeUsageMetadata(context.Background(), requestID, responseMeta)
	_ = a.store.FinishUsage(context.Background(), requestID, status, usage)
	a.releaseReservation(context.Background(), requestID, decision, reserved)
	if readErr != nil {
		return nil, upstreamName, readErr
	}
	if resp.StatusCode >= 400 {
		return responseBody, upstreamName, fmt.Errorf("hermes returned status %d", resp.StatusCode)
	}
	if providerFailureStatus(responseBody) != 0 {
		return responseBody, upstreamName, errors.New("hermes generation provider failed")
	}
	return responseBody, upstreamName, nil
}
func generatedName(name, fallback string) string {
	name = filepath.Base(strings.TrimSpace(name))
	if name == "" || name == "." {
		name = fallback
	}
	if len(name) > 160 {
		name = name[:160]
	}
	return name
}

func (a *app) storeGeneratedGuestAttachment(ctx context.Context, identity guestIdentity, p store.GuestAccessPolicy, name, source, mediaType string, data []byte) (store.GuestAttachment, error) {
	files, bytes, err := a.store.GuestAttachmentUsage(ctx, identity.Subject)
	if err != nil {
		return store.GuestAttachment{}, err
	}
	if (p.MaxStoredFiles > 0 && files >= int64(p.MaxStoredFiles)) || (p.MaxStoredBytes > 0 && bytes+int64(len(data)) > p.MaxStoredBytes) {
		return store.GuestAttachment{}, errors.New("guest attachment storage limit exceeded")
	}
	id, err := newAttachmentID()
	if err != nil {
		return store.GuestAttachment{}, err
	}
	dir := guestAttachmentDir(identity)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return store.GuestAttachment{}, err
	}
	storagePath := filepath.Join(dir, id)
	if err := os.WriteFile(storagePath, data, 0o640); err != nil {
		return store.GuestAttachment{}, err
	}
	h := sha256.Sum256(data)
	extractStatus, extractedText := extractAttachmentContent(storagePath, name, mediaType)
	rec, err := a.store.CreateGuestAttachment(ctx, store.GuestAttachment{
		ID: id, GuestSubject: identity.Subject, DeviceID: identity.DeviceID, Name: name,
		RelativePath: name, Source: source, MediaType: mediaType, SizeBytes: int64(len(data)),
		SHA256: hex.EncodeToString(h[:]), StoragePath: storagePath, ExtractStatus: extractStatus, ExtractedText: extractedText,
	})
	if err != nil {
		_ = os.Remove(storagePath)
	}
	return rec, err
}

func (a *app) guestGenerateFile(w http.ResponseWriter, r *http.Request) {
	p, _, err := a.store.GuestAccessPolicy(r.Context())
	if err != nil || !p.Enabled || !p.AllowFileGeneration {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "guest_file_generation_disabled"})
		return
	}
	identity := guestIdentityForRequest(r)
	a.recordGuestIdentity(r.Context(), identity)
	var body struct {
		Prompt    string `json:"prompt"`
		Name      string `json:"name"`
		MediaType string `json:"mediaType"`
		Format    string `json:"format"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body) != nil || strings.TrimSpace(body.Prompt) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "prompt is required"})
		return
	}
	spec, err := generatedFileSpecFor(body.Format, body.Name, body.MediaType, body.Prompt)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := a.enforceGuestDailyCapability(r.Context(), "filegen", identity.Subject, p.FileGenerationsPerDay); err != nil {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "guest_file_generation_limit"})
		return
	}
	responseBody, upstream, err := a.runGuestCoreGeneration(r.Context(), identity, p, "file-generation:"+spec.Format, "guest-skills", generatedFilePrompt(spec, body.Prompt))
	if err != nil {
		a.refundGuestDailyCapability(r.Context(), "filegen", identity.Subject, p.FileGenerationsPerDay)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "guest_file_generation_failed", "upstream": upstream})
		return
	}
	content := chatCompletionText(responseBody)
	data, err := normalizeGeneratedFile(spec, content)
	if err != nil {
		a.refundGuestDailyCapability(r.Context(), "filegen", identity.Subject, p.FileGenerationsPerDay)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "guest_file_generation_invalid", "detail": err.Error(), "format": spec.Format})
		return
	}
	if p.MaxGeneratedFileBytes > 0 && int64(len(data)) > p.MaxGeneratedFileBytes {
		a.refundGuestDailyCapability(r.Context(), "filegen", identity.Subject, p.FileGenerationsPerDay)
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "generated_file_exceeds_limit"})
		return
	}
	rec, err := a.storeGeneratedGuestAttachment(r.Context(), identity, p, spec.Name, "generated-file", spec.MediaType, data)
	if err != nil {
		a.refundGuestDailyCapability(r.Context(), "filegen", identity.Subject, p.FileGenerationsPerDay)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "generated_file_storage_failed"})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"attachment": guestAttachmentPublic(rec), "upstream": upstream, "format": spec.Format})
}
func (a *app) guestGenerateImage(w http.ResponseWriter, r *http.Request) {
	p, _, err := a.store.GuestAccessPolicy(r.Context())
	if err != nil || !p.Enabled || !p.AllowImageGeneration {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "guest_image_generation_disabled"})
		return
	}
	identity := guestIdentityForRequest(r)
	a.recordGuestIdentity(r.Context(), identity)
	var body struct {
		Prompt string `json:"prompt"`
		Name   string `json:"name"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body) != nil || strings.TrimSpace(body.Prompt) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "prompt is required"})
		return
	}
	if err := a.enforceGuestDailyCapability(r.Context(), "imagegen", identity.Subject, p.ImageGenerationsPerDay); err != nil {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "guest_image_generation_limit"})
		return
	}
	prompt := strings.TrimSpace(body.Prompt)
	data, mediaType, upstream, err := a.generateGuestImageViaOpenRouter(r.Context(), prompt)
	if err != nil && !errors.Is(err, errImageProviderAuth) {
		// Preserve Hermes as a secondary path for installations that configure an
		// image_gen provider such as FAL/OpenAI. match-infra currently has an
		// OpenRouter provider, so normal traffic uses the dedicated Images API.
		hermesPrompt := "Generate an image for this request using the image_generate capability and return the generated image. Request: " + prompt
		responseBody, hermesUpstream, hermesErr := a.runGuestCoreGeneration(r.Context(), identity, p, "image-generation", "guest-media", hermesPrompt)
		if hermesErr == nil {
			content := chatCompletionText(responseBody)
			match := generatedImageDataRE.FindStringSubmatch(content)
			if len(match) == 3 {
				decoded, decodeErr := base64.StdEncoding.DecodeString(match[2])
				if decodeErr == nil && len(decoded) > 0 {
					data, mediaType, upstream, err = decoded, match[1], hermesUpstream, nil
				}
			}
		}
	}
	if err != nil || len(data) == 0 {
		slog.Warn("guest image generation failed", "upstream", upstream, "error", err)
		a.refundGuestDailyCapability(r.Context(), "imagegen", identity.Subject, p.ImageGenerationsPerDay)
		code := "image_generation_unavailable"
		if errors.Is(err, errImageProviderAuth) {
			code = "image_generation_provider_auth_failed"
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": code, "upstream": upstream})
		return
	}
	if (p.MaxGeneratedImageBytes > 0 && int64(len(data)) > p.MaxGeneratedImageBytes) || int64(len(data)) > maxInjectedImageBytes {
		a.refundGuestDailyCapability(r.Context(), "imagegen", identity.Subject, p.ImageGenerationsPerDay)
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "generated_image_exceeds_limit"})
		return
	}
	ext := ".png"
	if exts, _ := mime.ExtensionsByType(mediaType); len(exts) > 0 {
		ext = exts[0]
	}
	name := generatedName(body.Name, "generated"+ext)
	if filepath.Ext(name) == "" {
		name += ext
	}
	rec, err := a.storeGeneratedGuestAttachment(r.Context(), identity, p, name, "generated-image", mediaType, data)
	if err != nil {
		a.refundGuestDailyCapability(r.Context(), "imagegen", identity.Subject, p.ImageGenerationsPerDay)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "generated_image_storage_failed"})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"attachment": guestAttachmentPublic(rec), "upstream": upstream})
}
