package main

import (
	"net/http"
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
	// Domain classification alone is intentionally NOT enough to load Hermes'
	// heavy skills catalog. Normal coding/data/document/planning requests stay on
	// the lean user profile; only explicit skill-learning/use intent opts in.
	_ = selected
	return false
}

func wantsHermesAgentProfile(body []byte) bool {
	return containsHermesIntent(latestUserText(body),
		"delegate", "subagent", "sub-agent", "multi-agent", "multiple agents", "parallel agents", "agent team",
		"หลาย agent", "หลายเอเจนต์", "แบ่งงานให้ agent", "ทำงานขนาน", "parallel task", "orchestrate agents",
	)
}

func applyHermesSessionScope(req *http.Request, baseKey string, payload []byte) {
	// Daiki/PostgreSQL owns transcript history. Supplying X-Hermes-Session-Id
	// makes Hermes replace the request messages[] history with its own state.db
	// history for that id, so normal Daiki inference must leave it unset.
	req.Header.Del("X-Hermes-Session-Id")
	if key := hermesModelScopedSessionKey(baseKey, payload); key != "" {
		req.Header.Set("X-Hermes-Session-Key", key)
	}
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
