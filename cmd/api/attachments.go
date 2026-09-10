package main

import (
	"context"
	"crypto/rand"
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
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/poom-ytthpch/daiki-ai-passport-backend/internal/store"
)

const (
	maxAttachmentBytes       = int64(25 << 20)
	maxMultipartRequestBytes = int64(30 << 20)
	maxInjectedImageBytes    = 4 << 20
)

func attachmentRoot() string {
	if v := strings.TrimSpace(os.Getenv("ATTACHMENT_DIR")); v != "" {
		return v
	}
	return "/data/attachments"
}

func newAttachmentID() (string, error) {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "att_" + hex.EncodeToString(buf), nil
}

func cleanRelativePath(v, fallback string) string {
	v = strings.TrimSpace(strings.ReplaceAll(v, "\\", "/"))
	if v == "" {
		v = fallback
	}
	v = strings.TrimPrefix(path.Clean("/"+v), "/")
	if v == "." || v == "" {
		return fallback
	}
	return v
}

func textAttachment(name, mediaType string) bool {
	if strings.HasPrefix(strings.ToLower(mediaType), "text/") {
		return true
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".txt", ".md", ".mdx", ".csv", ".tsv", ".json", ".jsonl", ".yaml", ".yml", ".xml", ".html", ".css", ".scss", ".less", ".js", ".jsx", ".ts", ".tsx", ".py", ".go", ".rs", ".java", ".kt", ".swift", ".c", ".cc", ".cpp", ".h", ".hpp", ".sql", ".sh", ".bash", ".zsh", ".toml", ".ini", ".conf", ".env", ".log", ".graphql", ".gql":
		return true
	default:
		return false
	}
}

var errAttachmentUploadRateExceeded = errors.New("attachment upload rate exceeded")

func decodeResourceLimits(p store.Policy) store.ResourceLimits {
	var limits store.ResourceLimits
	if len(p.ResourceLimits) > 0 {
		_ = json.Unmarshal(p.ResourceLimits, &limits)
	}
	return limits
}

func attachmentPolicyLimit(configured, hard int64) int64 {
	if configured <= 0 || configured > hard {
		return hard
	}
	return configured
}

func attachmentCountLimit(configured, hard int) int {
	if configured <= 0 || configured > hard {
		return hard
	}
	return configured
}

func (a *app) enforceAttachmentUploadRate(ctx context.Context, subject string, limit int) error {
	if limit == 0 {
		return nil
	}
	if a.redis == nil {
		return errors.New("attachment upload limiter unavailable")
	}
	key := "attachment-upload:hour:user:" + subject
	value, err := a.redis.Incr(ctx, key).Result()
	if err != nil {
		return err
	}
	if value == 1 {
		_ = a.redis.Expire(ctx, key, time.Hour).Err()
	}
	if value > int64(limit) {
		return errAttachmentUploadRateExceeded
	}
	return nil
}

func attachmentPublic(a store.Attachment) map[string]any {
	return map[string]any{
		"id": a.ID, "name": a.Name, "relativePath": a.RelativePath, "source": a.Source,
		"mediaType": a.MediaType, "sizeBytes": a.SizeBytes, "sha256": a.SHA256,
		"extractStatus": a.ExtractStatus, "createdAt": a.CreatedAt,
	}
}

func (a *app) uploadAttachment(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(r)
	if !ok || u.Status != "approved" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "file upload requires approved access"})
		return
	}
	policy, _, err := a.effectiveUserQuota(r.Context(), u.Subject)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "upload policy unavailable"})
		return
	}
	resource := decodeResourceLimits(policy)
	if err := a.enforceAttachmentUploadRate(r.Context(), u.Subject, resource.MaxUploadsPerHour); err != nil {
		if errors.Is(err, errAttachmentUploadRateExceeded) {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "upload_rate_limited"})
		} else {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "upload limiter unavailable"})
		}
		return
	}
	storedFiles, storedBytes, err := a.store.AttachmentUsage(r.Context(), u.Subject)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "attachment quota unavailable"})
		return
	}
	if (resource.MaxStoredFiles > 0 && storedFiles >= int64(resource.MaxStoredFiles)) || (resource.MaxStoredBytes > 0 && storedBytes >= resource.MaxStoredBytes) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "attachment_storage_limit"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxMultipartRequestBytes)
	if err := r.ParseMultipartForm(maxMultipartRequestBytes); err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "attachment exceeds upload limit"})
		return
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "missing file"})
		return
	}
	defer func() { _ = f.Close() }()
	name := filepath.Base(strings.TrimSpace(hdr.Filename))
	if name == "." || name == "" {
		name = "attachment"
	}
	source := strings.ToLower(strings.TrimSpace(r.FormValue("source")))
	if source != "image" && source != "folder" {
		source = "file"
	}
	mediaHint := strings.ToLower(hdr.Header.Get("Content-Type"))
	isImage := source == "image" || strings.HasPrefix(mediaHint, "image/")
	hardLimit := maxAttachmentBytes
	configuredLimit := resource.MaxFileBytes
	if isImage {
		hardLimit = maxInjectedImageBytes
		configuredLimit = resource.MaxImageBytes
	}
	maxBytes := attachmentPolicyLimit(configuredLimit, hardLimit)
	if hdr.Size > maxBytes || (resource.MaxStoredBytes > 0 && storedBytes+hdr.Size > resource.MaxStoredBytes) {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "attachment exceeds configured size or storage limit"})
		return
	}
	relativePath := cleanRelativePath(r.FormValue("relativePath"), name)
	id, err := newAttachmentID()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "attachment id generation failed"})
		return
	}
	ownerHash := sha256.Sum256([]byte(u.Subject))
	dir := filepath.Join(attachmentRoot(), hex.EncodeToString(ownerHash[:8]))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		writeJSON(w, 503, map[string]string{"error": "attachment storage unavailable"})
		return
	}
	storagePath := filepath.Join(dir, id)
	out, err := os.OpenFile(storagePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		writeJSON(w, 503, map[string]string{"error": "attachment storage unavailable"})
		return
	}
	h := sha256.New()
	limited := io.LimitReader(f, maxBytes+1)
	n, copyErr := io.Copy(io.MultiWriter(out, h), limited)
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil || n > maxBytes || (resource.MaxStoredBytes > 0 && storedBytes+n > resource.MaxStoredBytes) {
		_ = os.Remove(storagePath)
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "attachment exceeds configured size or storage limit"})
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
	rec, err := a.store.CreateAttachment(r.Context(), store.Attachment{
		ID: id, OwnerSubject: u.Subject, Name: name, RelativePath: relativePath, Source: source,
		MediaType: mediaType, SizeBytes: n, SHA256: hex.EncodeToString(h.Sum(nil)), StoragePath: storagePath,
		ExtractStatus: extractStatus, ExtractedText: extractedText,
	})
	if err != nil {
		_ = os.Remove(storagePath)
		writeJSON(w, 503, map[string]string{"error": "attachment metadata unavailable"})
		return
	}
	writeJSON(w, http.StatusCreated, attachmentPublic(rec))
}

func (a *app) listAttachments(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(r)
	if !ok || u.Status != "approved" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "attachment access requires approved access"})
		return
	}
	rows, err := a.store.UserAttachments(r.Context(), u.Subject, 200)
	if err != nil {
		writeJSON(w, 503, map[string]string{"error": "attachments unavailable"})
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, attachmentPublic(row))
	}
	writeJSON(w, 200, out)
}

func (a *app) downloadAttachment(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(r)
	if !ok || u.Status != "approved" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "attachment access requires approved access"})
		return
	}
	rec, err := a.store.Attachment(r.Context(), u.Subject, chi.URLParam(r, "id"))
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, 404, map[string]string{"error": "attachment not found"})
		return
	}
	if err != nil {
		writeJSON(w, 503, map[string]string{"error": "attachment unavailable"})
		return
	}
	f, err := os.Open(rec.StoragePath)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "attachment content missing"})
		return
	}
	defer func() { _ = f.Close() }()
	w.Header().Set("Content-Type", rec.MediaType)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`inline; filename=%q`, rec.Name))
	w.Header().Set("Cache-Control", "private, max-age=60")
	_, _ = io.Copy(w, f)
}

func (a *app) deleteAttachment(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(r)
	if !ok || u.Status != "approved" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "attachment access requires approved access"})
		return
	}
	rec, err := a.store.DeleteAttachment(r.Context(), u.Subject, chi.URLParam(r, "id"))
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, 404, map[string]string{"error": "attachment not found"})
		return
	}
	if err != nil {
		writeJSON(w, 503, map[string]string{"error": "attachment unavailable"})
		return
	}
	_ = os.Remove(rec.StoragePath)
	w.WriteHeader(http.StatusNoContent)
}

type expandedAttachment struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	RelativePath string `json:"relativePath"`
	MediaType    string `json:"mediaType"`
	SizeBytes    int64  `json:"sizeBytes"`
	Kind         string `json:"kind"`
}

func (a *app) expandChatAttachments(r *http.Request, body []byte) ([]byte, []expandedAttachment, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, nil, errors.New("invalid chat payload")
	}
	rawIDs, _ := payload["attachmentIds"].([]any)
	if len(rawIDs) == 0 {
		delete(payload, "attachmentIds")
		out, _ := json.Marshal(payload)
		return out, nil, nil
	}
	ids := make([]string, 0, len(rawIDs))
	seen := map[string]bool{}
	for _, raw := range rawIDs {
		id, _ := raw.(string)
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	u, _ := currentUser(r)
	policy, _, policyErr := a.effectiveUserQuota(r.Context(), u.Subject)
	if policyErr != nil {
		return nil, nil, errors.New("attachment policy unavailable")
	}
	resource := decodeResourceLimits(policy)
	maxCount := attachmentCountLimit(resource.MaxAttachmentsPerMessage, 100)
	if len(ids) > maxCount {
		return nil, nil, fmt.Errorf("too many attachments; maximum is %d", maxCount)
	}
	rows, err := a.store.Attachments(r.Context(), u.Subject, ids)
	if err != nil {
		return nil, nil, errors.New("attachments unavailable")
	}
	if len(rows) != len(ids) {
		return nil, nil, errors.New("one or more attachments are missing")
	}
	for _, rec := range rows {
		isImage := strings.HasPrefix(strings.ToLower(rec.MediaType), "image/")
		hardLimit := maxAttachmentBytes
		configuredLimit := resource.MaxFileBytes
		if isImage {
			hardLimit = maxInjectedImageBytes
			configuredLimit = resource.MaxImageBytes
		}
		if rec.SizeBytes > attachmentPolicyLimit(configuredLimit, hardLimit) {
			return nil, nil, fmt.Errorf("attachment %s exceeds the current admin size policy", rec.Name)
		}
	}
	messages, _ := payload["messages"].([]any)
	if len(messages) == 0 {
		return nil, nil, errors.New("chat requires messages")
	}
	last, _ := messages[len(messages)-1].(map[string]any)
	if last == nil {
		return nil, nil, errors.New("invalid last message")
	}
	query := latestUserText(body)
	textBudget := maxAttachmentContextBytes
	contentParts := []any{}
	switch content := last["content"].(type) {
	case string:
		if content != "" {
			contentParts = append(contentParts, map[string]any{"type": "text", "text": content})
		}
	case []any:
		contentParts = append(contentParts, content...)
	}
	summary := []expandedAttachment{}
	for _, rec := range rows {
		kind := "file"
		if strings.HasPrefix(strings.ToLower(rec.MediaType), "image/") {
			kind = "image"
		}
		if rec.Source == "folder" {
			kind = "folder-file"
		}
		if rec.ExtractedText == "" && rec.ExtractStatus == "stored" && !strings.HasPrefix(strings.ToLower(rec.MediaType), "image/") {
			status, text := extractAttachmentContent(rec.StoragePath, rec.Name, rec.MediaType)
			if status != "stored" {
				rec.ExtractStatus, rec.ExtractedText = status, text
				_ = a.store.UpdateAttachmentExtraction(r.Context(), u.Subject, rec.ID, status, text)
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
			url := "data:" + rec.MediaType + ";base64," + base64.StdEncoding.EncodeToString(data)
			contentParts = append(contentParts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
			continue
		}
		contentParts = append(contentParts, map[string]any{"type": "text", "text": "\n[Attached file: " + rec.RelativePath + " · extraction=" + rec.ExtractStatus + " · no text content available]"})
	}
	last["content"] = contentParts
	messages[len(messages)-1] = last
	payload["messages"] = messages
	delete(payload, "attachmentIds")
	out, err := json.Marshal(payload)
	return out, summary, err
}
