package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
)

func (a *app) generateImage(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(r)
	if !ok || u.Status != "approved" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "image generation requires approved access"})
		return
	}
	var body struct {
		Prompt string `json:"prompt"`
		Name   string `json:"name"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body) != nil || strings.TrimSpace(body.Prompt) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "prompt is required"})
		return
	}

	_, policy, err := a.quotaFor(r)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "image generation policy unavailable"})
		return
	}

	prompt := strings.TrimSpace(body.Prompt)
	data, mediaType, upstream, err := a.generateImageViaOpenRouter(r.Context(), prompt)
	if err != nil && !errors.Is(err, errImageProviderAuth) {
		// Keep the fallback isolated to Hermes' image-only profile. The primary
		// path is the configured OpenRouter Images provider above.
		responseBody, hermesUpstream, hermesErr := a.runUserCoreGeneration(r, "image-generation", "guest-media", "Generate an image for this request using the image_generate capability and return the generated image as a data URL. Request: "+prompt)
		if hermesErr == nil {
			if decoded, detectedType, decodeErr := decodeGeneratedImageFromChatCompletion(responseBody); decodeErr == nil {
				data, mediaType, upstream, err = decoded, detectedType, hermesUpstream, nil
			}
		}
	}
	if err != nil || len(data) == 0 {
		code := "image_generation_unavailable"
		if errors.Is(err, errImageProviderAuth) {
			code = "image_generation_provider_auth_failed"
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": code, "upstream": upstream})
		return
	}

	detectedType, detectErr := detectGeneratedImageMediaType(data)
	if detectErr != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "generated_image_invalid"})
		return
	}
	mediaType = detectedType
	ext := generatedImageExtension(mediaType)
	name := generatedName(body.Name, "generated"+ext)
	if filepath.Ext(name) == "" {
		name += ext
	}
	rec, err := a.storeGeneratedUserAttachment(r.Context(), u.Subject, policy, name, "generated-image", mediaType, data)
	if err != nil {
		switch {
		case errors.Is(err, errGeneratedAttachmentTooLarge):
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "generated_image_exceeds_limit"})
		case errors.Is(err, errGeneratedAttachmentStorageLimit):
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "attachment_storage_limit"})
		default:
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "generated_image_storage_failed"})
		}
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"attachment": attachmentPublic(rec), "upstream": upstream})
}
