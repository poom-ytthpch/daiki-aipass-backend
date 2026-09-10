package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
)

type chatCommandOption struct {
	ID           string
	Name         string
	Description  string
	GuestAllowed bool
	Instruction  string
	ResearchMode string
}

type chatCommandSelection struct {
	Mode       string   `json:"mode,omitempty"`
	Skills     []string `json:"skills,omitempty"`
	AutoSkills []string `json:"autoSkills,omitempty"`
}

var chatCommandModes = []chatCommandOption{
	{ID: "plan", Name: "Plan", Description: "Turn the request into an ordered, practical plan with checks and dependencies.", GuestAllowed: true, Instruction: "MODE PLAN: Produce a practical ordered plan. Include dependencies, risks, verification/success checks, and the smallest useful next action. Avoid generic filler."},
	{ID: "deep-search", Name: "Deep Search", Description: "Search the public web first, then synthesize fresh evidence and sources.", GuestAllowed: true, ResearchMode: "web", Instruction: "MODE DEEP SEARCH: Synthesize the fresh web evidence carefully. Prefer primary/official evidence, compare conflicts, state uncertainty, and answer the user's actual decision or question rather than dumping results."},
	{ID: "analyze", Name: "Analyze", Description: "Inspect evidence, assumptions, trade-offs and edge cases before answering.", GuestAllowed: true, Instruction: "MODE ANALYZE: Analyze the supplied information systematically. Separate evidence from assumptions, check important edge cases, compare meaningful alternatives, and finish with a clear conclusion."},
	{ID: "brainstorm", Name: "Brainstorm", Description: "Generate several distinct useful ideas, then narrow to the strongest options.", GuestAllowed: true, Instruction: "MODE BRAINSTORM: Generate distinct, non-duplicate ideas. Group related ideas, identify the strongest options, and explain the key trade-off for each without padding the answer."},
	{ID: "concise", Name: "Concise", Description: "Prefer the shortest complete answer that still solves the request.", GuestAllowed: true, Instruction: "MODE CONCISE: Give the shortest complete answer that solves the request. Keep only necessary context, caveats, and steps."},
}

var chatCommandSkills = []chatCommandOption{
	{ID: "pdf", Name: "PDF", Description: "Review and reason over attached PDF content with document-specific checks.", GuestAllowed: true, Instruction: "SKILL PDF: When Hermes skill_view is available, load the installed daiki-document-analysis skill before analyzing the attachment. Treat attached PDF extraction as the primary evidence. Preserve headings, names, numbers and table-like structure where available. If the extraction cannot represent an image, scan, chart or layout detail, say so instead of inventing it."},
	{ID: "sheet", Name: "Sheet", Description: "Analyze XLSX/XLSM/ODS workbooks, sheets, tables and formulas represented in attachments.", GuestAllowed: true, Instruction: "SKILL SHEET: When Hermes skill_view is available, load the installed daiki-document-analysis skill before analyzing the attachment. Analyze spreadsheet content by sheet and columns. Check headers, units, totals, duplicates, missing values and row relationships. Do not invent formulas or cells that are not present in the extracted workbook context."},
	{ID: "csv", Name: "CSV", Description: "Analyze CSV/TSV rows, columns, filters, joins, totals and anomalies.", GuestAllowed: true, Instruction: "SKILL CSV: When Hermes skill_view is available, load the installed daiki-document-analysis skill before analyzing the attachment. Treat the attached delimited data as structured rows and columns. Verify headers and data types, preserve exact identifiers, check counts/totals when relevant, and call out malformed or missing values."},
	{ID: "document", Name: "Document", Description: "Work with DOCX, PPTX and other extracted documents while preserving structure and facts.", GuestAllowed: true, Instruction: "SKILL DOCUMENT: When Hermes skill_view is available, load the installed daiki-document-analysis skill before analyzing the attachment. Ground the answer in attached document content. Preserve important wording, names, dates, numbers, sections and decisions; distinguish document evidence from inference."},
	{ID: "data", Name: "Data", Description: "Perform careful data analysis across tables, JSON and numeric attachments.", GuestAllowed: true, Instruction: "SKILL DATA: Define the metric or comparison, check units and denominators, validate arithmetic, identify missing/inconsistent data, and separate observed values from inferred conclusions."},
	{ID: "code", Name: "Code", Description: "Debug, explain and produce implementation-ready code with failure-mode checks.", GuestAllowed: true, Instruction: "SKILL CODE: Inspect constraints first, identify the smallest correct implementation, keep code runnable, mention meaningful failure modes, and never invent APIs or repository contents."},
	{ID: "graft", Name: "Graft", Description: "Use Graft code-intelligence workflow for repository maps, symbol lookup, call traces and blast-radius analysis.", GuestAllowed: true, Instruction: "SKILL GRAFT: When the Hermes skills capability is available, call skill_view for the installed graft-code-intelligence skill before repository analysis, then follow its freshness -> map -> targeted retrieval -> blast-radius -> verification discipline. Never claim Graft commands or repository inspection ran unless an actual Graft-capable tool is available in this runtime."},
	{ID: "summarize", Name: "Summarize", Description: "Extract facts, decisions, risks and next actions from long content.", GuestAllowed: true, Instruction: "SKILL SUMMARIZE: Preserve names, numbers, decisions and caveats. Separate facts from inference and prioritize decisions, risks and next actions over filler."},
	{ID: "translate", Name: "Translate", Description: "Translate while preserving intent, terminology, numbers and formatting.", GuestAllowed: true, Instruction: "SKILL TRANSLATE: Preserve meaning, tone, terminology, numbers and formatting. Do not add facts or commentary unless the user asks for it."},
	{ID: "image", Name: "Image", Description: "Inspect attached images using the vision route when available.", GuestAllowed: true, Instruction: "SKILL IMAGE: Use native image pixels when available. When Hermes skill_view is available, load the installed daiki-image-analysis skill first. Analyze only what is actually visible or supplied from the image. Distinguish observation from inference and never claim to read text or details that are not legible."},
}

func commandOptionsPublic(options []chatCommandOption, guest bool) []map[string]any {
	out := make([]map[string]any, 0, len(options))
	for _, option := range options {
		if guest && !option.GuestAllowed {
			continue
		}
		out = append(out, map[string]any{"id": option.ID, "name": option.Name, "description": option.Description, "guest": option.GuestAllowed})
	}
	return out
}

func chatCommandCatalog(guest bool) map[string]any {
	return map[string]any{
		"modes":  commandOptionsPublic(chatCommandModes, guest),
		"skills": commandOptionsPublic(chatCommandSkills, guest),
	}
}

func (a *app) guestCapabilities(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"mode": "guest", "maxToolRounds": 0, "skills": []any{}, "tools": []any{}, "commands": chatCommandCatalog(true)})
}

func findCommandOption(options []chatCommandOption, id string, guest bool) (chatCommandOption, bool) {
	id = strings.ToLower(strings.TrimSpace(id))
	for _, option := range options {
		if option.ID == id && (!guest || option.GuestAllowed) {
			return option, true
		}
	}
	return chatCommandOption{}, false
}

func requestedCommandSkills(raw any) ([]string, error) {
	if raw == nil {
		return nil, nil
	}
	var values []string
	switch v := raw.(type) {
	case []any:
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("commandSkills must contain strings")
			}
			values = append(values, s)
		}
	case []string:
		values = append(values, v...)
	default:
		return nil, fmt.Errorf("commandSkills must be an array")
	}
	if len(values) > 4 {
		return nil, fmt.Errorf("at most 4 command skills can be selected")
	}
	return values, nil
}

func applyChatCommands(body []byte, guest bool) ([]byte, chatCommandSelection, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, chatCommandSelection{}, fmt.Errorf("invalid chat payload")
	}
	selection := chatCommandSelection{}
	instructions := make([]string, 0, 5)
	if rawMode, ok := payload["commandMode"]; ok {
		mode := strings.ToLower(strings.TrimSpace(fmt.Sprint(rawMode)))
		if mode != "" {
			option, found := findCommandOption(chatCommandModes, mode, guest)
			if !found {
				return nil, selection, fmt.Errorf("command mode %q is not allowed", mode)
			}
			selection.Mode = option.ID
			instructions = append(instructions, option.Instruction)
			if option.ResearchMode != "" {
				payload["researchMode"] = option.ResearchMode
			}
		}
	}
	rawSkills, err := requestedCommandSkills(payload["commandSkills"])
	if err != nil {
		return nil, selection, err
	}
	seen := map[string]bool{}
	for _, raw := range rawSkills {
		id := strings.ToLower(strings.TrimSpace(raw))
		if id == "" || seen[id] {
			continue
		}
		option, found := findCommandOption(chatCommandSkills, id, guest)
		if !found {
			return nil, selection, fmt.Errorf("command skill %q is not allowed", id)
		}
		seen[id] = true
		selection.Skills = append(selection.Skills, option.ID)
		instructions = append(instructions, option.Instruction)
	}
	delete(payload, "commandMode")
	delete(payload, "commandSkills")
	if len(instructions) > 0 {
		commandInstruction := "DAIKI EXPLICIT COMMANDS (selected by the user; apply them to this turn only):\n" + strings.Join(instructions, "\n")
		messages, _ := payload["messages"].([]any)
		if len(messages) > 0 {
			if first, ok := messages[0].(map[string]any); ok && strings.EqualFold(strings.TrimSpace(fmt.Sprint(first["role"])), "system") {
				first["content"] = strings.TrimSpace(fmt.Sprint(first["content"])) + "\n\n" + commandInstruction
				messages[0] = first
			} else {
				messages = append([]any{map[string]any{"role": "system", "content": commandInstruction}}, messages...)
			}
		} else {
			messages = []any{map[string]any{"role": "system", "content": commandInstruction}}
		}
		payload["messages"] = messages
	}
	out, err := json.Marshal(payload)
	return out, selection, err
}

func attachmentCommandSkill(a expandedAttachment) string {
	mediaType := strings.ToLower(strings.TrimSpace(strings.Split(a.MediaType, ";")[0]))
	ext := strings.ToLower(filepath.Ext(a.Name))
	if a.Kind == "image" || strings.HasPrefix(mediaType, "image/") {
		return "image"
	}
	switch ext {
	case ".pdf":
		return "pdf"
	case ".xlsx", ".xls", ".xlsm", ".ods":
		return "sheet"
	case ".csv", ".tsv":
		return "csv"
	case ".json", ".jsonl", ".ndjson":
		return "data"
	case ".js", ".jsx", ".ts", ".tsx", ".mjs", ".cjs", ".py", ".go", ".rs", ".java", ".kt", ".swift", ".c", ".cc", ".cpp", ".h", ".hpp", ".sql", ".sh", ".bash", ".zsh", ".graphql", ".gql":
		return "code"
	}
	if mediaType == "application/pdf" {
		return "pdf"
	}
	if strings.Contains(mediaType, "spreadsheet") || strings.Contains(mediaType, "excel") || strings.Contains(mediaType, "opendocument.spreadsheet") {
		return "sheet"
	}
	if mediaType == "text/csv" || mediaType == "text/tab-separated-values" {
		return "csv"
	}
	if strings.Contains(mediaType, "json") {
		return "data"
	}
	return "document"
}

func autoAttachmentSkills(attachments []expandedAttachment) []string {
	out := make([]string, 0, 4)
	seen := map[string]bool{}
	for _, attachment := range attachments {
		id := attachmentCommandSkill(attachment)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
		if len(out) == 4 {
			break
		}
	}
	return out
}

// applyAutomaticAttachmentSkills makes attachment handling capability-driven rather
// than dependent on the user knowing about @image/@pdf/@sheet/etc. Explicit command
// skills still win when the four-skill UI budget is already full; routing continues
// to inspect the actual attachment payload independently.
func applyAutomaticAttachmentSkills(body []byte, attachments []expandedAttachment, selection chatCommandSelection, guest bool) ([]byte, chatCommandSelection, error) {
	if len(attachments) == 0 {
		return body, selection, nil
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, selection, fmt.Errorf("invalid chat payload")
	}
	seen := map[string]bool{}
	for _, id := range selection.Skills {
		seen[id] = true
	}
	instructions := []string{}
	for _, id := range autoAttachmentSkills(attachments) {
		if seen[id] {
			continue
		}
		option, found := findCommandOption(chatCommandSkills, id, guest)
		if !found {
			continue
		}
		seen[id] = true
		selection.AutoSkills = append(selection.AutoSkills, id)
		if len(selection.Skills) < 4 {
			selection.Skills = append(selection.Skills, id)
		}
		instructions = append(instructions, option.Instruction)
	}
	if len(instructions) == 0 {
		return body, selection, nil
	}
	autoInstruction := "DAIKI AUTOMATIC ATTACHMENT SKILLS (selected from the attached media type; apply them to this turn):\n" + strings.Join(instructions, "\n")
	messages, _ := payload["messages"].([]any)
	if len(messages) > 0 {
		if first, ok := messages[0].(map[string]any); ok && strings.EqualFold(strings.TrimSpace(fmt.Sprint(first["role"])), "system") {
			first["content"] = strings.TrimSpace(fmt.Sprint(first["content"])) + "\n\n" + autoInstruction
			messages[0] = first
		} else {
			messages = append([]any{map[string]any{"role": "system", "content": autoInstruction}}, messages...)
		}
	} else {
		messages = []any{map[string]any{"role": "system", "content": autoInstruction}}
	}
	payload["messages"] = messages
	out, err := json.Marshal(payload)
	return out, selection, err
}

func commandSelectionNeedsSkillsProfile(selection chatCommandSelection) bool {
	for _, id := range append(append([]string{}, selection.Skills...), selection.AutoSkills...) {
		switch id {
		case "graft", "pdf", "sheet", "csv", "document", "image":
			return true
		}
	}
	return false
}
