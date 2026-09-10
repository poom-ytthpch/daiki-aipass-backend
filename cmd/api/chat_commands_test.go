package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/poom-ytthpch/daiki-ai-passport-backend/internal/store"
)

func TestApplyGuestChatCommandsAllowsWhitelistedModeAndSkills(t *testing.T) {
	body := []byte(`{"model":"fast","commandMode":"plan","commandSkills":["pdf","csv"],"messages":[{"role":"user","content":"review this"}]}`)
	prepared, selection, err := applyChatCommands(body, true)
	if err != nil {
		t.Fatal(err)
	}
	if selection.Mode != "plan" || len(selection.Skills) != 2 || selection.Skills[0] != "pdf" || selection.Skills[1] != "csv" {
		t.Fatalf("unexpected selection: %#v", selection)
	}
	var payload map[string]any
	if err := json.Unmarshal(prepared, &payload); err != nil {
		t.Fatal(err)
	}
	if _, ok := payload["commandMode"]; ok {
		t.Fatal("commandMode must not be forwarded as a provider field")
	}
	if _, ok := payload["commandSkills"]; ok {
		t.Fatal("commandSkills must not be forwarded as a provider field")
	}
	messages, _ := payload["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("expected command system message + user message, got %d", len(messages))
	}
	first, _ := messages[0].(map[string]any)
	instruction, _ := first["content"].(string)
	for _, expected := range []string{"MODE PLAN", "SKILL PDF", "SKILL CSV"} {
		if !strings.Contains(instruction, expected) {
			t.Fatalf("missing %s in command instruction: %s", expected, instruction)
		}
	}
}

func TestApplyGuestGraftSkillIsWhitelisted(t *testing.T) {
	body := []byte(`{"commandSkills":["graft"],"messages":[{"role":"user","content":"inspect this repo"}]}`)
	prepared, selection, err := applyChatCommands(body, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(selection.Skills) != 1 || selection.Skills[0] != "graft" {
		t.Fatalf("unexpected graft selection: %#v", selection)
	}
	if !strings.Contains(string(prepared), "SKILL GRAFT") || !strings.Contains(string(prepared), "graft-code-intelligence") {
		t.Fatalf("graft instruction missing: %s", prepared)
	}
}
func TestApplyGuestDeepSearchForcesBackendResearch(t *testing.T) {
	body := []byte(`{"commandMode":"deep-search","messages":[{"role":"user","content":"latest release"}]}`)
	prepared, selection, err := applyChatCommands(body, true)
	if err != nil {
		t.Fatal(err)
	}
	if selection.Mode != "deep-search" {
		t.Fatalf("unexpected mode: %#v", selection)
	}
	var payload map[string]any
	if err := json.Unmarshal(prepared, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["researchMode"] != "web" {
		t.Fatalf("deep-search must force server research: %#v", payload)
	}
}

func TestGuestSanitizerPreservesOnlyWhitelistedCommandInputs(t *testing.T) {
	p := store.DefaultGuestAccessPolicy()
	restricted, err := restrictGuestChat([]byte(`{"model":"deep","researchMode":"web","commandMode":"deep-search","commandSkills":["pdf","csv"],"tools":[{"type":"function"}],"messages":[{"role":"user","content":"review and verify"}]}`), p)
	if err != nil {
		t.Fatal(err)
	}
	prepared, selection, err := applyChatCommands(restricted, true)
	if err != nil {
		t.Fatal(err)
	}
	if selection.Mode != "deep-search" || len(selection.Skills) != 2 {
		t.Fatalf("unexpected preserved command selection: %#v", selection)
	}
	var payload map[string]any
	if err := json.Unmarshal(prepared, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["model"] != "fast" || payload["researchMode"] != "web" {
		t.Fatalf("guest command should enable only the intended fast + research path: %#v", payload)
	}
	if _, exists := payload["tools"]; exists {
		t.Fatalf("raw Guest tool payload must remain stripped: %#v", payload)
	}
}

func TestApplyGuestChatCommandsRejectsUnknownCapabilities(t *testing.T) {
	for _, body := range [][]byte{
		[]byte(`{"commandMode":"terminal","messages":[{"role":"user","content":"hello"}]}`),
		[]byte(`{"commandSkills":["shell"],"messages":[{"role":"user","content":"hello"}]}`),
	} {
		if _, _, err := applyChatCommands(body, true); err == nil {
			t.Fatalf("expected guest command rejection for %s", body)
		}
	}
}

func TestApplyChatCommandsLimitsSkillFanout(t *testing.T) {
	body := []byte(`{"commandSkills":["pdf","sheet","csv","data","code"],"messages":[{"role":"user","content":"hello"}]}`)
	if _, _, err := applyChatCommands(body, true); err == nil {
		t.Fatal("expected more than four selected skills to be rejected")
	}
}

func TestAutomaticAttachmentSkillsMatchMediaTypes(t *testing.T) {
	attachments := []expandedAttachment{
		{Name: "photo.jpeg", MediaType: "image/jpeg", Kind: "image"},
		{Name: "manual.pdf", MediaType: "application/pdf", Kind: "file"},
		{Name: "stock.xlsx", MediaType: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", Kind: "file"},
		{Name: "events.csv", MediaType: "text/csv", Kind: "file"},
	}
	got := autoAttachmentSkills(attachments)
	want := []string{"image", "pdf", "sheet", "csv"}
	if len(got) != len(want) {
		t.Fatalf("auto skills=%v want=%v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("auto skills=%v want=%v", got, want)
		}
	}
}

func TestApplyAutomaticAttachmentSkillsInjectsAndTracksSelection(t *testing.T) {
	body := []byte(`{"messages":[{"role":"system","content":"base"},{"role":"user","content":"what car is this?"}]}`)
	prepared, selection, err := applyAutomaticAttachmentSkills(body, []expandedAttachment{{Name: "car.jpg", MediaType: "image/jpeg", Kind: "image"}}, chatCommandSelection{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(selection.Skills) != 1 || selection.Skills[0] != "image" || len(selection.AutoSkills) != 1 || selection.AutoSkills[0] != "image" {
		t.Fatalf("unexpected automatic selection: %#v", selection)
	}
	if !strings.Contains(string(prepared), "DAIKI AUTOMATIC ATTACHMENT SKILLS") || !strings.Contains(string(prepared), "SKILL IMAGE") {
		t.Fatalf("automatic image instruction missing: %s", prepared)
	}
}

func TestApplyAutomaticAttachmentSkillsDoesNotDuplicateExplicitSkill(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"review"}]}`)
	selection := chatCommandSelection{Skills: []string{"pdf"}}
	prepared, got, err := applyAutomaticAttachmentSkills(body, []expandedAttachment{{Name: "manual.pdf", MediaType: "application/pdf", Kind: "file"}}, selection, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Skills) != 1 || len(got.AutoSkills) != 0 {
		t.Fatalf("explicit skill should not be duplicated: %#v", got)
	}
	if string(prepared) != string(body) {
		t.Fatalf("body should remain unchanged when explicit skill already covers the attachment")
	}
}

func TestAutomaticAttachmentSkillStillAppliesWhenExplicitSkillBudgetIsFull(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"review"}]}`)
	selection := chatCommandSelection{Skills: []string{"code", "data", "summarize", "translate"}}
	prepared, got, err := applyAutomaticAttachmentSkills(body, []expandedAttachment{{Name: "manual.pdf", MediaType: "application/pdf", Kind: "file"}}, selection, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Skills) != 4 {
		t.Fatalf("explicit skill budget must remain capped: %#v", got)
	}
	if len(got.AutoSkills) != 1 || got.AutoSkills[0] != "pdf" {
		t.Fatalf("attachment skill must still be applied out-of-band: %#v", got)
	}
	if !strings.Contains(string(prepared), "SKILL PDF") {
		t.Fatalf("PDF attachment instruction missing: %s", prepared)
	}
	if !commandSelectionNeedsSkillsProfile(got) {
		t.Fatal("automatic PDF skill must select the Hermes skills profile")
	}
}

func TestAttachmentSkillsRequireHermesSkillsProfile(t *testing.T) {
	if !commandSelectionNeedsSkillsProfile(chatCommandSelection{Skills: []string{"document"}}) {
		t.Fatal("document attachment skill should require the Hermes skills profile")
	}
	if commandSelectionNeedsSkillsProfile(chatCommandSelection{Skills: []string{"translate"}}) {
		t.Fatal("plain translate should not enable the Hermes skills profile")
	}
}
