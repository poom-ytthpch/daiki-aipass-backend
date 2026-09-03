package main

import (
	"bytes"
	"encoding/json"
	"strings"
)

const storedPayloadLimit = 256 << 10

type cappedBuffer struct {
	buf bytes.Buffer
	max int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	original := len(p)
	remaining := c.max - c.buf.Len()
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = c.buf.Write(p)
	}
	return original, nil
}

func (c *cappedBuffer) Bytes() []byte { return c.buf.Bytes() }

func usageRequestMetadata(body []byte, authKind, apiKeyID string) map[string]any {
	meta := map[string]any{
		"authKind":           authKind,
		"apiKeyId":           apiKeyID,
		"requestSizeBytes":   len(body),
		"requestPayload":     safePayloadSnapshot(body),
		"payloadSnapshotCap": storedPayloadLimit,
	}
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return meta
	}
	meta["tools"] = requestedToolNames(payload)
	meta["skills"] = capabilityNames(payload, "skills")
	meta["mcpServers"] = capabilityNames(payload, "mcpServers", "mcp_servers", "mcp")
	meta["connectors"] = capabilityNames(payload, "connectors", "connections")
	if messages, ok := payload["messages"].([]any); ok {
		meta["messageCount"] = len(messages)
	}
	if model, ok := payload["model"].(string); ok {
		meta["requestedModel"] = model
	}
	return meta
}

func usageResponseMetadata(body []byte) map[string]any {
	return map[string]any{
		"responseSizeBytes": len(body),
		"responsePayload":   safePayloadSnapshot(body),
		"toolsUsed":         responseToolNames(body),
	}
}

func safePayloadSnapshot(body []byte) any {
	if len(body) == 0 {
		return nil
	}
	var v any
	if json.Unmarshal(body, &v) == nil {
		v = redactValue(v)
		encoded, err := json.Marshal(v)
		if err == nil && len(encoded) <= storedPayloadLimit {
			return v
		}
		if err == nil {
			return map[string]any{"truncated": true, "sizeBytes": len(encoded), "preview": string(encoded[:storedPayloadLimit])}
		}
	}
	preview := body
	if len(preview) > storedPayloadLimit {
		preview = preview[:storedPayloadLimit]
	}
	return map[string]any{"raw": string(preview), "truncated": len(body) > len(preview), "sizeBytes": len(body)}
}

func redactValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, value := range x {
			if sensitivePayloadKey(k) {
				out[k] = "[REDACTED]"
				continue
			}
			out[k] = redactValue(value)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i := range x {
			out[i] = redactValue(x[i])
		}
		return out
	default:
		return v
	}
}

func sensitivePayloadKey(key string) bool {
	k := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "-", "_"), " ", "_"))
	for _, word := range []string{"password", "secret", "authorization", "access_token", "refresh_token", "api_key", "apikey", "credential", "cookie"} {
		if strings.Contains(k, word) {
			return true
		}
	}
	return false
}

func requestedToolNames(payload map[string]any) []string {
	out := []string{}
	seen := map[string]bool{}
	items, _ := payload["tools"].([]any)
	for _, item := range items {
		m, _ := item.(map[string]any)
		fn, _ := m["function"].(map[string]any)
		name, _ := fn["name"].(string)
		if name == "" {
			name, _ = m["name"].(string)
		}
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

func capabilityNames(payload map[string]any, keys ...string) []string {
	out := []string{}
	seen := map[string]bool{}
	collect := func(v any) {
		switch x := v.(type) {
		case string:
			if x != "" && !seen[x] {
				seen[x] = true
				out = append(out, x)
			}
		case []any:
			for _, item := range x {
				switch y := item.(type) {
				case string:
					if y != "" && !seen[y] {
						seen[y] = true
						out = append(out, y)
					}
				case map[string]any:
					for _, field := range []string{"name", "id", "server", "provider"} {
						if name, ok := y[field].(string); ok && name != "" && !seen[name] {
							seen[name] = true
							out = append(out, name)
							break
						}
					}
				}
			}
		}
	}
	for _, key := range keys {
		collect(payload[key])
		if metadata, ok := payload["metadata"].(map[string]any); ok {
			collect(metadata[key])
		}
	}
	return out
}

func responseToolNames(body []byte) []string {
	seen := map[string]bool{}
	out := []string{}
	collectJSON := func(data []byte) {
		var root map[string]any
		if json.Unmarshal(data, &root) != nil {
			return
		}
		choices, _ := root["choices"].([]any)
		for _, choice := range choices {
			cm, _ := choice.(map[string]any)
			for _, field := range []string{"message", "delta"} {
				msg, _ := cm[field].(map[string]any)
				calls, _ := msg["tool_calls"].([]any)
				for _, call := range calls {
					m, _ := call.(map[string]any)
					fn, _ := m["function"].(map[string]any)
					name, _ := fn["name"].(string)
					if name != "" && !seen[name] {
						seen[name] = true
						out = append(out, name)
					}
				}
			}
		}
	}
	if bytes.Contains(body, []byte("data:")) {
		for _, line := range bytes.Split(body, []byte("\n")) {
			line = bytes.TrimSpace(line)
			if bytes.HasPrefix(line, []byte("data:")) {
				data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
				if string(data) != "[DONE]" {
					collectJSON(data)
				}
			}
		}
	} else {
		collectJSON(body)
	}
	return out
}
