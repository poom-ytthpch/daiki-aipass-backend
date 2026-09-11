package main

import (
	"context"
	"errors"
	"fmt"
	htmlstd "html"
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
	QualityScore      int
	QualityGrade      string
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
	case strings.HasSuffix(host, ".go.th") || strings.HasSuffix(host, ".gov") || strings.Contains(host, ".gov."):
		return "government"
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
	relevance := researchRelevanceScore(query, title, rawURL, content)
	rank += float64(relevance) / 8
	if researchFreshnessIntent(query) {
		rank += float64(researchFreshnessScore(query, title+" "+content, time.Now())) / 30
	}
	if prefs.Region == "TH" {
		if researchThailandSource(title, rawURL, content) {
			rank += 4
		}
		host := researchHost(rawURL)
		if strings.HasSuffix(host, ".go.th") && researchRegulatoryIntent(query) {
			rank += 2
		} else if strings.HasSuffix(host, ".ac.th") && researchAcademicIntent(query) {
			rank += 2
		} else if strings.HasSuffix(host, ".or.th") || strings.HasSuffix(host, ".co.th") {
			rank += 1
		}
		if containsThai(title + " " + content) {
			rank += 0.75
		}
	}
	return rank
}

func researchSearchPlan(query string, prefs researchPreferences) []researchSearchQuery {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil
	}
	focusQuery := strings.TrimSpace(query)
	entityPhrase := researchEntityPhrase(query)
	currentYear := time.Now().Year()
	const maxQueries = 4
	plan := make([]researchSearchQuery, 0, maxQueries)
	add := func(q, region, locale, stage string) {
		q = strings.TrimSpace(q)
		if q == "" || len(plan) >= maxQueries {
			return
		}
		for _, existing := range plan {
			if strings.EqualFold(existing.Query, q) {
				return
			}
		}
		plan = append(plan, researchSearchQuery{Query: q, Region: region, Locale: locale, Stage: stage})
	}
	addSocial := func(base, region, locale, stage string) {
		add("(site:facebook.com OR site:instagram.com OR site:tiktok.com OR site:youtube.com OR site:reddit.com OR site:pantip.com) "+base, region, locale, stage)
	}

	if prefs.Region == "TH" && prefs.Scope != "global" {
		if researchRegulatoryIntent(query) {
			add("site:go.th "+focusQuery, "TH", "th-TH", "local-regulatory")
		} else if researchAcademicIntent(query) {
			add("site:ac.th "+focusQuery, "TH", "th-TH", "local-academic")
		}
		if entityPhrase != "" {
			// Keep the first query intentionally simple/high-recall. Google frequently
			// returns zero results for over-constrained combinations such as
			// "official distributor current campaign" even when relevant pages exist.
			if researchFreshnessIntent(query) {
				add(fmt.Sprintf(`%s ราคาล่าสุด สเปก Thailand %d`, entityPhrase, currentYear), "TH", "th-TH", "local-primary-current")
			} else {
				add(fmt.Sprintf(`%s ราคา สเปก Thailand %d`, entityPhrase, currentYear), "TH", "th-TH", "local-primary")
			}
			add(entityPhrase+` Thailand official distributor`, "TH", "th-TH", "local-primary-distributor")
		} else {
			add(focusQuery+" Thailand", "TH", "th-TH", "local")
		}
		if prefs.Depth == "deep" || researchSocialIntent(query) {
			base := focusQuery
			if entityPhrase != "" {
				base = entityPhrase + " Thailand รีวิว ปัญหา"
			}
			addSocial(base, "TH", "th-TH", "social-local")
		}
	}

	if prefs.Scope != "local-only" && len(plan) < maxQueries {
		if entityPhrase != "" {
			add(entityPhrase+` specifications`, "GLOBAL", "all", "global-primary")
		} else {
			add(focusQuery+" official documentation", "GLOBAL", "all", "global-primary")
		}
	}
	if len(plan) < maxQueries {
		for _, variant := range researchQueryVariants(query) {
			add(variant, "GLOBAL", "all", "entity")
			if len(plan) >= maxQueries {
				break
			}
		}
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

type researchOutboundCandidate struct {
	URL            string
	Host           string
	Score          int
	OfficialSignal bool
}

func researchIgnoredOutboundHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" || researchSocialPlatform("https://"+host) != "" {
		return true
	}
	for _, blocked := range []string{
		"google.com", "google.co.th", "gstatic.com", "googleapis.com", "googletagmanager.com",
		"doubleclick.net", "cloudflare.com", "line.me", "linktr.ee", "apple.com", "microsoft.com",
	} {
		if host == blocked || strings.HasSuffix(host, "."+blocked) {
			return true
		}
	}
	return false
}

func researchOutboundCandidates(baseURL, rawHTML, query string) []researchOutboundCandidate {
	base, err := url.Parse(baseURL)
	if err != nil || base.Hostname() == "" {
		return nil
	}
	baseHost := strings.ToLower(strings.TrimPrefix(base.Hostname(), "www."))
	type acc struct {
		candidate researchOutboundCandidate
		count     int
	}
	byHost := map[string]*acc{}
	for _, match := range researchHrefRE.FindAllStringSubmatchIndex(rawHTML, -1) {
		if len(match) < 4 || match[2] < 0 || match[3] <= match[2] {
			continue
		}
		href := htmlstd.UnescapeString(strings.TrimSpace(rawHTML[match[2]:match[3]]))
		ref, err := url.Parse(href)
		if err != nil {
			continue
		}
		target := base.ResolveReference(ref)
		target.Fragment = ""
		if !isHTTPURL(target.String()) {
			continue
		}
		host := strings.ToLower(strings.TrimPrefix(target.Hostname(), "www."))
		if host == baseHost || researchIgnoredOutboundHost(host) {
			continue
		}
		start := max(0, match[0]-280)
		end := min(len(rawHTML), match[1]+360)
		contextText := htmlstd.UnescapeString(researchTagRE.ReplaceAllString(rawHTML[start:end], " "))
		contextText = researchSpaceRE.ReplaceAllString(contextText, " ")
		score := 1
		official := researchOfficialDistributorSignal(contextText, contextText)
		if official {
			score += 8
		}
		lowerTarget := strings.ToLower(target.String())
		lowerContext := strings.ToLower(contextText)
		for _, term := range researchEntityTerms(query) {
			if len(term) < 3 || researchPureNumericModelTerm(term) {
				continue
			}
			if strings.Contains(lowerTarget, term) {
				score += 4
			} else if strings.Contains(lowerContext, term) {
				score += 2
			}
		}
		if researchCandidateRelevant(query, contextText, target.String(), contextText) {
			score += 5
		}
		entry := byHost[host]
		if entry == nil {
			entry = &acc{candidate: researchOutboundCandidate{URL: target.String(), Host: host, Score: score, OfficialSignal: official}}
			byHost[host] = entry
		} else {
			entry.count++
			entry.candidate.Score += min(3, entry.count)
			entry.candidate.OfficialSignal = entry.candidate.OfficialSignal || official
			if score > entry.candidate.Score {
				entry.candidate.URL = target.String()
			}
		}
	}
	out := make([]researchOutboundCandidate, 0, len(byHost))
	for _, entry := range byHost {
		out = append(out, entry.candidate)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > 6 {
		out = out[:6]
	}
	return out
}

func researchSitemapDirectives(origin string, robots string) []string {
	base, err := url.Parse(origin)
	if err != nil || base.Hostname() == "" {
		return nil
	}
	host := researchHost(origin)
	out := make([]string, 0, 3)
	seen := map[string]bool{}
	add := func(raw string) {
		u, err := url.Parse(strings.TrimSpace(htmlstd.UnescapeString(raw)))
		if err != nil || !isHTTPURL(u.String()) || researchHost(u.String()) != host || seen[u.String()] {
			return
		}
		seen[u.String()] = true
		out = append(out, u.String())
	}
	for _, match := range researchSitemapRE.FindAllStringSubmatch(robots, -1) {
		if len(match) > 1 {
			add(match[1])
		}
		if len(out) >= 2 {
			break
		}
	}
	defaultURL := *base
	defaultURL.Path = "/sitemap.xml"
	defaultURL.RawQuery = ""
	defaultURL.Fragment = ""
	add(defaultURL.String())
	if len(out) > 2 {
		out = out[:2]
	}
	return out
}

func researchSitemapEntityURLs(query, rawXML string) []string {
	type candidate struct {
		url   string
		score int
	}
	seen := map[string]bool{}
	rows := make([]candidate, 0, 12)
	for _, match := range researchLocRE.FindAllStringSubmatch(rawXML, -1) {
		if len(match) < 2 {
			continue
		}
		raw := strings.TrimSpace(htmlstd.UnescapeString(match[1]))
		if seen[raw] || !isHTTPURL(raw) {
			continue
		}
		seen[raw] = true
		relevance := researchRelevanceScore(query, raw, raw, raw)
		if relevance < 58 {
			continue
		}
		score := relevance
		lower := strings.ToLower(raw)
		if strings.Contains(lower, "/model/") || strings.Contains(lower, "/models/") || strings.Contains(lower, "/product/") {
			score += 20
		}
		if strings.Contains(lower, "overview") {
			score += 12
		}
		if strings.Contains(lower, "tech-spec") || strings.Contains(lower, "spec") {
			score += 8
		}
		if researchPriceIntent(query) && (strings.Contains(lower, "price") || strings.Contains(lower, "campaign")) {
			score += 6
		}
		rows = append(rows, candidate{url: raw, score: score})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].score > rows[j].score })
	out := make([]string, 0, min(4, len(rows)))
	for _, row := range rows[:min(4, len(rows))] {
		out = append(out, row.url)
	}
	return out
}

func researchOrigin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	u.Path, u.RawQuery, u.Fragment = "/", "", ""
	return u.String()
}

func (a *app) discoverLinkedOfficialSources(ctx context.Context, query string, prefs researchPreferences, rows []searxResult) []researchSource {
	if len(rows) == 0 || strings.TrimSpace(researchEntityPhrase(query)) == "" {
		return nil
	}
	client := newPublicWebClient(7 * time.Second)
	seedRows := append([]searxResult(nil), rows...)
	seedPriority := func(stage string) int {
		switch stage {
		case "local-primary-distributor":
			return 4
		case "local-primary-current", "local-primary":
			return 3
		case "discovered-primary":
			return 2
		default:
			if strings.HasPrefix(stage, "local-") {
				return 1
			}
			return 0
		}
	}
	sort.SliceStable(seedRows, func(i, j int) bool { return seedPriority(seedRows[i].Stage) > seedPriority(seedRows[j].Stage) })
	candidateByHost := map[string]researchOutboundCandidate{}
	seedPages := 0
	for _, row := range seedRows {
		if seedPages >= 2 {
			break
		}
		if !isHTTPURL(row.URL) || researchSocialPlatform(row.URL) != "" || !researchCandidateRelevant(query, row.Title, row.URL, row.Content) {
			continue
		}
		if !strings.HasPrefix(row.Stage, "local-") && row.Stage != "discovered-primary" {
			continue
		}
		rawHTML, ct, err := fetchPublicDocument(ctx, client, row.URL, 550<<10)
		if err != nil || (!strings.Contains(ct, "html") && !strings.Contains(strings.ToLower(rawHTML[:min(len(rawHTML), 300)]), "<html")) {
			continue
		}
		seedPages++
		for _, candidate := range researchOutboundCandidates(row.URL, rawHTML, query) {
			if existing, ok := candidateByHost[candidate.Host]; !ok || candidate.Score > existing.Score {
				candidateByHost[candidate.Host] = candidate
			} else if candidate.OfficialSignal && !existing.OfficialSignal {
				existing.OfficialSignal = true
				candidateByHost[candidate.Host] = existing
			}
		}
	}
	if len(candidateByHost) == 0 {
		return nil
	}
	candidates := make([]researchOutboundCandidate, 0, len(candidateByHost))
	for _, candidate := range candidateByHost {
		candidates = append(candidates, candidate)
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].Score > candidates[j].Score })
	if len(candidates) > 3 {
		candidates = candidates[:3]
	}

	out := make([]researchSource, 0, 2)
	for _, candidate := range candidates {
		origin := researchOrigin(candidate.URL)
		if origin == "" {
			continue
		}
		official := candidate.OfficialSignal
		if homeText, err := fetchPublicPage(ctx, client, origin); err == nil && researchOfficialDistributorSignal(homeText, homeText) {
			official = true
		}

		robotsURL, _ := url.Parse(origin)
		robotsURL.Path = "/robots.txt"
		robotsURL.RawQuery, robotsURL.Fragment = "", ""
		robots, _, _ := fetchPublicDocument(ctx, client, robotsURL.String(), 96<<10)
		sitemaps := researchSitemapDirectives(origin, robots)
		targets := make([]string, 0, 4)
		seenTargets := map[string]bool{}
		for _, sitemapURL := range sitemaps {
			xml, _, err := fetchPublicDocument(ctx, client, sitemapURL, 1400<<10)
			if err != nil {
				continue
			}
			for _, target := range researchSitemapEntityURLs(query, xml) {
				if researchHost(target) != candidate.Host || seenTargets[target] {
					continue
				}
				seenTargets[target] = true
				targets = append(targets, target)
				if len(targets) >= 4 {
					break
				}
			}
			if len(targets) >= 4 {
				break
			}
		}
		if len(targets) == 0 && researchCandidateRelevant(query, candidate.URL, candidate.URL, candidate.URL) {
			targets = append(targets, candidate.URL)
		}
		for _, target := range targets {
			text, err := fetchPublicPage(ctx, client, target)
			if err != nil || !researchCandidateRelevant(query, target, target, text) {
				continue
			}
			if researchOfficialDistributorSignal(text, text) {
				official = true
			}
			authority := "secondary"
			stage := "discovered-linked-direct"
			if official {
				authority = "primary"
				stage = "discovered-official-direct"
			}
			source := researchSource{
				Title:      first(researchEntityPhrase(query), candidate.Host) + " — " + candidate.Host,
				URL:        target,
				Excerpt:    researchFocusedExcerpt(query, text, 6500),
				Engine:     "direct-discovery",
				Region:     researchSourceRegion("", target, text, prefs),
				Authority:  authority,
				SourceType: "web",
				Stage:      stage,
			}
			if prefs.Region == "TH" {
				source.Region = "TH"
			}
			scoreResearchSource(query, &source, time.Now())
			boostResearchSourceForPlan(query, prefs, &source)
			if source.QualityScore < 58 {
				continue
			}
			out = append(out, source)
			if len(out) >= 2 {
				return out
			}
		}
	}
	return out
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
	result.QualityScore, result.QualityGrade = aggregateResearchQuality(sources)
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
			source := researchSource{Title: title, URL: raw, Excerpt: researchFocusedExcerpt(query, text, 6500), Engine: "direct", Region: researchSourceRegion(title, raw, text, prefs), Authority: researchAuthorityForCandidate(query, title, raw, text, "direct", true), SourceType: researchSourceType(raw), Platform: researchSocialPlatform(raw), Stage: "direct"}
			scoreResearchSource(query, &source, time.Now())
			direct = append(direct, source)
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
		for j := range found {
			found[j].Stage = item.Stage
			found[j].SearchQuery = item.Query
		}
		results = append(results, found...)
	}
	if entityPhrase := researchEntityPhrase(query); entityPhrase != "" && len(results) > 0 {
		for _, host := range researchDiscoveredPrimaryHosts(query, results) {
			primaryQuery := fmt.Sprintf(`site:%s %s %d`, host, entityPhrase, time.Now().Year())
			found, err := a.searxSearchWithLanguage(ctx, base, primaryQuery, first(prefs.Locale, "all"))
			if err != nil {
				continue
			}
			for j := range found {
				found[j].Stage = "discovered-primary"
				found[j].SearchQuery = primaryQuery
			}
			results = append(results, found...)
			queries = append(queries, primaryQuery)
		}
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
	discoveredOfficial := a.discoverLinkedOfficialSources(ctx, query, prefs, results)

	limit := a.cfg.WebResearchMaxResults
	if limit <= 0 || limit > 8 {
		if prefs.Depth == "deep" {
			limit = 8
		} else {
			limit = 5
		}
	}
	localCurrent := make([]researchSource, 0, limit)
	localWeb := make([]researchSource, 0, limit)
	localSocial := make([]researchSource, 0, limit)
	globalWeb := make([]researchSource, 0, limit)
	globalSocial := make([]researchSource, 0, limit)
	for _, row := range results {
		if !isHTTPURL(row.URL) || !researchCandidateRelevant(query, row.Title, row.URL, row.Content) {
			continue
		}
		source := researchSource{Title: clipText(row.Title, 300), URL: row.URL, Snippet: clipText(row.Content, 1200), Engine: clipText(row.Engine, 80), Region: researchSourceRegion(row.Title, row.URL, row.Content, prefs), Authority: researchAuthorityForCandidate(query, row.Title, row.URL, row.Content, row.Stage, false), SourceType: researchSourceType(row.URL), Platform: researchSocialPlatform(row.URL), Stage: row.Stage}
		// A result discovered by a Thailand-scoped Google query remains local
		// evidence even when the domain is .com and the short search snippet omits
		// the word Thailand (common for official local distributor/model pages).
		if prefs.Region == "TH" && strings.HasPrefix(row.Stage, "local-") {
			source.Region = "TH"
		}
		scoreResearchSource(query, &source, time.Now())
		boostResearchSourceForPlan(query, prefs, &source)
		if source.QualityScore < 55 {
			continue
		}
		if source.Region == "TH" && source.SourceType == "social" {
			localSocial = append(localSocial, source)
		} else if source.Region == "TH" {
			if researchFreshnessIntent(query) && (strings.Contains(source.Stage, "primary-current") || strings.Contains(source.Stage, "local-market")) {
				localCurrent = append(localCurrent, source)
			}
			localWeb = append(localWeb, source)
		} else if source.SourceType == "social" {
			globalSocial = append(globalSocial, source)
		} else {
			globalWeb = append(globalWeb, source)
		}
	}
	for _, rows := range [][]researchSource{localCurrent, localWeb, localSocial, globalWeb, globalSocial} {
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].QualityScore > rows[j].QualityScore })
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
	appendRowsN(discoveredOfficial, 2)
	if prefs.Region == "TH" && prefs.Scope != "global" {
		if researchFreshnessIntent(query) {
			appendRowsN(localCurrent, 1)
		}
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
		if sources[i].SourceType == "social" || strings.TrimSpace(sources[i].Excerpt) != "" {
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
				sources[idx].Excerpt = researchFocusedExcerpt(query, text, 6500)
				if sources[idx].Region != "TH" && researchSourceRegion(sources[idx].Title, sources[idx].URL, text, prefs) == "TH" {
					sources[idx].Region = "TH"
				}
			}
		}(idx)
	}
	wg.Wait()
	verified := make([]researchSource, 0, len(sources))
	now := time.Now()
	for i := range sources {
		source := &sources[i]
		// For fetched web pages, the page body is stronger evidence than the search
		// snippet. Drop candidates whose actual page is not about the requested
		// entity/topic even if the search engine produced a misleading snippet.
		if source.SourceType != "social" && source.Stage != "direct" && strings.TrimSpace(source.Excerpt) != "" &&
			!researchCandidateRelevant(query, source.Title, source.URL, source.Excerpt) {
			continue
		}
		scoreResearchSource(query, source, now)
		boostResearchSourceForPlan(query, prefs, source)
		if source.Stage != "direct" && source.QualityScore < 58 {
			continue
		}
		verified = append(verified, *source)
	}
	if len(verified) == 0 {
		return webResearchResult{}, errors.New("no verified research sources")
	}
	sources = verified
	for i := range sources {
		sources[i].Index = i + 1
	}
	result := summarizeWebResearchResult(sources, queries)
	a.publishResearchProgress(ctx, map[string]any{"phase": "verifying-sources", "region": prefs.Region, "scope": prefs.Scope, "depth": prefs.Depth, "queries": queries, "sourceCount": len(sources), "localSourceCount": result.LocalSourceCount, "globalSourceCount": result.GlobalSourceCount, "socialSourceCount": result.SocialSourceCount, "socialPlatforms": result.SocialPlatforms, "qualityScore": result.QualityScore, "qualityGrade": result.QualityGrade})
	return result, nil
}
