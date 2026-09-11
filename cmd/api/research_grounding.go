package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/poom-ytthpch/daiki-ai-passport-backend/internal/store"
)

var researchCitationTokenRE = regexp.MustCompile(`\[(?:\d{1,2})(?:\s*[,;-]\s*\d{1,2})*\]`)
var researchListNumberRE = regexp.MustCompile(`(?m)^\s*\d{1,2}[.)]\s+`)
var researchNumberClaimRE = regexp.MustCompile(`(?i)(?:฿\s*)?\b\d[\d,]*(?:\.\d+)?(?:\s*(?:บาท|baht|%|kwh|kw|nm|km|mm|cm|kg|hp|ps|mah|wh|w|v|a|นิ้ว|กม\.?|มม\.?|กก\.?|ลิตร))?\b`)
var researchAlphaNumericClaimRE = regexp.MustCompile(`(?i)\b\d+(?:\.\d+)?(?:wd|kwh|kw|nm|km|mm|cm|kg|hp|ps|mah|wh|w|v|a)\b`)
var researchNamedClaimRE = regexp.MustCompile(`(?i)(?:ผลิตโดยบริษัท|ผู้ผลิต|manufactured by|manufacturer|แบรนด์|brand|ควบคู่กับ)\s*[:\-]?\s*([a-z][a-z0-9._-]{2,})`)

func normalizeGroundingToken(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	v = strings.ReplaceAll(v, ",", "")
	v = strings.ReplaceAll(v, "฿", "")
	return strings.Join(strings.Fields(v), " ")
}

func researchEvidenceCorpus(meta researchMetadata) string {
	var b strings.Builder
	for _, source := range meta.Sources {
		b.WriteString(" ")
		b.WriteString(source.Title)
		b.WriteString(" ")
		b.WriteString(source.Snippet)
		b.WriteString(" ")
		b.WriteString(source.Excerpt)
	}
	now := time.Now()
	fmt.Fprintf(&b, " %04d %02d %02d %s", now.Year(), int(now.Month()), now.Day(), now.Format("2006-01-02"))
	return normalizeGroundingToken(b.String())
}

func researchUnsupportedClaims(text string, meta researchMetadata) []string {
	if strings.TrimSpace(text) == "" || len(meta.Sources) == 0 {
		return nil
	}
	evidence := researchEvidenceCorpus(meta)
	evidenceCompact := strings.ReplaceAll(evidence, " ", "")
	clean := researchCitationTokenRE.ReplaceAllString(text, " ")
	clean = researchListNumberRE.ReplaceAllString(clean, " ")
	seen := map[string]bool{}
	violations := make([]string, 0, 8)
	add := func(raw string) {
		token := normalizeGroundingToken(raw)
		if token == "" || seen[token] {
			return
		}
		// Bare one-digit numbers are normally list/model labels and are too noisy
		// to use as a hard grounding gate. Unit-bearing/alphanumeric values are
		// checked separately below.
		digitsOnly := true
		for _, r := range token {
			if r < '0' || r > '9' {
				digitsOnly = false
				break
			}
		}
		if digitsOnly && len(token) < 2 {
			return
		}
		seen[token] = true
		if !strings.Contains(evidence, token) && !strings.Contains(evidenceCompact, strings.ReplaceAll(token, " ", "")) {
			violations = append(violations, raw)
		}
	}
	for _, raw := range researchNumberClaimRE.FindAllString(clean, -1) {
		add(raw)
	}
	for _, raw := range researchAlphaNumericClaimRE.FindAllString(clean, -1) {
		add(raw)
	}
	for _, match := range researchNamedClaimRE.FindAllStringSubmatch(clean, -1) {
		if len(match) > 1 {
			add(match[1])
		}
	}
	lowerText := strings.ToLower(text)
	if strings.Contains(lowerText, "ไม่รวมภาษี") || strings.Contains(lowerText, "ภาษีและค่าธรรมเนียม") || strings.Contains(lowerText, "tax not included") || strings.Contains(lowerText, "excluding tax") || strings.Contains(lowerText, "taxes and fees") {
		add("ภาษี tax")
	}
	if strings.Contains(lowerText, "ไม่รวมค่าธรรมเนียม") || strings.Contains(lowerText, "ค่าธรรมเนียมอื่น") || strings.Contains(lowerText, "fees not included") || strings.Contains(lowerText, "excluding fees") {
		add("ค่าธรรมเนียม fees")
	}
	sort.Strings(violations)
	if len(violations) > 12 {
		violations = violations[:12]
	}
	return violations
}

func researchGroundingRetryBody(body []byte, meta researchMetadata, violations []string) []byte {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return body
	}
	messages, _ := payload["messages"].([]any)
	guard := fmt.Sprintf(`GROUNDING RETRY: The previous draft failed evidence validation because it introduced unsupported claims (%s). Rewrite the answer from scratch using ONLY facts explicitly present in <web_sources> already supplied in the research system message. Do not use model memory to fill gaps. Every price, date, quantity, specification, range, power figure, dimension, warranty term, version, manufacturer/brand attribution, or availability statement must appear in the supplied source text. If a requested detail is absent, say it was not verified and omit the number. Keep the requested entity exactly anchored to: %s. Prefer the newest Thailand primary/local evidence for Thailand market facts. Cite factual paragraphs with the matching [n] source. Never invent a source or URL.`, strings.Join(violations, ", "), first(meta.ResolvedQuery, meta.Query))
	guardMessage := map[string]any{"role": "system", "content": guard}
	if len(messages) > 0 {
		messages = append([]any{messages[0], guardMessage}, messages[1:]...)
	} else {
		messages = []any{guardMessage}
	}
	payload["messages"] = messages
	payload["temperature"] = 0
	payload["top_p"] = 0.2
	payload["stream"] = false
	delete(payload, "stream_options")
	out, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return out
}

func forceChatNonStream(body []byte) []byte {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return body
	}
	payload["stream"] = false
	delete(payload, "stream_options")
	if _, ok := payload["temperature"]; !ok {
		payload["temperature"] = 0
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return out
}

func replaceChatCompletionText(body []byte, text string) []byte {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		payload = map[string]any{"object": "chat.completion"}
	}
	choices, _ := payload["choices"].([]any)
	var choice map[string]any
	if len(choices) > 0 {
		choice, _ = choices[0].(map[string]any)
	}
	if choice == nil {
		choice = map[string]any{"index": 0}
	}
	message, _ := choice["message"].(map[string]any)
	if message == nil {
		message = map[string]any{"role": "assistant"}
	}
	message["role"] = "assistant"
	message["content"] = text
	delete(message, "tool_calls")
	choice["message"] = message
	choice["finish_reason"] = "stop"
	payload["choices"] = []any{choice}
	out, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return out
}

func researchGroundingFallback(body []byte, meta researchMetadata) []byte {
	thai := containsThai(meta.Query)
	var b strings.Builder
	if thai {
		b.WriteString("ฉันพบแหล่งข้อมูลที่เกี่ยวข้อง แต่ร่างคำตอบอัตโนมัติมีรายละเอียดที่ตรวจสอบกับหลักฐานไม่ได้ จึงตัดข้อมูลที่เสี่ยงผิดออกแทนที่จะเดา\n\nแหล่งข้อมูลที่ตรวจสอบได้:\n")
	} else {
		b.WriteString("I found relevant sources, but the generated draft contained claims that could not be verified against the evidence. I removed the risky details instead of guessing.\n\nVerified sources:\n")
	}
	for i, source := range meta.Sources {
		if i >= 5 {
			break
		}
		fmt.Fprintf(&b, "- [%d] %s — %s\n", source.Index, first(source.Title, researchHost(source.URL)), source.URL)
	}
	if thai {
		b.WriteString("\nถ้ารายละเอียดใดไม่มีอยู่ในแหล่งข้อมูลข้างต้น ฉันจะไม่ใส่ตัวเลขหรือสเปกจากความจำของโมเดล")
	} else {
		b.WriteString("\nI will not fill missing prices, specifications, dates, or other factual details from model memory.")
	}
	return replaceChatCompletionText(body, b.String())
}

type researchGroundingResult struct {
	Body       []byte
	ExtraUsage store.Usage
	Retried    bool
	Fallback   bool
	Violations []string
}

func (a *app) enforceResearchGrounding(ctx context.Context, upstreamBody, responseBody []byte, meta researchMetadata, model, profile string, makeReq inferenceRequestFactory) researchGroundingResult {
	result := researchGroundingResult{Body: responseBody}
	if !meta.Used || len(meta.Sources) == 0 {
		return result
	}
	text := chatCompletionText(responseBody)
	violations := researchUnsupportedClaims(text, meta)
	if len(violations) == 0 {
		return result
	}
	result.Retried = true
	result.Violations = violations
	originalUsage := parseUsagePayload(responseBody)
	retryPayload := researchGroundingRetryBody(upstreamBody, meta, violations)
	resp, _, _, _, err := a.doModelRequestWithRecovery(ctx, retryPayload, model, profile, makeReq)
	if err == nil && resp != nil {
		defer resp.Body.Close()
		if resp.StatusCode < http.StatusBadRequest {
			if raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 16<<20)); readErr == nil {
				retryViolations := researchUnsupportedClaims(chatCompletionText(raw), meta)
				if len(retryViolations) == 0 {
					result.Body = raw
					result.ExtraUsage = originalUsage
					return result
				}
				result.ExtraUsage = originalUsage
				result.Violations = retryViolations
				result.Body = researchGroundingFallback(raw, meta)
				result.Fallback = true
				return result
			}
		}
	}
	result.Body = researchGroundingFallback(responseBody, meta)
	result.Fallback = true
	return result
}

func chatCompletionToSSE(body []byte) []byte {
	text := chatCompletionText(body)
	var original map[string]any
	_ = json.Unmarshal(body, &original)
	event := map[string]any{
		"object": "chat.completion.chunk",
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{"content": text},
			"finish_reason": "stop",
		}},
	}
	for _, key := range []string{"id", "created", "model", "usage"} {
		if value, ok := original[key]; ok {
			event[key] = value
		}
	}
	raw, _ := json.Marshal(event)
	var out bytes.Buffer
	out.WriteString("data: ")
	out.Write(raw)
	out.WriteString("\n\ndata: [DONE]\n\n")
	return out.Bytes()
}
