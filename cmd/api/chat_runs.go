package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/poom-ytthpch/daiki-ai-passport-backend/internal/store"
)

var chatRunCancels sync.Map

type chatRunIdentity struct {
	Claims    claims
	User      store.User
	Principal principal
}

type chatRunStartInput struct {
	ResearchMode string `json:"researchMode"`
	ThinkingMode string `json:"thinkingMode"`
}

func captureChatRunIdentity(r *http.Request) chatRunIdentity {
	c := current(r)
	u, _ := currentUser(r)
	p := currentPrincipal(r)
	c.RealmAccess.Roles = append([]string{}, c.RealmAccess.Roles...)
	u.Roles = append([]string{}, u.Roles...)
	p.Scopes = append([]string{}, p.Scopes...)
	return chatRunIdentity{Claims: c, User: u, Principal: p}
}

func (a *app) startChatRun(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "id")
	if active, err := a.store.ActiveChatRun(r.Context(), current(r).Sub, sessionID); err == nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "chat_run_already_active", "run": active})
		return
	}
	var in chatRunStartInput
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in)
	mode := normalizeResearchMode(in.ResearchMode)
	if mode == "" {
		mode = "auto"
	}
	thinking := normalizeThinkingMode(in.ThinkingMode)
	token, err := randomURLToken(12)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "unable to create chat run"})
		return
	}
	initial, _ := json.Marshal(map[string]any{"phase": "queued", "research": map[string]any{"mode": mode}, "thinking": map[string]any{"mode": thinking}})
	run, err := a.store.CreateChatRun(r.Context(), store.ChatRun{ID: "run_" + token, SessionID: sessionID, OwnerSubject: current(r).Sub, ResearchMode: mode, ThinkingMode: thinking, Activity: initial})
	if err != nil {
		if err == pgx.ErrNoRows {
			writeJSON(w, 404, map[string]string{"error": "chat not found"})
		} else {
			writeJSON(w, 503, map[string]string{"error": "unable to create chat run"})
		}
		return
	}
	a.launchChatRun(run, captureChatRunIdentity(r))
	writeJSON(w, http.StatusAccepted, run)
}

func (a *app) latestChatRun(w http.ResponseWriter, r *http.Request) {
	run, err := a.store.LatestChatRun(r.Context(), current(r).Sub, chi.URLParam(r, "id"))
	if err != nil {
		if err == pgx.ErrNoRows {
			writeJSON(w, 404, map[string]string{"error": "chat run not found"})
		} else {
			writeJSON(w, 503, map[string]string{"error": "chat run unavailable"})
		}
		return
	}
	writeJSON(w, 200, run)
}
func (a *app) getChatRun(w http.ResponseWriter, r *http.Request) {
	run, err := a.store.ChatRun(r.Context(), current(r).Sub, chi.URLParam(r, "runID"))
	if err != nil {
		if err == pgx.ErrNoRows {
			writeJSON(w, 404, map[string]string{"error": "chat run not found"})
		} else {
			writeJSON(w, 503, map[string]string{"error": "chat run unavailable"})
		}
		return
	}
	writeJSON(w, 200, run)
}
func (a *app) controlChatRun(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runID")
	run, err := a.store.ChatRun(r.Context(), current(r).Sub, runID)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "chat run not found"})
		return
	}
	var in struct {
		Action string `json:"action"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in) != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid run action"})
		return
	}
	switch strings.ToLower(strings.TrimSpace(in.Action)) {
	case "pause":
		if run.Status != "running" && run.Status != "queued" {
			writeJSON(w, 409, map[string]string{"error": "run is not running"})
			return
		}
		run, err = a.store.SetChatRunStatus(r.Context(), current(r).Sub, runID, "paused")
		if cancel, ok := chatRunCancels.Load(runID); ok {
			cancel.(context.CancelFunc)()
		}
	case "resume", "play":
		if run.Status != "paused" && run.Status != "failed" && run.Status != "cancelled" {
			writeJSON(w, 409, map[string]string{"error": "run cannot be resumed"})
			return
		}
		run, err = a.store.ResetChatRun(r.Context(), current(r).Sub, runID)
		if err == nil {
			a.launchChatRun(run, captureChatRunIdentity(r))
		}
	case "cancel", "stop":
		run, err = a.store.SetChatRunStatus(r.Context(), current(r).Sub, runID, "cancelled")
		if cancel, ok := chatRunCancels.Load(runID); ok {
			cancel.(context.CancelFunc)()
		}
	default:
		writeJSON(w, 400, map[string]string{"error": "action must be pause, resume, or cancel"})
		return
	}
	if err != nil {
		writeJSON(w, 503, map[string]string{"error": "unable to update chat run"})
		return
	}
	writeJSON(w, 200, run)
}

func (a *app) launchChatRun(run store.ChatRun, identity chatRunIdentity) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	chatRunCancels.Store(run.ID, cancel)
	go func() {
		defer cancel()
		defer chatRunCancels.Delete(run.ID)
		_ = a.executeChatRun(ctx, run, identity)
	}()
}

func (a *app) executeChatRun(ctx context.Context, run store.ChatRun, identity chatRunIdentity) error {
	currentRun, err := a.store.SetChatRunStatus(ctx, run.OwnerSubject, run.ID, "running")
	if err != nil {
		return err
	}
	run = currentRun
	started := time.Now()
	_ = a.store.UpdateChatRunActivity(ctx, run.ID, map[string]any{"phase": "processing", "startedAt": started.UTC()})
	session, err := a.store.ChatSession(ctx, run.OwnerSubject, run.SessionID)
	if err != nil {
		return a.failBackgroundRun(run, started, "chat session unavailable", "")
	}
	messages, err := a.store.ChatMessages(ctx, run.OwnerSubject, run.SessionID)
	if err != nil {
		return a.failBackgroundRun(run, started, "chat history unavailable", "")
	}
	if len(messages) == 0 {
		return a.failBackgroundRun(run, started, "chat has no messages", "")
	}
	payloadMessages := make([]map[string]any, 0, len(messages))
	var attachmentIDs []string
	lastUserText := ""
	for _, m := range messages {
		payloadMessages = append(payloadMessages, map[string]any{"role": m.Role, "content": m.Content})
		if m.Role == "user" {
			attachmentIDs = append([]string{}, m.AttachmentIDs...)
			lastUserText = m.Content
		}
	}
	profile := thinkingProfileFor(run.ThinkingMode)
	_ = a.store.UpdateChatRunActivity(ctx, run.ID, map[string]any{"research": map[string]any{"mode": run.ResearchMode, "query": clipText(lastUserText, 500)}, "thinking": map[string]any{"mode": run.ThinkingMode, "reasoningBudget": profile.ReasoningBudget}})
	payload := map[string]any{"model": session.ModelAlias, "researchMode": run.ResearchMode, "thinkingMode": run.ThinkingMode, "messages": payloadMessages, "attachmentIds": attachmentIDs, "stream": false}
	invoke := func(body []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat", bytes.NewReader(body)).WithContext(ctx)
		req.Header.Set("x-daiki-chat-run-id", run.ID)
		req = req.WithContext(context.WithValue(req.Context(), claimsKey, identity.Claims))
		req = req.WithContext(context.WithValue(req.Context(), appUserKey, identity.User))
		req = req.WithContext(context.WithValue(req.Context(), principalKey, identity.Principal))
		rr := httptest.NewRecorder()
		a.proxyLiteLLM(rr, req, "/v1/chat/completions", false)
		return rr
	}
	raw, _ := json.Marshal(payload)
	rr := invoke(raw)
	if rr.Code == http.StatusBadRequest && unexpectedBlockedToolCall(rr.Body.Bytes()) {
		// Groq documents that some reasoning models may still try to call a tool
		// when tool_choice is none. Retry once with stronger textual steering.
		payloadMessages = append([]map[string]any{{"role": "system", "content": "PROVIDER COMPATIBILITY: Answer this request using normal text only. Do not call, invoke, request, or emit any tool/function. Do not use provider-native browser_search or code_interpreter. If a calculation is needed, estimate it from the supplied context and state assumptions."}}, payloadMessages...)
		payload["messages"] = payloadMessages
		raw, _ = json.Marshal(payload)
		_ = a.store.UpdateChatRunActivity(context.Background(), run.ID, map[string]any{"providerCompatibilityRetry": "unexpected_tool_call"})
		rr = invoke(raw)
	}
	requestID := rr.Header().Get("x-daiki-request-id")
	latest, _ := a.store.ChatRun(context.Background(), run.OwnerSubject, run.ID)
	if latest.Status == "paused" || latest.Status == "cancelled" {
		return nil
	}
	if ctx.Err() != nil {
		return a.failBackgroundRun(run, started, "run interrupted", requestID)
	}
	if rr.Code < 200 || rr.Code >= 300 {
		var e struct {
			Error             any           `json:"error"`
			Quota             quotaDecision `json:"quota"`
			RetryAfterSeconds int           `json:"retryAfterSeconds"`
		}
		_ = json.Unmarshal(rr.Body.Bytes(), &e)
		msg := chatRunErrorMessage(e.Error)
		if msg == "" || msg == "<nil>" {
			msg = "inference failed"
		}
		patch := map[string]any{"httpStatus": rr.Code}
		if e.Quota.Mode != "" {
			patch["quota"] = e.Quota
		}
		if e.RetryAfterSeconds > 0 {
			patch["retryAfterSeconds"] = e.RetryAfterSeconds
		}
		_ = a.store.UpdateChatRunActivity(context.Background(), run.ID, patch)
		return a.failBackgroundRun(run, started, msg, requestID)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
			Details          struct {
				ReasoningTokens int64 `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	if json.Unmarshal(rr.Body.Bytes(), &out) != nil || len(out.Choices) == 0 {
		return a.failBackgroundRun(run, started, "invalid model response", requestID)
	}
	answer := strings.TrimSpace(out.Choices[0].Message.Content)
	if answer == "" {
		return a.failBackgroundRun(run, started, "empty model response", requestID)
	}
	activity := a.chatRunActivity(context.Background(), requestID, rr.Header(), started, time.Now(), out.Usage.PromptTokens, out.Usage.CompletionTokens, out.Usage.Details.ReasoningTokens, out.Usage.TotalTokens)
	if _, err = a.store.AddChatMessageForRun(context.Background(), run.OwnerSubject, run.SessionID, "assistant", answer, []string{}, run.ID); err != nil {
		slog.Error("background run answer persistence failed", "run_id", run.ID, "request_id", requestID, "error", err)
		return a.failBackgroundRun(run, started, "unable to save answer", requestID)
	}
	_, err = a.store.CompleteChatRun(context.Background(), run.OwnerSubject, run.ID, requestID, answer, activity)
	return err
}

func unexpectedBlockedToolCall(body []byte) bool {
	text := strings.ToLower(string(body))
	return strings.Contains(text, "tool choice is none") &&
		(strings.Contains(text, "model called a tool") || strings.Contains(text, "called a tool"))
}

func chatRunErrorMessage(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case map[string]any:
		for _, key := range []string{"message", "detail", "error"} {
			if text := chatRunErrorMessage(v[key]); text != "" {
				return text
			}
		}
		if b, err := json.Marshal(v); err == nil {
			return string(b)
		}
	case nil:
		return ""
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
	return ""
}

func (a *app) failBackgroundRun(run store.ChatRun, started time.Time, message, requestID string) error {
	activity := map[string]any{"phase": "failed", "durationMs": time.Since(started).Milliseconds()}
	_, err := a.store.FailChatRun(context.Background(), run.OwnerSubject, run.ID, requestID, message, activity)
	return err
}

func safeRunResearchActivity(meta researchMetadata) map[string]any {
	out := map[string]any{"mode": meta.Mode, "query": meta.Query, "used": meta.Used, "error": meta.Error}
	if len(meta.Sources) > 0 {
		sources := make([]map[string]any, 0, min(len(meta.Sources), 8))
		for _, source := range meta.Sources {
			sources = append(sources, map[string]any{
				"index": source.Index, "title": source.Title, "url": source.URL, "engine": source.Engine,
				"snippet": clipText(strings.TrimSpace(source.Snippet), 280),
			})
		}
		out["sources"] = sources
		out["sourceCount"] = len(sources)
	}
	return out
}

func (a *app) chatRunActivity(ctx context.Context, requestID string, headers http.Header, started, ended time.Time, prompt, completion, reasoning, total int64) map[string]any {
	activity := map[string]any{
		"phase": "completed", "durationMs": ended.Sub(started).Milliseconds(), "requestId": requestID,
		"thinking": map[string]any{"mode": headers.Get("x-daiki-thinking-mode")},
		"tokens":   map[string]any{"input": prompt, "reasoning": reasoning, "answer": max(int64(0), completion-reasoning), "total": total},
	}
	if requestID == "" {
		return activity
	}
	raw, err := a.store.UsageMetadata(ctx, requestID)
	if err != nil {
		return activity
	}
	var meta map[string]any
	if json.Unmarshal(raw, &meta) != nil {
		return activity
	}
	if research, ok := meta["research"].(map[string]any); ok {
		safe := map[string]any{"mode": research["mode"], "query": research["query"], "used": research["used"], "error": research["error"]}
		if rows, ok := research["sources"].([]any); ok {
			sources := make([]map[string]any, 0, min(len(rows), 8))
			for _, row := range rows {
				m, ok := row.(map[string]any)
				if !ok {
					continue
				}
				sources = append(sources, map[string]any{"index": m["index"], "title": m["title"], "url": m["url"], "engine": m["engine"], "snippet": clipText(strings.TrimSpace(fmt.Sprint(m["snippet"])), 280)})
			}
			safe["sources"] = sources
			safe["sourceCount"] = len(sources)
		}
		activity["research"] = safe
	}
	if thinking, ok := meta["thinking"].(map[string]any); ok {
		activity["thinking"] = map[string]any{"mode": thinking["mode"], "reasoningBudget": thinking["reasoningBudget"], "estimate": thinking["estimate"]}
	}
	return activity
}

func (a *app) editChatMessage(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "id")
	messageID, err := strconv.ParseInt(chi.URLParam(r, "messageID"), 10, 64)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid message id"})
		return
	}
	var in struct {
		Content string `json:"content"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20)).Decode(&in) != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid message payload"})
		return
	}
	in.Content = strings.TrimSpace(in.Content)
	if in.Content == "" {
		writeJSON(w, 400, map[string]string{"error": "message cannot be empty"})
		return
	}
	if active, e := a.store.ActiveChatRun(r.Context(), current(r).Sub, sessionID); e == nil {
		_, _ = a.store.SetChatRunStatus(r.Context(), current(r).Sub, active.ID, "cancelled")
		if cancel, ok := chatRunCancels.Load(active.ID); ok {
			cancel.(context.CancelFunc)()
		}
	}
	m, err := a.store.EditUserChatMessageAndTruncate(r.Context(), current(r).Sub, sessionID, messageID, in.Content)
	if err != nil {
		if err == pgx.ErrNoRows {
			writeJSON(w, 404, map[string]string{"error": "user message not found"})
		} else {
			writeJSON(w, 503, map[string]string{"error": "unable to edit message"})
		}
		return
	}
	writeJSON(w, 200, m)
}
