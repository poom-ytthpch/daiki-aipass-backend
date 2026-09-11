package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

type researchPreferences struct {
	Region string
	Locale string
	Scope  string
	Depth  string
	Focus  string
}

type researchSearchQuery struct {
	Query  string
	Region string
	Locale string
	Stage  string
}

type webResearchResult struct {
	Sources           []researchSource
	Queries           []string
	LocalSourceCount  int
	GlobalSourceCount int
	SocialSourceCount int
	SocialPlatforms   []string
}

type researchRunContextKey struct{}

func normalizeResearchRegion(v any) string {
	s := strings.ToUpper(strings.TrimSpace(fmt.Sprint(v)))
	switch s {
	case "TH", "THA", "THAILAND", "ไทย", "ประเทศไทย":
		return "TH"
	case "GLOBAL", "WORLD", "WORLDWIDE":
		return "GLOBAL"
	default:
		return ""
	}
}

func normalizeResearchLocale(v any) string {
	s := strings.TrimSpace(fmt.Sprint(v))
	if len(s) > 32 {
		s = s[:32]
	}
	return s
}

func normalizeResearchDepth(v any) string {
	s := strings.ToLower(strings.TrimSpace(fmt.Sprint(v)))
	switch s {
	case "deep", "extensive", "thorough":
		return "deep"
	default:
		return "standard"
	}
}

func normalizeResearchScope(v any, region string) string {
	s := strings.ToLower(strings.TrimSpace(fmt.Sprint(v)))
	switch s {
	case "local-only", "country-only":
		if region != "" && region != "GLOBAL" {
			return "local-only"
		}
		return "global"
	case "global", "worldwide":
		return "global"
	case "local-first", "country-first":
		if region != "" && region != "GLOBAL" {
			return "local-first"
		}
		return "global"
	default:
		if region == "TH" {
			return "local-first"
		}
		return "global"
	}
}

func researchPreferencesFromPayload(payload map[string]any, query string) researchPreferences {
	region := normalizeResearchRegion(payload["researchRegion"])
	locale := normalizeResearchLocale(payload["researchLocale"])
	if region == "" && (strings.HasPrefix(strings.ToLower(locale), "th") || containsThai(query)) {
		region = "TH"
	}
	depth := normalizeResearchDepth(payload["researchDepth"])
	focus := strings.TrimSpace(fmt.Sprint(payload["researchFocus"]))
	if len(focus) > 240 {
		focus = focus[:240]
	}
	return researchPreferences{
		Region: region,
		Locale: locale,
		Scope:  normalizeResearchScope(payload["researchScope"], region),
		Depth:  depth,
		Focus:  focus,
	}
}

func deleteResearchPreferenceFields(payload map[string]any) {
	for _, key := range []string{"researchRegion", "researchLocale", "researchScope", "researchDepth", "researchFocus"} {
		delete(payload, key)
	}
}

func containsThai(s string) bool {
	for _, r := range s {
		if r >= '\u0E00' && r <= '\u0E7F' {
			return true
		}
	}
	return false
}

func researchHost(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimPrefix(strings.TrimSuffix(u.Hostname(), "."), "www."))
}

func researchSocialPlatform(rawURL string) string {
	host := researchHost(rawURL)
	switch {
	case host == "facebook.com" || strings.HasSuffix(host, ".facebook.com"):
		return "Facebook"
	case host == "instagram.com" || strings.HasSuffix(host, ".instagram.com"):
		return "Instagram"
	case host == "tiktok.com" || strings.HasSuffix(host, ".tiktok.com"):
		return "TikTok"
	case host == "x.com" || strings.HasSuffix(host, ".x.com") || host == "twitter.com" || strings.HasSuffix(host, ".twitter.com"):
		return "X"
	case host == "youtube.com" || strings.HasSuffix(host, ".youtube.com") || host == "youtu.be":
		return "YouTube"
	case host == "reddit.com" || strings.HasSuffix(host, ".reddit.com"):
		return "Reddit"
	case host == "threads.net" || strings.HasSuffix(host, ".threads.net"):
		return "Threads"
	case host == "linkedin.com" || strings.HasSuffix(host, ".linkedin.com"):
		return "LinkedIn"
	case host == "pantip.com" || strings.HasSuffix(host, ".pantip.com"):
		return "Pantip"
	default:
		return ""
	}
}

func researchSourceType(rawURL string) string {
	if researchSocialPlatform(rawURL) != "" {
		return "social"
	}
	return "web"
}

func researchSocialIntent(query string) bool {
	q := strings.ToLower(query)
	for _, signal := range []string{"facebook", "instagram", "tiktok", "social", "reddit", "pantip", "youtube", "twitter", "threads", "community", "review", "รีวิว", "โซเชียล", "social media", "ผู้ใช้จริง", "ประสบการณ์ผู้ใช้"} {
		if strings.Contains(q, signal) {
			return true
		}
	}
	return false
}

func researchThailandSource(title, rawURL, content string) bool {
	host := researchHost(rawURL)
	if host == "" {
		return false
	}
	if strings.HasSuffix(host, ".th") {
		return true
	}
	hay := strings.ToLower(title + " " + rawURL + " " + content)
	return strings.Contains(hay, "thailand") || strings.Contains(hay, "ประเทศไทย") || strings.Contains(hay, "กรุงเทพ") || strings.Contains(hay, " bangkok ")
}

func researchAuthority(rawURL string) string {
	host := researchHost(rawURL)
	switch {
	case researchOfficialHost(rawURL):
		return "official"
	case strings.HasSuffix(host, ".ac.th") || strings.HasSuffix(host, ".edu") || strings.Contains(host, ".edu."):
		return "academic"
	case researchSocialPlatform(rawURL) != "":
		return "community"
	default:
		return "secondary"
	}
}

func researchResultRankForPreferences(query, title, rawURL, content string, base float64, prefs researchPreferences) float64 {
	rank := researchResultRank(query, title, rawURL, content, base)
	if prefs.Region == "TH" {
		if researchThailandSource(title, rawURL, content) {
			rank += 7
		}
		host := researchHost(rawURL)
		if strings.HasSuffix(host, ".go.th") {
			rank += 5
		} else if strings.HasSuffix(host, ".ac.th") {
			rank += 3
		} else if strings.HasSuffix(host, ".or.th") || strings.HasSuffix(host, ".co.th") {
			rank += 2
		}
		if containsThai(title + " " + content) {
			rank += 1.25
		}
	}
	return rank
}

func researchSearchPlan(query string, prefs researchPreferences) []researchSearchQuery {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil
	}
	focusQuery := query
	if prefs.Focus != "" {
		focusQuery = strings.TrimSpace(query + " " + prefs.Focus)
	}
	plan := make([]researchSearchQuery, 0, 14)
	add := func(q, region, locale, stage string) {
		q = strings.TrimSpace(q)
		if q == "" {
			return
		}
		for _, existing := range plan {
			if strings.EqualFold(existing.Query, q) {
				return
			}
		}
		plan = append(plan, researchSearchQuery{Query: q, Region: region, Locale: locale, Stage: stage})
	}
	addSocial := func(base, region, locale, stage, locality string, compact bool) {
		locality = strings.TrimSpace(locality)
		if locality != "" {
			base = strings.TrimSpace(base + " " + locality)
		}
		add("site:facebook.com "+base, region, locale, stage)
		add("site:instagram.com "+base, region, locale, stage)
		add("site:tiktok.com "+base, region, locale, stage)
		if compact {
			add("(site:youtube.com OR site:reddit.com OR site:pantip.com OR site:x.com OR site:threads.net) "+base, region, locale, stage)
			return
		}
		add("(site:youtube.com OR site:reddit.com OR site:pantip.com) "+base, region, locale, stage)
		add("(site:x.com OR site:twitter.com OR site:threads.net OR site:linkedin.com) "+base, region, locale, stage)
	}

	if prefs.Region == "TH" && prefs.Scope != "global" {
		localBase := focusQuery
		lowerBase := strings.ToLower(localBase)
		if !strings.Contains(lowerBase, "thailand") && !strings.Contains(localBase, "ประเทศไทย") && !strings.Contains(localBase, "ไทย") {
			add(localBase+" ประเทศไทย", "TH", "th-TH", "local")
		}
		add(localBase+" Thailand", "TH", "th-TH", "local")
		add("site:go.th "+localBase, "TH", "th-TH", "local-official")
		if prefs.Depth == "deep" {
			add("site:ac.th "+localBase, "TH", "th-TH", "local-academic")
			switch {
			case strings.Contains(lowerBase, "price") || strings.Contains(lowerBase, "promo") || strings.Contains(lowerBase, "ราคา") || strings.Contains(lowerBase, "โปรโมชั่น"):
				add(localBase+" ราคา โปรโมชั่น ตัวแทนจำหน่าย ไทย", "TH", "th-TH", "local-market")
			case strings.Contains(lowerBase, "car") || strings.Contains(lowerBase, "รถ") || strings.Contains(lowerBase, "ev"):
				add(localBase+" ไทย สเปก ราคา ศูนย์บริการ ประกัน", "TH", "th-TH", "local-market")
			default:
				add(localBase+" ไทย ข่าว ข้อมูลล่าสุด", "TH", "th-TH", "local-current")
			}
			addSocial(localBase, "TH", "th-TH", "social-local", "ประเทศไทย Thailand ไทย", false)
		} else if researchSocialIntent(query) {
			addSocial(localBase, "TH", "th-TH", "social-local", "ประเทศไทย Thailand ไทย", true)
		}
	}

	if prefs.Scope != "local-only" {
		add(focusQuery, "GLOBAL", "all", "global")
		if prefs.Depth == "deep" {
			add(focusQuery+" official documentation", "GLOBAL", "all", "global-official")
			lower := strings.ToLower(focusQuery)
			if strings.Contains(lower, "compare") || strings.Contains(lower, "comparison") || strings.Contains(lower, "เปรียบเทียบ") {
				add(focusQuery+" comparison review", "GLOBAL", "all", "global-comparison")
			}
			if strings.Contains(lower, "safe") || strings.Contains(lower, "safety") || strings.Contains(lower, "risk") || strings.Contains(lower, "อันตราย") || strings.Contains(lower, "ปลอดภัย") {
				add(focusQuery+" safety risk recall", "GLOBAL", "all", "global-safety")
			}
			addSocial(focusQuery, "GLOBAL", "all", "social-global", "", true)
		} else if researchSocialIntent(query) {
			addSocial(focusQuery, "GLOBAL", "all", "social-global", "", true)
		}
	}

	for _, variant := range researchQueryVariants(query) {
		if len(plan) >= 14 {
			break
		}
		add(variant, "GLOBAL", "all", "entity")
	}
	if prefs.Depth != "deep" && len(plan) > 5 {
		plan = plan[:5]
	}
	if len(plan) > 14 {
		plan = plan[:14]
	}
	return plan
}

func researchPlanQueries(plan []researchSearchQuery) []string {
	out := make([]string, 0, len(plan))
	for _, item := range plan {
		out = append(out, item.Query)
	}
	return out
}

func researchSourceRegion(title, rawURL, content string, prefs researchPreferences) string {
	if prefs.Region == "TH" && researchThailandSource(title, rawURL, content) {
		return "TH"
	}
	return "GLOBAL"
}

func sortResearchResults(results []searxResult, query string, prefs researchPreferences) {
	sort.SliceStable(results, func(i, j int) bool {
		return researchResultRankForPreferences(query, results[i].Title, results[i].URL, results[i].Content, results[i].Score, prefs) > researchResultRankForPreferences(query, results[j].Title, results[j].URL, results[j].Content, results[j].Score, prefs)
	})
}

func withResearchRunID(ctx context.Context, runID string) context.Context {
	if strings.TrimSpace(runID) == "" {
		return ctx
	}
	return context.WithValue(ctx, researchRunContextKey{}, strings.TrimSpace(runID))
}

func researchRunID(ctx context.Context) string {
	id, _ := ctx.Value(researchRunContextKey{}).(string)
	return strings.TrimSpace(id)
}

func (a *app) publishResearchProgress(ctx context.Context, research map[string]any) {
	if a.store == nil {
		return
	}
	if runID := researchRunID(ctx); runID != "" {
		_ = a.store.UpdateChatRunActivity(context.Background(), runID, map[string]any{"phase": "research", "research": research})
	}
}

func summarizeWebResearchResult(sources []researchSource, queries []string) webResearchResult {
	result := webResearchResult{Sources: sources, Queries: queries}
	seenPlatforms := map[string]bool{}
	for _, source := range sources {
		if source.Region == "TH" {
			result.LocalSourceCount++
		} else {
			result.GlobalSourceCount++
		}
		if source.SourceType == "social" {
			result.SocialSourceCount++
			if source.Platform != "" && !seenPlatforms[source.Platform] {
				seenPlatforms[source.Platform] = true
				result.SocialPlatforms = append(result.SocialPlatforms, source.Platform)
			}
		}
	}
	return result
}

func (a *app) webResearchWithPreferences(ctx context.Context, query string, prefs researchPreferences) (webResearchResult, error) {
	if prefs.Depth == "" {
		prefs.Depth = "standard"
	}
	if prefs.Scope == "" {
		prefs.Scope = normalizeResearchScope("", prefs.Region)
	}
	plan := researchSearchPlan(query, prefs)
	queries := researchPlanQueries(plan)
	a.publishResearchProgress(ctx, map[string]any{"phase": "planning", "region": prefs.Region, "scope": prefs.Scope, "depth": prefs.Depth, "queries": queries})

	directURLs := append([]string{}, extractResearchURLs(query)...)
	directURLs = append(directURLs, preferredResearchURLs(query)...)
	direct := make([]researchSource, 0, len(directURLs))
	if len(directURLs) > 0 {
		client := newPublicWebClient(10 * time.Second)
		seenDirect := map[string]bool{}
		for _, raw := range directURLs {
			if seenDirect[raw] {
				continue
			}
			seenDirect[raw] = true
			text, err := fetchPublicPage(ctx, client, raw)
			if err != nil {
				continue
			}
			title := raw
			if u, err := url.Parse(raw); err == nil && u.Hostname() != "" {
				title = u.Hostname()
			}
			direct = append(direct, researchSource{Title: title, URL: raw, Excerpt: clipText(text, 6500), Engine: "direct", Region: researchSourceRegion(title, raw, text, prefs), Authority: researchAuthority(raw), SourceType: researchSourceType(raw), Platform: researchSocialPlatform(raw)})
		}
		if len(direct) > 0 && prefs.Depth != "deep" {
			for i := range direct {
				direct[i].Index = i + 1
			}
			return summarizeWebResearchResult(direct, queries), nil
		}
	}

	base := strings.TrimRight(strings.TrimSpace(a.cfg.SearXNGBase), "/")
	if base == "" {
		if len(direct) > 0 {
			for i := range direct {
				direct[i].Index = i + 1
			}
			return summarizeWebResearchResult(direct, queries), nil
		}
		return webResearchResult{}, errors.New("search service not configured")
	}

	var results []searxResult
	var lastSearchErr error
	lastPhase := ""
	for i, item := range plan {
		phase := "searching-global"
		if item.Stage == "social-local" {
			phase = "searching-social-local"
		} else if item.Stage == "social-global" {
			phase = "searching-social-global"
		} else if item.Region == "TH" {
			phase = "searching-local"
		}
		if phase != lastPhase {
			a.publishResearchProgress(ctx, map[string]any{"phase": phase, "region": prefs.Region, "scope": prefs.Scope, "depth": prefs.Depth, "queries": queries, "queryIndex": i + 1, "queryCount": len(plan), "stage": item.Stage})
			lastPhase = phase
		}
		found, err := a.searxSearchWithLanguage(ctx, base, item.Query, item.Locale)
		if err != nil {
			lastSearchErr = err
			continue
		}
		results = append(results, found...)
	}
	if len(results) == 0 {
		if len(direct) > 0 {
			for i := range direct {
				direct[i].Index = i + 1
			}
			return summarizeWebResearchResult(direct, queries), nil
		}
		if lastSearchErr != nil {
			return webResearchResult{}, lastSearchErr
		}
		return webResearchResult{}, errors.New("no search results")
	}

	seenURLs := map[string]bool{}
	unique := make([]searxResult, 0, len(results))
	for _, row := range results {
		if row.URL == "" || seenURLs[row.URL] {
			continue
		}
		seenURLs[row.URL] = true
		unique = append(unique, row)
	}
	results = unique
	sortResearchResults(results, query, prefs)

	limit := a.cfg.WebResearchMaxResults
	if limit <= 0 || limit > 8 {
		if prefs.Depth == "deep" {
			limit = 8
		} else {
			limit = 5
		}
	}
	localWeb := make([]researchSource, 0, limit)
	localSocial := make([]researchSource, 0, limit)
	globalWeb := make([]researchSource, 0, limit)
	globalSocial := make([]researchSource, 0, limit)
	for _, row := range results {
		if !isHTTPURL(row.URL) || !researchResultRelevant(query, row.Title, row.URL, row.Content) {
			continue
		}
		source := researchSource{Title: clipText(row.Title, 300), URL: row.URL, Snippet: clipText(row.Content, 1200), Engine: clipText(row.Engine, 80), Region: researchSourceRegion(row.Title, row.URL, row.Content, prefs), Authority: researchAuthority(row.URL), SourceType: researchSourceType(row.URL), Platform: researchSocialPlatform(row.URL)}
		if source.Region == "TH" && source.SourceType == "social" {
			localSocial = append(localSocial, source)
		} else if source.Region == "TH" {
			localWeb = append(localWeb, source)
		} else if source.SourceType == "social" {
			globalSocial = append(globalSocial, source)
		} else {
			globalWeb = append(globalWeb, source)
		}
	}

	sources := make([]researchSource, 0, limit)
	selected := map[string]bool{}
	appendRowsN := func(rows []researchSource, maxAdd int) {
		added := 0
		for _, source := range rows {
			if len(sources) >= limit || (maxAdd > 0 && added >= maxAdd) {
				break
			}
			if selected[source.URL] {
				continue
			}
			selected[source.URL] = true
			sources = append(sources, source)
			added++
		}
	}
	appendRowsN(direct, 0)
	if prefs.Region == "TH" && prefs.Scope != "global" {
		if prefs.Depth == "deep" {
			localWebQuota, localSocialQuota, globalWebQuota := 3, 2, 2
			if limit <= 5 {
				// Keep Thailand first, but reserve room for global corroboration even
				// when the deployment still uses the legacy five-source cap.
				localWebQuota, localSocialQuota, globalWebQuota = 2, 1, 1
			}
			appendRowsN(localWeb, localWebQuota)
			appendRowsN(localSocial, localSocialQuota)
			if prefs.Scope != "local-only" {
				appendRowsN(globalWeb, globalWebQuota)
				appendRowsN(globalSocial, 1)
			}
		} else {
			appendRowsN(localWeb, 0)
			if researchSocialIntent(query) {
				appendRowsN(localSocial, 1)
			}
			if prefs.Scope != "local-only" {
				appendRowsN(globalWeb, 2)
			}
			if prefs.Scope != "local-only" && researchSocialIntent(query) {
				appendRowsN(globalSocial, 1)
			}
		}
		appendRowsN(localWeb, 0)
		appendRowsN(localSocial, 0)
		if prefs.Scope != "local-only" {
			appendRowsN(globalWeb, 0)
			appendRowsN(globalSocial, 0)
		}
	} else {
		if prefs.Depth == "deep" {
			appendRowsN(globalWeb, 5)
			appendRowsN(globalSocial, 3)
		} else {
			appendRowsN(globalWeb, 0)
			if researchSocialIntent(query) {
				appendRowsN(globalSocial, 1)
			}
		}
		appendRowsN(globalWeb, 0)
		appendRowsN(globalSocial, 0)
		appendRowsN(localWeb, 0)
		appendRowsN(localSocial, 0)
	}
	if len(sources) == 0 {
		return webResearchResult{}, errors.New("no usable search results")
	}
	for i := range sources {
		sources[i].Index = i + 1
	}

	fetchN := a.cfg.WebResearchFetchPages
	if fetchN <= 0 || fetchN > 5 {
		if prefs.Depth == "deep" {
			fetchN = 5
		} else {
			fetchN = 3
		}
	}
	fetchIndexes := make([]int, 0, fetchN)
	for i := range sources {
		// Public social pages are frequently login-walled or bot-blocked. Keep the
		// indexed search snippet instead of failing the full research run.
		if sources[i].SourceType == "social" {
			continue
		}
		fetchIndexes = append(fetchIndexes, i)
		if len(fetchIndexes) >= fetchN {
			break
		}
	}
	a.publishResearchProgress(ctx, map[string]any{"phase": "reading-sources", "region": prefs.Region, "scope": prefs.Scope, "depth": prefs.Depth, "queries": queries, "sourceCount": len(sources)})
	client := newPublicWebClient(8 * time.Second)
	var wg sync.WaitGroup
	for _, idx := range fetchIndexes {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			if text, err := fetchPublicPage(ctx, client, sources[idx].URL); err == nil {
				sources[idx].Excerpt = clipText(text, 6500)
				if sources[idx].Region != "TH" && researchSourceRegion(sources[idx].Title, sources[idx].URL, text, prefs) == "TH" {
					sources[idx].Region = "TH"
				}
			}
		}(idx)
	}
	wg.Wait()
	result := summarizeWebResearchResult(sources, queries)
	a.publishResearchProgress(ctx, map[string]any{"phase": "verifying-sources", "region": prefs.Region, "scope": prefs.Scope, "depth": prefs.Depth, "queries": queries, "sourceCount": len(sources), "localSourceCount": result.LocalSourceCount, "globalSourceCount": result.GlobalSourceCount, "socialSourceCount": result.SocialSourceCount, "socialPlatforms": result.SocialPlatforms})
	return result, nil
}
