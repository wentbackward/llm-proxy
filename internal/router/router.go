// Package router resolves virtual model names to concrete backends and
// applies per-model parameter profiles (defaults → caller → locked).
package router

import (
	"fmt"

	"github.com/wentbackward/llm-proxy/internal/config"
)

// samplingKeys is the set of parameters that may be overridden by route profiles.
var samplingKeys = map[string]bool{
	"temperature": true, "top_p": true, "top_k": true,
	"max_tokens": true, "presence_penalty": true,
	"frequency_penalty": true, "seed": true, "stop": true,
	"enable_thinking": true, "thinking_budget": true,
}

// Resolution is the output of routing: a concrete backend, the real model
// name to send upstream, and the merged sampling parameters.
type Resolution struct {
	Backend   *config.Backend
	RealModel string
	// Params contains only sampling parameters — callers must merge these
	// back into the request body themselves.
	Params map[string]interface{}
}

// Router resolves virtual model names.
type Router struct {
	cfg *config.Config
}

func New(cfg *config.Config) *Router {
	return &Router{cfg: cfg}
}

// Resolve returns a Resolution for the given virtual model name, or an error
// if the model is unknown. body should be the decoded request body; sampling
// params present in it participate in the merge.
func (r *Router) Resolve(modelName string, body map[string]interface{}) (*Resolution, error) {
	return r.resolve(modelName, body, 0)
}

func (r *Router) resolve(modelName string, body map[string]interface{}, depth int) (*Resolution, error) {
	if depth > 1 {
		return nil, fmt.Errorf("routing cycle: auto_route chains more than one level deep (model %q)", modelName)
	}

	route, ok := r.cfg.Route(modelName)
	if !ok {
		return nil, fmt.Errorf("unknown model %q", modelName)
	}

	// Auto-routing: inspect message content then recurse once.
	if route.AutoRoute != nil {
		messages, _ := body["messages"].([]interface{})
		target := route.AutoRoute.Text
		if isMultimodal(messages) {
			target = route.AutoRoute.Vision
		}
		return r.resolve(target, body, depth+1)
	}

	backend, ok := r.cfg.Backend(route.Backend)
	if !ok {
		return nil, fmt.Errorf("route %q references unknown backend %q", modelName, route.Backend)
	}

	params := mergeParams(route.Defaults, body, route.Locked)

	realModel := route.RealModel
	if realModel == "" {
		realModel = modelName
	}

	return &Resolution{
		Backend:   backend,
		RealModel: realModel,
		Params:    params,
	}, nil
}

// mergeParams applies the three-layer merge: defaults < caller < locked.
func mergeParams(defaults map[string]interface{}, body map[string]interface{}, locked map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(defaults)+len(locked))

	for k, v := range defaults {
		out[k] = v
	}
	for k, v := range body {
		if samplingKeys[k] {
			out[k] = v
		}
	}
	// Locked always wins — caller cannot override.
	for k, v := range locked {
		out[k] = v
	}
	return out
}

// isMultimodal returns true if any message contains a non-text content part
// (image, video, document, file).
func isMultimodal(messages []interface{}) bool {
	for _, m := range messages {
		msg, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		parts, ok := msg["content"].([]interface{})
		if !ok {
			continue // string content → text only
		}
		for _, p := range parts {
			part, ok := p.(map[string]interface{})
			if !ok {
				continue
			}
			switch part["type"] {
			case "image_url", "image", "video_url", "video", "document", "file":
				return true
			}
		}
	}
	return false
}
