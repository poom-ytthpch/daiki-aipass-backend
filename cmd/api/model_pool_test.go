package main

import (
	"testing"
	"time"

	"github.com/poom-ytthpch/daiki-ai-passport-backend/internal/store"
)

func TestModelDailyPacingSpreadsScarceFreeQuota(t *testing.T) {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 9, 12, 0, 5, 0, 0, loc)
	midday := time.Date(2026, 9, 12, 12, 0, 0, 0, loc)
	late := time.Date(2026, 9, 12, 23, 50, 0, 0, loc)
	if got := modelDailyAdmissionCapacity(20); got != 18 {
		t.Fatalf("20 RPD should reserve 10%% headroom, got %d", got)
	}
	if got := modelDailyPacedAllowance(20, start); got < 1 || got > 4 {
		t.Fatalf("early-day allowance should keep a small burst, got %d", got)
	}
	if got := modelDailyPacedAllowance(20, midday); got < 10 || got > 13 {
		t.Fatalf("midday allowance should preserve roughly half-day quota, got %d", got)
	}
	if got := modelDailyPacedAllowance(20, late); got != 18 {
		t.Fatalf("late-day allowance should expose full safe capacity, got %d", got)
	}
}

func TestPoolCandidateScoreRejectsFullDailyQuotaAndPacesScarceModel(t *testing.T) {
	m := store.ProviderModel{FreePoolWeight: 100, QualityScore: 100}
	if score, _ := poolCandidateScore(m, modelPoolHealth{}, 18, 18, 18); score != 0 {
		t.Fatalf("full daily quota must be unavailable, score=%f", score)
	}
	score, paced := poolCandidateScore(m, modelPoolHealth{Successes: 10, Failures: 0, LatencyMS: 10000}, 5, 18, 5)
	if score <= 0 || !paced {
		t.Fatalf("paced candidate should remain emergency-eligible with a low score, score=%f paced=%v", score, paced)
	}
}

func TestPoolCandidateHealthAndQualityInfluenceScore(t *testing.T) {
	good := store.ProviderModel{FreePoolWeight: 100, QualityScore: 95}
	weak := store.ProviderModel{FreePoolWeight: 100, QualityScore: 60}
	goodScore, _ := poolCandidateScore(good, modelPoolHealth{Successes: 20, Failures: 1, LatencyMS: 20000}, 0, 0, 0)
	weakScore, _ := poolCandidateScore(weak, modelPoolHealth{Successes: 5, Failures: 10, LatencyMS: 60000}, 0, 0, 0)
	if goodScore <= weakScore {
		t.Fatalf("healthy high-quality model should score higher: good=%f weak=%f", goodScore, weakScore)
	}
}

func TestPoolRouteContext(t *testing.T) {
	ctx := withModelPoolRoute(t.Context(), "deep")
	state := modelPoolState(ctx)
	if state == nil || state.Route != "deep" || state.Visited == nil {
		t.Fatalf("unexpected pool state: %#v", state)
	}
	if normalizePoolRoute("unknown") != "" {
		t.Fatal("unsupported routes must not activate the pool")
	}
}

func TestWeightedPoolOffsetScattersConsecutiveRequests(t *testing.T) {
	seenBuckets := map[int]bool{}
	for i := int64(1); i <= 12; i++ {
		pick := weightedPoolOffset(i, 1000)
		switch {
		case pick < 400:
			seenBuckets[0] = true
		case pick < 700:
			seenBuckets[1] = true
		default:
			seenBuckets[2] = true
		}
	}
	if len(seenBuckets) < 3 {
		t.Fatalf("consecutive weighted picks should spread across buckets, got %#v", seenBuckets)
	}
}
