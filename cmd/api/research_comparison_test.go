package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestComparisonEntitiesInheritSharedProductFamily(t *testing.T) {
	query := "เปรียบเทียบ iphone 18 pro กับ 17 pro ว่าแตกต่างกันเรื่องอะไรบ้าง"
	got := researchComparisonEntities(query)
	if len(got) != 2 || got[0] != "iphone 18 pro" || got[1] != "iphone 17 pro" {
		t.Fatalf("comparison entities=%#v", got)
	}
}

func TestComparisonResearchPlanSearchesEachSideIndependently(t *testing.T) {
	query := "เปรียบเทียบ iphone 18 pro กับ 17 pro ว่าแตกต่างกันเรื่องอะไรบ้าง"
	prefs := researchPreferences{Region: "TH", Locale: "th-TH", Scope: "local-first", Depth: "deep"}
	plan := researchSearchPlan(query, prefs)
	if len(plan) != 4 {
		t.Fatalf("comparison plan must use four bounded Google queries, got %#v", plan)
	}
	joined := strings.ToLower(strings.Join(researchPlanQueries(plan), "\n"))
	if strings.Contains(joined, "iphone 18 pro 17") {
		t.Fatalf("comparison entities were incorrectly merged: %s", joined)
	}
	for _, want := range []string{"iphone 18 pro thailand official specifications", "iphone 17 pro thailand official specifications", "iphone 18 pro official specifications", "iphone 17 pro official specifications"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("comparison plan missing %q: %#v", want, plan)
		}
	}
}

func TestComparisonRelevanceAcceptsEvidenceForEitherSide(t *testing.T) {
	query := "เปรียบเทียบ iphone 18 pro กับ 17 pro ว่าแตกต่างกันเรื่องอะไรบ้าง"
	if !researchCandidateRelevant(query, "iPhone 17 Pro และ 17 Pro Max - ข้อมูลทางเทคนิค", "https://www.apple.com/th/iphone-17-pro/specs/", "iPhone 17 Pro technical specifications A19 Pro") {
		t.Fatal("17 Pro primary source must be usable comparison evidence")
	}
	if !researchCandidateRelevant(query, "iPhone 18 Pro - Technical Specifications", "https://www.apple.com/iphone-18-pro/specs/", "iPhone 18 Pro technical specifications") {
		t.Fatal("18 Pro primary source must be usable comparison evidence")
	}
	if researchCandidateRelevant(query, "iPhone 16 Pro specifications", "https://example.com/iphone-16-pro", "iPhone 16 Pro specifications") {
		t.Fatal("sibling model outside the comparison must still be rejected")
	}
}

func TestSearxSearchFallsBackFromGoogleCaptchaToCSE(t *testing.T) {
	var engines []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		engine := r.URL.Query().Get("engines")
		engines = append(engines, engine)
		w.Header().Set("content-type", "application/json")
		if engine == "google" {
			_, _ = w.Write([]byte(`{"results":[],"unresponsive_engines":[["google","Suspended: CAPTCHA"]]}`))
			return
		}
		_, _ = w.Write([]byte(`{"results":[{"title":"iPhone 17 Pro - Apple TH","url":"https://www.apple.com/th/iphone-17-pro/specs/","content":"iPhone 17 Pro technical specifications"}]}`))
	}))
	defer server.Close()
	a := &app{http: server.Client()}
	results, err := a.searxSearchWithLanguage(context.Background(), server.URL, "iPhone 17 Pro Thailand specifications", "th-TH")
	if err != nil || len(results) != 1 {
		t.Fatalf("CAPTCHA fallback to Google CSE failed: results=%#v err=%v", results, err)
	}
	if got := strings.Join(engines, ","); got != "google,google cse" {
		t.Fatalf("expected Google then Google CSE, got %q", got)
	}
}

func TestPrimaryAuthorityDoesNotPromoteBrandNamedBlog(t *testing.T) {
	query := "เปรียบเทียบ iphone 18 pro กับ 17 pro ว่าแตกต่างกันเรื่องอะไรบ้าง"
	apple := researchAuthorityForCandidate(query, "iPhone 17 Pro - Technical Specifications - Apple", "https://www.apple.com/th/iphone-17-pro/specs/", "iPhone 17 Pro technical specifications", "local-primary-comparison-right", false)
	if apple != "primary" {
		t.Fatalf("official specs path authority=%q want primary", apple)
	}
	blog := researchAuthorityForCandidate(query, "iPhone 18 Pro leak", "https://www.iphonemod.net/iphone-18-pro-battery-leak.html", "iPhone 18 Pro rumor and leak", "local-primary-comparison-left", false)
	if blog == "primary" {
		t.Fatalf("brand-named editorial host must not be promoted to primary")
	}
}
