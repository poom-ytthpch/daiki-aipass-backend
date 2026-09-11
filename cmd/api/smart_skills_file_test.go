package main

import (
	"encoding/json"
	"testing"
)

func smartSkillBody(t *testing.T, text string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"messages": []map[string]any{{"role": "user", "content": text}}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func hasSmartSkill(skills []smartSkill, want string) bool {
	for _, skill := range skills {
		if skill.ID == want {
			return true
		}
	}
	return false
}

func TestSelectSmartSkillsAutoFileArtifactThaiPDF(t *testing.T) {
	skills := selectSmartSkills(smartSkillBody(t, "ช่วยสร้างรายงานเป็น PDF ภาษาไทยให้หน่อย"))
	if !hasSmartSkill(skills, "file-artifacts") {
		t.Fatalf("skills=%+v", skills)
	}
}

func TestSelectSmartSkillsAutoFileArtifactCSV(t *testing.T) {
	skills := selectSmartSkills(smartSkillBody(t, "Create CSV with sku, price and status"))
	if !hasSmartSkill(skills, "file-artifacts") || !hasSmartSkill(skills, "data-analysis") {
		t.Fatalf("skills=%+v", skills)
	}
}
