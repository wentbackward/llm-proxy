// Package balancer provides load balancing for backend groups.
// This file implements Prometheus metrics parsing for vLLM and SGLang engines.
package balancer

import (
	"bufio"
	"io"
	"strconv"
	"strings"
)

// EngineType identifies the inference engine behind a backend.
type EngineType string

const (
	EngineUnknown EngineType = ""
	EngineVLML    EngineType = "vllm"
	EngineSGLang  EngineType = "sglang"
)

// ScrapeResult holds parsed metrics from a backend's /metrics endpoint.
type ScrapeResult struct {
	RunningReqs int
	WaitingReqs int
	KVCachePct  float64
	Parsed      bool
}

// parsePrometheusMetrics parses Prometheus text exposition format and extracts
// the vLLM or SGLang scheduling metrics.
//
// vLLM metrics:
//   - vllm:num_requests_running gauge
//   - vllm:num_requests_waiting gauge
//   - vllm:gpu_kv_cache_usage_perc gauge
//
// SGLang metrics:
//   - sglang:num_running_reqs gauge
//   - sglang:num_queue_reqs gauge
//   - sglang:mem_usage_ratio gauge
func parsePrometheusMetrics(reader io.Reader, engine EngineType) ScrapeResult {
	scanner := bufio.NewScanner(reader)
	result := ScrapeResult{}

	for scanner.Scan() {
		line := scanner.Text()

		switch engine {
		case EngineVLML:
			result = parseVLMLLine(line, result)
		case EngineSGLang:
			result = parseSGLangLine(line, result)
		default:
			// Auto-detect: try both parsers
			prev := result
			result = parseVLMLLine(line, result)
			if !result.Parsed {
				result = prev
				result = parseSGLangLine(line, result)
			}
		}
	}

	return result
}

// parseVLMLLine tries to extract a vLLM metric from a single line.
func parseVLMLLine(line string, result ScrapeResult) ScrapeResult {
	// Format: metric_name{labels} value
	// We strip labels for simplicity since we only care about the metric name + value
	parts := strings.SplitN(line, " ", 2)
	if len(parts) != 2 {
		return result
	}

	name := strings.TrimSpace(parts[0])
	valueStr := strings.TrimSpace(parts[1])

	// Strip comments
	if name == "" || name[0] == '#' {
		return result
	}

	value, err := strconv.ParseFloat(valueStr, 64)
	if err != nil {
		return result
	}

	// Strip labels from metric name (e.g. "vllm:num_requests_running{engine="mp_rank"}" -> "vllm:num_requests_running")
	baseName := name
	if idx := strings.Index(baseName, "{"); idx >= 0 {
		baseName = baseName[:idx]
	}

	switch baseName {
	case "vllm:num_requests_running":
		result.RunningReqs = int(value)
		result.Parsed = true
	case "vllm:num_requests_waiting":
		result.WaitingReqs = int(value)
		result.Parsed = true
	case "vllm:gpu_kv_cache_usage_perc":
		result.KVCachePct = value
		result.Parsed = true
	}

	return result
}

// parseSGLangLine tries to extract a SGLang metric from a single line.
func parseSGLangLine(line string, result ScrapeResult) ScrapeResult {
	parts := strings.SplitN(line, " ", 2)
	if len(parts) != 2 {
		return result
	}

	name := strings.TrimSpace(parts[0])
	valueStr := strings.TrimSpace(parts[1])

	if name == "" || name[0] == '#' {
		return result
	}

	value, err := strconv.ParseFloat(valueStr, 64)
	if err != nil {
		return result
	}

	// Strip labels from metric name
	baseName := name
	if idx := strings.Index(baseName, "{"); idx >= 0 {
		baseName = baseName[:idx]
	}

	switch baseName {
	case "sglang:num_running_reqs":
		result.RunningReqs = int(value)
		result.Parsed = true
	case "sglang:num_queue_reqs":
		result.WaitingReqs = int(value)
		result.Parsed = true
	case "sglang:mem_usage_ratio":
		result.KVCachePct = value
		result.Parsed = true
	}

	return result
}
