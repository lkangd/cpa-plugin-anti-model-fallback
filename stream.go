package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Probing limits. The processing model arrives in the very first SSE event, so
// these ceilings only exist to stop the plugin buffering a whole response when
// a protocol does not report a model at all.
const (
	maxProbeChunks = 32
	maxProbeBytes  = 256 * 1024
)

type rpcStreamEmitRequest struct {
	StreamID string `json:"stream_id"`
	Payload  []byte `json:"payload,omitempty"`
	Error    string `json:"error,omitempty"`
}

type rpcStreamCloseRequest struct {
	StreamID string `json:"stream_id"`
	Error    string `json:"error,omitempty"`
}

func emitPluginStreamChunk(streamID string, payload []byte) error {
	if strings.TrimSpace(streamID) == "" {
		return fmt.Errorf("plugin stream id is required")
	}
	_, errCall := callHost(pluginabi.MethodHostStreamEmit, rpcStreamEmitRequest{
		StreamID: streamID,
		Payload:  payload,
	})
	return errCall
}

func closePluginStream(streamID, errMsg string) {
	if strings.TrimSpace(streamID) == "" {
		return
	}
	_, _ = callHost(pluginabi.MethodHostStreamClose, rpcStreamCloseRequest{
		StreamID: streamID,
		Error:    strings.TrimSpace(errMsg),
	})
}

func closeHostModelStream(streamID string) {
	if strings.TrimSpace(streamID) == "" {
		return
	}
	_, _ = callHost(pluginabi.MethodHostModelStreamClose, pluginapi.HostModelStreamCloseRequest{StreamID: streamID})
}

func executeStream(raw []byte) ([]byte, error) {
	var req rpcExecutorRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	streamID := strings.TrimSpace(req.StreamID)
	if streamID == "" {
		return errorEnvelope("executor_error", "stream_id is required for executor.execute_stream"), nil
	}

	activeExecutions.Add(1)
	go func() {
		defer activeExecutions.Add(-1)
		defer func() {
			if recovered := recover(); recovered != nil {
				closePluginStream(streamID, fmt.Sprintf("anti-model-fallback panic: %v", recovered))
			}
		}()
		if errRun := runGuardedStream(req, streamID); errRun != nil {
			closePluginStream(streamID, errRun.Error())
			return
		}
		closePluginStream(streamID, "")
	}()

	return okEnvelope(map[string]any{
		"headers": http.Header{"Content-Type": []string{"text/event-stream"}},
	})
}

// runGuardedStream retries the upstream stream until the processing model
// matches the protected model or the retry budget is exhausted. Nothing is
// forwarded downstream until an attempt has been accepted, so the client never
// observes a partial response from a substituted model.
func runGuardedStream(req rpcExecutorRequest, streamID string) error {
	cfg := loadedConfig()
	clientModel := strings.TrimSpace(req.Model)
	entry, expected, protected := resolveProtection(cfg, clientModel, clientModel)
	budget := 0
	if protected {
		budget = cfg.retryBudget(entry)
	}

	var lastActual string
	for attempt := 0; attempt <= budget; attempt++ {
		if attempt > 0 {
			retryPause(cfg)
		}
		actual, errRun := forwardOnce(req, streamID, expected, protected)
		if errRun != nil {
			if errRun == errModelFallback {
				lastActual = actual
				continue
			}
			return errRun
		}
		return nil
	}

	// Retry budget spent: surface a loud error instead of a silently wrong model.
	// The host turns this into the terminating SSE error event, so the plugin
	// must not emit one itself or the client would receive it twice.
	return fmt.Errorf("%s", fallbackMessage(expected, lastActual, budget+1))
}

// errModelFallback signals that an attempt was discarded because a different
// model served it. It is never surfaced to the client.
var errModelFallback = fmt.Errorf("model fallback detected")

// forwardOnce runs a single upstream stream attempt. It buffers chunks until the
// processing model is known; once the attempt is accepted the buffer is flushed
// and the remainder is streamed straight through.
func forwardOnce(req rpcExecutorRequest, streamID, expected string, protected bool) (string, error) {
	resp, errStart := hostModelExecuteStream(req)
	if errStart != nil {
		return "", errStart
	}
	if strings.TrimSpace(resp.StreamID) == "" {
		return "", fmt.Errorf("host model stream: empty stream_id")
	}
	defer closeHostModelStream(resp.StreamID)

	// Upstream errors are forwarded verbatim and never consume retry budget.
	if resp.StatusCode >= 400 {
		return "", forwardRemainder(resp.StreamID, streamID, nil)
	}

	var buffered [][]byte
	var bufferedBytes int
	var probe bytes.Buffer
	accepted := !protected

	for {
		chunk, errRead := readHostModelChunk(resp.StreamID)
		if errRead != nil {
			return "", errRead
		}
		if chunk.Error != "" {
			return "", fmt.Errorf("%s", chunk.Error)
		}
		if len(chunk.Payload) > 0 {
			payload := bytes.Clone(chunk.Payload)
			if accepted {
				if errEmit := emitPluginStreamChunk(streamID, payload); errEmit != nil {
					return "", errEmit
				}
			} else {
				buffered = append(buffered, payload)
				bufferedBytes += len(payload)
				probe.Write(payload)

				actual := extractProcessingModel(probe.Bytes())
				switch {
				case actual != "" && !modelMatches(expected, actual):
					// Discard the buffer; the client has seen nothing yet.
					return actual, errModelFallback
				case actual != "":
					accepted = true
				case len(buffered) >= maxProbeChunks || bufferedBytes >= maxProbeBytes:
					// No model reported in time: fail open rather than stall.
					accepted = true
				}
				if accepted {
					if errFlush := flushChunks(streamID, buffered); errFlush != nil {
						return "", errFlush
					}
					buffered = nil
				}
			}
		}
		if chunk.Done {
			break
		}
	}

	// A stream that ended before reporting a model is delivered as-is.
	if !accepted && len(buffered) > 0 {
		if errFlush := flushChunks(streamID, buffered); errFlush != nil {
			return "", errFlush
		}
	}
	return "", nil
}

// forwardRemainder streams whatever the host produced without inspection. It is
// used for upstream error responses, which must reach the client untouched.
func forwardRemainder(hostStreamID, streamID string, pending [][]byte) error {
	if errFlush := flushChunks(streamID, pending); errFlush != nil {
		return errFlush
	}
	for {
		chunk, errRead := readHostModelChunk(hostStreamID)
		if errRead != nil {
			return errRead
		}
		if chunk.Error != "" {
			return fmt.Errorf("%s", chunk.Error)
		}
		if len(chunk.Payload) > 0 {
			if errEmit := emitPluginStreamChunk(streamID, bytes.Clone(chunk.Payload)); errEmit != nil {
				return errEmit
			}
		}
		if chunk.Done {
			return nil
		}
	}
}

func flushChunks(streamID string, chunks [][]byte) error {
	for _, chunk := range chunks {
		if errEmit := emitPluginStreamChunk(streamID, chunk); errEmit != nil {
			return errEmit
		}
	}
	return nil
}

func readHostModelChunk(hostStreamID string) (pluginapi.HostModelStreamReadResponse, error) {
	raw, errCall := callHost(pluginabi.MethodHostModelStreamRead, pluginapi.HostModelStreamReadRequest{StreamID: hostStreamID})
	if errCall != nil {
		return pluginapi.HostModelStreamReadResponse{}, errCall
	}
	var chunk pluginapi.HostModelStreamReadResponse
	if errDecode := json.Unmarshal(raw, &chunk); errDecode != nil {
		return pluginapi.HostModelStreamReadResponse{}, errDecode
	}
	return chunk, nil
}

func hostModelExecuteStream(req rpcExecutorRequest) (pluginapi.HostModelStreamResponse, error) {
	headers := cloneHeaders(req.Headers)
	headers.Set(bypassHeader, newBypassToken())

	raw, errCall := callHost(pluginabi.MethodHostModelExecuteStream, hostModelExecutionRequest{
		HostModelExecutionRequest: pluginapi.HostModelExecutionRequest{
			EntryProtocol: formatOrDefault(req.SourceFormat, req.Format),
			ExitProtocol:  formatOrDefault(req.Format, req.SourceFormat),
			Model:         strings.TrimSpace(req.Model),
			Stream:        true,
			Body:          requestBody(req.ExecutorRequest),
			Headers:       headers,
			Query:         req.Query,
			Alt:           req.Alt,
		},
		HostCallbackID: req.HostCallbackID,
	})
	if errCall != nil {
		return pluginapi.HostModelStreamResponse{}, errCall
	}
	var resp pluginapi.HostModelStreamResponse
	if errDecode := json.Unmarshal(raw, &resp); errDecode != nil {
		return pluginapi.HostModelStreamResponse{}, errDecode
	}
	return resp, nil
}
