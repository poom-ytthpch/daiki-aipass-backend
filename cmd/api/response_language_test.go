package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func languageBody(t *testing.T, messages ...map[string]any) []byte {
	t.Helper()
	items := make([]any, 0, len(messages))
	for _, m := range messages {
		items = append(items, m)
	}
	b, err := json.Marshal(map[string]any{"model": "fast", "messages": items})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestResponseLanguageKeepsThaiAcrossAutomaticAttachmentPrompt(t *testing.T) {
	body := languageBody(t,
		map[string]any{"role": "user", "content": "ตอนนี้ใช้ model อะไร"},
		map[string]any{"role": "assistant", "content": "ตอนนี้ใช้ Qwen ครับ"},
		map[string]any{"role": "user", "content": "ขอรายละเอียดหน่อย"},
		map[string]any{"role": "assistant", "content": "ได้ครับ"},
		map[string]any{"role": "user", "content": "Please review the attached content."},
	)
	pref := resolveResponseLanguage(body)
	if pref.Code != "th-TH" {
		t.Fatalf("code=%q source=%q", pref.Code, pref.Source)
	}
	out, applied, err := applyResponseLanguage(body)
	if err != nil || applied.Code != "th-TH" {
		t.Fatalf("applied=%+v err=%v", applied, err)
	}
	if !strings.Contains(string(out), "DAIKI RESPONSE LANGUAGE") || !strings.Contains(string(out), "Thai") {
		t.Fatalf("missing Thai language instruction: %s", out)
	}
}

func TestResponseLanguageExplicitSwitchWins(t *testing.T) {
	body := languageBody(t,
		map[string]any{"role": "user", "content": "คุยภาษาไทยก่อน"},
		map[string]any{"role": "assistant", "content": "ได้ครับ"},
		map[string]any{"role": "user", "content": "ตอบเป็นภาษาอังกฤษตั้งแต่ข้อความนี้"},
	)
	pref := resolveResponseLanguage(body)
	if pref.Code != "en" || pref.Source != "explicit" {
		t.Fatalf("pref=%+v", pref)
	}
}

func TestResponseLanguageUsesLatestSubstantiveUserLanguage(t *testing.T) {
	body := languageBody(t,
		map[string]any{"role": "user", "content": "สวัสดีครับ"},
		map[string]any{"role": "assistant", "content": "สวัสดีครับ"},
		map[string]any{"role": "user", "content": "Explain this API contract in English"},
	)
	pref := resolveResponseLanguage(body)
	if pref.Code != "en" {
		t.Fatalf("pref=%+v", pref)
	}
}

func TestResponseLanguageSkipsSyntheticAttachmentPrompt(t *testing.T) {
	body := languageBody(t,
		map[string]any{"role": "user", "content": "สวัสดี ตอนนี้คุยกันเป็นภาษาไทยนะ"},
		map[string]any{"role": "assistant", "content": "ได้ครับ"},
		map[string]any{"role": "user", "content": "ช่วยดู promotion_status กับ batch นี้ให้หน่อย"},
		map[string]any{"role": "assistant", "content": "ได้ครับ"},
		map[string]any{"role": "user", "content": "Analyze SKU CSV-TARGET-7319 and promotion_type with normal_price and selling_price"},
		map[string]any{"role": "assistant", "content": "กำลังดูให้ครับ"},
		map[string]any{"role": "user", "content": "Please review the attached content."},
	)
	pref := resolveResponseLanguage(body)
	if pref.Code != "en" || pref.Source != "latest-user" {
		t.Fatalf("pref=%+v", pref)
	}
}

func TestResponseLanguageLatestThaiWinsOverEarlierEnglish(t *testing.T) {
	body := languageBody(t,
		map[string]any{"role": "user", "content": "Explain the architecture briefly"},
		map[string]any{"role": "assistant", "content": "Here is the architecture."},
		map[string]any{"role": "user", "content": "List the deployment risks"},
		map[string]any{"role": "assistant", "content": "The main risks are..."},
		map[string]any{"role": "user", "content": "แล้วตอนนี้ควรแก้อะไรก่อน ช่วยตอบเป็นขั้นตอนหน่อย"},
	)
	pref := resolveResponseLanguage(body)
	if pref.Code != "th-TH" || pref.Source != "latest-user" {
		t.Fatalf("pref=%+v", pref)
	}
}

func TestResponseLanguageLatestEnglishWinsOverEarlierThai(t *testing.T) {
	body := languageBody(t,
		map[string]any{"role": "user", "content": "ช่วยสรุประบบนี้ให้หน่อย"},
		map[string]any{"role": "assistant", "content": "ได้ครับ"},
		map[string]any{"role": "user", "content": "ขอรายละเอียดเรื่อง quota เพิ่ม"},
		map[string]any{"role": "assistant", "content": "ได้ครับ"},
		map[string]any{"role": "user", "content": "Now answer the next part in English"},
	)
	pref := resolveResponseLanguage(body)
	if pref.Code != "en" {
		t.Fatalf("pref=%+v", pref)
	}
}
