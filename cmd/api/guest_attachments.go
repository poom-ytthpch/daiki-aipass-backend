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
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/poom-ytthpch/daiki-ai-passport-backend/internal/store"
)

func guestAttachmentPublic(a store.GuestAttachment) map[string]any {
	return map[string]any{
		"id": a.ID, "name": a.Name, "relativePath": a.RelativePath, "source": a.Source,
		"mediaType": a.MediaType, "sizeBytes": a.SizeBytes, "sha256": a.SHA256,
		"extractStatus": a.ExtractStatus, "createdAt": a.CreatedAt,
	}
}

func (a *app) cleanupExpiredGuestAttachments(ctx context.Context, p store.GuestAccessPolicy) {
	if a.store == nil || p.AttachmentRetentionHours <= 0 {
		return
	}
	rows, err := a.store.ExpireGuestAttachments(ctx, time.Now().UTC().Add(-time.Duration(p.AttachmentRetentionHours)*time.Hour), 200)
	if err != nil {
		return
	}
	for _, row := range rows {
		_ = os.Remove(row.StoragePath)
	}
}

func guestAttachmentDir(identity guestIdentity) string {
	subjectHash := sha256.Sum256([]byte(identity.Subject))
	deviceHash := sha256.Sum256([]byte(identity.DeviceID))
	return filepath.Join(attachmentRoot(), "guest", hex.EncodeToString(subjectHash[:8]), hex.EncodeToString(deviceHash[:8]))
}

var errGuestUploadRateExceeded = errors.New("guest upload rate exceeded")

func (a *app) enforceGuestUploadRate(ctx context.Context, subject string, limit int) error {
	if limit == 0 {
		return nil
	}
	if a.redis == nil {
		return errors.New("guest upload limiter unavailable")
	}
	key := "guest-upload:hour:" + subject
	value, err := a.redis.Incr(ctx, key).Result()
	if err != nil {
		return err
	}
	if value == 1 {
		_ = a.redis.Expire(ctx, key, time.Hour).Err()
	}
	if value > int64(limit) {
		return errGuestUploadRateExceeded
	}
	return nil
}

func guestUploadLimit(p store.GuestAccessPolicy, image bool) int64 {
	configured := p.MaxUploadBytes
	hard := maxAttachmentBytes
	if image {
		configured = p.MaxImageUploadBytes
		hard = maxInjectedImageBytes
	}
	if configured <= 0 || configured > hard {
		return hard
	}
	return configured
}

func (a *app) guestUploadAttachment(w http.ResponseWriter, r *http.Request) {
	if a.store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "guest attachments unavailable"})
		return
	}
	p, _, err := a.store.GuestAccessPolicy(r.Context())
	if err != nil || !p.Enabled || !p.AllowUploads {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "guest uploads disabled"})
		return
	}
	identity := guestIdentityForRequest(r)
	a.recordGuestIdentity(r.Context(), identity)
	a.cleanupExpiredGuestAttachments(r.Context(), p)

	if err := a.enforceGuestUploadRate(r.Context(), identity.Subject, p.MaxUploadsPerHour); err != nil {
		if errors.Is(err, errGuestUploadRateExceeded) {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "guest_upload_rate_limited"})
		} else {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "guest upload limit unavailable"})
		}
		return
	}
	files, bytes, err := a.store.GuestAttachmentUsage(r.Context(), identity.Subject)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "guest attachment quota unavailable"})
		return
	}
	if (p.MaxStoredFiles > 0 && files >= int64(p.MaxStoredFiles)) || (p.MaxStoredBytes > 0 && bytes >= p.MaxStoredBytes) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "guest_attachment_storage_limit"})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxMultipartRequestBytes)
	if err := r.ParseMultipartForm(maxMultipartRequestBytes); err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "guest attachment exceeds upload limit"})
		return
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing file"})
		return
	}
	defer func() { _ = f.Close() }()
	name := filepath.Base(strings.TrimSpace(hdr.Filename))
	if name == "." || name == "" {
		name = "attachment"
	}
	source := strings.ToLower(strings.TrimSpace(r.FormValue("source")))
	if source != "image" {
		source = "file"
	}
	mediaHint := strings.ToLower(hdr.Header.Get("Content-Type"))
	isImage := source == "image" || strings.HasPrefix(mediaHint, "image/")
	maxBytes := guestUploadLimit(p, isImage)
	if hdr.Size > maxBytes || (p.MaxStoredBytes > 0 && bytes+hdr.Size > p.MaxStoredBytes) {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "guest attachment exceeds upload or storage limit"})
		return
	}
	relativePath := cleanRelativePath(r.FormValue("relativePath"), name)
	id, err := newAttachmentID()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "attachment id generation failed"})
		return
	}
	dir := guestAttachmentDir(identity)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "guest attachment storage unavailable"})
		return
	}
	storagePath := filepath.Join(dir, id)
	out, err := os.OpenFile(storagePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "guest attachment storage unavailable"})
		return
	}
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(out, h), io.LimitReader(f, maxBytes+1))
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil || n > maxBytes || (p.MaxStoredBytes > 0 && bytes+n > p.MaxStoredBytes) {
		_ = os.Remove(storagePath)
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "guest attachment exceeds upload limit"})
		return
	}
	mediaType := hdr.Header.Get("Content-Type")
	if mediaType == "" || mediaType == "application/octet-stream" {
		mediaType = mime.TypeByExtension(strings.ToLower(filepath.Ext(name)))
	}
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	extractStatus, extractedText := extractAttachmentContent(storagePath, name, mediaType)
	rec, err := a.store.CreateGuestAttachment(r.Context(), store.GuestAttachment{
		ID: id, GuestSubject: identity.Subject, DeviceID: identity.DeviceID, Name: name,
		RelativePath: relativePath, Source: source, MediaType: mediaType, SizeBytes: n,
		SHA256: hex.EncodeToString(h.Sum(nil)), StoragePath: storagePath,
		ExtractStatus: extractStatus, ExtractedText: extractedText,
	})
	if err != nil {
		_ = os.Remove(storagePath)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "guest attachment metadata unavailable"})
		return
	}
	writeJSON(w, http.StatusCreated, guestAttachmentPublic(rec))
}

func (a *app) guestListAttachments(w http.ResponseWriter, r *http.Request) {
	p, _, err := a.store.GuestAccessPolicy(r.Context())
	if err != nil || !p.Enabled || !p.AllowUploads {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "guest uploads disabled"})
		return
	}
	identity := guestIdentityForRequest(r)
	a.recordGuestIdentity(r.Context(), identity)
	a.cleanupExpiredGuestAttachments(r.Context(), p)
	rows, err := a.store.GuestAttachmentList(r.Context(), identity.Subject, identity.DeviceID, p.MaxStoredFiles)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "guest attachments unavailable"})
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, guestAttachmentPublic(row))
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *app) guestDownloadAttachment(w http.ResponseWriter, r *http.Request) {
	identity := guestIdentityForRequest(r)
	rec, err := a.store.GuestAttachment(r.Context(), identity.Subject, identity.DeviceID, chi.URLParam(r, "id"))
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "guest attachment not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "guest attachment unavailable"})
		return
	}
	f, err := os.Open(rec.StoragePath)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "guest attachment content missing"})
		return
	}
	defer func() { _ = f.Close() }()
	w.Header().Set("Content-Type", rec.MediaType)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`inline; filename=%q`, rec.Name))
	w.Header().Set("Cache-Control", "private, no-store")
	_, _ = io.Copy(w, f)
}

func (a *app) guestDeleteAttachment(w http.ResponseWriter, r *http.Request) {
	identity := guestIdentityForRequest(r)
	rec, err := a.store.DeleteGuestAttachment(r.Context(), identity.Subject, identity.DeviceID, chi.URLParam(r, "id"))
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "guest attachment not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "guest attachment unavailable"})
		return
	}
	_ = os.Remove(rec.StoragePath)
	w.WriteHeader(http.StatusNoContent)
}

func (a *app) expandGuestChatAttachments(ctx context.Context, identity guestIdentity, body []byte, p store.GuestAccessPolicy) ([]byte, []expandedAttachment, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, nil, errors.New("invalid chat payload")
	}
	rawIDs, _ := payload["attachmentIds"].([]any)
	if len(rawIDs) == 0 {
		delete(payload, "attachmentIds")
		out, err := json.Marshal(payload)
		return out, nil, err
	}
	if !p.AllowUploads {
		return nil, nil, errors.New("guest attachments are disabled")
	}
	ids := make([]string, 0, len(rawIDs))
	for _, raw := range rawIDs {
		id, ok := raw.(string)
		if !ok || strings.TrimSpace(id) == "" {
			return nil, nil, errors.New("invalid guest attachment id")
		}
		ids = append(ids, strings.TrimSpace(id))
	}
	rows, err := a.store.GuestAttachments(ctx, identity.Subject, identity.DeviceID, ids)
	if err != nil {
		return nil, nil, errors.New("guest attachments unavailable")
	}
	if len(rows) != len(ids) {
		return nil, nil, errors.New("one or more guest attachments are missing")
	}
	for _, rec := range rows {
		isImage := strings.HasPrefix(strings.ToLower(rec.MediaType), "image/")
		if rec.SizeBytes > guestUploadLimit(p, isImage) {
			return nil, nil, fmt.Errorf("attachment %s exceeds the current Guest size policy", rec.Name)
		}
	}
	messages, _ := payload["messages"].([]any)
	if len(messages) == 0 {
		return nil, nil, errors.New("chat requires messages")
	}
	last, _ := messages[len(messages)-1].(map[string]any)
	if last == nil || last["role"] != "user" {
		return nil, nil, errors.New("guest attachments must be sent with a user message")
	}
	query := latestUserText(body)
	textBudget := maxAttachmentContextBytes
	contentParts := []any{}
	if content, ok := last["content"].(string); ok && content != "" {
		contentParts = append(contentParts, map[string]any{"type": "text", "text": content})
	}
	summary := make([]expandedAttachment, 0, len(rows))
	for _, rec := range rows {
		kind := "file"
		if strings.HasPrefix(strings.ToLower(rec.MediaType), "image/") {
			kind = "image"
		}
		if rec.ExtractedText == "" && rec.ExtractStatus == "stored" && !strings.HasPrefix(strings.ToLower(rec.MediaType), "image/") {
			status, text := extractAttachmentContent(rec.StoragePath, rec.Name, rec.MediaType)
			if status != "stored" {
				rec.ExtractStatus, rec.ExtractedText = status, text
				_ = a.store.UpdateGuestAttachmentExtraction(ctx, identity.Subject, identity.DeviceID, rec.ID, status, text)
			}
		}
		summary = append(summary, expandedAttachment{ID: rec.ID, Name: rec.Name, RelativePath: rec.RelativePath, MediaType: rec.MediaType, SizeBytes: rec.SizeBytes, Kind: kind})
		if rec.ExtractedText != "" && textBudget > 0 {
			limit := min(textBudget, maxAttachmentExcerptBytes)
			excerpt, clipped := attachmentExcerpt(rec.ExtractedText, query, limit)
			textBudget -= len(excerpt)
			context := fmt.Sprintf("\n\n--- Daiki attachment context ---\nFile: %s\nMedia type: %s\nExtraction: %s\n", rec.RelativePath, rec.MediaType, rec.ExtractStatus)
			if clipped || strings.Contains(rec.ExtractStatus, "truncated") {
				context += "Note: This is a bounded relevant excerpt, not the entire file. Do not claim unseen rows/pages were reviewed.\n"
			}
			context += "Content excerpt:\n" + excerpt
			contentParts = append(contentParts, map[string]any{"type": "text", "text": context})
			continue
		}
		if strings.HasPrefix(strings.ToLower(rec.MediaType), "image/") {
			if rec.SizeBytes > maxInjectedImageBytes {
				return nil, nil, fmt.Errorf("image %s is too large for model input; maximum is 4 MiB", rec.Name)
			}
			data, err := os.ReadFile(rec.StoragePath)
			if err != nil {
				return nil, nil, fmt.Errorf("image %s is unavailable", rec.Name)
			}
			contentParts = append(contentParts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:" + rec.MediaType + ";base64," + base64.StdEncoding.EncodeToString(data)}})
			continue
		}
		contentParts = append(contentParts, map[string]any{"type": "text", "text": "\n[Attached binary file: " + rec.RelativePath + "]"})
	}
	last["content"] = contentParts
	messages[len(messages)-1] = last
	payload["messages"] = messages
	delete(payload, "attachmentIds")
	out, err := json.Marshal(payload)
	return out, summary, err
}
