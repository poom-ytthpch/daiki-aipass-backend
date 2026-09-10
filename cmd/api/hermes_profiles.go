package main

import (
	"strings"

	"github.com/poom-ytthpch/daiki-ai-passport-backend/internal/inference"
)

func containsHermesIntent(text string, words ...string) bool {
	text = strings.ToLower(text)
	for _, word := range words {
		if strings.Contains(text, strings.ToLower(word)) {
			return true
		}
	}
	return false
}

func wantsHermesSkillProfile(body []byte, selected []smartSkill) bool {
	text := latestUserText(body)
	if containsHermesIntent(text,
		"skill", "upskill", "learn this", "learn how", "remember as a skill", "improve skill", "update skill",
		"เรียนรู้เป็น skill", "เพิ่ม skill", "อัปสกิล", "up skill", "สร้าง skill", "ปรับ skill", "พัฒนาทักษะ",
	) {
		return true
	}
	for _, skill := range selected {
		switch skill.ID {
		case "coding", "data-analysis", "document-qa", "planning":
			return true
		}
	}
	return false
}

func wantsHermesAgentProfile(body []byte) bool {
	return containsHermesIntent(latestUserText(body),
		"delegate", "subagent", "sub-agent", "multi-agent", "multiple agents", "parallel agents", "agent team",
		"หลาย agent", "หลายเอเจนต์", "แบ่งงานให้ agent", "ทำงานขนาน", "parallel task", "orchestrate agents",
	)
}

func chooseHermesProfile(body []byte, research researchMetadata, route inference.Route, selected []smartSkill) string {
	if researchUsesHermesProfile(research) {
		return "research"
	}
	// Native multimodal is already handled by the model route; keep the lean user
	// profile so image pixels are not accompanied by unrelated skill/tool schemas.
	if route.Workload == inference.WorkloadVision {
		return "user"
	}
	if wantsHermesAgentProfile(body) {
		return "agent"
	}
	if wantsHermesSkillProfile(body, selected) {
		return "skills"
	}
	return "user"
}
