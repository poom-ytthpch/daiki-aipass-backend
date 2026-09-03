package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/poom-ytthpch/daiki-ai-passport-backend/internal/store"
)

type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type toolPlannerResponse struct {
	Choices []struct {
		Message struct {
			Role      string     `json:"role"`
			Content   any        `json:"content"`
			ToolCalls []toolCall `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		TotalTokens      int64 `json:"total_tokens"`
	} `json:"usage"`
}

func builtinToolDefinitions() []any {
	return []any{
		map[string]any{"type": "function", "function": map[string]any{"name": "calculator", "description": "Evaluate deterministic arithmetic. Use instead of mental arithmetic.", "parameters": map[string]any{"type": "object", "properties": map[string]any{"expression": map[string]any{"type": "string"}}, "required": []string{"expression"}, "additionalProperties": false}}},
		map[string]any{"type": "function", "function": map[string]any{"name": "current_datetime", "description": "Get the actual current date and time in an IANA timezone.", "parameters": map[string]any{"type": "object", "properties": map[string]any{"timezone": map[string]any{"type": "string"}}, "additionalProperties": false}}},
		map[string]any{"type": "function", "function": map[string]any{"name": "attachment_list", "description": "List files uploaded by the current user.", "parameters": map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}}},
		map[string]any{"type": "function", "function": map[string]any{"name": "attachment_search", "description": "Search the current user's uploaded text/code/data files by filename, path, or extracted content.", "parameters": map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 10}}, "required": []string{"query"}, "additionalProperties": false}}},
		map[string]any{"type": "function", "function": map[string]any{"name": "attachment_read", "description": "Read extracted text from one uploaded file by attachment id.", "parameters": map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}, "maxChars": map[string]any{"type": "integer", "minimum": 100, "maximum": 20000}}, "required": []string{"id"}, "additionalProperties": false}}},
	}
}

func shouldEnableSmartTools(body []byte) bool {
	text := strings.ToLower(latestUserText(body))
	if text == "" {
		return false
	}
	for _, word := range []string{"calculate", "calculation", "percent", "percentage", "คำนวณ", "คิดเลข", "เปอร์เซ็นต์", "กี่โมง", "วันนี้", "ตอนนี้", "วันที่", "current time", "current date", "time now", "my file", "my files", "uploaded", "attachment", "search file", "read file", "find in", "ไฟล์", "เอกสาร", "โฟลเดอร์", "ค้นใน", "อ่านไฟล์"} {
		if strings.Contains(text, word) {
			return true
		}
	}
	digits, ops := 0, 0
	for _, r := range text {
		if unicode.IsDigit(r) {
			digits++
		}
		if strings.ContainsRune("+-*/%^", r) {
			ops++
		}
	}
	return digits >= 2 && ops >= 1
}
func addUsage(a, b store.Usage) store.Usage {
	return store.Usage{InputTokens: a.InputTokens + b.InputTokens, OutputTokens: a.OutputTokens + b.OutputTokens, TotalTokens: a.TotalTokens + b.TotalTokens}
}

func (a *app) runSmartToolLoop(ctx context.Context, subject string, body []byte) ([]byte, []string, store.Usage, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, nil, store.Usage{}, err
	}
	payload["stream"] = false
	delete(payload, "stream_options")
	payload["tools"] = builtinToolDefinitions()
	payload["tool_choice"] = "auto"
	if _, ok := payload["temperature"]; !ok {
		payload["temperature"] = 0
	}
	usage := store.Usage{}
	used := []string{}
	seen := map[string]bool{}
	for round := 0; round < 2; round++ {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return body, used, usage, err
		}
		respBody, err := a.postLiteLLMJSON(ctx, encoded)
		if err != nil {
			return body, used, usage, err
		}
		var planner toolPlannerResponse
		if err := json.Unmarshal(respBody, &planner); err != nil || len(planner.Choices) == 0 {
			return body, used, usage, errors.New("tool planner returned invalid response")
		}
		usage = addUsage(usage, store.Usage{InputTokens: planner.Usage.PromptTokens, OutputTokens: planner.Usage.CompletionTokens, TotalTokens: planner.Usage.TotalTokens})
		msg := planner.Choices[0].Message
		if len(msg.ToolCalls) == 0 {
			break
		}
		messages, _ := payload["messages"].([]any)
		messages = append(messages, map[string]any{"role": "assistant", "content": msg.Content, "tool_calls": msg.ToolCalls})
		for _, call := range msg.ToolCalls {
			result := a.executeBuiltinTool(ctx, subject, call.Function.Name, call.Function.Arguments)
			if !seen[call.Function.Name] {
				seen[call.Function.Name] = true
				used = append(used, call.Function.Name)
			}
			encodedResult, _ := json.Marshal(result)
			messages = append(messages, map[string]any{"role": "tool", "tool_call_id": call.ID, "name": call.Function.Name, "content": string(encodedResult)})
		}
		payload["messages"] = messages
	}
	delete(payload, "tools")
	delete(payload, "tool_choice")
	delete(payload, "stream")
	finalBody, err := json.Marshal(payload)
	if err != nil {
		return body, used, usage, err
	}
	return finalBody, used, usage, nil
}
func (a *app) postLiteLLMJSON(ctx context.Context, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(a.cfg.LiteLLMBase, "/")+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	if a.cfg.LiteLLMKey != "" {
		req.Header.Set("authorization", "Bearer "+a.cfg.LiteLLMKey)
	}
	resp, err := a.inferenceHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("tool planner upstream status %d: %s", resp.StatusCode, strings.TrimSpace(buf.String()))
	}
	return buf.Bytes(), nil
}

func (a *app) executeBuiltinTool(ctx context.Context, subject, name, arguments string) any {
	var args map[string]any
	if strings.TrimSpace(arguments) != "" && json.Unmarshal([]byte(arguments), &args) != nil {
		return map[string]any{"ok": false, "error": "invalid tool arguments"}
	}
	if args == nil {
		args = map[string]any{}
	}
	switch name {
	case "calculator":
		expr, _ := args["expression"].(string)
		v, err := evalExpression(expr)
		if err != nil {
			return map[string]any{"ok": false, "error": err.Error()}
		}
		return map[string]any{"ok": true, "expression": expr, "result": v}
	case "current_datetime":
		zone, _ := args["timezone"].(string)
		if strings.TrimSpace(zone) == "" {
			zone = "Asia/Bangkok"
		}
		loc, err := time.LoadLocation(zone)
		if err != nil {
			return map[string]any{"ok": false, "error": "invalid timezone"}
		}
		now := time.Now().In(loc)
		return map[string]any{"ok": true, "timezone": zone, "iso8601": now.Format(time.RFC3339), "date": now.Format("2006-01-02"), "time": now.Format("15:04:05"), "weekday": now.Weekday().String()}
	case "attachment_list":
		if a.store == nil {
			return map[string]any{"ok": false, "error": "attachment store unavailable"}
		}
		rows, err := a.store.UserAttachments(ctx, subject, 30)
		if err != nil {
			return map[string]any{"ok": false, "error": "attachment lookup failed"}
		}
		items := make([]map[string]any, 0, len(rows))
		for _, row := range rows {
			items = append(items, map[string]any{"id": row.ID, "name": row.Name, "relativePath": row.RelativePath, "mediaType": row.MediaType, "sizeBytes": row.SizeBytes, "extractStatus": row.ExtractStatus, "createdAt": row.CreatedAt})
		}
		return map[string]any{"ok": true, "attachments": items}
	case "attachment_search":
		if a.store == nil {
			return map[string]any{"ok": false, "error": "attachment store unavailable"}
		}
		q, _ := args["query"].(string)
		limit := intArg(args["limit"], 5, 1, 10)
		rows, err := a.store.SearchAttachments(ctx, subject, strings.TrimSpace(q), limit)
		if err != nil {
			return map[string]any{"ok": false, "error": "attachment search failed"}
		}
		return map[string]any{"ok": true, "results": rows}
	case "attachment_read":
		if a.store == nil {
			return map[string]any{"ok": false, "error": "attachment store unavailable"}
		}
		id, _ := args["id"].(string)
		maxChars := intArg(args["maxChars"], 12000, 100, 20000)
		row, err := a.store.Attachment(ctx, subject, strings.TrimSpace(id))
		if err != nil {
			return map[string]any{"ok": false, "error": "attachment not found"}
		}
		text := row.ExtractedText
		truncated := false
		if len(text) > maxChars {
			text = text[:maxChars]
			truncated = true
		}
		return map[string]any{"ok": true, "id": row.ID, "name": row.Name, "relativePath": row.RelativePath, "mediaType": row.MediaType, "extractStatus": row.ExtractStatus, "text": text, "truncated": truncated}
	default:
		return map[string]any{"ok": false, "error": "unknown tool"}
	}
}
func intArg(v any, fallback, minValue, maxValue int) int {
	n := fallback
	switch x := v.(type) {
	case float64:
		n = int(x)
	case int:
		n = x
	case json.Number:
		if value, err := strconv.Atoi(x.String()); err == nil {
			n = value
		}
	}
	if n < minValue {
		n = minValue
	}
	if n > maxValue {
		n = maxValue
	}
	return n
}

type expressionParser struct {
	s string
	i int
}

func evalExpression(expression string) (float64, error) {
	p := &expressionParser{s: strings.TrimSpace(expression)}
	if p.s == "" || len(p.s) > 256 {
		return 0, errors.New("expression is empty or too long")
	}
	v, err := p.parseExpression()
	if err != nil {
		return 0, err
	}
	p.skipSpaces()
	if p.i != len(p.s) || math.IsInf(v, 0) || math.IsNaN(v) {
		return 0, errors.New("invalid arithmetic expression")
	}
	return v, nil
}
func (p *expressionParser) skipSpaces() {
	for p.i < len(p.s) && (p.s[p.i] == ' ' || p.s[p.i] == '\t') {
		p.i++
	}
}
func (p *expressionParser) parseExpression() (float64, error) {
	left, err := p.parseTerm()
	if err != nil {
		return 0, err
	}
	for {
		p.skipSpaces()
		if p.i >= len(p.s) || (p.s[p.i] != '+' && p.s[p.i] != '-') {
			return left, nil
		}
		op := p.s[p.i]
		p.i++
		right, err := p.parseTerm()
		if err != nil {
			return 0, err
		}
		if op == '+' {
			left += right
		} else {
			left -= right
		}
	}
}
func (p *expressionParser) parseTerm() (float64, error) {
	left, err := p.parsePower()
	if err != nil {
		return 0, err
	}
	for {
		p.skipSpaces()
		if p.i >= len(p.s) || !strings.ContainsRune("*/%", rune(p.s[p.i])) {
			return left, nil
		}
		op := p.s[p.i]
		p.i++
		right, err := p.parsePower()
		if err != nil {
			return 0, err
		}
		switch op {
		case '*':
			left *= right
		case '/':
			if right == 0 {
				return 0, errors.New("division by zero")
			}
			left /= right
		case '%':
			if right == 0 {
				return 0, errors.New("modulo by zero")
			}
			left = math.Mod(left, right)
		}
	}
}
func (p *expressionParser) parsePower() (float64, error) {
	left, err := p.parseUnary()
	if err != nil {
		return 0, err
	}
	p.skipSpaces()
	if p.i < len(p.s) && p.s[p.i] == '^' {
		p.i++
		right, err := p.parsePower()
		if err != nil {
			return 0, err
		}
		left = math.Pow(left, right)
	}
	return left, nil
}
func (p *expressionParser) parseUnary() (float64, error) {
	p.skipSpaces()
	if p.i < len(p.s) && (p.s[p.i] == '+' || p.s[p.i] == '-') {
		op := p.s[p.i]
		p.i++
		v, err := p.parseUnary()
		if err != nil {
			return 0, err
		}
		if op == '-' {
			v = -v
		}
		return v, nil
	}
	return p.parsePrimary()
}
func (p *expressionParser) parsePrimary() (float64, error) {
	p.skipSpaces()
	if p.i >= len(p.s) {
		return 0, errors.New("unexpected end of expression")
	}
	if p.s[p.i] == '(' {
		p.i++
		v, err := p.parseExpression()
		if err != nil {
			return 0, err
		}
		p.skipSpaces()
		if p.i >= len(p.s) || p.s[p.i] != ')' {
			return 0, errors.New("missing closing parenthesis")
		}
		p.i++
		return v, nil
	}
	start := p.i
	dot := false
	for p.i < len(p.s) {
		c := p.s[p.i]
		if c >= '0' && c <= '9' {
			p.i++
			continue
		}
		if c == '.' && !dot {
			dot = true
			p.i++
			continue
		}
		break
	}
	if start == p.i {
		return 0, errors.New("expected number")
	}
	return strconv.ParseFloat(p.s[start:p.i], 64)
}
