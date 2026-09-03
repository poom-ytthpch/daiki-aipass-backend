package main

import (
	"encoding/json"
	"net"
	"strings"
	"testing"
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

	if !researchResultRelevant(query, officialTitle, officialURL, officialContent) {
		t.Fatal("official AI Passport result must remain relevant")
	}
	if researchResultRelevant(query, unrelatedTitle, unrelatedURL, unrelatedContent) {
		t.Fatal("generic Thai passport result without AI signal must be filtered")
	}
	officialRank := researchResultRank(query, officialTitle, officialURL, officialContent, 0.5)
	secondaryRank := researchResultRank(query, secondaryTitle, secondaryURL, secondaryContent, 4.0)
	if officialRank <= secondaryRank {
		t.Fatalf("official source should outrank secondary source: official=%v secondary=%v", officialRank, secondaryRank)
	}
}

func TestExtractiveResearchIntent(t *testing.T) {
	for _, q := range []string{
		"ค้นหาข้อมูล thai ai passport",
		"หาข้อมูล OpenAI",
		"search for current LiteLLM docs",
		"find information about Qwen",
	} {
		if !shouldUseExtractiveResearchAnswer(q) {
			t.Fatalf("expected extractive research answer for %q", q)
		}
	}
	for _, q := range []string{
		"ค้นหาข้อมูล thai ai passport แล้วสรุป",
		"วิเคราะห์ข่าว AI ล่าสุด",
		"compare current vLLM and Ollama",
	} {
		if shouldUseExtractiveResearchAnswer(q) {
			t.Fatalf("did not expect extractive-only answer for synthesis query %q", q)
		}
	}
}

func TestRenderResearchEvidencePrefersOfficialSources(t *testing.T) {
	meta := researchMetadata{Query: "ค้นหาข้อมูล thai ai passport", Sources: []researchSource{
		{Index: 1, Title: "TH-AI Passport", URL: "https://aipass.go.th/", Snippet: "ลงทะเบียนผ่านเว็บไซต์ aipass.go.th"},
		{Index: 2, Title: "Secondary", URL: "https://example.com/article", Snippet: "secondary claim that should be omitted when official evidence exists"},
		{Index: 3, Title: "ข้อมูลโครงการ — TH-AI Passport", URL: "https://aipass.go.th/about", Snippet: "ส่งเสริมให้คนไทยเข้าถึง Generative AI"},
	}}
	answer := renderResearchEvidenceAnswer(meta)
	if !strings.Contains(answer, "https://aipass.go.th/") || !strings.Contains(answer, "https://aipass.go.th/about") {
		t.Fatalf("official evidence missing: %q", answer)
	}
	if strings.Contains(answer, "example.com") || strings.Contains(answer, "secondary claim") {
		t.Fatalf("secondary source should be omitted when official sources exist: %q", answer)
	}
	if !strings.Contains(answer, "ลงทะเบียนผ่านเว็บไซต์ aipass.go.th") {
		t.Fatalf("source snippet must be preserved: %q", answer)
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
