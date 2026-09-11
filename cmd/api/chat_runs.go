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
	ResearchMode   string   `json:"researchMode"`
	ResearchRegion string   `json:"researchRegion"`
	ResearchLocale string   `json:"researchLocale"`
	ResearchScope  string   `json:"researchScope"`
	ResearchDepth  string   `json:"researchDepth"`
	ResearchFocus  string   `json:"researchFocus"`
	ThinkingMode   string   `json:"thinkingMode"`
	CommandMode    string   `json:"commandMode"`
	CommandSkills  []string `json:"commandSkills"`
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

func chatRunResearchPreferences(run store.ChatRun) researchPreferences {
	prefs := researchPreferences{Depth: "standard"}
	var activity map[string]any
	if len(run.Activity) == 0 || json.Unmarshal(run.Activity, &activity) != nil {
		return prefs
	}
	research, _ := activity["research"].(map[string]any)
	if research == nil {
		return prefs
	}
	prefs.Region = normalizeResearchRegion(research["region"])
	prefs.Locale = normalizeResearchLocale(research["locale"])
	prefs.Scope = normalizeResearchScope(research["scope"], prefs.Region)
	prefs.Depth = normalizeResearchDepth(research["depth"])
	prefs.Focus = clipText(strings.TrimSpace(fmt.Sprint(research["focus"])), 240)
	return prefs
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
	commandPayload, _ := json.Marshal(map[string]any{"messages": []any{map[string]any{"role": "user", "content": "command validation"}}, "commandMode": in.CommandMode, "commandSkills": in.CommandSkills})
	_, commandSelection, commandErr := applyChatCommands(commandPayload, false)
	if commandErr != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": commandErr.Error()})
		return
	}
	if commandSelection.Mode == "deep-search" {
		mode = "web"
	}
	region := normalizeResearchRegion(in.ResearchRegion)
	locale := normalizeResearchLocale(in.ResearchLocale)
	depth := normalizeResearchDepth(in.ResearchDepth)
	if commandSelection.Mode == "deep-search" {
		depth = "deep"
	}
	scope := normalizeResearchScope(in.ResearchScope, region)
	focus := clipText(strings.TrimSpace(in.ResearchFocus), 240)
	token, err := randomURLToken(12)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "unable to create chat run"})
		return
	}
	initial, _ := json.Marshal(map[string]any{"phase": "queued", "research": map[string]any{"mode": mode, "region": region, "locale": locale, "scope": scope, "depth": depth, "focus": focus, "phase": "queued"}, "thinking": map[string]any{"mode": thinking}, "commands": commandSelection})
	run, err := a.store.CreateChatRun(r.Context(), store.ChatRun{ID: "run_" + token, SessionID: sessionID, OwnerSubject: current(r).Sub, ResearchMode: mode, ThinkingMode: thinking, CommandMode: commandSelection.Mode, CommandSkills: commandSelection.Skills, Activity: initial})
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
	var recentAttachmentIDs []string
	lastUserText := ""
	for _, m := range messages {
		payloadMessages = append(payloadMessages, map[string]any{"role": m.Role, "content": m.Content})
		if m.Role == "user" {
			attachmentIDs = append([]string{}, m.AttachmentIDs...)
			if len(m.AttachmentIDs) > 0 {
				recentAttachmentIDs = append([]string{}, m.AttachmentIDs...)
			}
			lastUserText = m.Content
		}
	}
	if len(attachmentIDs) == 0 && len(recentAttachmentIDs) > 0 && looksAttachmentFollowUp(lastUserText) {
		attachmentIDs = recentAttachmentIDs
		_ = a.store.UpdateChatRunActivity(ctx, run.ID, map[string]any{"attachmentContextInherited": true})
	}
	profile := thinkingProfileFor(run.ThinkingMode)
	researchPrefs := chatRunResearchPreferences(run)
	researchActivity := map[string]any{"mode": run.ResearchMode, "query": clipText(lastUserText, 500), "region": researchPrefs.Region, "locale": researchPrefs.Locale, "scope": researchPrefs.Scope, "depth": researchPrefs.Depth, "focus": researchPrefs.Focus, "phase": "planning"}
	initialThinking := map[string]any{
		"mode": run.ThinkingMode, "policyScore": thinkingPolicyScore(profile), "taskClass": thinkingTaskClassFromText(lastUserText),
		"reasoningBudget": profile.ReasoningBudget, "analysisPasses": profile.AnalysisPasses, "verificationPasses": profile.VerificationPasses,
		"alternativePaths": profile.AlternativePaths, "constraintAudit": profile.ConstraintAudit, "counterexampleAudit": profile.CounterexampleAudit,
		"uncertaintyAudit": profile.UncertaintyAudit, "taskAdaptation": profile.TaskAdaptation,
	}
	_ = a.store.UpdateChatRunActivity(ctx, run.ID, map[string]any{"research": researchActivity, "thinking": initialThinking, "commands": map[string]any{"mode": run.CommandMode, "skills": run.CommandSkills}})
	payload := map[string]any{
		"model": session.ModelAlias, "researchMode": run.ResearchMode, "researchRegion": researchPrefs.Region, "researchLocale": researchPrefs.Locale,
		"researchScope": researchPrefs.Scope, "researchDepth": researchPrefs.Depth, "researchFocus": researchPrefs.Focus,
		"thinkingMode": run.ThinkingMode, "commandMode": run.CommandMode, "commandSkills": run.CommandSkills,
		"messages": payloadMessages, "attachmentIds": attachmentIDs, "stream": false,
	}
	invoke := func(body []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat", bytes.NewReader(body)).WithContext(ctx)
		req.Header.Set("x-daiki-chat-run-id", run.ID)
		req.Header.Set("x-daiki-chat-session-id", run.SessionID)
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
	out := map[string]any{"mode": meta.Mode, "query": meta.Query, "used": meta.Used, "error": meta.Error, "contextInherited": meta.ContextInherited, "region": meta.Region, "locale": meta.Locale, "scope": meta.Scope, "depth": meta.Depth, "focus": meta.Focus, "phase": meta.Phase, "localSourceCount": meta.LocalSourceCount, "globalSourceCount": meta.GlobalSourceCount, "socialSourceCount": meta.SocialSourceCount, "socialPlatforms": meta.SocialPlatforms, "qualityScore": meta.QualityScore, "qualityGrade": meta.QualityGrade}
	if meta.ResolvedQuery != "" && meta.ResolvedQuery != meta.Query {
		out["resolvedQuery"] = clipText(meta.ResolvedQuery, 900)
	}
	if len(meta.Queries) > 0 {
		queries := make([]string, 0, min(len(meta.Queries), 10))
		for _, query := range meta.Queries[:min(len(meta.Queries), 10)] {
			queries = append(queries, clipText(query, 300))
		}
		out["queries"] = queries
	}
	if len(meta.Sources) > 0 {
		sources := make([]map[string]any, 0, min(len(meta.Sources), 8))
		for _, source := range meta.Sources[:min(len(meta.Sources), 8)] {
			sources = append(sources, map[string]any{
				"index": source.Index, "title": source.Title, "url": source.URL, "engine": source.Engine, "region": source.Region, "authority": source.Authority, "sourceType": source.SourceType, "platform": source.Platform, "stage": source.Stage, "qualityScore": source.QualityScore, "relevanceScore": source.RelevanceScore, "freshnessScore": source.FreshnessScore, "authorityScore": source.AuthorityScore, "evidenceScore": source.EvidenceScore,
				"snippet": clipText(strings.TrimSpace(source.Snippet), 280),
			})
		}
		out["sources"] = sources
		out["sourceCount"] = len(sources)
	}
	return out
}

func (a *app) chatRunActivity(ctx context.Context, requestID string, headers http.Header, started, ended time.Time, prompt, completion, reasoning, total int64) map[string]any {
	thinkingActivity := map[string]any{
		"mode":            headers.Get("x-daiki-thinking-mode"),
		"requestedEffort": headers.Get("x-daiki-reasoning-requested"),
		"effectiveEffort": headers.Get("x-daiki-reasoning-effective"),
		"nativeReasoning": strings.EqualFold(headers.Get("x-daiki-reasoning-native"), "true"),
		"model":           headers.Get("x-daiki-reasoning-model"),
	}
	activity := map[string]any{
		"phase": "completed", "durationMs": ended.Sub(started).Milliseconds(), "requestId": requestID,
		"thinking": thinkingActivity,
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
		safe := map[string]any{
			"mode": research["mode"], "query": research["query"], "used": research["used"], "error": research["error"],
			"region": research["region"], "locale": research["locale"], "scope": research["scope"], "depth": research["depth"], "focus": research["focus"],
			"phase": "completed", "localSourceCount": research["localSourceCount"], "globalSourceCount": research["globalSourceCount"], "socialSourceCount": research["socialSourceCount"], "socialPlatforms": research["socialPlatforms"], "qualityScore": research["qualityScore"], "qualityGrade": research["qualityGrade"],
		}
		if value := research["resolvedQuery"]; value != nil {
			safe["resolvedQuery"] = value
		}
		if queries, ok := research["queries"].([]any); ok {
			out := make([]string, 0, min(len(queries), 10))
			for _, query := range queries[:min(len(queries), 10)] {
				out = append(out, clipText(strings.TrimSpace(fmt.Sprint(query)), 300))
			}
			safe["queries"] = out
		}
		if rows, ok := research["sources"].([]any); ok {
			sources := make([]map[string]any, 0, min(len(rows), 8))
			for _, row := range rows {
				m, ok := row.(map[string]any)
				if !ok {
					continue
				}
				sources = append(sources, map[string]any{"index": m["index"], "title": m["title"], "url": m["url"], "engine": m["engine"], "region": m["region"], "authority": m["authority"], "sourceType": m["sourceType"], "platform": m["platform"], "stage": m["stage"], "qualityScore": m["qualityScore"], "relevanceScore": m["relevanceScore"], "freshnessScore": m["freshnessScore"], "authorityScore": m["authorityScore"], "evidenceScore": m["evidenceScore"], "snippet": clipText(strings.TrimSpace(fmt.Sprint(m["snippet"])), 280)})
			}
			safe["sources"] = sources
			safe["sourceCount"] = len(sources)
		}
		activity["research"] = safe
	}
	if thinking, ok := meta["thinking"].(map[string]any); ok {
		for _, key := range []string{"mode", "policyScore", "taskClass", "reasoningBudget", "analysisPasses", "verificationPasses", "alternativePaths", "constraintAudit", "counterexampleAudit", "uncertaintyAudit", "taskAdaptation", "estimate"} {
			if value, exists := thinking[key]; exists {
				thinkingActivity[key] = value
			}
		}
	}
	if recovery, ok := meta["modelRecovery"].(map[string]any); ok {
		if value := strings.TrimSpace(fmt.Sprint(recovery["requestedReasoningEffort"])); value != "" {
			thinkingActivity["requestedEffort"] = value
		}
		if value := strings.TrimSpace(fmt.Sprint(recovery["effectiveReasoningEffort"])); value != "" {
			thinkingActivity["effectiveEffort"] = value
		}
		if value := strings.TrimSpace(fmt.Sprint(recovery["reasoningModel"])); value != "" {
			thinkingActivity["model"] = value
		}
		if native, exists := recovery["nativeReasoning"].(bool); exists {
			thinkingActivity["nativeReasoning"] = native
		}
	}
	activity["thinking"] = thinkingActivity
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
