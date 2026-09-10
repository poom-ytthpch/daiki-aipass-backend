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
		"graft", "code graph", "repo map", "blast radius",
		"เรียนรู้เป็น skill", "เพิ่ม skill", "อัปสกิล", "up skill", "สร้าง skill", "ปรับ skill", "พัฒนาทักษะ",
	) {
		return true
	}
	// Attached document turns opt into the curated skills profile. Daiki injects
	// only a bounded relevant excerpt, so this enables document-analysis guidance
	// without paying for the skills catalog on ordinary chat turns.
	if strings.Contains(strings.ToLower(text), "--- daiki attachment context ---") {
		return true
	}
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

func commandSelectionHasSkill(selection chatCommandSelection, want string) bool {
	for _, skill := range selection.Skills {
		if strings.EqualFold(strings.TrimSpace(skill), want) {
			return true
		}
	}
	return false
}

func chooseHermesProfile(body []byte, research researchMetadata, route inference.Route, selected []smartSkill, commands chatCommandSelection) string {
	// Vision must win over research. The research profile is intentionally text-only;
	// routing an image turn there makes Hermes pre-analyze data URLs through its sandbox
	// fallback instead of passing pixels natively to the vision-capable model. Web
	// evidence, when explicitly requested, is already embedded in the request body and
	// can be synthesized by the dedicated lean vision profile.
	if route.Workload == inference.WorkloadVision {
		return "vision"
	}
	if researchUsesHermesProfile(research) {
		return "research"
	}
	if wantsHermesAgentProfile(body) {
		return "agent"
	}
	if commandSelectionHasSkill(commands, "graft") {
		return "skills"
	}
	if wantsHermesSkillProfile(body, selected) {
		return "skills"
	}
	return "user"
}
