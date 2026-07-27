package main

import (
	"encoding/json"
	"strings"
)

// modelProbe covers the shapes that carry the processing model across the
// protocols cpa bridges: Claude puts it under message.model in message_start
// events, OpenAI and Gemini put it at the top level.
type modelProbe struct {
	Model   string `json:"model"`
	Message struct {
		Model string `json:"model"`
	} `json:"message"`
	Response struct {
		Model string `json:"model"`
	} `json:"response"`
}

// extractModelFromJSON reads the processing model out of a single JSON document.
func extractModelFromJSON(payload []byte) string {
	trimmed := strings.TrimSpace(string(payload))
	if trimmed == "" || trimmed[0] != '{' {
		return ""
	}
	var probe modelProbe
	if errUnmarshal := json.Unmarshal([]byte(trimmed), &probe); errUnmarshal != nil {
		return ""
	}
	for _, candidate := range []string{probe.Model, probe.Message.Model, probe.Response.Model} {
		if value := strings.TrimSpace(candidate); value != "" {
			return value
		}
	}
	return ""
}

// extractModelFromSSE scans server-sent event data lines for the first payload
// that names a processing model. Claude reports it in the very first event
// (message_start), so this normally resolves on the opening chunk.
func extractModelFromSSE(payload []byte) string {
	for _, line := range strings.Split(string(payload), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		if model := extractModelFromJSON([]byte(data)); model != "" {
			return model
		}
	}
	return ""
}

// extractProcessingModel resolves the model that actually served a response,
// accepting either a raw JSON body or an SSE payload.
func extractProcessingModel(payload []byte) string {
	if model := extractModelFromJSON(payload); model != "" {
		return model
	}
	return extractModelFromSSE(payload)
}

// modelMatches reports whether actual is an acceptable stand-in for expected.
//
// Beyond exact equality it tolerates two shapes seen in practice:
//   - a version suffix appended upstream (expected glm-5.2, actual glm-5.2-0722)
//   - an alias prefix stripped upstream (expected my-glm-5.2, actual glm-5.2)
//
// Both relaxations require a separator at the boundary so unrelated names such
// as kimi-for-coding never slip through.
func modelMatches(expected, actual string) bool {
	exp := strings.ToLower(strings.TrimSpace(expected))
	act := strings.ToLower(strings.TrimSpace(actual))
	if exp == "" || act == "" {
		return false
	}
	if exp == act {
		return true
	}
	if strings.HasPrefix(act, exp) && isBoundary(act[len(exp)]) {
		return true
	}
	if strings.HasSuffix(exp, act) && isBoundary(exp[len(exp)-len(act)-1]) {
		return true
	}
	return false
}

// isBoundary reports whether c separates model name segments.
func isBoundary(c byte) bool {
	return c == '-' || c == '_' || c == '.' || c == '/' || c == ':'
}

// expectedModelFor resolves which name the upstream response must report.
func expectedModelFor(entry protectedModel, executionModel string) string {
	if entry.ExpectModel != "" {
		return entry.ExpectModel
	}
	if value := strings.TrimSpace(executionModel); value != "" {
		return value
	}
	return entry.Model
}
