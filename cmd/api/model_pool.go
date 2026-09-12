package main

import (
	"context"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/poom-ytthpch/daiki-ai-passport-backend/internal/store"
)

type modelPoolContextKey struct{}

type modelPoolRequestState struct {
	Route   string
	Visited map[string]bool
	Initial modelPoolSelection
}

type modelPoolSelection struct {
	Route          string
	Model          string
	CandidateCount int
	Reason         string
	Score          float64
	DailyUsed      int
	DailyCapacity  int
	DailyAllowance int
}

type modelPoolHealth struct {
	Successes        int64
	Failures         int64
	LatencyMS        int64
	ObservedRPDLimit int
}

type modelPoolAdminStatus struct {
	State          string `json:"state"`
	CircuitOpen    bool   `json:"circuitOpen"`
	DailyUsed      int    `json:"dailyUsed"`
	DailyCapacity  int    `json:"dailyCapacity"`
	DailyAllowance int    `json:"dailyAllowance"`
	Successes      int64  `json:"successes"`
	Failures       int64  `json:"failures"`
	AvgLatencyMS   int64  `json:"avgLatencyMs"`
}

type scoredPoolCandidate struct {
	Model          store.ProviderModel
	Score          float64
	Weight         int
	DailyUsed      int
	DailyCapacity  int
	DailyAllowance int
	OverPaced      bool
	Priority       int
}

func normalizePoolRoute(route string) string {
	route = strings.ToLower(strings.TrimSpace(route))
	switch route {
	case "fast", "balanced", "deep", "vision":
		return route
	default:
		return ""
	}
}

func withModelPoolRoute(ctx context.Context, route string) context.Context {
	route = normalizePoolRoute(route)
	if route == "" {
		return ctx
	}
	if state, ok := ctx.Value(modelPoolContextKey{}).(*modelPoolRequestState); ok && state != nil {
		state.Route = route
		if state.Visited == nil {
			state.Visited = map[string]bool{}
		}
		return ctx
	}
	return context.WithValue(ctx, modelPoolContextKey{}, &modelPoolRequestState{Route: route, Visited: map[string]bool{}})
}

func modelPoolState(ctx context.Context) *modelPoolRequestState {
	state, _ := ctx.Value(modelPoolContextKey{}).(*modelPoolRequestState)
	return state
}

func modelPoolHealthKey(model string) string {
	return "model:pool:health:" + strings.TrimPrefix(modelCircuitKey(model), "model:circuit:")
}

func modelPoolDailyKey(model string, now time.Time) (string, time.Duration) {
	day, ttl := pacificAdmissionWindow(now)
	return "model:rpd:" + strings.TrimPrefix(modelCircuitKey(model), "model:circuit:") + ":" + day, ttl
}

func pacificDayProgress(now time.Time) float64 {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		loc = time.FixedZone("PacificFallback", -8*60*60)
	}
	local := now.In(loc)
	start := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	next := start.AddDate(0, 0, 1)
	length := next.Sub(start)
	if length <= 0 {
		return 0
	}
	progress := local.Sub(start).Seconds() / length.Seconds()
	if progress < 0 {
		return 0
	}
	if progress > 1 {
		return 1
	}
	return progress
}

func modelDailyPacedAllowance(limit int, now time.Time) int {
	capacity := modelDailyAdmissionCapacity(limit)
	if capacity <= 0 {
		return 0
	}
	// Preserve a small burst while spreading scarce daily quota across the whole
	// Pacific provider day. This keeps a high-quality free model available later
	// instead of burning all RPD in the first traffic spike.
	burst := max(1, min(3, capacity/5))
	paced := int(math.Ceil(float64(capacity)*pacificDayProgress(now))) + burst
	if paced < burst {
		paced = burst
	}
	return min(capacity, paced)
}

func (a *app) peekModelDailyAdmission(ctx context.Context, model string, m store.ProviderModel) (used, capacity, allowance int) {
	limit := a.effectiveModelRPDLimit(ctx, model, m.RPDLimit)
	if limit <= 0 {
		return 0, 0, 0
	}
	capacity = modelDailyAdmissionCapacity(limit)
	allowance = modelDailyPacedAllowance(limit, time.Now())
	if a == nil || a.redis == nil {
		return 0, capacity, allowance
	}
	key, _ := modelPoolDailyKey(model, time.Now())
	value, err := a.redis.Get(ctx, key).Int()
	if err == nil && value > 0 {
		used = value
	}
	return used, capacity, allowance
}

func (a *app) loadModelPoolHealth(ctx context.Context, model string) modelPoolHealth {
	if a == nil || a.redis == nil {
		return modelPoolHealth{}
	}
	values, err := a.redis.HGetAll(ctx, modelPoolHealthKey(model)).Result()
	if err != nil {
		return modelPoolHealth{}
	}
	parse := func(key string) int64 {
		v, _ := strconv.ParseInt(values[key], 10, 64)
		return v
	}
	return modelPoolHealth{Successes: parse("ok"), Failures: parse("fail"), LatencyMS: parse("latency_ms"), ObservedRPDLimit: int(parse("observed_rpd_limit"))}
}

func (a *app) effectiveModelRPDLimit(ctx context.Context, model string, configured int) int {
	if configured > 0 {
		return configured
	}
	health := a.loadModelPoolHealth(ctx, model)
	if health.ObservedRPDLimit > 0 {
		return health.ObservedRPDLimit
	}
	return 0
}

func (a *app) observeModelRPDLimit(ctx context.Context, model string, limit int) {
	if a == nil || a.redis == nil || strings.TrimSpace(model) == "" || limit <= 0 {
		return
	}
	key := modelPoolHealthKey(model)
	pipe := a.redis.Pipeline()
	pipe.HSet(ctx, key, "observed_rpd_limit", limit)
	pipe.Expire(ctx, key, 30*24*time.Hour)
	_, _ = pipe.Exec(ctx)
}

func (a *app) recordModelPoolHealth(ctx context.Context, model string, status int, elapsed time.Duration) {
	if a == nil || a.redis == nil || strings.TrimSpace(model) == "" {
		return
	}
	key := modelPoolHealthKey(model)
	pipe := a.redis.Pipeline()
	if status > 0 && status < 400 {
		pipe.HIncrBy(ctx, key, "ok", 1)
	} else {
		pipe.HIncrBy(ctx, key, "fail", 1)
	}
	if elapsed > 0 {
		pipe.HIncrBy(ctx, key, "latency_ms", elapsed.Milliseconds())
	}
	pipe.Expire(ctx, key, 6*time.Hour)
	_, _ = pipe.Exec(ctx)
}

func (a *app) modelPoolStatus(ctx context.Context, model store.ProviderModel) modelPoolAdminStatus {
	status := modelPoolAdminStatus{State: "ready"}
	status.CircuitOpen = a.modelCircuitOpen(ctx, model.LiteLLMModelName)
	status.DailyUsed, status.DailyCapacity, status.DailyAllowance = a.peekModelDailyAdmission(ctx, model.LiteLLMModelName, model)
	health := a.loadModelPoolHealth(ctx, model.LiteLLMModelName)
	status.Successes, status.Failures = health.Successes, health.Failures
	if total := health.Successes + health.Failures; total > 0 {
		status.AvgLatencyMS = health.LatencyMS / total
	}
	switch {
	case status.CircuitOpen:
		status.State = "cooldown"
	case status.DailyCapacity > 0 && status.DailyUsed >= status.DailyCapacity:
		status.State = "daily-full"
	case status.DailyAllowance > 0 && status.DailyUsed >= status.DailyAllowance:
		status.State = "paced"
	}
	return status
}

func poolCandidateScore(m store.ProviderModel, health modelPoolHealth, used, capacity, allowance int) (float64, bool) {
	quality := m.QualityScore
	if quality <= 0 {
		quality = 50
	}
	weight := m.FreePoolWeight
	if weight <= 0 {
		weight = 100
	}
	score := float64(weight) * (0.55 + float64(quality)/100.0)
	total := health.Successes + health.Failures
	if total > 0 {
		successRate := float64(health.Successes+1) / float64(total+2)
		score *= 0.25 + 0.75*successRate
		avgLatency := float64(health.LatencyMS) / float64(total)
		if avgLatency > 0 {
			latencyFactor := 5000.0 / avgLatency
			if latencyFactor < 0.35 {
				latencyFactor = 0.35
			}
			if latencyFactor > 1.25 {
				latencyFactor = 1.25
			}
			score *= latencyFactor
		}
	}
	overPaced := false
	if capacity > 0 {
		if used >= capacity {
			return 0, true
		}
		remainingRatio := float64(capacity-used) / float64(capacity)
		score *= 0.55 + 0.45*remainingRatio
		overPaced = allowance > 0 && used >= allowance
		if overPaced {
			score *= 0.05
		}
	}
	return score, overPaced
}

func weightedPoolOffset(counter int64, totalWeight int) int {
	if totalWeight <= 0 {
		return 0
	}
	// Multiplicative hashing scatters consecutive Redis counters across the whole
	// weighted range. Using counter%total directly creates long startup streaks in
	// the first bucket and defeats load spreading when weights are large.
	x := uint64(counter) * uint64(11400714819323198485)
	return int(x % uint64(totalWeight))
}

func (a *app) selectFreePoolModel(ctx context.Context, route, current string, excluded map[string]bool) modelPoolSelection {
	selection := modelPoolSelection{Route: normalizePoolRoute(route)}
	if a == nil || a.store == nil || selection.Route == "" {
		return selection
	}
	models, err := a.store.FreePoolModels(ctx, selection.Route)
	if err != nil || len(models) == 0 {
		return selection
	}
	candidates := make([]scoredPoolCandidate, 0, len(models))
	hasPacedCandidate := false
	for _, model := range models {
		name := strings.TrimSpace(model.LiteLLMModelName)
		if name == "" || (excluded != nil && excluded[name]) || a.modelCircuitOpen(ctx, name) {
			continue
		}
		used, capacity, allowance := a.peekModelDailyAdmission(ctx, name, model)
		health := a.loadModelPoolHealth(ctx, name)
		score, overPaced := poolCandidateScore(model, health, used, capacity, allowance)
		if score <= 0 {
			continue
		}
		candidate := scoredPoolCandidate{Model: model, Score: score, Weight: max(1, int(math.Round(score*10))), DailyUsed: used, DailyCapacity: capacity, DailyAllowance: allowance, OverPaced: overPaced}
		candidates = append(candidates, candidate)
		if !overPaced {
			hasPacedCandidate = true
		}
	}
	if len(candidates) > 0 {
		bestPriority := candidates[0].Priority
		for _, candidate := range candidates[1:] {
			if candidate.Priority < bestPriority {
				bestPriority = candidate.Priority
			}
		}
		filtered := candidates[:0]
		for _, candidate := range candidates {
			if candidate.Priority == bestPriority {
				filtered = append(filtered, candidate)
			}
		}
		candidates = filtered
		hasPacedCandidate = false
		for _, candidate := range candidates {
			if !candidate.OverPaced {
				hasPacedCandidate = true
				break
			}
		}
	}
	if hasPacedCandidate {
		filtered := candidates[:0]
		for _, candidate := range candidates {
			if !candidate.OverPaced {
				filtered = append(filtered, candidate)
			}
		}
		candidates = filtered
	}
	if len(candidates) == 0 {
		return selection
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Score == candidates[j].Score {
			return candidates[i].Model.LiteLLMModelName < candidates[j].Model.LiteLLMModelName
		}
		return candidates[i].Score > candidates[j].Score
	})
	chosen := candidates[0]
	if a.redis != nil && len(candidates) > 1 {
		totalWeight := 0
		for i := range candidates {
			candidates[i].Weight = min(candidates[i].Weight, 100000)
			totalWeight += candidates[i].Weight
		}
		if totalWeight > 0 {
			counter, err := a.redis.Incr(ctx, "model:pool:rr:"+selection.Route).Result()
			if err == nil {
				_ = a.redis.Expire(ctx, "model:pool:rr:"+selection.Route, 24*time.Hour).Err()
				pick := weightedPoolOffset(counter, totalWeight)
				for _, candidate := range candidates {
					if pick < candidate.Weight {
						chosen = candidate
						break
					}
					pick -= candidate.Weight
				}
			}
		}
	}
	selection.Model = chosen.Model.LiteLLMModelName
	selection.CandidateCount = len(candidates)
	selection.Reason = "weighted_health_quota"
	selection.Score = chosen.Score
	selection.DailyUsed = chosen.DailyUsed
	selection.DailyCapacity = chosen.DailyCapacity
	selection.DailyAllowance = chosen.DailyAllowance
	return selection
}

func (a *app) selectInitialFreePoolModel(ctx context.Context, route, current string) (context.Context, modelPoolSelection) {
	ctx = withModelPoolRoute(ctx, route)
	selection := a.selectFreePoolModel(ctx, route, current, nil)
	state := modelPoolState(ctx)
	if state != nil {
		state.Initial = selection
		if selection.Model != "" {
			state.Visited[selection.Model] = true
		}
	}
	return ctx, selection
}

func (a *app) preparePoolFallback(ctx context.Context, current string, m store.ProviderModel) store.ProviderModel {
	state := modelPoolState(ctx)
	if state == nil || state.Route == "" {
		return m
	}
	if state.Visited == nil {
		state.Visited = map[string]bool{}
	}
	state.Visited[current] = true
	selection := a.selectFreePoolModel(ctx, state.Route, current, state.Visited)
	if selection.Model != "" && selection.Model != current {
		m.FallbackModelName = selection.Model
		if m.ContextStrategy == "" || m.ContextStrategy == "reject" || m.ContextStrategy == "trim" {
			m.ContextStrategy = "adaptive"
		}
		return m
	}
	if state.Route == "vision" {
		// Never let an image request escape the explicitly vision-capable free pool.
		m.FallbackModelName = ""
	}
	return m
}
