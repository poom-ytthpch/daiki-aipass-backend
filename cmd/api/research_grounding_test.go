package main

import (
	"strings"
	"testing"
)

func sealionGroundingMeta() researchMetadata {
	return researchMetadata{
		Used:          true,
		Query:         "หาข้อมูลรถ BYD SEALION 7 อย่างละเอียด",
		ResolvedQuery: "BYD SEALION 7 Thailand",
		Sources: []researchSource{
			{Index: 1, Title: "BYD SEALION 7 | Rêver Automotive", URL: "https://www.reverautomotive.com/model/sealion7/overview", Snippet: "BYD SEALION 7 Premium ราคา 1,199,900 บาท AWD Performance 1,299,900 บาท AWD Ultimate 1,349,900 บาท"},
			{Index: 2, Title: "BYD SEALION 7", URL: "https://www.byd.com/en-th/car/sealion7", Excerpt: "BYD SEALION 7 electric SUV Blade Battery e-Platform 3.0"},
		},
	}
}

func TestResearchGroundingRejectsUnsupportedVehicleSpecs(t *testing.T) {
	meta := sealionGroundingMeta()
	text := "BYD SEALION 7 ผลิตโดยบริษัท BYD ใช้เครื่องยนต์ 1.5 ลิตร 150 PS แรงบิด 200 Nm และราคา 1,199,900 บาท [1]"
	violations := researchUnsupportedClaims(text, meta)
	joined := strings.ToLower(strings.Join(violations, " "))
	for _, want := range []string{"1.5", "150", "200"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("expected unsupported claim %q in %#v", want, violations)
		}
	}
	if strings.Contains(joined, "1199900") || strings.Contains(joined, "1,199,900") {
		t.Fatalf("supported current price must not be rejected: %#v", violations)
	}
}

func TestResearchGroundingAcceptsSupportedPrices(t *testing.T) {
	meta := sealionGroundingMeta()
	text := "ราคาที่ตรวจพบคือ Premium 1,199,900 บาท, AWD Performance 1,299,900 บาท และ AWD Ultimate 1,349,900 บาท [1]"
	if violations := researchUnsupportedClaims(text, meta); len(violations) != 0 {
		t.Fatalf("supported answer must pass grounding: %#v", violations)
	}
}

func TestResearchGroundingRejectsUnsupportedTaxFeeDisclaimer(t *testing.T) {
	meta := sealionGroundingMeta()
	text := "Premium 1,199,900 บาท [1] ราคานี้อาจยังไม่รวมภาษีและค่าธรรมเนียมอื่น ๆ"
	violations := researchUnsupportedClaims(text, meta)
	joined := strings.ToLower(strings.Join(violations, " "))
	if !strings.Contains(joined, "tax") && !strings.Contains(joined, "ภาษี") {
		t.Fatalf("unsupported tax/fee disclaimer must be rejected: %#v", violations)
	}
}

func TestResearchGroundingRejectsUnsupportedConflictCause(t *testing.T) {
	meta := sealionGroundingMeta()
	text := "แหล่ง [2] ระบุราคา 1,349,000 บาท ซึ่งเป็นการพิมพ์ผิด แต่ราคา 1,349,900 บาทจาก [1] ถูกต้อง"
	violations := researchUnsupportedClaims(text, meta)
	if !strings.Contains(strings.ToLower(strings.Join(violations, " ")), "พิมพ์ผิด") {
		t.Fatalf("unsupported conflict cause must be rejected: %#v", violations)
	}
}

func TestResearchGroundingRejectsUnsupportedManufacturerAttribution(t *testing.T) {
	meta := sealionGroundingMeta()
	text := "รถรุ่นนี้ผลิตโดยบริษัท Chery และทำตลาดในชื่อ SEALION 7"
	violations := researchUnsupportedClaims(text, meta)
	if !strings.Contains(strings.ToLower(strings.Join(violations, " ")), "chery") {
		t.Fatalf("unsupported manufacturer must be rejected: %#v", violations)
	}
}

func TestResearchGroundingRetryForcesDeterministicEvidenceOnlyMode(t *testing.T) {
	meta := sealionGroundingMeta()
	body := []byte(`{"stream":true,"temperature":0.8,"messages":[{"role":"system","content":"research evidence"},{"role":"user","content":"สรุป"}]}`)
	out := researchGroundingRetryBody(body, meta, []string{"1.5", "150"})
	raw := string(out)
	for _, want := range []string{`"stream":false`, `"temperature":0`, "GROUNDING RETRY", "BYD SEALION 7 Thailand"} {
		if !strings.Contains(raw, want) {
			t.Fatalf("retry payload missing %q: %s", want, raw)
		}
	}
}

func TestChatCompletionToSSEKeepsValidatedContent(t *testing.T) {
	body := []byte(`{"id":"x","model":"m","choices":[{"message":{"role":"assistant","content":"ราคา 1,199,900 บาท"},"finish_reason":"stop"}],"usage":{"total_tokens":10}}`)
	sse := string(chatCompletionToSSE(body))
	if !strings.Contains(sse, "ราคา 1,199,900 บาท") || !strings.Contains(sse, "data: [DONE]") {
		t.Fatalf("unexpected SSE conversion: %s", sse)
	}
}
