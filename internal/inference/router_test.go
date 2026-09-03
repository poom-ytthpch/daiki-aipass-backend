package inference

import (
	"encoding/json"
	"testing"
)

func TestRouterKeepsPhysicalModelServerSide(t *testing.T) {
	r := NewRouter("physical-fast", "physical-balanced", "physical-deep", "physical-vision")
	route, body, err := r.RouteChat([]byte(`{"model":"fast","messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if route.Alias != "fast" || route.ResolvedAlias != "fast" || route.PhysicalModel != "physical-fast" || route.Workload != WorkloadFast {
		t.Fatalf("unexpected route %#v", route)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["model"] != "physical-fast" {
		t.Fatalf("physical model not rewritten: %#v", payload)
	}
	for _, m := range r.Aliases() {
		if m["id"] == "physical-fast" {
			t.Fatal("physical model leaked through aliases")
		}
	}
}
func TestRouterAutoDeepAndVision(t *testing.T) {
	r := NewRouter("f", "b", "d", "v")
	deep, _, err := r.RouteChat([]byte(`{"model":"auto","max_tokens":5000,"messages":[{"role":"user","content":"debug this"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if deep.Workload != WorkloadDeep || deep.ResolvedAlias != "deep" || deep.PhysicalModel != "d" {
		t.Fatalf("expected deep route %#v", deep)
	}
	vision, _, err := r.RouteChat([]byte(`{"model":"auto","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,x"}}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if vision.Workload != WorkloadVision || vision.ResolvedAlias != "vision" || vision.PhysicalModel != "v" {
		t.Fatalf("expected vision route %#v", vision)
	}
}
func TestRouterRejectsPhysicalModelName(t *testing.T) {
	r := NewRouter("physical-fast", "b", "d", "v")
	if _, _, err := r.RouteChat([]byte(`{"model":"physical-fast","messages":[]}`)); err == nil {
		t.Fatal("physical model name must not be accepted from client")
	}
}
