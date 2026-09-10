package main

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/poom-ytthpch/daiki-ai-passport-backend/internal/store"
)

func cleanChatTitle(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return "New chat"
	}
	r := []rune(v)
	if len(r) > 80 {
		v = string(r[:80])
	}
	return v
}
func validChatModel(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	switch v {
	case "auto", "fast", "balanced", "deep":
		return v
	}
	return "auto"
}
func (a *app) listChatSessions(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if runes := []rune(query); len(runes) > 200 {
		query = string(runes[:200])
	}
	var (
		xs  []store.ChatSession
		err error
	)
	if query == "" {
		xs, err = a.store.ChatSessions(r.Context(), current(r).Sub, 100)
	} else {
		xs, err = a.store.SearchChatSessions(r.Context(), current(r).Sub, query, 100)
	}
	if err != nil {
		writeJSON(w, 503, map[string]string{"error": "chat history unavailable"})
		return
	}
	writeJSON(w, 200, map[string]any{"sessions": xs})
}
func (a *app) createChatSession(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Title      string `json:"title"`
		ModelAlias string `json:"modelAlias"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in)
	id, err := randomURLToken(12)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "unable to create chat"})
		return
	}
	x, err := a.store.CreateChatSession(r.Context(), store.ChatSession{ID: "chat_" + id, OwnerSubject: current(r).Sub, Title: cleanChatTitle(in.Title), ModelAlias: validChatModel(in.ModelAlias)})
	if err != nil {
		writeJSON(w, 503, map[string]string{"error": "unable to create chat"})
		return
	}
	writeJSON(w, http.StatusCreated, x)
}
func (a *app) getChatSession(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	x, err := a.store.ChatSession(r.Context(), current(r).Sub, id)
	if err != nil {
		if err == pgx.ErrNoRows {
			writeJSON(w, 404, map[string]string{"error": "chat not found"})
		} else {
			writeJSON(w, 503, map[string]string{"error": "chat history unavailable"})
		}
		return
	}
	ms, err := a.store.ChatMessages(r.Context(), current(r).Sub, id)
	if err != nil {
		writeJSON(w, 503, map[string]string{"error": "chat history unavailable"})
		return
	}
	runs, runErr := a.store.ChatRuns(r.Context(), current(r).Sub, id)
	if runErr != nil {
		writeJSON(w, 503, map[string]string{"error": "chat runs unavailable"})
		return
	}
	attachmentIDs := make([]string, 0)
	seenAttachments := map[string]bool{}
	for _, message := range ms {
		for _, attachmentID := range message.AttachmentIDs {
			if attachmentID != "" && !seenAttachments[attachmentID] {
				seenAttachments[attachmentID] = true
				attachmentIDs = append(attachmentIDs, attachmentID)
			}
		}
	}
	attachmentRows, attachmentErr := a.store.Attachments(r.Context(), current(r).Sub, attachmentIDs)
	if attachmentErr != nil {
		writeJSON(w, 503, map[string]string{"error": "chat attachments unavailable"})
		return
	}
	attachments := make([]map[string]any, 0, len(attachmentRows))
	for _, row := range attachmentRows {
		attachments = append(attachments, attachmentPublic(row))
	}
	writeJSON(w, 200, map[string]any{"session": x, "messages": ms, "runs": runs, "attachments": attachments})
}
func (a *app) updateChatSession(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Title      string `json:"title"`
		ModelAlias string `json:"modelAlias"`
		Pinned     *bool  `json:"pinned"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid chat payload"})
		return
	}
	model := ""
	if strings.TrimSpace(in.ModelAlias) != "" {
		model = validChatModel(in.ModelAlias)
	}
	title := ""
	if strings.TrimSpace(in.Title) != "" {
		title = cleanChatTitle(in.Title)
	}
	x, err := a.store.UpdateChatSession(r.Context(), current(r).Sub, chi.URLParam(r, "id"), title, model, in.Pinned)
	if err != nil {
		if err == pgx.ErrNoRows {
			writeJSON(w, 404, map[string]string{"error": "chat not found"})
		} else {
			writeJSON(w, 503, map[string]string{"error": "unable to update chat"})
		}
		return
	}
	writeJSON(w, 200, x)
}
func (a *app) deleteChatSession(w http.ResponseWriter, r *http.Request) {
	if err := a.store.DeleteChatSession(r.Context(), current(r).Sub, chi.URLParam(r, "id")); err != nil {
		if err == pgx.ErrNoRows {
			writeJSON(w, 404, map[string]string{"error": "chat not found"})
		} else {
			writeJSON(w, 503, map[string]string{"error": "unable to delete chat"})
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (a *app) addChatMessage(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Role          string   `json:"role"`
		Content       string   `json:"content"`
		AttachmentIDs []string `json:"attachmentIds"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20)).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid message payload"})
		return
	}
	in.Role = strings.ToLower(strings.TrimSpace(in.Role))
	if in.Role != "user" && in.Role != "assistant" {
		writeJSON(w, 400, map[string]string{"error": "role must be user or assistant"})
		return
	}
	if len([]rune(in.Content)) > 200000 {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "message too large"})
		return
	}
	m, err := a.store.AddChatMessage(r.Context(), current(r).Sub, chi.URLParam(r, "id"), in.Role, in.Content, in.AttachmentIDs)
	if err != nil {
		if err == pgx.ErrNoRows {
			writeJSON(w, 404, map[string]string{"error": "chat not found"})
		} else {
			writeJSON(w, 503, map[string]string{"error": "unable to save message"})
		}
		return
	}
	writeJSON(w, http.StatusCreated, m)
}
