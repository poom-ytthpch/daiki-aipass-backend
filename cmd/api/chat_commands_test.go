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
