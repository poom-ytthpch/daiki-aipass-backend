package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/poom-ytthpch/daiki-ai-passport-backend/internal/inference"
	"github.com/poom-ytthpch/daiki-ai-passport-backend/internal/store"
)

var errGeneratedAttachmentTooLarge = errors.New("generated attachment exceeds configured size limit")
var errGeneratedAttachmentStorageLimit = errors.New("generated attachment storage limit exceeded")

func (a *app) runUserCoreGeneration(r *http.Request, capability, profile, prompt string) ([]byte, string, error) {
	ctx := r.Context()
	c := current(r)
	principal := currentPrincipal(r)
	alias, err := a.store.ModelAlias(ctx, "fast")
	if err != nil || alias.LiteLLMModelName == "" {
		return nil, "", errors.New("fast model unavailable")
	}
	decision, policy, err := a.quotaFor(r)
	if err != nil {
		return nil, "", errors.New("quota unavailable")
	}
	if !policyAllowsModel(policy, "fast") {
		return nil, "", errors.New("fast model is not allowed by quota policy")
	}
	body, _ := json.Marshal(map[string]any{
		"model":                 alias.LiteLLMModelName,
		"messages":              []map[string]any{{"role": "user", "content": prompt}},
		"max_completion_tokens": 4096,
		"stream":                false,
	})
	poolCtx, poolSelection := a.selectInitialFreePoolModel(ctx, "fast", alias.LiteLLMModelName)
	physicalModel := alias.LiteLLMModelName
	if poolSelection.Model != "" {
		body = setRequestModel(body, poolSelection.Model)
		physicalModel = poolSelection.Model
	}
	reserved := reservationTokens(body)
	requestID := fmt.Sprintf("user-%s-%d", strings.ReplaceAll(capability, ":", "-"), time.Now().UnixNano())
	if err := a.reserveQuota(ctx, requestID, decision, reserved); err != nil {
		return nil, "", err
	}
	if err := a.store.StartUsageForPrincipal(ctx, requestID, c.Sub, principal.APIKeyID, "fast", string(inference.WorkloadFast), reserved); err != nil {
		a.releaseReservation(ctx, requestID, decision, reserved)
		return nil, "", errors.New("usage ledger unavailable")
	}
	_ = a.store.MergeUsageMetadata(ctx, requestID, map[string]any{
		"authKind": principal.AuthKind, "generatedCapability": capability, "resolvedAlias": "fast",
		"physicalModel": physicalModel, "pool": poolSelection, "generatedPrompt": guestActivityText(prompt, 32<<10),
	})
	principalID := "user:" + c.Sub
	if principal.APIKeyID != "" {
		principalID = "api_key:" + principal.APIKeyID
	}
	if a.queue == nil {
		a.releaseReservation(ctx, requestID, decision, reserved)
		_ = a.store.FinishUsage(ctx, requestID, "failed", store.Usage{})
		return nil, "", errors.New("inference queue unavailable")
	}
	ticket, err := a.queue.Acquire(ctx, requestID, principalID, inference.WorkloadFast, 0)
	if err != nil {
		a.releaseReservation(context.Background(), requestID, decision, reserved)
		_ = a.store.FinishUsage(context.Background(), requestID, "failed", store.Usage{})
		return nil, "", err
	}
	defer ticket.Release(context.Background())

	upstreamURL, upstreamKey, upstreamName := a.authenticatedHermesUpstream("/v1/chat/completions", profile)
	makeReq := func(payload []byte) (*http.Request, error) {
		req, buildErr := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, strings.NewReader(string(payload)))
		if buildErr != nil {
			return nil, buildErr
		}
		req.Header.Set("content-type", "application/json")
		if upstreamKey != "" {
			req.Header.Set("authorization", "Bearer "+upstreamKey)
		}
		req.Header.Set("x-daiki-request-id", requestID)
		req.Header.Set("x-daiki-principal", principal.AuthKind)
		baseKey := hermesSessionKey(r)
		if baseKey != "" {
			baseKey += ":filegen:p:" + profile
		}
		applyHermesSessionScope(req, baseKey, payload)
		return req, nil
	}
	resp, _, recoveredModel, recovery, err := a.doModelRequestWithRecovery(poolCtx, body, physicalModel, profile, makeReq)
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
		return nil, upstreamName, errors.New("generation returned no response")
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
	_ = a.store.MergeUsageMetadata(context.Background(), requestID, usageResponseMetadata(responseBody))
	_ = a.store.FinishUsage(context.Background(), requestID, status, usage)
	a.releaseReservation(context.Background(), requestID, decision, reserved)
	if readErr != nil {
		return nil, upstreamName, readErr
	}
	if resp.StatusCode >= 400 {
		return responseBody, upstreamName, fmt.Errorf("generation upstream returned status %d", resp.StatusCode)
	}
	if providerFailureStatus(responseBody) != 0 {
		return responseBody, upstreamName, errors.New("generation provider failed")
	}
	return responseBody, upstreamName, nil
}

func (a *app) storeGeneratedUserAttachment(ctx context.Context, subject string, policy store.Policy, name, source, mediaType string, data []byte) (store.Attachment, error) {
	resource := decodeResourceLimits(policy)
	files, bytesUsed, err := a.store.AttachmentUsage(ctx, subject)
	if err != nil {
		return store.Attachment{}, err
	}
	if (resource.MaxStoredFiles > 0 && files >= int64(resource.MaxStoredFiles)) || (resource.MaxStoredBytes > 0 && bytesUsed+int64(len(data)) > resource.MaxStoredBytes) {
		return store.Attachment{}, errGeneratedAttachmentStorageLimit
	}
	configuredMax, hardMax := resource.MaxFileBytes, maxAttachmentBytes
	if source == "generated-image" || strings.HasPrefix(strings.ToLower(mediaType), "image/") {
		configuredMax, hardMax = resource.MaxImageBytes, int64(maxInjectedImageBytes)
	}
	maxBytes := attachmentPolicyLimit(configuredMax, hardMax)
	if int64(len(data)) > maxBytes {
		return store.Attachment{}, errGeneratedAttachmentTooLarge
	}
	id, err := newAttachmentID()
	if err != nil {
		return store.Attachment{}, err
	}
	ownerHash := sha256.Sum256([]byte(subject))
	dir := filepath.Join(attachmentRoot(), hex.EncodeToString(ownerHash[:8]))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return store.Attachment{}, err
	}
	storagePath := filepath.Join(dir, id)
	if err := os.WriteFile(storagePath, data, 0o640); err != nil {
		return store.Attachment{}, err
	}
	h := sha256.Sum256(data)
	extractStatus, extractedText := extractAttachmentContent(storagePath, name, mediaType)
	rec, err := a.store.CreateAttachment(ctx, store.Attachment{
		ID: id, OwnerSubject: subject, Name: name, RelativePath: name, Source: source,
		MediaType: mediaType, SizeBytes: int64(len(data)), SHA256: hex.EncodeToString(h[:]),
		StoragePath: storagePath, ExtractStatus: extractStatus, ExtractedText: extractedText,
	})
	if err != nil {
		_ = os.Remove(storagePath)
	}
	return rec, err
}

func (a *app) generateFile(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(r)
	if !ok || u.Status != "approved" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "file generation requires approved access"})
		return
	}
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
	responseBody, upstream, err := a.runUserCoreGeneration(r, "file-generation:"+spec.Format, "skills", generatedFilePrompt(spec, body.Prompt))
	if err != nil {
		status := http.StatusBadGateway
		code := "file_generation_failed"
		if strings.Contains(strings.ToLower(err.Error()), "quota exhausted") {
			status, code = http.StatusTooManyRequests, "quota_exhausted"
		}
		writeJSON(w, status, map[string]any{"error": code, "upstream": upstream})
		return
	}
	content := chatCompletionText(responseBody)
	data, err := normalizeGeneratedFile(spec, content)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "file_generation_invalid", "detail": err.Error(), "format": spec.Format})
		return
	}
	_, policy, err := a.quotaFor(r)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "file generation policy unavailable"})
		return
	}
	rec, err := a.storeGeneratedUserAttachment(r.Context(), u.Subject, policy, spec.Name, "generated-file", spec.MediaType, data)
	if err != nil {
		switch {
		case errors.Is(err, errGeneratedAttachmentTooLarge):
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "generated_file_exceeds_limit"})
		case errors.Is(err, errGeneratedAttachmentStorageLimit):
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "attachment_storage_limit"})
		default:
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "generated_file_storage_failed"})
		}
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"attachment": attachmentPublic(rec), "upstream": upstream, "format": spec.Format})
}
