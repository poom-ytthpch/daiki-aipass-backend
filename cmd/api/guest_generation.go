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
		return nil, upstreamName, errors.New("Hermes unavailable")
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
		return nil, upstreamName, errors.New("Hermes returned no response")
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
	_ = a.store.FinishUsage(context.Background(), requestID, status, usage)
	a.releaseReservation(context.Background(), requestID, decision, reserved)
	if readErr != nil {
		return nil, upstreamName, readErr
	}
	if resp.StatusCode >= 400 {
		return responseBody, upstreamName, fmt.Errorf("Hermes returned status %d", resp.StatusCode)
	}
	if providerFailureStatus(responseBody) != 0 {
		return responseBody, upstreamName, errors.New("Hermes generation provider failed")
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
	extractStatus, extractedText := "stored", ""
	if strings.HasPrefix(mediaType, "text/") || textAttachment(name, mediaType) {
		extractStatus = "text-ready"
		extractedText = string(data)
		if len(extractedText) > maxExtractedTextBytes {
			extractedText = extractedText[:maxExtractedTextBytes]
			extractStatus = "text-truncated"
		}
	}
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
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body) != nil || strings.TrimSpace(body.Prompt) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "prompt is required"})
		return
	}
	if err := a.enforceGuestDailyCapability(r.Context(), "filegen", identity.Subject, p.FileGenerationsPerDay); err != nil {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "guest_file_generation_limit"})
		return
	}
	name := generatedName(body.Name, "generated.txt")
	mediaType := strings.TrimSpace(body.MediaType)
	if mediaType == "" {
		mediaType = mime.TypeByExtension(strings.ToLower(filepath.Ext(name)))
	}
	if mediaType == "" {
		mediaType = "text/plain; charset=utf-8"
	}
	prompt := "Create the requested file contents. Return ONLY the final file contents with no Markdown fences, no explanation, and no filename header. User request:\n" + strings.TrimSpace(body.Prompt)
	responseBody, upstream, err := a.runGuestCoreGeneration(r.Context(), identity, p, "file-generation", "guest", prompt)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "guest_file_generation_failed", "upstream": upstream})
		return
	}
	content := chatCompletionText(responseBody)
	data := []byte(content)
	if len(data) == 0 {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "guest_file_generation_empty"})
		return
	}
	if p.MaxGeneratedFileBytes > 0 && int64(len(data)) > p.MaxGeneratedFileBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "generated_file_exceeds_limit"})
		return
	}
	rec, err := a.storeGeneratedGuestAttachment(r.Context(), identity, p, name, "generated-file", mediaType, data)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "generated_file_storage_failed"})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"attachment": guestAttachmentPublic(rec), "upstream": upstream})
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
	prompt := "Generate an image for this request using the image_generate capability and return the generated image. Request: " + strings.TrimSpace(body.Prompt)
	responseBody, upstream, err := a.runGuestCoreGeneration(r.Context(), identity, p, "image-generation", "guest-media", prompt)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "image_generation_unavailable", "upstream": upstream})
		return
	}
	content := chatCompletionText(responseBody)
	match := generatedImageDataRE.FindStringSubmatch(content)
	if len(match) != 3 {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "image_generation_unavailable", "upstream": upstream})
		return
	}
	data, err := base64.StdEncoding.DecodeString(match[2])
	if err != nil || len(data) == 0 {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "generated_image_invalid"})
		return
	}
	if (p.MaxGeneratedImageBytes > 0 && int64(len(data)) > p.MaxGeneratedImageBytes) || int64(len(data)) > maxInjectedImageBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "generated_image_exceeds_limit"})
		return
	}
	ext := ".png"
	if exts, _ := mime.ExtensionsByType(match[1]); len(exts) > 0 {
		ext = exts[0]
	}
	name := generatedName(body.Name, "generated"+ext)
	if filepath.Ext(name) == "" {
		name += ext
	}
	rec, err := a.storeGeneratedGuestAttachment(r.Context(), identity, p, name, "generated-image", match[1], data)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "generated_image_storage_failed"})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"attachment": guestAttachmentPublic(rec), "upstream": upstream})
}
