package balancer

import (
	"strings"
	"testing"
)

func TestParsePrometheusMetrics_vLLM(t *testing.T) {
	input := `# HELP vllm:num_requests_running Number of requests currently running on GPU.
# TYPE vllm:num_requests_running gauge
vllm:num_requests_running{engine="mp_rank}" 5
# HELP vllm:num_requests_waiting Number of requests waiting to be processed.
# TYPE vllm:num_requests_waiting gauge
vllm:num_requests_waiting{engine="mp_rank}" 12
# HELP vllm:gpu_kv_cache_usage_perc GPU KV cache usage percentage [0,1].
# TYPE vllm:gpu_kv_cache_usage_perc gauge
vllm:gpu_kv_cache_usage_perc{engine="mp_rank}" 0.82
`

	result := parsePrometheusMetrics(strings.NewReader(input), EngineVLML)
	if !result.Parsed {
		t.Fatal("expected metrics to be parsed")
	}
	if result.RunningReqs != 5 {
		t.Errorf("running = %d, want 5", result.RunningReqs)
	}
	if result.WaitingReqs != 12 {
		t.Errorf("waiting = %d, want 12", result.WaitingReqs)
	}
	if result.KVCachePct != 0.82 {
		t.Errorf("kvCachePct = %f, want 0.82", result.KVCachePct)
	}
}

func TestParsePrometheusMetrics_SGLang(t *testing.T) {
	input := `# HELP sglang:num_running_reqs Number of running requests.
# TYPE sglang:num_running_reqs gauge
sglang:num_running_reqs 8
# HELP sglang:num_queue_reqs Number of queued requests.
# TYPE sglang:num_queue_reqs gauge
sglang:num_queue_reqs 3
# HELP sglang:mem_usage_ratio Memory usage ratio [0,1].
# TYPE sglang:mem_usage_ratio gauge
sglang:mem_usage_ratio 0.75
`

	result := parsePrometheusMetrics(strings.NewReader(input), EngineSGLang)
	if !result.Parsed {
		t.Fatal("expected metrics to be parsed")
	}
	if result.RunningReqs != 8 {
		t.Errorf("running = %d, want 8", result.RunningReqs)
	}
	if result.WaitingReqs != 3 {
		t.Errorf("waiting = %d, want 3", result.WaitingReqs)
	}
	if result.KVCachePct != 0.75 {
		t.Errorf("kvCachePct = %f, want 0.75", result.KVCachePct)
	}
}

func TestParsePrometheusMetrics_Empty(t *testing.T) {
	input := `# HELP some_other_metric Some random metric.
# TYPE some_other_metric gauge
some_other_metric 42
`

	result := parsePrometheusMetrics(strings.NewReader(input), EngineVLML)
	if result.Parsed {
		t.Error("expected no vLLM metrics to be parsed")
	}
	if result.RunningReqs != 0 {
		t.Errorf("running = %d, want 0", result.RunningReqs)
	}
}

func TestParsePrometheusMetrics_AutoDetect_vLLM(t *testing.T) {
	input := `vllm:num_requests_running{engine="mp_rank}" 7
vllm:num_requests_waiting{engine="mp_rank}" 2
vllm:gpu_kv_cache_usage_perc{engine="mp_rank}" 0.91
`

	result := parsePrometheusMetrics(strings.NewReader(input), EngineUnknown)
	if !result.Parsed {
		t.Fatal("expected auto-detection to parse vLLM metrics")
	}
	if result.RunningReqs != 7 {
		t.Errorf("running = %d, want 7", result.RunningReqs)
	}
}

func TestParsePrometheusMetrics_AutoDetect_SGLang(t *testing.T) {
	input := `sglang:num_running_reqs 10
sglang:num_queue_reqs 5
sglang:mem_usage_ratio 0.60
`

	result := parsePrometheusMetrics(strings.NewReader(input), EngineUnknown)
	if !result.Parsed {
		t.Fatal("expected auto-detection to parse SGLang metrics")
	}
	if result.RunningReqs != 10 {
		t.Errorf("running = %d, want 10", result.RunningReqs)
	}
}
