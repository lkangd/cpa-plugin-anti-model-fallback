package main

import "testing"

// realFallbackChunk is the opening SSE event captured from
// upstream.example.com/cn-cch while requesting glm-5.2.
const realFallbackChunk = `event: message_start
data: {"type":"message_start","message":{"id":"msg_8yfh7syvCUeNiiK7KCzQzmnP","type":"message","role":"assistant","content":[],"model":"kimi-for-coding","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":35988,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":0,"service_tier":"standard","inference_geo":"not_available"}}}

`

func TestExtractProcessingModelFromRealFallbackChunk(t *testing.T) {
	if got := extractProcessingModel([]byte(realFallbackChunk)); got != "kimi-for-coding" {
		t.Fatalf("extractProcessingModel = %q, want %q", got, "kimi-for-coding")
	}
}

func TestExtractProcessingModel(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    string
	}{
		{"claude non stream", `{"id":"msg_1","type":"message","model":"glm-5.2"}`, "glm-5.2"},
		{"openai chunk", `{"id":"c1","object":"chat.completion","model":"gpt-5.6-sol"}`, "gpt-5.6-sol"},
		{"sse with done", "data: {\"type\":\"message_start\",\"message\":{\"model\":\"glm-5.2\"}}\n\ndata: [DONE]\n", "glm-5.2"},
		{"sse ping first", "event: ping\ndata: {\"type\":\"ping\"}\n\ndata: {\"message\":{\"model\":\"glm-5.2\"}}\n", "glm-5.2"},
		{"no model", `{"type":"ping"}`, ""},
		{"empty", "", ""},
		{"malformed json", `{"model":`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractProcessingModel([]byte(tc.payload)); got != tc.want {
				t.Fatalf("extractProcessingModel = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestModelMatches(t *testing.T) {
	cases := []struct {
		name     string
		expected string
		actual   string
		want     bool
	}{
		{"exact", "glm-5.2", "glm-5.2", true},
		{"case insensitive", "GLM-5.2", "glm-5.2", true},
		{"version suffix upstream", "glm-5.2", "glm-5.2-0722", true},
		{"alias prefix stripped", "my-glm-5.2", "glm-5.2", true},
		{"real fallback rejected", "glm-5.2", "kimi-for-coding", false},
		{"real fallback rejected via alias", "my-glm-5.2", "kimi-for-coding", false},
		{"sibling version rejected", "glm-5.2", "glm-5.1", false},
		{"substring without boundary rejected", "glm-5.2", "glm-5.21", false},
		{"unrelated longer name rejected", "glm-5", "glm-52", false},
		{"empty expected", "", "glm-5.2", false},
		{"empty actual", "glm-5.2", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := modelMatches(tc.expected, tc.actual); got != tc.want {
				t.Fatalf("modelMatches(%q, %q) = %v, want %v", tc.expected, tc.actual, got, tc.want)
			}
		})
	}
}

func TestClassifyAttempt(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		payload  string
		expected string
		want     attemptOutcome
	}{
		{"match accepted", 200, `{"model":"glm-5.2"}`, "glm-5.2", outcomeAccepted},
		{"fallback detected", 200, realFallbackChunk, "glm-5.2", outcomeFallback},
		{"upstream error passthrough", 429, `{"error":"rate limited"}`, "glm-5.2", outcomePassthrough},
		{"server error passthrough", 500, ``, "glm-5.2", outcomePassthrough},
		{"missing model fails open", 200, `{"type":"ping"}`, "glm-5.2", outcomeAccepted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := classifyAttempt(tc.status, []byte(tc.payload), tc.expected)
			if got != tc.want {
				t.Fatalf("classifyAttempt = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestExpectedModelFor(t *testing.T) {
	explicit := protectedModel{Model: "my-glm-5.2", ExpectModel: "glm-5.2"}
	if got := expectedModelFor(explicit, "my-glm-5.2"); got != "glm-5.2" {
		t.Fatalf("expect_model should win, got %q", got)
	}
	implicit := protectedModel{Model: "my-glm-5.2"}
	if got := expectedModelFor(implicit, "my-glm-5.2"); got != "my-glm-5.2" {
		t.Fatalf("execution model should be used, got %q", got)
	}
	if got := expectedModelFor(implicit, ""); got != "my-glm-5.2" {
		t.Fatalf("declared model should be the fallback, got %q", got)
	}
}
