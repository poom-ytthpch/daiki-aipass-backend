package main

import (
	"net/http"
	"testing"

	"github.com/poom-ytthpch/daiki-ai-passport-backend/internal/inference"
)

func route(workload inference.Workload) inference.Route { return inference.Route{Workload: workload} }

func TestChooseHermesProfile(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		research researchMetadata
		workload inference.Workload
		want     string
	}{
		{"plain chat", `{"messages":[{"role":"user","content":"สวัสดี เป็นยังไงบ้าง"}]}`, researchMetadata{}, inference.WorkloadFast, "user"},
		{"research", `{"messages":[{"role":"user","content":"ค้นหาข้อมูลล่าสุด"}]}`, researchMetadata{Mode: "web", Query: "latest", Used: true}, inference.WorkloadFast, "research"},
		{"vision stays lean", `{"messages":[{"role":"user","content":"review image"}]}`, researchMetadata{}, inference.WorkloadVision, "user"},
		{"explicit upskill", `{"messages":[{"role":"user","content":"ช่วย upskill เรื่อง Kubernetes ให้หน่อย"}]}`, researchMetadata{}, inference.WorkloadFast, "skills"},
		{"coding domain stays lean", `{"messages":[{"role":"user","content":"debug this Go API bug"}]}`, researchMetadata{}, inference.WorkloadFast, "user"},
		{"delegation", `{"messages":[{"role":"user","content":"delegate this to multiple agents in parallel"}]}`, researchMetadata{}, inference.WorkloadFast, "agent"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(tc.body)
			got := chooseHermesProfile(body, tc.research, route(tc.workload), selectSmartSkills(body))
			if got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestHermesDomainTaskDoesNotLoadHeavySkillsCatalog(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"debug this Go code"}]}`)
	if wantsHermesSkillProfile(body, selectSmartSkills(body)) {
		t.Fatal("ordinary coding should stay on lean user profile")
	}
}

func TestApplyHermesSessionScopeKeepsBodyHistoryAuthoritative(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "http://hermes/v1/chat/completions", nil)
	req.Header.Set("X-Hermes-Session-Id", "must-be-removed")
	payload := []byte(`{"model":"model-a","messages":[{"role":"user","content":"first"},{"role":"assistant","content":"answer"},{"role":"user","content":"follow up"}]}`)
	applyHermesSessionScope(req, "daiki:test", payload)
	if got := req.Header.Get("X-Hermes-Session-Id"); got != "" {
		t.Fatalf("Hermes session id must be omitted so request history is preserved, got %q", got)
	}
	if got := req.Header.Get("X-Hermes-Session-Key"); got == "" {
		t.Fatal("expected stable scoped session key")
	}
}
