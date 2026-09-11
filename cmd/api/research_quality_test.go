package main

import (
	"strings"
	"testing"
	"time"
)

func TestSealion7ResearchRelevanceRejectsMismatchedSources(t *testing.T) {
	query := "BYD Sealion 7 ราคา ล่าสุด Thailand"
	cases := []struct {
		name    string
		title   string
		url     string
		content string
		want    bool
	}{
		{
			name:    "current manufacturer overview",
			title:   "BYD SEALION 7 | RÊVER Automotive",
			url:     "https://www.reverautomotive.com/model/sealion7/overview",
			content: "BYD SEALION 7 ราคา 1,199,900 บาท แบตเตอรี่ 91.3 kWh ระยะทาง 600 km NEDC",
			want:    true,
		},
		{
			name:    "unrelated government one2car page",
			title:   "รวมรถมือสอง ซื้อขายรถบ้าน รถเต็นท์ ราคาดีที่สุด ที่ตลาดรถ One2car",
			url:     "https://alro.go.th/th/roiet/news-activity/article-category-52-16/example",
			content: "ซื้อขายรถมือสอง รถบ้านเจ้าของขายเอง รถเต็นท์",
			want:    false,
		},
		{
			name:    "unrelated revenue department page",
			title:   "0811(กม.04)/02 - The Revenue Department",
			url:     "https://www.rd.go.th/25472.html",
			content: "4 มกราคม 2544 ภาษีเงินได้บุคคลธรรมดา กรณีรถยนต์ประจำตำแหน่ง",
			want:    false,
		},
		{
			name:    "unrelated percentage calculator",
			title:   "โปรแกรมคำนวณเปอร์เซ็นต์ คิดร้อยละ",
			url:     "https://hdmall.co.th/blog/percentage-calculator/",
			content: "โปรแกรมช่วยคำนวณเปอร์เซ็นต์และร้อยละ",
			want:    false,
		},
		{
			name:    "sibling model on tiktok",
			title:   "BYD SEALION 6 DM-i Super PHEV",
			url:     "https://www.tiktok.com/@bydreverthailand/video/123",
			content: "BYD SEALION 6 ราคาและการใช้งานในประเทศไทย",
			want:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := researchCandidateRelevant(query, tc.title, tc.url, tc.content)
			if got != tc.want {
				t.Fatalf("relevant=%v want=%v score=%d", got, tc.want, researchRelevanceScore(query, tc.title, tc.url, tc.content))
			}
		})
	}
}

func TestResearchAuthorityDoesNotConfuseGovernmentWithProductPrimary(t *testing.T) {
	query := "BYD Sealion 7 ราคา ล่าสุด Thailand"
	primary := researchAuthorityForCandidate(query,
		"BYD SEALION 7 | RÊVER Automotive",
		"https://www.reverautomotive.com/model/sealion7/overview",
		"BYD SEALION 7 ราคา 1,199,900 บาท",
		"local-primary", false)
	if primary != "primary" {
		t.Fatalf("manufacturer product page authority=%q want primary", primary)
	}
	government := researchAuthorityForCandidate(query,
		"รถมือสอง One2car",
		"https://alro.go.th/example",
		"รถมือสอง",
		"local", false)
	if government != "government" {
		t.Fatalf("government domain authority=%q want government", government)
	}
}

func TestProductResearchPlanAvoidsBlindGovernmentSearch(t *testing.T) {
	prefs := researchPreferences{Region: "TH", Locale: "th-TH", Scope: "local-first", Depth: "deep"}
	productPlan := researchSearchPlan("BYD Sealion 7 ราคา ล่าสุด", prefs)
	var productQueries []string
	for _, item := range productPlan {
		productQueries = append(productQueries, item.Query)
	}
	joined := strings.Join(productQueries, "\n")
	if strings.Contains(joined, "site:go.th") {
		t.Fatalf("product research should not blindly query .go.th: %s", joined)
	}
	if !strings.Contains(joined, "Thailand official") || !strings.Contains(joined, "price specifications") {
		t.Fatalf("product research missing primary/current discovery queries: %s", joined)
	}

	regulatoryPlan := researchSearchPlan("กฎหมายภาษีนำเข้า BYD Sealion 7 ประเทศไทย", prefs)
	var regulatoryQueries []string
	for _, item := range regulatoryPlan {
		regulatoryQueries = append(regulatoryQueries, item.Query)
	}
	if !strings.Contains(strings.Join(regulatoryQueries, "\n"), "site:go.th") {
		t.Fatalf("regulatory research should include government sources: %#v", regulatoryPlan)
	}
}

func TestFreshnessScoringPrefersCurrentEvidence(t *testing.T) {
	now := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	query := "BYD Sealion 7 ราคา ล่าสุด"
	current := researchFreshnessScore(query, "โปรโมชั่น กันยายน 2026", now)
	lastYear := researchFreshnessScore(query, "ราคาเปิดตัว November 2025", now)
	old := researchFreshnessScore(query, "ข้อมูลปี 2022", now)
	if !(current > lastYear && lastYear > old) {
		t.Fatalf("freshness ordering current=%d lastYear=%d old=%d", current, lastYear, old)
	}
}

func TestSealion7ResearchBenchmarkScore(t *testing.T) {
	now := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	query := "BYD Sealion 7 ราคา ล่าสุด Thailand"

	// Reproduces the failure mode from the reported conversation: irrelevant
	// government/calculator pages and a sibling Sealion 6 social result were mixed
	// with only a small amount of genuinely relevant evidence.
	baseline := []researchSource{
		{Title: "รวมรถมือสอง ซื้อขายรถบ้าน รถเต็นท์ ราคาดีที่สุด ที่ตลาดรถ One2car", URL: "https://alro.go.th/th/example", Snippet: "ซื้อขายรถมือสอง", Authority: "government"},
		{Title: "0811(กม.04)/02 - The Revenue Department", URL: "https://www.rd.go.th/25472.html", Snippet: "4 มกราคม 2544 ภาษีเงินได้บุคคลธรรมดา รถยนต์ประจำตำแหน่ง", Authority: "government"},
		{Title: "โปรแกรมคำนวณเปอร์เซ็นต์ คิดร้อยละ", URL: "https://hdmall.co.th/blog/percentage-calculator/", Snippet: "โปรแกรมคำนวณเปอร์เซ็นต์", Authority: "secondary"},
		{Title: "BYD SEALION 6 DM-i Super PHEV", URL: "https://www.tiktok.com/@bydreverthailand/video/123", Snippet: "BYD SEALION 6 April 2026", SourceType: "social", Platform: "TikTok"},
		{Title: "รถนายกฯ ส่วนลด BYD Sealion 7", URL: "https://www.facebook.com/autolifethailand.tv/posts/1", Snippet: "BYD Sealion 7 ราคา 1,199,900 - 1,299,900 บาท March 2026", SourceType: "social", Platform: "Facebook"},
		{Title: "โปรโมชั่น BYD SEALION 7 กันยายน 2026", URL: "https://www.bydchonburi.com/promotion/byd-sealion7", Snippet: "Premium 1,199,900 AWD Performance 1,299,900", Authority: "secondary"},
		{Title: "แคมเปญส่งเสริมการขาย BYD SEALION 7", URL: "https://www.reverautomotive.com/news/byd-sealion-7-price-announcement", Snippet: "BYD SEALION 7 November 2024 รับประกัน 8 ปี", Authority: "primary"},
		{Title: "BYD SEAL 6 ราคาเริ่มต้น", URL: "https://www.instagram.com/p/example", Snippet: "BYD SEAL 6 March 2026", SourceType: "social", Platform: "Instagram"},
	}

	improved := []researchSource{
		{Title: "BYD SEALION 7 | RÊVER Automotive", URL: "https://www.reverautomotive.com/model/sealion7/overview", Excerpt: "BYD SEALION 7 ราคา 1,199,900 บาท ระยะทาง 600 km NEDC กำลังมอเตอร์ 390 kW แบตเตอรี่ 91.3 kWh 2026", Authority: "primary"},
		{Title: "โปรโมชั่น BYD SEALION 7 กันยายน 2026", URL: "https://www.reverautomotive.com/news/byd-sealion-7-campaign-september-2026", Excerpt: "3 September 2026 Premium 1,199,900 AWD Performance 1,299,900 AWD Ultimate 1,349,900", Authority: "primary"},
		{Title: "โปรโมชั่น BYD SEALION 7 กันยายน 2026", URL: "https://www.bydchonburi.com/promotion/byd-sealion7", Excerpt: "September 2026 BYD Sealion 7 Premium 1,199,900 AWD Performance 1,299,900", Authority: "secondary"},
		{Title: "BYD Sealion 7 ราคาไทยและรีวิว", URL: "https://example.co.th/byd-sealion-7-review-2026", Excerpt: "BYD Sealion 7 Thailand review September 2026 ราคาและการใช้งานจริง", Authority: "secondary"},
		{Title: "BYD Sealion 7 Thailand owners", URL: "https://www.facebook.com/groups/bydthailand/posts/sealion7", Snippet: "BYD Sealion 7 ผู้ใช้จริงในไทย September 2026", SourceType: "social", Platform: "Facebook", Authority: "community"},
		{Title: "รีวิวขับจริง BYD SEALION 7", URL: "https://www.youtube.com/watch?v=sealion7", Snippet: "BYD SEALION 7 Thailand review 2026", SourceType: "social", Platform: "YouTube", Authority: "community"},
	}

	expected := []string{"reverautomotive.com"}
	forbidden := []string{"alro.go.th", "rd.go.th", "hdmall.co.th"}
	baselineScore := researchRetrievalBenchmarkScore(query, baseline, expected, forbidden, now)
	improvedScore := researchRetrievalBenchmarkScore(query, improved, expected, forbidden, now)
	t.Logf("Sealion 7 retrieval benchmark: baseline=%d/100 upgraded=%d/100", baselineScore, improvedScore)
	if baselineScore >= 65 {
		t.Fatalf("bad retrieval baseline unexpectedly high: %d", baselineScore)
	}
	if improvedScore < 80 {
		t.Fatalf("upgraded retrieval score too low: %d", improvedScore)
	}
	if improvedScore < baselineScore+25 {
		t.Fatalf("upgrade did not improve enough: baseline=%d upgraded=%d", baselineScore, improvedScore)
	}
}

func TestDiscoveredPrimaryHostPrefersRelevantManufacturer(t *testing.T) {
	query := "BYD Sealion 7 ราคา ล่าสุด Thailand"
	rows := []searxResult{
		{Title: "BYD SEALION 7 | RÊVER Automotive", URL: "https://www.reverautomotive.com/model/sealion7/overview", Content: "BYD Sealion 7 Thailand price", Stage: "local-primary"},
		{Title: "รถมือสอง", URL: "https://alro.go.th/example", Content: "ตลาดรถ One2car", Stage: "local"},
		{Title: "BYD Sealion 7 review", URL: "https://example.com/review", Content: "BYD Sealion 7 review", Stage: "global"},
	}
	hosts := researchDiscoveredPrimaryHosts(query, rows)
	if len(hosts) == 0 || hosts[0] != "reverautomotive.com" {
		t.Fatalf("discovered primary hosts=%#v", hosts)
	}
}

type benchmarkDimensions struct {
	Retrieval int
	Relevance int
	Freshness int
	Authority int
	Evidence  int
	Quality   int
	Aggregate int
	Grade     string
}

func scoreBenchmarkDimensions(query string, sources []researchSource, expectedHosts, forbiddenHosts []string, now time.Time) benchmarkDimensions {
	if len(sources) == 0 {
		return benchmarkDimensions{Grade: "F"}
	}
	scored := append([]researchSource(nil), sources...)
	result := benchmarkDimensions{Retrieval: researchRetrievalBenchmarkScore(query, sources, expectedHosts, forbiddenHosts, now)}
	for i := range scored {
		scoreResearchSource(query, &scored[i], now)
		result.Relevance += scored[i].RelevanceScore
		result.Freshness += scored[i].FreshnessScore
		result.Authority += scored[i].AuthorityScore
		result.Evidence += scored[i].EvidenceScore
		result.Quality += scored[i].QualityScore
	}
	result.Relevance /= len(scored)
	result.Freshness /= len(scored)
	result.Authority /= len(scored)
	result.Evidence /= len(scored)
	result.Quality /= len(scored)
	result.Aggregate, result.Grade = aggregateResearchQuality(scored)
	return result
}

func TestResearchQualityBenchmarkSuite(t *testing.T) {
	now := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	type benchmarkCase struct {
		name           string
		query          string
		expectedHosts  []string
		forbiddenHosts []string
		baseline       []researchSource
		upgraded       []researchSource
		minUpgraded    int
		minGain        int
		minEvidence    int
	}
	cases := []benchmarkCase{
		{
			name: "software_release_nextjs", query: "Next.js 16.3.3 latest release docs 2026",
			expectedHosts: []string{"nextjs.org", "github.com"}, forbiddenHosts: []string{"medium.com"}, minUpgraded: 88, minGain: 25, minEvidence: 65,
			baseline: []researchSource{
				{Title: "Next.js 14 migration notes", URL: "https://medium.com/example/nextjs-14", Snippet: "Next.js 14 migration guide 2024", Authority: "secondary"},
				{Title: "React framework comparison", URL: "https://example.com/react-frameworks", Snippet: "Next.js compared with other React frameworks in 2025", Authority: "secondary"},
				{Title: "Next.js tutorial", URL: "https://www.youtube.com/watch?v=old-next", Snippet: "Next.js tutorial 2024", SourceType: "social", Platform: "YouTube", Authority: "community"},
			},
			upgraded: []researchSource{
				{Title: "Next.js 16.3.3", URL: "https://nextjs.org/blog/next-16-3-3", Excerpt: "Next.js 16.3.3 release notes September 2026 fixes and upgrade guidance", Authority: "primary", Stage: "global-primary"},
				{Title: "vercel/next.js v16.3.3", URL: "https://github.com/vercel/next.js/releases/tag/v16.3.3", Excerpt: "Next.js v16.3.3 release September 2026 changelog", Authority: "primary", Stage: "global-primary"},
				{Title: "next 16.3.3 package", URL: "https://www.npmjs.com/package/next/v/16.3.3", Excerpt: "Next.js 16.3.3 package published 2026", Authority: "secondary"},
			},
		},
		{
			name: "thai_ev_tax_regulation", query: "Thailand EV import tax regulation 2026 กฎหมายภาษีนำเข้า",
			expectedHosts: []string{"customs.go.th", "excise.go.th"}, forbiddenHosts: []string{"facebook.com", "randomdealer.co.th"}, minUpgraded: 86, minGain: 25, minEvidence: 65,
			baseline: []researchSource{
				{Title: "EV import tax discussion", URL: "https://www.facebook.com/groups/evthai/posts/tax", Snippet: "Thailand EV import tax discussion 2025", SourceType: "social", Platform: "Facebook", Authority: "community"},
				{Title: "EV import tax promotion", URL: "https://randomdealer.co.th/ev-promo", Snippet: "Thailand EV import tax estimate 2025", Authority: "secondary"},
				{Title: "Thailand EV tax guide", URL: "https://example.com/thailand-ev-tax", Snippet: "Thailand EV import tax regulation overview 2024", Authority: "secondary"},
			},
			upgraded: []researchSource{
				{Title: "Customs tariff for electric vehicles", URL: "https://www.customs.go.th/ev-import-tax-2026", Excerpt: "Thailand EV import tax regulation effective 2026 tariff and import requirements", Authority: "government", Stage: "local-government"},
				{Title: "Excise tax measures for electric vehicles", URL: "https://www.excise.go.th/ev-tax-2026", Excerpt: "Thailand EV excise import tax regulation 2026 official conditions and rates", Authority: "government", Stage: "local-government"},
				{Title: "Thailand EV policy analysis", URL: "https://example.or.th/ev-policy-2026", Excerpt: "Independent analysis of Thailand EV import and excise tax regulation 2026", Authority: "secondary"},
			},
		},
		{
			name: "security_advisory_openssl", query: "OpenSSL security advisory latest 2026 CVE",
			expectedHosts: []string{"openssl-library.org", "nvd.nist.gov"}, forbiddenHosts: []string{"medium.com"}, minUpgraded: 88, minGain: 25, minEvidence: 65,
			baseline: []researchSource{
				{Title: "OpenSSL tips and tricks", URL: "https://medium.com/example/openssl", Snippet: "OpenSSL security tips 2023", Authority: "secondary"},
				{Title: "OpenSSL vulnerability discussion", URL: "https://www.reddit.com/r/netsec/comments/example", Snippet: "OpenSSL CVE discussion 2025", SourceType: "social", Platform: "Reddit", Authority: "community"},
				{Title: "TLS security overview", URL: "https://example.com/tls-security", Snippet: "General TLS security information 2024", Authority: "secondary"},
			},
			upgraded: []researchSource{
				{Title: "OpenSSL Security Advisory", URL: "https://openssl-library.org/news/secadv/20260901.txt", Excerpt: "OpenSSL security advisory published September 2026 CVE details affected versions and fixes", Authority: "primary", Stage: "global-primary"},
				{Title: "NVD CVE record for OpenSSL", URL: "https://nvd.nist.gov/vuln/detail/CVE-2026-12345", Excerpt: "CVE-2026-12345 OpenSSL security advisory published 2026 affected versions", Authority: "government"},
				{Title: "OpenSSL release notes", URL: "https://openssl-library.org/news/openssl-3.5-notes/", Excerpt: "OpenSSL 2026 release notes security fixes and advisory references", Authority: "primary", Stage: "global-primary"},
			},
		},
		{
			name: "academic_lfp_safety", query: "LFP battery thermal runaway research 2026 study",
			expectedHosts: []string{"mit.edu", "nature.com"}, forbiddenHosts: []string{"tiktok.com"}, minUpgraded: 84, minGain: 25, minEvidence: 65,
			baseline: []researchSource{
				{Title: "LFP battery fire test viral clip", URL: "https://www.tiktok.com/@battery/video/1", Snippet: "LFP battery thermal runaway demo 2025", SourceType: "social", Platform: "TikTok", Authority: "community"},
				{Title: "Battery safety blog", URL: "https://example.com/lfp-safety", Snippet: "LFP battery thermal runaway explained 2024", Authority: "secondary"},
				{Title: "EV battery forum", URL: "https://www.reddit.com/r/electricvehicles/comments/lfp", Snippet: "LFP battery thermal runaway owner discussion 2025", SourceType: "social", Platform: "Reddit", Authority: "community"},
			},
			upgraded: []researchSource{
				{Title: "LFP battery thermal runaway study", URL: "https://energy.mit.edu/research/lfp-thermal-runaway-2026", Excerpt: "2026 research study of LFP battery thermal runaway propagation methodology measurements and limitations", Authority: "academic"},
				{Title: "Thermal runaway mechanisms in LFP cells", URL: "https://www.nature.com/articles/lfp-thermal-runaway-2026", Excerpt: "Peer-reviewed 2026 LFP battery thermal runaway research study experimental results", Authority: "academic"},
				{Title: "LFP battery safety review 2026", URL: "https://example.edu/battery/lfp-review", Excerpt: "Academic LFP battery thermal runaway research study published 2026", Authority: "academic"},
			},
		},
		{
			name: "social_owner_experience_sealion7", query: "BYD Sealion 7 owner issues Thailand 2026 social review",
			expectedHosts: []string{"facebook.com", "pantip.com", "reverautomotive.com"}, forbiddenHosts: []string{"tiktok.com"}, minUpgraded: 80, minGain: 25, minEvidence: 60,
			baseline: []researchSource{
				{Title: "BYD SEALION 6 owner review", URL: "https://www.tiktok.com/@car/video/sealion6", Snippet: "BYD Sealion 6 owner issue Thailand 2026", SourceType: "social", Platform: "TikTok", Authority: "community"},
				{Title: "BYD cars discussion", URL: "https://example.com/byd-forum", Snippet: "general BYD owner discussion 2025", Authority: "secondary"},
				{Title: "EV problems", URL: "https://www.youtube.com/watch?v=generic-ev", Snippet: "generic EV owner issues 2026", SourceType: "social", Platform: "YouTube", Authority: "community"},
			},
			upgraded: []researchSource{
				{Title: "BYD Sealion 7 Thailand owners", URL: "https://www.facebook.com/groups/bydthailand/posts/sealion7-issues", Snippet: "BYD Sealion 7 owner issues Thailand August 2026 recurring user experiences", SourceType: "social", Platform: "Facebook", Authority: "community"},
				{Title: "ผู้ใช้ BYD Sealion 7 แชร์ประสบการณ์", URL: "https://pantip.com/topic/sealion7-2026", Snippet: "BYD Sealion 7 owner review Thailand 2026 ประสบการณ์ใช้งาน", SourceType: "social", Platform: "Pantip", Authority: "community"},
				{Title: "BYD SEALION 7 | RÊVER Automotive", URL: "https://www.reverautomotive.com/model/sealion7/overview", Excerpt: "BYD Sealion 7 Thailand 2026 official specifications warranty and support information", Authority: "primary", Stage: "local-primary"},
				{Title: "BYD Sealion 7 long-term owner review", URL: "https://www.youtube.com/watch?v=sealion7-owner-2026", Snippet: "BYD Sealion 7 owner review Thailand September 2026", SourceType: "social", Platform: "YouTube", Authority: "community"},
			},
		},
		{
			name: "official_program_th_ai_passport", query: "TH-AI Passport latest eligibility Thailand 2026",
			expectedHosts: []string{"aipass.go.th"}, forbiddenHosts: []string{"thaiembassy.org", "example.com"}, minUpgraded: 92, minGain: 35, minEvidence: 65,
			baseline: []researchSource{
				{Title: "Thai Passport", URL: "https://thaiembassy.org/thai-passport", Snippet: "Thai passport application requirements 2025", Authority: "secondary"},
				{Title: "AI passport photo generator", URL: "https://example.com/ai-passport-photo", Snippet: "AI passport photo generator 2026", Authority: "secondary"},
				{Title: "Passport renewal Thailand", URL: "https://example.com/passport-renewal", Snippet: "Thailand passport renewal 2025", Authority: "secondary"},
			},
			upgraded: []researchSource{
				{Title: "TH-AI Passport", URL: "https://aipass.go.th/", Excerpt: "TH-AI Passport eligibility Thailand 2026 registration requirements and official program information", Authority: "primary", Stage: "local-primary"},
				{Title: "TH-AI Passport FAQ", URL: "https://aipass.go.th/faq", Excerpt: "TH-AI Passport 2026 eligibility FAQ registration and participant requirements", Authority: "primary", Stage: "local-primary"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			baseline := scoreBenchmarkDimensions(tc.query, tc.baseline, tc.expectedHosts, tc.forbiddenHosts, now)
			upgraded := scoreBenchmarkDimensions(tc.query, tc.upgraded, tc.expectedHosts, tc.forbiddenHosts, now)
			t.Logf("%s baseline retrieval=%d relevance=%d freshness=%d authority=%d evidence=%d quality=%d aggregate=%d(%s)", tc.name, baseline.Retrieval, baseline.Relevance, baseline.Freshness, baseline.Authority, baseline.Evidence, baseline.Quality, baseline.Aggregate, baseline.Grade)
			t.Logf("%s upgraded retrieval=%d relevance=%d freshness=%d authority=%d evidence=%d quality=%d aggregate=%d(%s)", tc.name, upgraded.Retrieval, upgraded.Relevance, upgraded.Freshness, upgraded.Authority, upgraded.Evidence, upgraded.Quality, upgraded.Aggregate, upgraded.Grade)
			if upgraded.Retrieval < tc.minUpgraded {
				t.Fatalf("upgraded retrieval score=%d want >=%d", upgraded.Retrieval, tc.minUpgraded)
			}
			if upgraded.Retrieval < baseline.Retrieval+tc.minGain {
				t.Fatalf("retrieval gain=%d want >=%d (baseline=%d upgraded=%d)", upgraded.Retrieval-baseline.Retrieval, tc.minGain, baseline.Retrieval, upgraded.Retrieval)
			}
			if upgraded.Relevance < 80 {
				t.Fatalf("upgraded relevance average=%d want >=80", upgraded.Relevance)
			}
			if upgraded.Evidence < tc.minEvidence {
				t.Fatalf("upgraded evidence average=%d want >=%d", upgraded.Evidence, tc.minEvidence)
			}
		})
	}
}

func TestResearchEntityTermsPreserveSemanticVersion(t *testing.T) {
	terms := researchEntityTerms("Next.js 16.3.3 latest release docs 2026")
	joined := strings.Join(terms, "|")
	if !strings.Contains(joined, "16.3.3") {
		t.Fatalf("semantic version must stay a single entity token: %#v", terms)
	}
	for _, unwanted := range []string{"release", "docs"} {
		for _, term := range terms {
			if term == unwanted {
				t.Fatalf("research intent word %q must not become an entity token: %#v", unwanted, terms)
			}
		}
	}
	if score := researchRelevanceScore(
		"Next.js 16.3.3 latest release docs 2026",
		"vercel/next.js v16.3.3",
		"https://github.com/vercel/next.js/releases/tag/v16.3.3",
		"Next.js v16.3.3 release September 2026 changelog",
	); score < 80 {
		t.Fatalf("official semver release relevance=%d want >=80", score)
	}
}

func TestAggregateResearchQualityTrustsIndependentGovernmentAndAcademicEvidence(t *testing.T) {
	government := []researchSource{
		{Authority: "government", QualityScore: 94},
		{Authority: "government", QualityScore: 92},
		{Authority: "secondary", QualityScore: 84},
	}
	academic := []researchSource{
		{Authority: "academic", QualityScore: 95},
		{Authority: "academic", QualityScore: 93},
		{Authority: "academic", QualityScore: 90},
	}
	govScore, govGrade := aggregateResearchQuality(government)
	academicScore, academicGrade := aggregateResearchQuality(academic)
	if govScore < 90 || govGrade != "A" {
		t.Fatalf("strong government evidence score=%d grade=%s want A", govScore, govGrade)
	}
	if academicScore < 90 || academicGrade != "A" {
		t.Fatalf("strong academic evidence score=%d grade=%s want A", academicScore, academicGrade)
	}
}

func TestProductAuthorityDoesNotPromoteSecondaryArticlesFromPrimaryQueryStage(t *testing.T) {
	query := "BYD Sealion 7 ราคา ล่าสุด Thailand"
	cases := []struct {
		name string
		url  string
		want string
	}{
		{name: "manufacturer model page", url: "https://www.reverautomotive.com/model/sealion7/overview", want: "primary"},
		{name: "automotive news article", url: "https://www.autoinfo.co.th/online/572782", want: "secondary"},
		{name: "magazine specs article", url: "https://www.grandprix.co.th/byd-sealion-7-specs-price/", want: "secondary"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := researchAuthorityForCandidate(query, "BYD SEALION 7 Thailand price 2026", tc.url, "BYD SEALION 7 ราคาไทย 2026", "local-primary", false)
			if got != tc.want {
				t.Fatalf("authority=%q want=%q for %s", got, tc.want, tc.url)
			}
		})
	}
}
