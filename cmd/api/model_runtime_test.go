package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseRateLimitObservationITPM(t *testing.T) {
	body := []byte(`API call failed after 3 retries: HTTP 429: Request too large for model on input tokens per minute (ITPM): Limit 7000, Requested 11705, please reduce your message size`)
	got := parseRateLimitObservation(429, body)
	if got.Kind != "itpm" || got.Limit != 7000 || got.Requested != 11705 {
		t.Fatalf("unexpected observation: %#v", got)
	}
}

func TestTrimChatContextKeepsNewestTurn(t *testing.T) {
	messages := []map[string]any{{"role": "user", "content": "old " + strings.Repeat("a", 12000)}, {"role": "assistant", "content": strings.Repeat("b", 12000)}, {"role": "user", "content": "LATEST QUESTION"}}
	body, _ := json.Marshal(map[string]any{"model": "m", "messages": messages})
	trimmed, orig, final, err := trimChatContext(body, 1500)
	if err != nil {
		t.Fatal(err)
	}
	if final >= orig || final > 1700 {
		t.Fatalf("context was not compacted enough orig=%d final=%d", orig, final)
	}
	if !strings.Contains(string(trimmed), "LATEST QUESTION") {
		t.Fatalf("newest turn was lost: %s", trimmed)
	}
}

func TestModelRequestRecoversITPMBeforeReturning(t *testing.T) {
	calls := 0
	var secondTokens int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		b, _ := io.ReadAll(r.Body)
		if calls == 1 {
			w.WriteHeader(429)
			_, _ = w.Write([]byte(`RateLimitError input tokens per minute (ITPM): Limit 7000, Requested 11705`))
			return
		}
		secondTokens = estimateChatInputTokens(b)
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"total_tokens":10}}`))
	}))
	defer srv.Close()
	payload, _ := json.Marshal(map[string]any{"model": "qwen", "messages": []map[string]any{{"role": "user", "content": strings.Repeat("x", 46000)}, {"role": "user", "content": "keep this"}}})
	a := &app{inferenceHTTP: srv.Client()}
	makeReq := func(body []byte) (*http.Request, error) {
		return http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, strings.NewReader(string(body)))
	}
	resp, _, _, meta, err := a.doModelRequestWithRecovery(context.Background(), payload, "qwen", makeReq)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || calls != 2 {
		t.Fatalf("status=%d calls=%d meta=%#v", resp.StatusCode, calls, meta)
	}
	if !meta.ContextTrimmed || secondTokens >= 7000 {
		t.Fatalf("retry did not compact below limit tokens=%d meta=%#v", secondTokens, meta)
	}
}
