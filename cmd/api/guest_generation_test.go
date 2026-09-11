package main

import (
	"encoding/base64"
	"testing"
)

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
