package main

import (
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var researchEntityTokenRE = regexp.MustCompile(`(?i)v?[0-9]+(?:\.[0-9]+){1,3}|[a-z0-9][a-z0-9._-]{1,}|[0-9]{1,4}`)
var researchYearRE = regexp.MustCompile(`\b(?:20[0-9]{2}|25[0-9]{2})\b`)
var researchPriceEvidenceRE = regexp.MustCompile(`(?i)(?:[$€£¥฿]\s*\d[\d,.]*|(?:thb|usd|eur|gbp|jpy)\s*\d[\d,.]*|\d[\d,.]*\s*(?:บาท|baht|thb|usd|eur|gbp|jpy))`)
var researchLoosePriceRE = regexp.MustCompile(`(?i)(?:ราคา|price|starting\s+price)\s*(?::|=|-|เริ่มต้น|starting\s+at)?\s*[$€£¥฿]?\s*(\d[\d,.]*)`)

var researchEntityStopwords = map[string]bool{
	"find": true, "search": true, "research": true, "latest": true, "current": true, "today": true,
	"price": true, "prices": true, "review": true, "reviews": true, "spec": true, "specs": true,
	"specification": true, "specifications": true, "official": true, "website": true, "thailand": true,
	"thai": true, "data": true, "info": true, "information": true, "detail": true, "details": true,
	"car": true, "cars": true, "vehicle": true, "vehicles": true, "model": true, "models": true, "product": true, "products": true,
	"ev": true, "new": true, "news": true, "compare": true, "comparison": true, "promo": true,
	"promotion": true, "promotions": true, "market": true, "release": true, "releases": true,
	"docs": true, "documentation": true, "changelog": true, "version": true, "versions": true,
	"owner": true, "owners": true, "issue": true, "issues": true, "social": true,
	"the": true, "meaning": true, "traditional": true, "interpretation": true, "interpretive": true,
	"horoscope": true, "astrology": true, "astrological": true, "ephemeris": true, "follow-up": true, "followup": true,
	"january": true, "february": true, "march": true, "april": true, "may": true, "june": true,
	"july": true, "august": true, "september": true, "october": true, "november": true, "december": true,
	"guide": true, "reference": true,
}

func clampResearchScore(v int) int {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

func researchEntityTerms(query string) []string {
	tokens := researchEntityTokenRE.FindAllString(strings.ToLower(query), -1)
	out := make([]string, 0, len(tokens))
	seen := map[string]bool{}
	for _, token := range tokens {
		token = strings.Trim(strings.TrimSpace(token), ".-_ ")
		if token == "" || researchEntityStopwords[token] || seen[token] {
			continue
		}
		// Single-letter alphabetic tokens create too many false matches. Numeric
		// tokens are retained because product generations/model numbers matter.
		if len(token) < 2 && (token[0] < '0' || token[0] > '9') {
			continue
		}
		seen[token] = true
		out = append(out, token)
		if len(out) >= 6 {
			break
		}
	}
	return out
}

func researchEntityPhrase(query string) string {
	return strings.Join(researchEntityTerms(query), " ")
}

func researchTermWeight(term string, hasAlpha bool) int {
	if term == "" {
		return 0
	}
	if term[0] >= '0' && term[0] <= '9' {
		if hasAlpha {
			return 3
		}
		return 1
	}
	if len(term) >= 5 {
		return 3
	}
	if len(term) >= 3 {
		return 2
	}
	return 1
}

func researchPureNumericModelTerm(term string) bool {
	if term == "" || len(term) > 3 {
		return false
	}
	for _, r := range term {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func researchTermHasLetter(term string) bool {
	for _, r := range term {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			return true
		}
	}
	return false
}

func researchRelevanceScore(query, title, rawURL, content string) int {
	if !researchResultRelevant(query, title, rawURL, content) {
		return 0
	}
	terms := researchEntityTerms(query)
	if len(terms) == 0 {
		// Thai-only/general queries still use the legacy token rank. The strict
		// entity gate is intentionally applied only when a stable product/name
		// anchor can be extracted without language-specific tokenization.
		return 70
	}
	hay := " " + normalizeResearchText(title+" "+rawURL+" "+content) + " "
	hasAlpha := false
	for _, term := range terms {
		if researchTermHasLetter(term) {
			hasAlpha = true
			break
		}
	}
	totalWeight, matchedWeight, longAlphaMatches, alphaMatches := 0, 0, 0, 0
	modelNumberTerms := make([]string, 0, 2)
	longAlphaValues := make([]string, 0, 3)
	for _, term := range terms {
		weight := researchTermWeight(term, hasAlpha)
		totalWeight += weight
		if researchPureNumericModelTerm(term) {
			modelNumberTerms = append(modelNumberTerms, term)
		}
		if len(term) >= 5 && (term[0] < '0' || term[0] > '9') {
			longAlphaValues = append(longAlphaValues, normalizeResearchText(term))
		}
		if strings.Contains(hay, " "+normalizeResearchText(term)+" ") || strings.Contains(hay, normalizeResearchText(term)) {
			matchedWeight += weight
			if researchTermHasLetter(term) {
				alphaMatches++
			}
			if len(term) >= 5 && (term[0] < '0' || term[0] > '9') {
				longAlphaMatches++
			}
		}
	}
	if totalWeight == 0 {
		return 60
	}
	if hasAlpha && alphaMatches == 0 {
		return 0
	}
	// If a query contains a distinctive product/name token (e.g. Sealion), a
	// candidate that does not contain any such token is not about the entity.
	longAlphaTerms := 0
	for _, term := range terms {
		if len(term) >= 5 && (term[0] < '0' || term[0] > '9') {
			longAlphaTerms++
		}
	}
	if longAlphaTerms > 0 && longAlphaMatches == 0 {
		return 0
	}
	// Do not drift to a sibling product/model. For example, a Sealion 7 query
	// must reject Sealion 6 even though the brand/family terms are similar.
	for _, number := range modelNumberTerms {
		anchor := ""
		for i, term := range terms {
			if term != number {
				continue
			}
			for j := i - 1; j >= 0; j-- {
				if researchTermHasLetter(terms[j]) {
					anchor = normalizeResearchText(terms[j])
					break
				}
			}
			break
		}
		matched := strings.Contains(hay, " "+number+" ")
		if anchor != "" {
			matched = false
			for _, candidate := range []string{
				anchor + number,
				anchor + " " + number,
				anchor + " model " + number,
				anchor + " series " + number,
				anchor + " รุ่น " + number,
			} {
				if strings.Contains(hay, candidate) {
					matched = true
					break
				}
			}
		}
		if !matched {
			return 0
		}
	}
	score := 20 + matchedWeight*80/totalWeight
	phrase := normalizeResearchText(researchEntityPhrase(query))
	if phrase != "" && strings.Contains(normalizeResearchText(title+" "+content), phrase) {
		score += 10
	}
	return clampResearchScore(score)
}

func researchCandidateRelevant(query, title, rawURL, content string) bool {
	return researchRelevanceScore(query, title, rawURL, content) >= 58
}

func researchFreshnessIntent(query string) bool {
	q := strings.ToLower(query)
	for _, signal := range []string{"latest", "current", "today", "price", "promo", "promotion", "availability", "release", "2026", "ล่าสุด", "ปัจจุบัน", "วันนี้", "ราคา", "โปรโมชั่น", "โปร", "มีขาย", "เปิดตัว", "กันยายน", "สิงหาคม"} {
		if strings.Contains(q, signal) {
			return true
		}
	}
	return false
}

func researchPriceIntent(query string) bool {
	q := strings.ToLower(query)
	for _, signal := range []string{"price", "prices", "how much", "ราคา", "กี่บาท", "เท่าไหร่", "เท่าไร"} {
		if strings.Contains(q, signal) {
			return true
		}
	}
	return false
}

func researchSpecificationIntent(query string) bool {
	q := strings.ToLower(query)
	for _, signal := range []string{"spec", "specs", "specification", "specifications", "สเปก", "สมรรถนะ", "แบตเตอรี่", "battery", "range", "ระยะทาง"} {
		if strings.Contains(q, signal) {
			return true
		}
	}
	return false
}

func researchMonthForYear(text string, year int) int {
	lower := strings.ToLower(text)
	normalized := strings.NewReplacer(",", " ", ".", " ", "-", " ", "/", " ", "_", " ").Replace(lower)
	fields := strings.Fields(normalized)
	monthNames := map[string]int{
		"january": 1, "jan": 1, "มกราคม": 1,
		"february": 2, "feb": 2, "กุมภาพันธ์": 2,
		"march": 3, "mar": 3, "มีนาคม": 3,
		"april": 4, "apr": 4, "เมษายน": 4,
		"may": 5, "พฤษภาคม": 5,
		"june": 6, "jun": 6, "มิถุนายน": 6,
		"july": 7, "jul": 7, "กรกฎาคม": 7,
		"august": 8, "aug": 8, "สิงหาคม": 8,
		"september": 9, "sep": 9, "sept": 9, "กันยายน": 9,
		"october": 10, "oct": 10, "ตุลาคม": 10,
		"november": 11, "nov": 11, "พฤศจิกายน": 11,
		"december": 12, "dec": 12, "ธันวาคม": 12,
	}
	yearTokens := map[string]bool{strconv.Itoa(year): true, strconv.Itoa(year + 543): true}
	best := 0
	for i, field := range fields {
		month := monthNames[field]
		if month == 0 {
			continue
		}
		start, end := max(0, i-2), min(len(fields), i+3)
		for _, nearby := range fields[start:end] {
			if yearTokens[nearby] && month > best {
				best = month
			}
		}
	}
	for i, field := range fields {
		if !yearTokens[field] || i+1 >= len(fields) {
			continue
		}
		month, err := strconv.Atoi(fields[i+1])
		if err == nil && month >= 1 && month <= 12 && month > best {
			best = month
		}
	}
	return best
}

func researchFreshnessScore(query, text string, now time.Time) int {
	if !researchFreshnessIntent(query) {
		return 75
	}
	currentYear := now.Year()
	bestYear := 0
	for _, raw := range researchYearRE.FindAllString(text, -1) {
		year := 0
		for _, ch := range raw {
			year = year*10 + int(ch-'0')
		}
		if year >= 2400 {
			year -= 543
		}
		if year > bestYear && year <= currentYear+1 {
			bestYear = year
		}
	}
	if bestYear == 0 {
		return 58
	}
	delta := currentYear - bestYear
	if month := researchMonthForYear(text, bestYear); month > 0 {
		monthsOld := delta*12 + int(now.Month()) - month
		switch {
		case monthsOld <= 0:
			return 100
		case monthsOld == 1:
			return 96
		case monthsOld == 2:
			return 90
		case monthsOld == 3:
			return 84
		case monthsOld == 4:
			return 78
		case monthsOld == 5:
			return 74
		case monthsOld == 6:
			return 70
		case monthsOld == 7:
			return 66
		case monthsOld == 8:
			return 62
		case monthsOld == 9:
			return 58
		case monthsOld <= 12:
			return 52
		case monthsOld <= 24:
			return 40
		default:
			return 30
		}
	}
	switch {
	case delta <= 0:
		return 90
	case delta == 1:
		return 72
	case delta == 2:
		return 56
	case delta == 3:
		return 44
	default:
		return 30
	}
}

func researchRegulatoryIntent(query string) bool {
	q := strings.ToLower(query)
	for _, signal := range []string{"law", "legal", "regulation", "regulatory", "tax", "import duty", "recall", "safety standard", "กฎหมาย", "ข้อบังคับ", "ภาษี", "ศุลกากร", "นำเข้า", "เรียกคืน", "มาตรฐานความปลอดภัย"} {
		if strings.Contains(q, signal) {
			return true
		}
	}
	return false
}

func researchAcademicIntent(query string) bool {
	q := strings.ToLower(query)
	for _, signal := range []string{"paper", "study", "research paper", "journal", "academic", "clinical", "วิจัย", "งานวิจัย", "วารสาร", "มหาวิทยาลัย", "การศึกษา"} {
		if strings.Contains(q, signal) {
			return true
		}
	}
	return false
}

func researchInterpretiveIntent(query string) bool {
	q := strings.ToLower(query)
	for _, signal := range []string{"horoscope", "astrology", "astrological", "zodiac", "tarot", "birth chart", "natal chart", "rising sign", "lucky day", "fortune", "ดูดวง", "ดวง", "โหราศาสตร์", "จักรราศี", "ไพ่ทาโรต์", "ฤกษ์", "ลัคนา"} {
		if strings.Contains(q, signal) {
			return true
		}
	}
	return false
}

func researchPrimaryPath(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	path := strings.ToLower(u.Path)
	for _, marker := range []string{"/model/", "/models/", "/product/", "/products/", "/vehicle/", "/vehicles/", "/support/", "/newsroom/"} {
		if strings.Contains(path, marker) {
			return true
		}
	}
	return false
}

func researchHostMatchesEntity(query, rawURL string) bool {
	host := researchHost(rawURL)
	if host == "" {
		return false
	}
	label := strings.Split(host, ".")[0]
	label = strings.ReplaceAll(normalizeResearchText(label), " ", "")
	if label == "" {
		return false
	}
	for _, term := range researchEntityTerms(query) {
		if len(term) < 3 || researchPureNumericModelTerm(term) {
			continue
		}
		norm := strings.ReplaceAll(normalizeResearchText(term), " ", "")
		if label == norm {
			return true
		}
		// Allow a distinctive long brand token to be part of the registrable
		// label (e.g. newbalance), but do not promote short tokens such as BYD
		// inside dealer domains like bydbdautogroup.com.
		if len(norm) >= 5 && strings.Contains(label, norm) && len(norm)*100/max(1, len(label)) >= 55 {
			return true
		}
	}
	return false
}

func researchLikelyPrimaryHost(query string, row searxResult) bool {
	host := researchHost(row.URL)
	if host == "" || researchSocialPlatform(row.URL) != "" || strings.HasSuffix(host, ".go.th") || strings.HasSuffix(host, ".ac.th") {
		return false
	}
	if researchRelevanceScore(query, row.Title, row.URL, row.Content) < 85 {
		return false
	}
	lower := strings.ToLower(row.Title + " " + row.Content)
	for _, signal := range []string{"used car", "มือสอง", "forum", "pantip", "reddit", "review aggregator", "marketplace"} {
		if strings.Contains(lower, signal) {
			return false
		}
	}
	return strings.Contains(row.Stage, "primary") && (researchPrimaryPath(row.URL) || researchHostMatchesEntity(query, row.URL))
}

func researchDiscoveredPrimaryHosts(query string, rows []searxResult) []string {
	type candidate struct {
		host  string
		score int
	}
	best := map[string]int{}
	for _, row := range rows {
		if !researchLikelyPrimaryHost(query, row) {
			continue
		}
		host := researchHost(row.URL)
		score := researchRelevanceScore(query, row.Title, row.URL, row.Content)
		if strings.Contains(row.Stage, "primary") {
			score += 10
		}
		if researchPrimaryPath(row.URL) {
			score += 5
		}
		if score > best[host] {
			best[host] = score
		}
	}
	ordered := make([]candidate, 0, len(best))
	for host, score := range best {
		ordered = append(ordered, candidate{host: host, score: score})
	}
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].score > ordered[j].score })
	out := make([]string, 0, min(2, len(ordered)))
	for _, candidate := range ordered[:min(2, len(ordered))] {
		out = append(out, candidate.host)
	}
	return out
}

func researchOfficialDistributorSignal(title, content string) bool {
	lower := strings.ToLower(title + " " + content)
	for _, signal := range []string{
		"official distributor", "authorized distributor", "official importer", "authorized importer",
		"official dealer network", "ผู้นำเข้าและจัดจำหน่ายอย่างเป็นทางการ", "ผู้จัดจำหน่ายอย่างเป็นทางการ",
		"ตัวแทนจำหน่ายอย่างเป็นทางการ", "ผู้นำเข้าอย่างเป็นทางการ",
	} {
		if strings.Contains(lower, signal) {
			return true
		}
	}
	return false
}

func researchAuthorityForCandidate(query, title, rawURL, content, stage string, direct bool) string {
	host := researchHost(rawURL)
	if researchSocialPlatform(rawURL) != "" {
		return "community"
	}
	if direct {
		return "provided"
	}
	if strings.HasSuffix(host, ".go.th") || strings.HasSuffix(host, ".gov") || strings.Contains(host, ".gov.") {
		return "government"
	}
	if strings.HasSuffix(host, ".ac.th") || strings.HasSuffix(host, ".edu") || strings.Contains(host, ".edu.") {
		return "academic"
	}
	if researchInterpretiveIntent(query) {
		// Astrology/tarot/fortune-telling sources are interpretive references,
		// not empirically verified facts. Keep that epistemic status explicit
		// instead of labelling them as an "official" source.
		return "interpretive"
	}
	relevance := researchRelevanceScore(query, title, rawURL, content)
	if relevance >= 85 && strings.Contains(stage, "primary") && (researchHostMatchesEntity(query, rawURL) || researchOfficialDistributorSignal(title, content)) {
		return "primary"
	}
	return "secondary"
}

func researchAuthorityScore(authority string) int {
	switch authority {
	case "provided":
		return 95
	case "primary":
		return 95
	case "government":
		return 82
	case "academic":
		return 80
	case "secondary":
		return 58
	case "community":
		return 42
	case "interpretive":
		return 70
	default:
		return 50
	}
}

func researchEvidenceScore(source researchSource) int {
	if strings.TrimSpace(source.Excerpt) != "" {
		return 100
	}
	if strings.TrimSpace(source.Snippet) != "" {
		if source.SourceType == "social" {
			return 52
		}
		return 68
	}
	return 25
}

func researchHasPriceEvidence(text string) bool {
	if researchPriceEvidenceRE.MatchString(text) {
		return true
	}
	for _, match := range researchLoosePriceRE.FindAllStringSubmatch(text, -1) {
		if len(match) < 2 {
			continue
		}
		raw := strings.TrimSpace(match[1])
		digits := strings.NewReplacer(",", "", ".", "").Replace(raw)
		value, err := strconv.Atoi(digits)
		if err != nil {
			continue
		}
		// A bare 4-digit current/B.E. year after the word "price" is usually
		// metadata such as "price 2026", not an actual monetary amount.
		if !strings.ContainsAny(raw, ",.") && len(digits) == 4 && value >= 1900 && value <= 2600 {
			continue
		}
		return true
	}
	return false
}

func researchEvidenceScoreForQuery(query string, source researchSource) int {
	score := researchEvidenceScore(source)
	if !researchPriceIntent(query) {
		return score
	}
	text := strings.TrimSpace(source.Title + " " + source.Snippet + " " + source.Excerpt)
	if researchHasPriceEvidence(text) {
		return score
	}
	if source.SourceType == "social" {
		return min(score, 22)
	}
	return min(score, 32)
}

func scoreResearchSource(query string, source *researchSource, now time.Time) {
	if source == nil {
		return
	}
	text := strings.TrimSpace(source.Snippet + " " + source.Excerpt)
	source.RelevanceScore = researchRelevanceScore(query, source.Title, source.URL, text)
	if source.Authority == "" {
		source.Authority = researchAuthorityForCandidate(query, source.Title, source.URL, text, source.Stage, false)
	}
	source.AuthorityScore = researchAuthorityScore(source.Authority)
	source.FreshnessScore = researchFreshnessScore(query, source.Title+" "+text, now)
	source.EvidenceScore = researchEvidenceScoreForQuery(query, *source)
	source.QualityScore = clampResearchScore((source.RelevanceScore*45 + source.AuthorityScore*25 + source.FreshnessScore*20 + source.EvidenceScore*10) / 100)
}

func researchFocusedExcerpt(query, text string, maxLen int) string {
	text = researchSpaceRE.ReplaceAllString(strings.TrimSpace(text), " ")
	if maxLen <= 0 || len(text) <= maxLen {
		return text
	}
	lower := strings.ToLower(text)
	markers := make([]string, 0, 32)
	if researchFreshnessIntent(query) {
		markers = append(markers, "฿", "ราคา", " price ", "starting price", "campaign", "โปรโมชั่น", "premium", "awd ultimate", "awd performance")
	}
	markers = append(markers,
		"battery", "แบต", "kwh", "charging", "ชาร์จ", "warranty", "รับประกัน",
		"safety", "ความปลอดภัย", "range", "ระยะทาง", "0-100", "power", "แรงบิด", "nm", "kw",
	)
	if phrase := strings.TrimSpace(researchEntityPhrase(query)); phrase != "" {
		markers = append(markers, strings.ToLower(phrase))
	}
	for _, term := range researchEntityTerms(query) {
		if len(term) >= 3 {
			markers = append(markers, strings.ToLower(term))
		}
	}
	type excerptRange struct{ start, end int }
	ranges := make([]excerptRange, 0, 10)
	addRange := func(pos int) {
		if pos < 0 {
			return
		}
		start := max(0, pos-500)
		end := min(len(text), pos+1500)
		for _, existing := range ranges {
			if start < existing.end && end > existing.start {
				return
			}
		}
		ranges = append(ranges, excerptRange{start: start, end: end})
	}
	for _, marker := range markers {
		if marker == "" {
			continue
		}
		from := 0
		for hits := 0; hits < 2 && from < len(lower); hits++ {
			idx := strings.Index(lower[from:], marker)
			if idx < 0 {
				break
			}
			idx += from
			addRange(idx)
			from = idx + len(marker)
		}
		if len(ranges) >= 8 {
			break
		}
	}
	if len(ranges) == 0 {
		return clipText(text, maxLen)
	}
	var b strings.Builder
	for _, r := range ranges {
		chunk := strings.TrimSpace(text[r.start:r.end])
		if chunk == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString(" … ")
		}
		remaining := maxLen - b.Len()
		if remaining <= 0 {
			break
		}
		if len(chunk) > remaining {
			chunk = chunk[:remaining]
		}
		b.WriteString(chunk)
		if b.Len() >= maxLen {
			break
		}
	}
	if b.Len() == 0 {
		return clipText(text, maxLen)
	}
	return strings.TrimSpace(b.String())
}

func boostResearchSourceForPlan(query string, prefs researchPreferences, source *researchSource) {
	if source == nil {
		return
	}
	boost := 0
	if prefs.Region == "TH" && source.Region == "TH" {
		if strings.Contains(source.Stage, "primary-current") && researchFreshnessIntent(query) {
			boost += 10
		}
		if strings.Contains(source.Stage, "primary-distributor") {
			boost += 4
		}
		if researchFreshnessIntent(query) {
			lower := strings.ToLower(source.Title + " " + source.Snippet + " " + source.Excerpt)
			if strings.Contains(lower, "ราคา") || strings.Contains(lower, "price") || strings.Contains(lower, "฿") {
				boost += 4
			}
		}
	}
	source.QualityScore = clampResearchScore(source.QualityScore + boost)
}

func researchQualityGrade(score int) string {
	switch {
	case score >= 90:
		return "A"
	case score >= 80:
		return "B"
	case score >= 70:
		return "C"
	case score >= 60:
		return "D"
	default:
		return "F"
	}
}

func aggregateResearchQuality(sources []researchSource) (int, string) {
	if len(sources) == 0 {
		return 0, "F"
	}
	copyRows := append([]researchSource(nil), sources...)
	sort.SliceStable(copyRows, func(i, j int) bool { return copyRows[i].QualityScore > copyRows[j].QualityScore })
	limit := min(len(copyRows), 5)
	total := 0
	primary := false
	strongGovernment := 0
	strongAcademic := 0
	strongInterpretive := 0
	for _, source := range copyRows[:limit] {
		total += source.QualityScore
		if source.Authority == "primary" || source.Authority == "provided" {
			primary = true
		}
		if source.QualityScore >= 80 && source.Authority == "government" {
			strongGovernment++
		}
		if source.QualityScore >= 80 && source.Authority == "academic" {
			strongAcademic++
		}
		if source.QualityScore >= 80 && source.Authority == "interpretive" {
			strongInterpretive++
		}
	}
	score := total / limit
	// Primary/provided evidence is ideal for product and organization facts.
	// Regulatory and academic tasks may correctly rely on multiple independent
	// government/academic sources instead of a manufacturer-owned page. Explicit
	// interpretive research follows the same fit-for-purpose rule without being
	// misrepresented as scientific fact.
	if primary || strongGovernment >= 2 || strongAcademic >= 2 || strongInterpretive >= 2 {
		score += 4
	} else {
		score -= 8
	}
	score = clampResearchScore(score)
	return score, researchQualityGrade(score)
}

func researchRetrievalBenchmarkScore(query string, sources []researchSource, expectedHosts, forbiddenHosts []string, now time.Time) int {
	if len(sources) == 0 {
		return 0
	}
	relevant := 0
	freshTotal := 0
	evidenceTotal := 0
	seenExpected := map[string]bool{}
	seenForbidden := false
	for _, source := range sources {
		if researchCandidateRelevant(query, source.Title, source.URL, source.Snippet+" "+source.Excerpt) {
			relevant++
		}
		freshTotal += researchFreshnessScore(query, source.Title+" "+source.Snippet+" "+source.Excerpt, now)
		evidenceTotal += researchEvidenceScoreForQuery(query, source)
		host := researchHost(source.URL)
		for _, expected := range expectedHosts {
			if host == expected || strings.HasSuffix(host, "."+expected) {
				seenExpected[expected] = true
			}
		}
		for _, forbidden := range forbiddenHosts {
			if host == forbidden || strings.HasSuffix(host, "."+forbidden) {
				seenForbidden = true
			}
		}
	}
	relevancePart := relevant * 35 / len(sources)
	expectedPart := 25
	if len(expectedHosts) > 0 {
		expectedPart = len(seenExpected) * 25 / len(expectedHosts)
	}
	forbiddenPart := 15
	if seenForbidden {
		forbiddenPart = 0
	}
	freshnessPart := (freshTotal / len(sources)) * 15 / 100
	evidencePart := (evidenceTotal / len(sources)) * 10 / 100
	return clampResearchScore(relevancePart + expectedPart + forbiddenPart + freshnessPart + evidencePart)
}
