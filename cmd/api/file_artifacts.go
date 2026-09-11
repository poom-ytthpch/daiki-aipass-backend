package main

import (
	"bytes"
	_ "embed"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/go-pdf/fpdf"
)

// Sarabun is distributed under the SIL Open Font License. The license text is
// kept next to the font in cmd/api/assets/fonts/OFL-Sarabun.txt.
//
//go:embed assets/fonts/Sarabun-Regular.ttf
var generatedPDFFont []byte

//go:embed assets/fonts/OFL-Sarabun.txt
var generatedPDFFontLicense []byte

const daikiPDFTextMarker = "%DAIKI-TEXT-BASE64:"

type generatedFileSpec struct {
	Format    string
	Name      string
	MediaType string
}

func normalizeGeneratedFileFormat(v string) string {
	v = strings.TrimSpace(strings.ToLower(v))
	v = strings.TrimPrefix(v, ".")
	switch v {
	case "text", "plain", "txt":
		return "txt"
	case "markdown", "md":
		return "md"
	case "csv", "tsv":
		return "csv"
	case "json":
		return "json"
	case "pdf":
		return "pdf"
	default:
		return ""
	}
}

func generatedFileSpecFor(format, name, mediaType, prompt string) (generatedFileSpec, error) {
	format = normalizeGeneratedFileFormat(format)
	name = generatedName(name, "")
	mediaType = strings.TrimSpace(strings.ToLower(strings.Split(mediaType, ";")[0]))

	if format == "" && name != "" {
		format = normalizeGeneratedFileFormat(filepath.Ext(name))
	}
	if format == "" {
		switch mediaType {
		case "text/csv", "text/tab-separated-values":
			format = "csv"
		case "application/pdf":
			format = "pdf"
		case "application/json":
			format = "json"
		case "text/markdown":
			format = "md"
		case "text/plain":
			format = "txt"
		}
	}
	if format == "" {
		p := strings.ToLower(prompt)
		switch {
		case strings.Contains(p, "pdf") || strings.Contains(p, "พีดีเอฟ"):
			format = "pdf"
		case strings.Contains(p, "csv") || strings.Contains(p, "ซีเอสวี"):
			format = "csv"
		case strings.Contains(p, "json"):
			format = "json"
		case strings.Contains(p, "markdown") || strings.Contains(p, "มาร์กดาวน์"):
			format = "md"
		default:
			format = "txt"
		}
	}

	var ext, mimeType string
	switch format {
	case "txt":
		ext, mimeType = ".txt", "text/plain; charset=utf-8"
	case "md":
		ext, mimeType = ".md", "text/markdown; charset=utf-8"
	case "csv":
		ext, mimeType = ".csv", "text/csv; charset=utf-8"
	case "json":
		ext, mimeType = ".json", "application/json"
	case "pdf":
		ext, mimeType = ".pdf", "application/pdf"
	default:
		return generatedFileSpec{}, fmt.Errorf("unsupported generated file format %q", format)
	}
	if name == "" {
		name = "generated" + ext
	} else if strings.ToLower(filepath.Ext(name)) != ext {
		name = strings.TrimSuffix(name, filepath.Ext(name)) + ext
	}
	return generatedFileSpec{Format: format, Name: name, MediaType: mimeType}, nil
}

func generatedFilePrompt(spec generatedFileSpec, userPrompt string) string {
	base := "Create the requested file contents. When Hermes skill_view is available, load the installed daiki-file-artifacts skill first and follow it. Preserve the user's language unless the user explicitly asks for another language. Do not explain what you are doing. "
	switch spec.Format {
	case "csv":
		return base + "Return ONLY valid RFC 4180 CSV text. The first row must contain headers when tabular data has named fields. Quote cells correctly, preserve exact identifiers/numbers, do not use Markdown fences, and do not add prose before or after the CSV. User request:\n" + strings.TrimSpace(userPrompt)
	case "json":
		return base + "Return ONLY valid JSON, with no Markdown fences or commentary. User request:\n" + strings.TrimSpace(userPrompt)
	case "pdf":
		return base + "Return ONLY the document body as plain text with optional simple Markdown-style headings (#, ##) and bullets (-). Do not use Markdown code fences, HTML, or a filename header. The returned text will be rendered into a Unicode PDF. User request:\n" + strings.TrimSpace(userPrompt)
	case "md":
		return base + "Return ONLY the Markdown file body with no surrounding code fence and no filename header. User request:\n" + strings.TrimSpace(userPrompt)
	default:
		return base + "Return ONLY the final UTF-8 text file body, with no Markdown fence, explanation, or filename header. User request:\n" + strings.TrimSpace(userPrompt)
	}
}

func stripGeneratedFence(content string) string {
	content = strings.TrimSpace(strings.TrimPrefix(content, "\ufeff"))
	if !strings.HasPrefix(content, "```") {
		return content
	}
	lines := strings.Split(content, "\n")
	if len(lines) < 2 {
		return content
	}
	lines = lines[1:]
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "```" {
		lines = lines[:len(lines)-1]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func normalizeGeneratedCSV(content string) ([]byte, error) {
	content = stripGeneratedFence(content)
	if strings.TrimSpace(content) == "" {
		return nil, errors.New("generated csv is empty")
	}
	r := csv.NewReader(strings.NewReader(content))
	r.FieldsPerRecord = -1
	records, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("invalid generated csv: %w", err)
	}
	if len(records) == 0 {
		return nil, errors.New("generated csv has no rows")
	}
	var out bytes.Buffer
	w := csv.NewWriter(&out)
	for _, row := range records {
		if err := w.Write(row); err != nil {
			return nil, err
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func normalizeGeneratedJSON(content string) ([]byte, error) {
	content = stripGeneratedFence(content)
	candidates := []string{content}
	if extracted := extractGeneratedJSONCandidate(content); extracted != "" && extracted != content {
		candidates = append(candidates, extracted)
	}
	for _, candidate := range candidates {
		attempts := []string{candidate, removeGeneratedJSONTrailingCommas(candidate)}
		for _, attempt := range attempts {
			var value any
			if err := json.Unmarshal([]byte(attempt), &value); err != nil {
				continue
			}
			out, err := json.MarshalIndent(value, "", "  ")
			if err != nil {
				return nil, err
			}
			return append(out, '\n'), nil
		}
	}
	return nil, errors.New("invalid generated json after safe repair")
}

func removeGeneratedJSONTrailingCommas(content string) string {
	var out strings.Builder
	out.Grow(len(content))
	inString := false
	escaped := false
	for i := 0; i < len(content); i++ {
		c := content[i]
		if !inString && c == ',' {
			j := i + 1
			for j < len(content) && (content[j] == ' ' || content[j] == '\n' || content[j] == '\r' || content[j] == '\t') {
				j++
			}
			if j < len(content) && (content[j] == '}' || content[j] == ']') {
				continue
			}
		}
		out.WriteByte(c)
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' {
				escaped = true
				continue
			}
			if c == '"' {
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
		}
	}
	return out.String()
}

func extractGeneratedJSONCandidate(content string) string {
	content = strings.TrimSpace(content)
	start := -1
	for i, r := range content {
		if r == '{' || r == '[' {
			start = i
			break
		}
	}
	if start < 0 {
		return ""
	}
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(content); i++ {
		c := content[i]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' {
				escaped = true
				continue
			}
			if c == '"' {
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				return strings.TrimSpace(content[start : i+1])
			}
			if depth < 0 {
				return ""
			}
		}
	}
	return ""
}

func renderGeneratedPDF(content, title string) ([]byte, error) {
	content = stripGeneratedFence(content)
	if strings.TrimSpace(content) == "" {
		return nil, errors.New("generated pdf content is empty")
	}
	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.SetMargins(18, 18, 18)
	pdf.SetAutoPageBreak(true, 18)
	pdf.SetTitle(title, true)
	pdf.SetAuthor("Daiki AI Passport", true)
	pdf.AddUTF8FontFromBytes("Sarabun", "", generatedPDFFont)
	if pdf.Error() != nil {
		return nil, pdf.Error()
	}
	pdf.AddPage()
	setSize := func(size float64) error {
		pdf.SetFont("Sarabun", "", size)
		return pdf.Error()
	}
	if err := setSize(11); err != nil {
		return nil, err
	}
	for _, raw := range strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			pdf.Ln(3)
			continue
		}
		size, height := 11.0, 5.5
		text := line
		switch {
		case strings.HasPrefix(line, "### "):
			size, height, text = 13, 6.2, strings.TrimSpace(strings.TrimPrefix(line, "### "))
		case strings.HasPrefix(line, "## "):
			size, height, text = 15, 7, strings.TrimSpace(strings.TrimPrefix(line, "## "))
		case strings.HasPrefix(line, "# "):
			size, height, text = 18, 8, strings.TrimSpace(strings.TrimPrefix(line, "# "))
		case strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "* "):
			text = "• " + strings.TrimSpace(line[2:])
		}
		if err := setSize(size); err != nil {
			return nil, err
		}
		pdf.MultiCell(0, height, text, "", "L", false)
		if size > 11 {
			pdf.Ln(1.5)
		}
	}
	var out bytes.Buffer
	if err := pdf.Output(&out); err != nil {
		return nil, err
	}
	// Keep a machine-readable copy of the exact generated Unicode text as a PDF
	// comment. Viewers ignore the comment, while Daiki can recover Thai and other
	// complex scripts reliably if the PDF is downloaded and uploaded again.
	out.WriteByte('\n')
	out.WriteString(daikiPDFTextMarker)
	out.WriteString(base64.RawStdEncoding.EncodeToString([]byte(content)))
	out.WriteByte('\n')
	return out.Bytes(), nil
}

func normalizeGeneratedFile(spec generatedFileSpec, content string) ([]byte, error) {
	switch spec.Format {
	case "csv":
		return normalizeGeneratedCSV(content)
	case "json":
		return normalizeGeneratedJSON(content)
	case "pdf":
		return renderGeneratedPDF(content, strings.TrimSuffix(spec.Name, filepath.Ext(spec.Name)))
	case "md", "txt":
		content = stripGeneratedFence(content)
		if strings.TrimSpace(content) == "" {
			return nil, errors.New("generated file is empty")
		}
		return []byte(content + "\n"), nil
	default:
		return nil, fmt.Errorf("unsupported generated file format %q", spec.Format)
	}
}
