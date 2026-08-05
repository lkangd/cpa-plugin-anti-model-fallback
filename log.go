package main

import (
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

// Log levels accepted by the host. Anything else is logged at debug level.
const (
	logLevelInfo  = "info"
	logLevelWarn  = "warn"
	logLevelError = "error"
)

// hostLogRequest mirrors the host's host.log payload.
type hostLogRequest struct {
	HostCallbackID string         `json:"host_callback_id,omitempty"`
	Level          string         `json:"level,omitempty"`
	Message        string         `json:"message,omitempty"`
	Fields         map[string]any `json:"fields,omitempty"`
}

// hostLog writes one entry into the host log, which surfaces it in the cpa log
// viewer. Logging is best effort: a failure here must never affect the request.
func hostLog(callbackID, level, message string, fields map[string]any) {
	_, _ = callHost(pluginabi.MethodHostLog, hostLogRequest{
		HostCallbackID: callbackID,
		Level:          level,
		Message:        message,
		Fields:         fields,
	})
}

// Stable message prefixes, greppable in the log viewer. Filtering by
// "anti-model-fallback" shows every event; the suffix separates a single
// detected attempt from the final outcome of the request.
const (
	msgFallbackDetected = "anti-model-fallback: fallback detected"
	msgFallbackBlocked  = "anti-model-fallback: fallback blocked"
	msgFallbackResolved = "anti-model-fallback: retry resolved fallback"
)

// The host formatter renders only a fixed allowlist of log fields and drops
// everything else, so detail has to travel inside the message. Only `model`
// and `reason` from that allowlist are useful here.
const (
	fieldModel  = "model"
	fieldReason = "reason"
)

// logDetail renders the plugin's own key/value detail into the message body,
// matching the "key=value" style the host uses for its allowlisted fields.
func logDetail(pairs ...string) string {
	if len(pairs) == 0 {
		return ""
	}
	return " | " + strings.Join(pairs, " ")
}

func kv(key, value string) string {
	if strings.TrimSpace(value) == "" {
		value = "unknown"
	}
	return key + "=" + value
}

func kvInt(key string, value int) string {
	return fmt.Sprintf("%s=%d", key, value)
}

func transport(stream bool) string {
	if stream {
		return "stream"
	}
	return "non-stream"
}

// logFallbackDetected records one upstream attempt that came back with the
// wrong model. One line is emitted per attempt, so counting these lines counts
// fallback occurrences.
func logFallbackDetected(callbackID, clientModel, expected, actual string, attempt, budget int, stream bool) {
	hostLog(callbackID, logLevelWarn, msgFallbackDetected+logDetail(
		kv("requested", clientModel),
		kv("expected", expected),
		kv("served", actual),
		fmt.Sprintf("attempt=%d/%d", attempt, budget+1),
		kv("transport", transport(stream)),
	), map[string]any{
		fieldModel:  actual,
		fieldReason: "served by " + actual + " instead of " + expected,
	})
}

// logFallbackBlocked records that the retry budget was spent and the client
// received an error instead of a substituted model.
func logFallbackBlocked(callbackID, clientModel, expected, actual string, attempts int, stream bool) {
	hostLog(callbackID, logLevelError, msgFallbackBlocked+logDetail(
		kv("requested", clientModel),
		kv("expected", expected),
		kv("served", actual),
		kvInt("attempts", attempts),
		kv("transport", transport(stream)),
	), map[string]any{
		fieldModel:  actual,
		fieldReason: "retry budget exhausted",
	})
}

// logFallbackResolved records that a retry eventually landed on the right
// model, which is the case where the plugin actually rescued the request.
func logFallbackResolved(callbackID, clientModel, expected, actual string, attempt int, stream bool) {
	hostLog(callbackID, logLevelInfo, msgFallbackResolved+logDetail(
		kv("requested", clientModel),
		kv("expected", expected),
		kv("served", actual),
		kvInt("attempt", attempt),
		kv("transport", transport(stream)),
	), map[string]any{
		fieldModel: actual,
	})
}
