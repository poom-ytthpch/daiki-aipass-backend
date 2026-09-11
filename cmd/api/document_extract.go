package main

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	pdf "github.com/ledongthuc/pdf"
	"github.com/xuri/excelize/v2"
)

const (
	maxExtractedDocumentBytes = 1 << 20
	maxExtractedTextBytes     = maxExtractedDocumentBytes
	maxAttachmentContextBytes = 16 << 10
	maxAttachmentExcerptBytes = 10 << 10
)

func clipUTF8Bytes(s string, limit int) (string, bool) {
	if limit <= 0 || len(s) <= limit {
		return s, false
	}
	cut := limit
	for cut > 0 && !utf8.ValidString(s[:cut]) {
		cut--
	}
	return s[:cut], true
}

func readTextAttachment(path string) (string, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxExtractedDocumentBytes+1))
	if err != nil {
		return "", false, err
	}
	truncated := len(b) > maxExtractedDocumentBytes
	if truncated {
		b = b[:maxExtractedDocumentBytes]
	}
	return string(bytes.ToValidUTF8(b, []byte("�"))), truncated, nil
}

func extractDaikiPDFText(path string) (string, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	idx := bytes.LastIndex(b, []byte(daikiPDFTextMarker))
	if idx < 0 {
		return "", false
	}
	encoded := strings.TrimSpace(string(b[idx+len(daikiPDFTextMarker):]))
	if encoded == "" {
		return "", false
	}
	decoded, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil {
		return "", false
	}
	text, _ := clipUTF8Bytes(string(decoded), maxExtractedDocumentBytes)
	return text, true
}

func extractPDFText(path string) (text string, truncated bool, err error) {
	if embedded, ok := extractDaikiPDFText(path); ok {
		return embedded, len(embedded) >= maxExtractedDocumentBytes, nil
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("pdf parser failed: %v", r)
		}
	}()
	f, reader, err := pdf.Open(path)
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	plain, err := reader.GetPlainText()
	if err != nil {
		return "", false, err
	}
	b, err := io.ReadAll(io.LimitReader(plain, maxExtractedDocumentBytes+1))
	if err != nil {
		return "", false, err
	}
	truncated = len(b) > maxExtractedDocumentBytes
	if truncated {
		b = b[:maxExtractedDocumentBytes]
	}
	return string(bytes.ToValidUTF8(b, []byte("�"))), truncated, nil
}

func extractXLSXText(path string) (string, bool, error) {
	book, err := excelize.OpenFile(path)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = book.Close() }()
	var b strings.Builder
	truncated := false
	for _, sheet := range book.GetSheetList() {
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString("# Sheet: ")
		b.WriteString(sheet)
		b.WriteByte('\n')
		rows, err := book.GetRows(sheet)
		if err != nil {
			return "", false, err
		}
		for _, row := range rows {
			line := strings.Join(row, "\t") + "\n"
			remaining := maxExtractedDocumentBytes - b.Len()
			if remaining <= 0 {
				truncated = true
				break
			}
			if len(line) > remaining {
				part, _ := clipUTF8Bytes(line, remaining)
				b.WriteString(part)
				truncated = true
				break
			}
			b.WriteString(line)
		}
		if truncated {
			break
		}
	}
	return b.String(), truncated, nil
}

func extractZipXMLText(path string, include func(string) bool) (string, bool, error) {
	r, err := zip.OpenReader(path)
	if err != nil {
		return "", false, err
	}
	defer r.Close()
	files := make([]*zip.File, 0)
	for _, f := range r.File {
		if include(f.Name) {
			files = append(files, f)
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	var b strings.Builder
	truncated := false
	for _, f := range files {
		rc, err := f.Open()
		if err != nil {
			return "", false, err
		}
		dec := xml.NewDecoder(rc)
		for {
			tok, err := dec.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				_ = rc.Close()
				return "", false, err
			}
			switch t := tok.(type) {
			case xml.CharData:
				v := strings.TrimSpace(string(t))
				if v != "" {
					if b.Len() > 0 && !strings.HasSuffix(b.String(), "\n") {
						b.WriteByte(' ')
					}
					b.WriteString(v)
				}
			case xml.EndElement:
				switch strings.ToLower(t.Name.Local) {
				case "p", "tr", "row":
					b.WriteByte('\n')
				case "tc", "cell":
					b.WriteByte('\t')
				}
			}
			if b.Len() >= maxExtractedDocumentBytes {
				truncated = true
				break
			}
		}
		_ = rc.Close()
		if truncated {
			break
		}
	}
	out, clipped := clipUTF8Bytes(b.String(), maxExtractedDocumentBytes)
	return out, truncated || clipped, nil
}

func extractAttachmentContent(storagePath, name, mediaType string) (string, string) {
	ext := strings.ToLower(filepath.Ext(name))
	var (
		text      string
		truncated bool
		err       error
		kind      string
	)
	switch {
	case textAttachment(name, mediaType):
		kind = "text"
		text, truncated, err = readTextAttachment(storagePath)
	case ext == ".pdf" || strings.EqualFold(mediaType, "application/pdf"):
		kind = "pdf"
		text, truncated, err = extractPDFText(storagePath)
	case ext == ".xlsx" || ext == ".xlsm" || ext == ".xltx" || ext == ".xltm":
		kind = "xlsx"
		text, truncated, err = extractXLSXText(storagePath)
	case ext == ".docx":
		kind = "docx"
		text, truncated, err = extractZipXMLText(storagePath, func(n string) bool {
			return n == "word/document.xml" || strings.HasPrefix(n, "word/header") || strings.HasPrefix(n, "word/footer")
		})
	case ext == ".pptx":
		kind = "pptx"
		text, truncated, err = extractZipXMLText(storagePath, func(n string) bool { return strings.HasPrefix(n, "ppt/slides/slide") && strings.HasSuffix(n, ".xml") })
	case ext == ".ods":
		kind = "ods"
		text, truncated, err = extractZipXMLText(storagePath, func(n string) bool { return n == "content.xml" })
	default:
		return "stored", ""
	}
	if err != nil {
		return kind + "-extract-failed", ""
	}
	if strings.TrimSpace(text) == "" {
		return kind + "-empty", ""
	}
	if truncated {
		return kind + "-truncated", text
	}
	return kind + "-ready", text
}

func looksAttachmentFollowUp(q string) bool {
	q = strings.TrimSpace(q)
	if q == "" || len([]rune(q)) > 260 {
		return false
	}
	if looksContextualFollowUp(q) {
		return true
	}
	n := strings.ToLower(q)
	markers := []string{"file", "document", "attachment", "pdf", "csv", "excel", "xlsx", "sheet", "row", "column", "table", "above", "same data", "this data", "summarize", "ไฟล์", "เอกสาร", "ข้อมูลนี้", "ตาราง", "แถว", "คอลัมน์", "ชีต", "สรุป", "วิเคราะห์ต่อ", "จากด้านบน"}
	for _, marker := range markers {
		if strings.Contains(n, marker) {
			return true
		}
	}
	return false
}

func attachmentQueryTerms(query string) []string {
	generic := map[string]bool{"please": true, "review": true, "attached": true, "attachment": true, "content": true, "file": true, "document": true, "image": true, "ช่วย": true, "ดู": true, "ไฟล์": true, "เอกสาร": true, "รูป": true, "ภาพ": true, "นี้": true, "หน่อย": true}
	fields := strings.FieldsFunc(strings.ToLower(query), func(r rune) bool { return !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-') })
	seen := map[string]bool{}
	out := make([]string, 0, 8)
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if len([]rune(f)) < 3 || generic[f] || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
		if len(out) == 8 {
			break
		}
	}
	return out
}

func attachmentExcerpt(text, query string, limit int) (string, bool) {
	if limit <= 0 {
		return "", len(text) > 0
	}
	if len(text) <= limit {
		return text, false
	}
	lines := strings.Split(text, "\n")
	terms := attachmentQueryTerms(query)
	selected := make([]string, 0, 80)
	seen := map[int]bool{}
	add := func(i int) {
		if i < 0 || i >= len(lines) || seen[i] {
			return
		}
		seen[i] = true
		selected = append(selected, lines[i])
	}
	// Preserve structural context: headers / first rows or opening paragraphs.
	for i := 0; i < len(lines) && i < 16; i++ {
		add(i)
	}
	if len(terms) > 0 {
		// Search the most specific query terms first. A generic column word such as
		// "sku" or "status" can match every row in a large table; scanning rows
		// first would fill the excerpt before a unique key near the end is reached.
		sort.SliceStable(terms, func(i, j int) bool {
			score := func(term string) int {
				n := len([]rune(term))
				if strings.ContainsAny(term, "-_") {
					n += 24
				}
				for _, r := range term {
					if unicode.IsDigit(r) {
						n += 32
						break
					}
				}
				return n
			}
			return score(terms[i]) > score(terms[j])
		})
		for _, term := range terms {
			for i, line := range lines {
				if !strings.Contains(strings.ToLower(line), term) {
					continue
				}
				add(i - 1)
				add(i)
				add(i + 1)
				if len(selected) >= 80 {
					break
				}
			}
			if len(selected) >= 80 {
				break
			}
		}
	}
	// Keep a small tail sample for totals/footer rows when space allows.
	for i := max(0, len(lines)-8); i < len(lines); i++ {
		add(i)
	}
	joined := strings.Join(selected, "\n")
	out, _ := clipUTF8Bytes(joined, limit)
	return out, true
}
