package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/poom-ytthpch/daiki-ai-passport-backend/internal/store"
)

func TestNormalizeResearchMode(t *testing.T) {
	cases := map[any]string{
		"web":      "web",
		"research": "web",
		"off":      "off",
		"auto":     "auto",
		"":         "auto",
		nil:        "auto",
	}
	for in, want := range cases {
		if got := normalizeResearchMode(in); got != want {
			t.Fatalf("normalizeResearchMode(%v)=%q want %q", in, got, want)
		}
	}
}

func TestAutoResearchSignals(t *testing.T) {
	for _, q := range []string{
		"What is the latest LiteLLM release?",
		"ช่วยค้นราคา GPU ล่าสุดให้หน่อย",
		"compare current vLLM and Ollama",
		"thai aipass",
		"https://aipass.go.th/",
	} {
		if !shouldAutoResearch(q) {
			t.Fatalf("expected research for %q", q)
		}
	}
	for _, q := range []string{"write a haiku", "2+2 เท่ากับเท่าไหร่", "explain a binary tree"} {
		if shouldAutoResearch(q) {
			t.Fatalf("did not expect research for %q", q)
		}
	}
}

func TestAttachmentReviewDoesNotTriggerUnrelatedAutoResearch(t *testing.T) {
	payload := map[string]any{
		"attachmentIds": []any{"att_image"},
		"messages":      []any{map[string]any{"role": "user", "content": "Please review the attached content."}},
	}
	query, _, useWeb, _, _ := contextualResearchPlan(payload, "auto")
	if query != "Please review the attached content." || useWeb {
		t.Fatalf("generic attachment review must stay grounded in the attachment, query=%q useWeb=%v", query, useWeb)
	}
}

func TestAttachmentCanStillRequestExplicitWebResearch(t *testing.T) {
	payload := map[string]any{
		"attachmentIds": []any{"att_image"},
		"messages":      []any{map[string]any{"role": "user", "content": "Review the attached image and search the web for current safety information."}},
	}
	_, _, useWeb, _, _ := contextualResearchPlan(payload, "auto")
	if !useWeb {
		t.Fatal("explicit web research with an attachment must remain enabled")
	}
}

func TestExtractResearchURLs(t *testing.T) {
	got := extractResearchURLs("อ่าน https://aipass.go.th/ ให้หน่อย และ https://example.com/docs).")
	if len(got) != 2 || got[0] != "https://aipass.go.th/" || got[1] != "https://example.com/docs" {
		t.Fatalf("unexpected URLs %#v", got)
	}
}
func TestPreferredResearchURLsForAIPass(t *testing.T) {
	for _, query := range []string{"thai aipass", "TH-AI Passport", "https://aipass.go.th"} {
		got := preferredResearchURLs(query)
		if len(got) != 1 || got[0] != "https://aipass.go.th/" {
			t.Fatalf("preferredResearchURLs(%q)=%#v", query, got)
		}
	}
}
func TestResearchQueryVariantsForAIPass(t *testing.T) {
	variants := researchQueryVariants("thai aipass")
	joined := strings.Join(variants, "\n")
	if !strings.Contains(joined, `site:aipass.go.th "TH-AI Passport"`) {
		t.Fatalf("missing authoritative fallback query: %#v", variants)
	}
}
func TestWebCapabilityQuestion(t *testing.T) {
	for _, q := range []string{
		"ตอนนี้เข้า internet ได้ยัง",
		"Daiki เข้าถึงเว็บได้ไหม",
		"does web access work?",
	} {
		if !isWebCapabilityQuestion(q) {
			t.Fatalf("expected web capability intent for %q", q)
		}
	}
	for _, q := range []string{"explain how the internet works", "เขียนเว็บด้วย React"} {
		if isWebCapabilityQuestion(q) {
			t.Fatalf("did not expect capability intent for %q", q)
		}
	}
}

func TestLastUserText(t *testing.T) {
	payload := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": "rules"},
		map[string]any{"role": "assistant", "content": "hello"},
		map[string]any{"role": "user", "content": "latest qwen release"},
	}}
	if got := lastUserText(payload); got != "latest qwen release" {
		t.Fatalf("got %q", got)
	}
}

func TestResearchOffStillAddsSafeReasoningInstruction(t *testing.T) {
	a := &app{cfg: config{}}
	body, meta, err := a.enrichChatWithResearch(t.Context(), []byte(`{"model":"auto","researchMode":"off","messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if meta.Mode != "off" || meta.Used {
		t.Fatalf("unexpected metadata %#v", meta)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if _, exists := payload["researchMode"]; exists {
		t.Fatal("researchMode must not be forwarded to LiteLLM")
	}
	messages, _ := payload["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("expected system + user messages, got %#v", messages)
	}
	first, _ := messages[0].(map[string]any)
	if first["role"] != "system" {
		t.Fatalf("expected system instruction, got %#v", first)
	}
}

func TestPublicIPGuard(t *testing.T) {
	blocked := []string{"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.1.1", "169.254.169.254", "0.0.0.0", "::1"}
	for _, raw := range blocked {
		if publicIP(net.ParseIP(raw)) {
			t.Fatalf("unsafe address allowed: %s", raw)
		}
	}
	for _, raw := range []string{"1.1.1.1", "8.8.8.8", "2606:4700:4700::1111"} {
		if !publicIP(net.ParseIP(raw)) {
			t.Fatalf("public address blocked: %s", raw)
		}
	}
}

func TestResearchRankingAndRelevanceForAIPassport(t *testing.T) {
	query := "thai ai passport"
	officialURL := "https://aipass.go.th/"
	officialTitle := "ยินดีต้อนรับสู่ ไทย เอไอ พาส"
	officialContent := "TH-AI Passport ลงทะเบียนสำหรับคนไทย"
	secondaryURL := "https://example.com/th-ai-passport"
	secondaryTitle := "วิธีลงทะเบียน TH-AI Passport"
	secondaryContent := "ข้อมูล TH-AI Passport"
	unrelatedURL := "https://thaiembassy.org/thai-passport"
	unrelatedTitle := "Thai Passport"
	unrelatedContent := "Instruction for obtaining e-passport"
	photoURL := "https://photogpt.example/passport"
	photoTitle := "Passport Photo AI Image Generator"
	photoContent := "Create passport-style AI photos online"

	if !researchResultRelevant(query, officialTitle, officialURL, officialContent) {
		t.Fatal("official AI Passport result must remain relevant")
	}
	if researchResultRelevant(query, unrelatedTitle, unrelatedURL, unrelatedContent) {
		t.Fatal("generic Thai passport result without AI signal must be filtered")
	}
	if researchResultRelevant(query, photoTitle, photoURL, photoContent) {
		t.Fatal("generic AI passport-photo result must be filtered for AiPASS intent")
	}
	officialRank := researchResultRank(query, officialTitle, officialURL, officialContent, 0.5)
	secondaryRank := researchResultRank(query, secondaryTitle, secondaryURL, secondaryContent, 4.0)
	if officialRank <= secondaryRank {
		t.Fatalf("official source should outrank secondary source: official=%v secondary=%v", officialRank, secondaryRank)
	}
}

func TestResearchPreferencesThailandDefaultsLocalFirst(t *testing.T) {
	prefs := researchPreferencesFromPayload(map[string]any{"researchLocale": "th-TH", "researchDepth": "deep"}, "รถไฟฟ้ารุ่นใหม่")
	if prefs.Region != "TH" || prefs.Scope != "local-first" || prefs.Depth != "deep" {
		t.Fatalf("unexpected Thailand preferences: %#v", prefs)
	}
	prefs = researchPreferencesFromPayload(map[string]any{"researchRegion": "GLOBAL", "researchScope": "global"}, "รถไฟฟ้ารุ่นใหม่")
	if prefs.Region != "GLOBAL" || prefs.Scope != "global" {
		t.Fatalf("explicit global preference must win over Thai query text: %#v", prefs)
	}
}

func TestDeepThailandResearchPlanSearchesLocalAndSocialBeforeGlobal(t *testing.T) {
	prefs := researchPreferences{Region: "TH", Locale: "th-TH", Scope: "local-first", Depth: "deep", Focus: "ราคาและประสบการณ์ผู้ใช้จริง"}
	plan := researchSearchPlan("BYD Sealion 7", prefs)
	if len(plan) == 0 {
		t.Fatal("expected a research plan")
	}
	joined := make([]string, 0, len(plan))
	firstGlobal := -1
	lastLocal := -1
	seenStage := map[string]bool{}
	for i, item := range plan {
		joined = append(joined, item.Query)
		seenStage[item.Stage] = true
		if item.Region == "TH" {
			lastLocal = i
		}
		if item.Region == "GLOBAL" && firstGlobal < 0 {
			firstGlobal = i
		}
	}
	queries := strings.Join(joined, "\n")
	for _, want := range []string{"site:go.th", "site:facebook.com", "site:instagram.com", "site:tiktok.com"} {
		if !strings.Contains(queries, want) {
			t.Fatalf("deep Thailand plan missing %q: %#v", want, plan)
		}
	}
	if !seenStage["social-local"] {
		t.Fatalf("deep Thailand plan must include a local social stage: %#v", plan)
	}
	if firstGlobal < 0 || lastLocal < 0 || firstGlobal <= lastLocal {
		t.Fatalf("Thailand stages must be planned before global expansion: firstGlobal=%d lastLocal=%d plan=%#v", firstGlobal, lastLocal, plan)
	}
}

func TestSocialPlatformClassification(t *testing.T) {
	cases := map[string]string{
		"https://www.facebook.com/example/posts/1": "Facebook",
		"https://instagram.com/p/example":          "Instagram",
		"https://www.tiktok.com/@example/video/1":  "TikTok",
		"https://x.com/example/status/1":           "X",
		"https://youtube.com/watch?v=1":            "YouTube",
		"https://www.reddit.com/r/cars/comments/1": "Reddit",
		"https://pantip.com/topic/123":             "Pantip",
		"https://www.threads.net/@example/post/1":  "Threads",
	}
	for raw, want := range cases {
		if got := researchSocialPlatform(raw); got != want {
			t.Fatalf("researchSocialPlatform(%q)=%q want %q", raw, got, want)
		}
		if got := researchSourceType(raw); got != "social" {
			t.Fatalf("researchSourceType(%q)=%q want social", raw, got)
		}
	}
	if got := researchSocialPlatform("https://example.com/article"); got != "" {
		t.Fatalf("ordinary web source classified as social: %q", got)
	}
}

func TestThailandRankingBoostsRelevantLocalSource(t *testing.T) {
	prefs := researchPreferences{Region: "TH", Scope: "local-first", Depth: "deep"}
	query := "BYD Sealion 7 price"
	local := researchResultRankForPreferences(query, "BYD Sealion 7 ราคาไทย", "https://example.co.th/sealion-7", "ราคา Thailand ประเทศไทย", 1, prefs)
	global := researchResultRankForPreferences(query, "BYD Sealion 7 price", "https://example.com/sealion-7", "global price overview", 1, prefs)
	if local <= global {
		t.Fatalf("Thailand-relevant result should outrank global result: local=%v global=%v", local, global)
	}
}

func TestFetchPublicPageDirectURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><head><title>TH-AI Passport</title></head><body><main>TH-AI Passport official project information for Thai citizens age 15 and above.</main></body></html>`))
	}))
	defer server.Close()
	text, err := fetchPublicPage(context.Background(), server.Client(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "TH-AI Passport official project information") {
		t.Fatalf("unexpected page text %q", text)
	}
}
func TestFetchPublicPageTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(40 * time.Millisecond)
		_, _ = w.Write([]byte("this response should arrive after the client timeout and must not be accepted"))
	}))
	defer server.Close()
	client := server.Client()
	client.Timeout = 5 * time.Millisecond
	if _, err := fetchPublicPage(context.Background(), client, server.URL); err == nil {
		t.Fatal("expected timeout error")
	}
}
func TestWebResearchNoResults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer server.Close()
	a := &app{cfg: config{SearXNGBase: server.URL}, http: server.Client()}
	if _, err := a.webResearch(context.Background(), "query with no results"); err == nil || !strings.Contains(err.Error(), "no search results") {
		t.Fatalf("expected no search results error, got %v", err)
	}
}
func TestExplicitWebResearchFailureStillLetsModelAnswer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer server.Close()
	a := &app{cfg: config{SearXNGBase: server.URL}, http: server.Client()}
	body, meta, err := a.enrichChatWithResearch(context.Background(), []byte(`{"model":"auto","researchMode":"web","messages":[{"role":"user","content":"find a thing that does not exist"}]}`))
	if err != nil {
		t.Fatalf("research failure should degrade gracefully, got %v", err)
	}
	if meta.Used || meta.Error == "" {
		t.Fatalf("expected failed research metadata, got %#v", meta)
	}
	if !strings.Contains(string(body), "WEB RESEARCH STATUS: FAILED") {
		t.Fatalf("model must receive explicit failed-research context: %s", body)
	}
}
func TestInternetCapabilityQuestionDoesNotTriggerDateTimeTool(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"ตอนนี้เข้า internet ได้ยัง"}]}`)
	if shouldEnableSmartTools(body) {
		t.Fatal("internet capability question must not trigger unrelated date/time tool planning")
	}
}

func TestApplySmartSkillsMergesExistingResearchSystemMessage(t *testing.T) {
	body := []byte(`{"messages":[{"role":"system","content":"WEB RESEARCH STATUS: SUCCEEDED"},{"role":"user","content":"ตอนนี้เข้า internet ได้ยัง"}]}`)
	out, err := applySmartSkills(body, []smartSkill{{ID: "problem-solving", Name: "Problem Solving", Prompt: "be careful"}})
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatal(err)
	}
	messages, _ := payload["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("expected one merged system message + user, got %d", len(messages))
	}
	first, _ := messages[0].(map[string]any)
	content, _ := first["content"].(string)
	if !strings.Contains(content, "WEB RESEARCH STATUS: SUCCEEDED") || !strings.Contains(content, "small 4B model") {
		t.Fatalf("merged system context missing research or smart prompt: %q", content)
	}
}

func TestResearchUsesHermesProfile(t *testing.T) {
	cases := []struct {
		meta researchMetadata
		want bool
	}{
		{researchMetadata{Mode: "web", Query: "anything"}, true},
		{researchMetadata{Mode: "auto", Query: "ค้นหาข้อมูล https://www.overdrive.qd.je/"}, true},
		{researchMetadata{Mode: "auto", Query: "hello"}, false},
		{researchMetadata{Mode: "off", Query: "https://www.overdrive.qd.je/"}, false},
	}
	for _, tc := range cases {
		if got := researchUsesHermesProfile(tc.meta); got != tc.want {
			t.Fatalf("meta=%+v got=%v want=%v", tc.meta, got, tc.want)
		}
	}
}

func TestContextualFollowUpInheritsPreviousResearch(t *testing.T) {
	payload := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": "ค้นหาข้อมูล https://www.overdrive.qd.je/ แล้วสรุปมาเป็นภาษาไทย"},
		map[string]any{"role": "assistant", "content": "OverDrive เป็นแอป dash-cam สำหรับรถ BYD และต้องติดตั้งผ่าน ADB"},
		map[string]any{"role": "user", "content": "อันตรายต่อการใช้งานไหม"},
	}}
	query, resolved, useWeb, inherited, ctx := contextualResearchPlan(payload, "auto")
	if query != "อันตรายต่อการใช้งานไหม" {
		t.Fatalf("query=%q", query)
	}
	if !useWeb || !inherited || !ctx.IsFollowUp {
		t.Fatalf("expected inherited web follow-up, useWeb=%v inherited=%v ctx=%#v", useWeb, inherited, ctx)
	}
	if !strings.Contains(resolved, "https://www.overdrive.qd.je/") || !strings.Contains(resolved, "อันตรายต่อการใช้งานไหม") {
		t.Fatalf("resolved query lost topic: %q", resolved)
	}
	instruction := continuityInstruction(ctx)
	if !strings.Contains(instruction, "OverDrive") || !strings.Contains(instruction, "อันตรายต่อการใช้งานไหม") {
		t.Fatalf("continuity instruction missing context: %q", instruction)
	}
}

func TestDetailExpansionTypoInheritsPreviousResearch(t *testing.T) {
	payload := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": "ค้นหาข้อมูล https://www.overdrive.qd.je/ แล้วสรุปมาเป็นภาษาไทย"},
		map[string]any{"role": "assistant", "content": "OverDrive เป็นแอป dash-cam สำหรับ BYD"},
		map[string]any{"role": "user", "content": "อันตรายต่อการใช้งานไหม"},
		map[string]any{"role": "assistant", "content": "อันตรายโดยตรงไม่มี แต่เป็นรุ่น alpha และควรใช้ด้วยความระมัดระวัง"},
		map[string]any{"role": "user", "content": "ขอรายระเอีนดมากกว่านี้"},
	}}
	query, resolved, useWeb, inherited, ctx := contextualResearchPlan(payload, "auto")
	if query != "ขอรายระเอีนดมากกว่านี้" {
		t.Fatalf("query=%q", query)
	}
	if !ctx.IsFollowUp || !useWeb || !inherited {
		t.Fatalf("detail expansion must inherit topic/research: ctx=%#v useWeb=%v inherited=%v", ctx, useWeb, inherited)
	}
	if !strings.Contains(resolved, "https://www.overdrive.qd.je/") || !strings.Contains(resolved, "ขอรายระเอีนดมากกว่านี้") {
		t.Fatalf("resolved query lost prior subject: %q", resolved)
	}
	if !strings.Contains(continuityInstruction(ctx), "อันตรายโดยตรงไม่มี") {
		t.Fatalf("continuity anchor must preserve immediately preceding answer: %q", continuityInstruction(ctx))
	}
}

func TestContextualFollowUpDoesNotOverrideResearchOff(t *testing.T) {
	payload := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": "ค้นหาข้อมูล https://www.overdrive.qd.je/"},
		map[string]any{"role": "assistant", "content": "OverDrive information"},
		map[string]any{"role": "user", "content": "อันตรายไหม"},
	}}
	_, _, useWeb, inherited, _ := contextualResearchPlan(payload, "off")
	if useWeb || inherited {
		t.Fatalf("research off must stay off, useWeb=%v inherited=%v", useWeb, inherited)
	}
}

func TestNewTopicDoesNotInheritPreviousResearch(t *testing.T) {
	payload := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": "ค้นหาข้อมูล https://www.overdrive.qd.je/"},
		map[string]any{"role": "assistant", "content": "OverDrive information"},
		map[string]any{"role": "user", "content": "เขียน SQL join ตารางสินค้าให้หน่อย"},
	}}
	_, _, useWeb, inherited, ctx := contextualResearchPlan(payload, "auto")
	if useWeb || inherited || ctx.IsFollowUp {
		t.Fatalf("new topic must not inherit previous research: useWeb=%v inherited=%v ctx=%#v", useWeb, inherited, ctx)
	}
}

func TestHermesSessionKeyIsScopedByChatSession(t *testing.T) {
	a := httptest.NewRequest(http.MethodPost, "/v1/chat", nil)
	b := httptest.NewRequest(http.MethodPost, "/v1/chat", nil)
	user := store.User{Subject: "same-user"}
	principal := principal{AuthKind: "oidc"}
	for _, tc := range []struct {
		r       *http.Request
		session string
	}{{a, "chat-a"}, {b, "chat-b"}} {
		ctx := context.WithValue(tc.r.Context(), appUserKey, user)
		ctx = context.WithValue(ctx, principalKey, principal)
		tc.r = tc.r.WithContext(ctx)
		tc.r.Header.Set("x-daiki-chat-session-id", tc.session)
		if tc.session == "chat-a" {
			a = tc.r
		} else {
			b = tc.r
		}
	}
	ka, kb := hermesSessionKey(a), hermesSessionKey(b)
	if ka == "" || kb == "" || ka == kb {
		t.Fatalf("chat sessions must have distinct Hermes keys: %q %q", ka, kb)
	}
	if strings.Contains(ka, "chat-a") || strings.Contains(kb, "chat-b") {
		t.Fatalf("raw session ids must not appear in Hermes keys")
	}
}
