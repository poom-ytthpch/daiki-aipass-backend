package main

import (
	"encoding/base64"
	"testing"
)

func TestDecodeGeminiImageResponseFromOutputImage(t *testing.T) {
	want := []byte("\x89PNG\r\n\x1a\nrest")
	body := []byte(`{"output_image":{"data":"` + base64.StdEncoding.EncodeToString(want) + `","mime_type":"image/png"}}`)
	got, mediaType, err := decodeGeminiImageResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) || mediaType != "image/png" {
		t.Fatalf("got=%q mediaType=%q", got, mediaType)
	}
}

func TestDecodeGeminiImageResponseFromModelOutputStep(t *testing.T) {
	want := []byte("\xff\xd8\xffjpeg")
	body := []byte(`{"steps":[{"type":"model_output","content":[{"type":"text","text":"done"},{"type":"image","data":"` + base64.StdEncoding.EncodeToString(want) + `","mime_type":"image/jpeg"}]}]}`)
	got, mediaType, err := decodeGeminiImageResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) || mediaType != "image/jpeg" {
		t.Fatalf("got=%q mediaType=%q", got, mediaType)
	}
}

func TestDecodeGeminiImageResponseRejectsMissingImage(t *testing.T) {
	if _, _, err := decodeGeminiImageResponse([]byte(`{"steps":[{"type":"model_output","content":[{"type":"text","text":"no image"}]}]}`)); err == nil {
		t.Fatal("expected missing Gemini image error")
	}
}

func TestDecodeOpenRouterImageResponse(t *testing.T) {
	want := []byte("png-bytes")
	body := []byte(`{"data":[{"b64_json":"` + base64.StdEncoding.EncodeToString(want) + `","media_type":"image/png"}]}`)
	got, mediaType, err := decodeOpenRouterImageResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) || mediaType != "image/png" {
		t.Fatalf("got data=%q mediaType=%q", got, mediaType)
	}
}

func TestDecodeOpenRouterImageResponseDefaultsMediaType(t *testing.T) {
	body := []byte(`{"data":[{"b64_json":"` + base64.StdEncoding.EncodeToString([]byte("img")) + `"}]}`)
	_, mediaType, err := decodeOpenRouterImageResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if mediaType != "image/png" {
		t.Fatalf("mediaType=%q", mediaType)
	}
}

func TestDecodeOpenRouterImageResponseRejectsMissingImage(t *testing.T) {
	if _, _, err := decodeOpenRouterImageResponse([]byte(`{"data":[]}`)); err == nil {
		t.Fatal("expected missing image error")
	}
}

func TestDetectGeneratedImageMediaType(t *testing.T) {
	if got, err := detectGeneratedImageMediaType([]byte("\x89PNG\r\n\x1a\nrest")); err != nil || got != "image/png" {
		t.Fatalf("got=%q err=%v", got, err)
	}
	if _, err := detectGeneratedImageMediaType([]byte("<svg></svg>")); err == nil {
		t.Fatal("expected active or unsupported image format to be rejected")
	}
}
