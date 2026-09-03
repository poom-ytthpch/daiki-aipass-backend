package main

import (
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

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/poom-ytthpch/daiki-ai-passport-backend/internal/store"
)

const (
	maxAttachmentBytes       = int64(25 << 20)
	maxMultipartRequestBytes = int64(30 << 20)
	maxExtractedTextBytes    = 1 << 20
	maxInjectedTextBytes     = 384 << 10
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
	defer f.Close()
	if hdr.Size > maxAttachmentBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "file exceeds 25 MiB limit"})
		return
	}
	name := filepath.Base(strings.TrimSpace(hdr.Filename))
	if name == "." || name == "" {
		name = "attachment"
	}
	source := strings.ToLower(strings.TrimSpace(r.FormValue("source")))
	if source != "image" && source != "folder" {
		source = "file"
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
	limited := io.LimitReader(f, maxAttachmentBytes+1)
	n, copyErr := io.Copy(io.MultiWriter(out, h), limited)
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil || n > maxAttachmentBytes {
		_ = os.Remove(storagePath)
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "attachment exceeds 25 MiB limit"})
		return
	}
	mediaType := hdr.Header.Get("Content-Type")
	if mediaType == "" || mediaType == "application/octet-stream" {
		mediaType = mime.TypeByExtension(strings.ToLower(filepath.Ext(name)))
	}
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	extractStatus := "stored"
	extractedText := ""
	if textAttachment(name, mediaType) {
		content, readErr := os.ReadFile(storagePath)
		if readErr == nil {
			if len(content) > maxExtractedTextBytes {
				content = content[:maxExtractedTextBytes]
				extractStatus = "text-truncated"
			} else {
				extractStatus = "text-ready"
			}
			extractedText = string(content)
		}
	}
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
	defer f.Close()
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
	if len(ids) > 100 {
		return nil, nil, errors.New("too many attachments; maximum is 100")
	}
	u, _ := currentUser(r)
	rows, err := a.store.Attachments(r.Context(), u.Subject, ids)
	if err != nil {
		return nil, nil, errors.New("attachments unavailable")
	}
	if len(rows) != len(ids) {
		return nil, nil, errors.New("one or more attachments are missing")
	}
	messages, _ := payload["messages"].([]any)
	if len(messages) == 0 {
		return nil, nil, errors.New("chat requires messages")
	}
	last, _ := messages[len(messages)-1].(map[string]any)
	if last == nil {
		return nil, nil, errors.New("invalid last message")
	}
	textBudget := maxInjectedTextBytes
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
		summary = append(summary, expandedAttachment{ID: rec.ID, Name: rec.Name, RelativePath: rec.RelativePath, MediaType: rec.MediaType, SizeBytes: rec.SizeBytes, Kind: kind})
		if rec.ExtractedText != "" && textBudget > 0 {
			text := rec.ExtractedText
			if len(text) > textBudget {
				text = text[:textBudget]
			}
			textBudget -= len(text)
			label := rec.RelativePath
			contentParts = append(contentParts, map[string]any{"type": "text", "text": "\n\n--- Attached file: " + label + " ---\n" + text})
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
		contentParts = append(contentParts, map[string]any{"type": "text", "text": "\n[Attached binary file: " + rec.RelativePath + " (stored, content parser not available yet)]"})
	}
	last["content"] = contentParts
	messages[len(messages)-1] = last
	payload["messages"] = messages
	delete(payload, "attachmentIds")
	out, err := json.Marshal(payload)
	return out, summary, err
}
