package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"
)

func TestAttachmentExcerptPrefersHeaderAndMatchingRows(t *testing.T) {
	text := "sku,price,status\nA,10,ok\nB,20,ok\nC,30,cancelled\nD,40,ok\n" + strings.Repeat("Z,1,ok\n", 200)
	excerpt, truncated := attachmentExcerpt(text, "why is C cancelled", 180)
	if !truncated || !strings.Contains(excerpt, "sku,price,status") || !strings.Contains(excerpt, "C,30,cancelled") {
		t.Fatalf("unexpected excerpt: truncated=%v %q", truncated, excerpt)
	}
}

func TestExtractXLSXAttachment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "book.xlsx")
	f := excelize.NewFile()
	if err := f.SetCellValue("Sheet1", "A1", "sku"); err != nil {
		t.Fatal(err)
	}
	if err := f.SetCellValue("Sheet1", "B1", "price"); err != nil {
		t.Fatal(err)
	}
	if err := f.SetCellValue("Sheet1", "A2", "ABC"); err != nil {
		t.Fatal(err)
	}
	if err := f.SetCellValue("Sheet1", "B2", 42); err != nil {
		t.Fatal(err)
	}
	if err := f.SaveAs(path); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	status, text := extractAttachmentContent(path, "book.xlsx", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	if status != "xlsx-ready" || !strings.Contains(text, "# Sheet: Sheet1") || !strings.Contains(text, "ABC\t42") {
		t.Fatalf("status=%q text=%q", status, text)
	}
}

func TestExtractDocxLikeXML(t *testing.T) {
	// Unknown binaries remain stored instead of being interpreted as text.
	path := filepath.Join(t.TempDir(), "legacy.xls")
	if err := os.WriteFile(path, []byte("binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	status, text := extractAttachmentContent(path, "legacy.xls", "application/vnd.ms-excel")
	if status != "stored" || text != "" {
		t.Fatalf("status=%q text=%q", status, text)
	}
}

func writeSimplePDF(t *testing.T, path, text string) {
	t.Helper()
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offsets := []int{0}
	stream := fmt.Sprintf("BT /F1 12 Tf 20 120 Td (%s) Tj ET\n", text)
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 300 200] /Resources << /Font << /F1 5 0 R >> >> /Contents 4 0 R >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(stream), stream),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}
	for i, obj := range objects {
		offsets = append(offsets, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, obj)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, off := range offsets[1:] {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestExtractPDFAttachment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sample.pdf")
	writeSimplePDF(t, path, "DAIKI PDF CHECK 4827")
	status, text := extractAttachmentContent(path, "sample.pdf", "application/pdf")
	if status != "pdf-ready" || !strings.Contains(text, "DAIKI PDF CHECK 4827") {
		t.Fatalf("status=%q text=%q", status, text)
	}
}

func TestLargeCSVExcerptIsTokenBounded(t *testing.T) {
	var b strings.Builder
	b.WriteString("sku,price,status\n")
	for i := 0; i < 50000; i++ {
		fmt.Fprintf(&b, "SKU-%05d,%d,ok\n", i, i)
	}
	b.WriteString("TARGET-731,999,cancelled\n")
	excerpt, clipped := attachmentExcerpt(b.String(), "find TARGET-731 cancelled", maxAttachmentExcerptBytes)
	if !clipped || len(excerpt) > maxAttachmentExcerptBytes || !strings.Contains(excerpt, "TARGET-731,999,cancelled") || !strings.Contains(excerpt, "sku,price,status") {
		t.Fatalf("clipped=%v len=%d target=%v", clipped, len(excerpt), strings.Contains(excerpt, "TARGET-731,999,cancelled"))
	}
}
