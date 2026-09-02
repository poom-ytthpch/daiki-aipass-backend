package inference

import (
	"encoding/json"
	"errors"
	"strings"
)

type Workload string

const (
	WorkloadFast      Workload = "fast"
	WorkloadDeep      Workload = "deep"
	WorkloadVision    Workload = "vision"
	WorkloadImage     Workload = "image"
	WorkloadEmbedding Workload = "embedding"
	WorkloadBatch     Workload = "batch"
)

type Route struct {
	Alias         string   `json:"alias"`
	Workload      Workload `json:"workload"`
	PhysicalModel string   `json:"-"`
	Priority      int      `json:"priority"`
}

type Router struct {
	models map[string]string
}

func NewRouter(fast, balanced, deep, vision string) *Router {
	if fast == "" {
		fast = "qwen-local"
	}
	if balanced == "" {
		balanced = fast
	}
	if deep == "" {
		deep = balanced
	}
	if vision == "disabled" {
		vision = ""
	}
	return &Router{models: map[string]string{"fast": fast, "balanced": balanced, "deep": deep, "vision": vision}}
}

func (r *Router) Aliases() []map[string]any {
	return []map[string]any{
		{"id": "auto", "object": "model", "owned_by": "daiki-router"},
		{"id": "fast", "object": "model", "owned_by": "daiki-router"},
		{"id": "balanced", "object": "model", "owned_by": "daiki-router"},
		{"id": "deep", "object": "model", "owned_by": "daiki-router"},
	}
}

type chatEnvelope struct {
	Model               string `json:"model"`
	MaxTokens           int    `json:"max_tokens"`
	MaxCompletionTokens int    `json:"max_completion_tokens"`
	Messages            []struct {
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

func (r *Router) RouteChat(body []byte) (Route, []byte, error) {
	var in chatEnvelope
	if err := json.Unmarshal(body, &in); err != nil {
		return Route{}, nil, errors.New("invalid chat payload")
	}
	alias := strings.ToLower(strings.TrimSpace(in.Model))
	if alias == "" {
		alias = "auto"
	}
	vision := false
	for _, m := range in.Messages {
		if strings.Contains(string(m.Content), `"image_url"`) || strings.Contains(string(m.Content), `"input_image"`) {
			vision = true
			break
		}
	}
	resolved := alias
	workload := WorkloadFast
	priority := 5
	if vision {
		resolved = "vision"
		workload = WorkloadVision
		priority = 6
	} else {
		switch alias {
		case "auto":
			if len(body) > 12000 || in.MaxTokens > 4096 || in.MaxCompletionTokens > 4096 {
				resolved = "deep"
				workload = WorkloadDeep
				priority = 4
			} else {
				resolved = "fast"
			}
		case "fast":
			workload = WorkloadFast
			priority = 6
		case "balanced":
			workload = WorkloadFast
			priority = 5
		case "deep":
			workload = WorkloadDeep
			priority = 4
		default:
			return Route{}, nil, errors.New("unsupported model alias; use auto, fast, balanced, or deep")
		}
	}
	physical, ok := r.models[resolved]
	if !ok || physical == "" {
		return Route{}, nil, errors.New("route unavailable")
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return Route{}, nil, errors.New("invalid chat payload")
	}
	payload["model"] = physical
	rewritten, err := json.Marshal(payload)
	if err != nil {
		return Route{}, nil, err
	}
	return Route{Alias: alias, Workload: workload, PhysicalModel: physical, Priority: priority}, rewritten, nil
}
