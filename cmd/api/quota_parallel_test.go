package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/poom-ytthpch/daiki-ai-passport-backend/internal/store"
)

func TestQuotaWindowHourCount(t *testing.T) {
	now := time.Date(2026, 9, 4, 11, 15, 0, 0, time.UTC)
	start, reset := quotaWindow(store.Policy{IntervalKind: "hour", IntervalCount: 3}, now)
	wantStart := time.Date(2026, 9, 4, 9, 0, 0, 0, time.UTC)
	wantReset := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	if !start.Equal(wantStart) || reset == nil || !reset.Equal(wantReset) {
		t.Fatalf("unexpected 3-hour quota window start=%v reset=%v", start, reset)
	}
}

func TestPolicyQuotaSpecsIncludeParallelLimits(t *testing.T) {
	weekly := int64(10_000_000)
	p := store.Policy{
		QuotaMode:      "limited",
		TokenLimit:     &weekly,
		IntervalKind:   "week",
		IntervalCount:  1,
		ParallelLimits: json.RawMessage(`[{"id":"parallel-1","tokenLimit":10000,"intervalKind":"hour","intervalCount":2}]`),
	}
	specs, err := policyQuotaSpecs(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 2 {
		t.Fatalf("expected primary + parallel limit, got %#v", specs)
	}
	if !specs[0].Primary || specs[0].Limit != 10_000_000 || specs[0].IntervalKind != "week" {
		t.Fatalf("unexpected primary spec %#v", specs[0])
	}
	if specs[1].Primary || specs[1].Limit != 10_000 || specs[1].IntervalKind != "hour" || specs[1].IntervalCount != 2 {
		t.Fatalf("unexpected parallel spec %#v", specs[1])
	}
}

func TestQuotaRetryAfterSeconds(t *testing.T) {
	if got := quotaRetryAfterSeconds(quotaDecision{}); got != 0 {
		t.Fatalf("lifetime/no-reset quota must not advertise Retry-After, got %d", got)
	}
	reset := time.Now().Add(90 * time.Second)
	got := quotaRetryAfterSeconds(quotaDecision{ResetAt: &reset})
	if got < 89 || got > 90 {
		t.Fatalf("unexpected Retry-After seconds: got %d", got)
	}
	past := time.Now().Add(-time.Second)
	if got := quotaRetryAfterSeconds(quotaDecision{ResetAt: &past}); got != 1 {
		t.Fatalf("past reset should retry immediately with a bounded 1s header, got %d", got)
	}
}
