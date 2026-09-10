package main

import (
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
		{"coding domain", `{"messages":[{"role":"user","content":"debug this Go API bug"}]}`, researchMetadata{}, inference.WorkloadFast, "skills"},
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

func TestHermesModeDoesNotNeedLegacySmallModelPrompt(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"debug this Go code"}]}`)
	if !wantsHermesSkillProfile(body, selectSmartSkills(body)) {
		t.Fatal("coding task should route to skills profile")
	}
}
