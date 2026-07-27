package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// bypassHeader marks a host execution issued by this plugin's own executor.
// Without it the router would claim the replayed request again and recurse
// until the process ran out of stack or upstream quota.
const bypassHeader = "X-Cpa-Anti-Model-Fallback-Bypass"

// maxConcurrentExecutions is a circuit breaker. It bounds the damage if the
// bypass header is ever dropped by the host: instead of recursing without
// limit the router starts declining and the request falls back to normal
// handling.
const maxConcurrentExecutions = 64

var activeExecutions atomic.Int64

// newBypassToken returns a per-execution marker value. The value only needs to
// be non-empty; it is random so it can be correlated in host logs.
func newBypassToken() string {
	buf := make([]byte, 16)
	if _, errRead := rand.Read(buf); errRead != nil {
		return "anti-model-fallback"
	}
	return hex.EncodeToString(buf)
}

// carriesBypass reports whether headers carry a replay marker from this plugin.
func carriesBypass(headers http.Header) bool {
	if headers == nil {
		return false
	}
	return strings.TrimSpace(headers.Get(bypassHeader)) != ""
}

func routeModel(raw []byte) ([]byte, error) {
	var req rpcModelRouteRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	decline := pluginapi.ModelRouteResponse{Handled: false}

	cfg := loadedConfig()
	if !cfg.Enabled {
		return okEnvelope(decline)
	}
	// Never claim a request this plugin replayed itself.
	if carriesBypass(req.Headers) {
		return okEnvelope(decline)
	}
	if activeExecutions.Load() >= maxConcurrentExecutions {
		return okEnvelope(decline)
	}
	if _, protected := cfg.findProtectedModel(req.RequestedModel); !protected {
		return okEnvelope(decline)
	}
	return okEnvelope(pluginapi.ModelRouteResponse{
		Handled:    true,
		TargetKind: pluginapi.ModelRouteTargetSelf,
		Reason:     "anti_model_fallback_guarded",
	})
}
