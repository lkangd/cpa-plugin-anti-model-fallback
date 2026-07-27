package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// fallbackStatus is returned to the client once the retry budget is spent.
const fallbackStatus = http.StatusServiceUnavailable

// attemptOutcome classifies one upstream attempt.
type attemptOutcome int

const (
	// outcomeAccepted means the response may be delivered to the client.
	outcomeAccepted attemptOutcome = iota
	// outcomeFallback means a different model served the request.
	outcomeFallback
	// outcomePassthrough means the response is an upstream error and must be
	// forwarded verbatim without consuming retry budget.
	outcomePassthrough
)

// classifyAttempt decides what to do with one upstream response.
//
// A missing model field is treated as accepted (fail open): without evidence of
// a fallback the plugin must not burn the retry budget or fail a good response.
func classifyAttempt(statusCode int, payload []byte, expected string) (attemptOutcome, string) {
	if statusCode >= 400 {
		return outcomePassthrough, ""
	}
	actual := extractProcessingModel(payload)
	if actual == "" {
		return outcomeAccepted, ""
	}
	if modelMatches(expected, actual) {
		return outcomeAccepted, actual
	}
	return outcomeFallback, actual
}

// fallbackMessage renders the human-readable reason delivered to the client.
func fallbackMessage(expected, actual string, attempts int) string {
	if actual == "" {
		actual = "an unreported model"
	}
	return fmt.Sprintf(
		"model %q was served by %q after %d attempts; anti-model-fallback refused to return a substituted model",
		expected, actual, attempts,
	)
}

// retryPause sleeps between attempts so a load-balanced upstream has a chance
// to land on a different backend.
func retryPause(cfg pluginConfig) {
	if cfg.RetryDelayMs <= 0 {
		return
	}
	time.Sleep(time.Duration(cfg.RetryDelayMs) * time.Millisecond)
}

// resolveProtection returns the protection rule and expected model for a request.
func resolveProtection(cfg pluginConfig, clientModel, executionModel string) (protectedModel, string, bool) {
	entry, ok := cfg.findProtectedModel(clientModel)
	if !ok {
		entry, ok = cfg.findProtectedModel(executionModel)
	}
	if !ok {
		return protectedModel{}, "", false
	}
	return entry, expectedModelFor(entry, executionModel), true
}

func execute(raw []byte) ([]byte, error) {
	var req rpcExecutorRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}

	activeExecutions.Add(1)
	defer activeExecutions.Add(-1)

	cfg := loadedConfig()
	clientModel := strings.TrimSpace(req.Model)
	entry, expected, protected := resolveProtection(cfg, clientModel, clientModel)
	budget := 0
	if protected {
		budget = cfg.retryBudget(entry)
	}

	var lastActual string

	// budget retries on top of the initial attempt.
	for attempt := 0; attempt <= budget; attempt++ {
		if attempt > 0 {
			retryPause(cfg)
		}
		resp, errCall := hostModelExecute(req)
		if errCall != nil {
			return errorEnvelope("executor_error", errCall.Error()), nil
		}
		if !protected {
			return okEnvelope(executorResponseFrom(resp))
		}
		outcome, actual := classifyAttempt(resp.StatusCode, resp.Body, expected)
		switch outcome {
		case outcomeAccepted, outcomePassthrough:
			return okEnvelope(executorResponseFrom(resp))
		case outcomeFallback:
			lastActual = actual
		}
	}

	return errorEnvelopeWithStatus(
		"model_fallback_blocked",
		fallbackMessage(expected, lastActual, budget+1),
		fallbackStatus,
	), nil
}

// hostModelExecute replays the request through the host execution chain with a
// bypass marker so the router does not claim it again.
func hostModelExecute(req rpcExecutorRequest) (pluginapi.HostModelExecutionResponse, error) {
	headers := cloneHeaders(req.Headers)
	headers.Set(bypassHeader, newBypassToken())

	raw, errCall := callHost(pluginabi.MethodHostModelExecute, hostModelExecutionRequest{
		HostModelExecutionRequest: pluginapi.HostModelExecutionRequest{
			EntryProtocol: formatOrDefault(req.SourceFormat, req.Format),
			ExitProtocol:  formatOrDefault(req.Format, req.SourceFormat),
			Model:         strings.TrimSpace(req.Model),
			Stream:        false,
			Body:          requestBody(req.ExecutorRequest),
			Headers:       headers,
			Query:         req.Query,
			Alt:           req.Alt,
		},
		HostCallbackID: req.HostCallbackID,
	})
	if errCall != nil {
		return pluginapi.HostModelExecutionResponse{}, errCall
	}
	var resp pluginapi.HostModelExecutionResponse
	if errDecode := json.Unmarshal(raw, &resp); errDecode != nil {
		return pluginapi.HostModelExecutionResponse{}, errDecode
	}
	return resp, nil
}

func executorResponseFrom(resp pluginapi.HostModelExecutionResponse) pluginapi.ExecutorResponse {
	out := pluginapi.ExecutorResponse{
		Payload: resp.Body,
		Headers: resp.Headers,
	}
	if resp.StatusCode != 0 && resp.StatusCode != http.StatusOK {
		out.Metadata = map[string]any{"status_code": resp.StatusCode}
	}
	return out
}

// requestBody prefers the original client payload so the replay is byte-identical
// to what the client asked for.
func requestBody(req pluginapi.ExecutorRequest) []byte {
	if len(req.OriginalRequest) > 0 {
		return req.OriginalRequest
	}
	return req.Payload
}

func formatOrDefault(primary, fallback string) string {
	if value := strings.TrimSpace(primary); value != "" {
		return value
	}
	return strings.TrimSpace(fallback)
}

func cloneHeaders(in http.Header) http.Header {
	out := http.Header{}
	for key, values := range in {
		for _, value := range values {
			out.Add(key, value)
		}
	}
	return out
}
