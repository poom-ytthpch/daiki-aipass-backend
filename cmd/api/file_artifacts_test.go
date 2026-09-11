package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGeneratedFileSpecInfersCSVAndPDF(t *testing.T) {
	csvSpec, err := generatedFileSpecFor("", "", "", "ช่วยสร้าง CSV รายการสินค้า")
	if err != nil || csvSpec.Format != "csv" || csvSpec.Name != "generated.csv" || !strings.HasPrefix(csvSpec.MediaType, "text/csv") {
		t.Fatalf("csv spec=%+v err=%v", csvSpec, err)
	}
	pdfSpec, err := generatedFileSpecFor("", "report.pdf", "", "สร้างรายงาน")
	if err != nil || pdfSpec.Format != "pdf" || pdfSpec.Name != "report.pdf" || pdfSpec.MediaType != "application/pdf" {
		t.Fatalf("pdf spec=%+v err=%v", pdfSpec, err)
	}
}

func TestNormalizeGeneratedCSV(t *testing.T) {
	data, err := normalizeGeneratedCSV("```csv\nname,qty,note\nสินค้า A,2,\"มี, comma\"\nสินค้า B,3,ok\n```")
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "name,qty,note\n") || !strings.Contains(text, "สินค้า A,2,\"มี, comma\"") {
		t.Fatalf("csv=%q", text)
	}
}

func TestRenderGeneratedPDFIsReadableByAttachmentExtractor(t *testing.T) {
	data, err := renderGeneratedPDF("# รายงานทดสอบ\n- จำนวนสินค้า 5 รายการ\nสวัสดีจาก Daiki", "รายงาน")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), "%PDF-") {
		t.Fatalf("not a pdf: %q", data[:min(len(data), 20)])
	}
	path := filepath.Join(t.TempDir(), "generated.pdf")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	status, text := extractAttachmentContent(path, "generated.pdf", "application/pdf")
	if status != "pdf-ready" {
		t.Fatalf("status=%q text=%q", status, text)
	}
	if !strings.Contains(text, "รายงานทดสอบ") || !strings.Contains(text, "สวัสดีจาก Daiki") {
		t.Fatalf("extracted pdf text=%q", text)
	}
}
