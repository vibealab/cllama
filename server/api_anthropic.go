package server

// Anthropic-format LLM API endpoints under /api/anthropic.
//
// These mimic the Anthropic Messages API so Anthropic-native clients — most
// notably Claude Code — can consume *any* cllama backend regardless of the
// backend's upstream type. Point Claude Code at a cllama server with:
//
//	ANTHROPIC_BASE_URL=http://<cllama>/api/anthropic
//
// Requests arrive in Anthropic format, are translated into the internal
// OpenAI-format pipeline (execChat / execChatStream), and the responses —
// including the SSE stream — are translated back.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"cllama/client"
)

// ── POST /api/anthropic/v1/messages ──────────────────────────────────────────

// handleAnthropicMessages serves the Anthropic Messages API.
func (s *Server) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondAnthropicErrorObj(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		respondAnthropicErrorObj(w, http.StatusBadRequest, "invalid_request_error", "failed to read body")
		return
	}
	defer r.Body.Close()

	var ar client.AnthropicRequest
	if err := json.Unmarshal(body, &ar); err != nil {
		respondAnthropicErrorObj(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON: "+err.Error())
		return
	}
	if ar.Model == "" {
		respondAnthropicErrorObj(w, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	mockModel := ar.Model

	chatReq, err := client.AnthropicRequestToChat(&ar)
	if err != nil {
		respondAnthropicErrorObj(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	chatReq.Stream = ar.Stream

	if ar.Stream {
		s.streamAnthropicChat(w, r, chatReq, mockModel)
		return
	}

	resp, err := s.execChat(r.Context(), chatReq)
	if err != nil {
		respondAnthropicError(w, mockModel, err)
		return
	}
	// Echo the requested (mock) model, mirroring Anthropic's behavior.
	anthResp := client.ChatResponseToAnthropic(resp, mockModel)
	if resp.ID != "" {
		anthResp.ID = resp.ID
	}
	respondJSON(w, http.StatusOK, anthResp)
}

// ── POST /api/anthropic/v1/messages/count_tokens ─────────────────────────────

// handleAnthropicCountTokens answers Claude Code's token-count preflight
// with a cheap size-based estimate; Claude Code only uses it for
// auto-compaction heuristics.
func (s *Server) handleAnthropicCountTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondAnthropicErrorObj(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		respondAnthropicErrorObj(w, http.StatusBadRequest, "invalid_request_error", "failed to read body")
		return
	}
	estimate := len(body) / 4
	if estimate < 1 {
		estimate = 1
	}
	respondJSON(w, http.StatusOK, map[string]int{"input_tokens": estimate})
}

// ── Errors ───────────────────────────────────────────────────────────────────

// respondAnthropicError maps routing failures onto Anthropic error envelopes.
// Queue pressure answers 529 (overloaded_error) so Anthropic clients apply
// their built-in retry/backoff instead of treating it as fatal.
func respondAnthropicError(w http.ResponseWriter, model string, err error) {
	switch {
	case errors.Is(err, ErrQueueFull):
		log.Printf("[http] 529 no backend for model %q and queue full", model)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]interface{}{ //nolint:errcheck
			"type": "error",
			"error": map[string]string{
				"type":    "overloaded_error",
				"message": fmt.Sprintf("no backend registered for model %q and request queue is full", model),
			},
		})
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// client went away; nothing to reply
		log.Printf("[http] anthropic request for model %q cancelled before routing: %v", model, err)
	default:
		log.Printf("[http] 500 model %q: %v", model, err)
		respondAnthropicErrorObj(w, http.StatusInternalServerError, "api_error", err.Error())
	}
}

// respondAnthropicErrorObj writes a literal Anthropic error envelope.
func respondAnthropicErrorObj(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{ //nolint:errcheck
		"type": "error",
		"error": map[string]string{
			"type":    errType,
			"message": message,
		},
	})
}

// ── Streaming ────────────────────────────────────────────────────────────────

// streamAnthropicChat runs the internal OpenAI-format stream and re-emits it
// as Anthropic SSE events.
func (s *Server) streamAnthropicChat(w http.ResponseWriter, r *http.Request, req *client.ChatRequest, mockModel string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		respondAnthropicErrorObj(w, http.StatusInternalServerError, "api_error", "streaming not supported")
		return
	}

	enc := &anthropicEncoder{w: w, flusher: flusher, model: mockModel, msgID: fmt.Sprintf("msg_%d", time.Now().UnixNano())}

	err := s.execChatStream(r.Context(), req, enc.handleEvent)
	if err == nil {
		return
	}
	if !enc.started {
		respondAnthropicError(w, mockModel, err)
		return
	}
	log.Printf("[http] anthropic stream model %q aborted after headers were sent: %v", mockModel, err)
	enc.send("error", map[string]interface{}{
		"type":  "error",
		"error": map[string]string{"type": "api_error", "message": err.Error()},
	})
}

// anthropicEncoder is a state machine translating internal OpenAI-format SSE
// events into Anthropic Messages SSE events. Only one content block is open
// at a time (text, thinking, or one tool_use); deltas switch blocks by
// closing and reopening.
type anthropicEncoder struct {
	w       http.ResponseWriter
	flusher http.Flusher
	model   string
	msgID   string
	started bool

	nextIndex int
	openIdx   int    // index of the open block, -1 when none
	openKind  string // "text" | "thinking" | "tool_use" | ""
	openTool  string // tool_use id of the open tool block

	toolBlocks map[string]int // tool call id → block index
	lastToolID string

	stopReason string
	usage      client.AnthropicUsage
}

func (e *anthropicEncoder) handleEvent(evt string) bool {
	data := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(evt), "data:"))
	if data == "" {
		return true
	}
	if !e.started {
		e.start()
	}
	if data == "[DONE]" {
		e.finish()
		return true
	}

	var chunk client.StreamingChatResponse
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return true // skip unparseable events
	}
	if chunk.Usage.PromptTokens > 0 {
		e.usage.InputTokens = chunk.Usage.PromptTokens
	}
	if chunk.Usage.CompletionTokens > 0 {
		e.usage.OutputTokens = chunk.Usage.CompletionTokens
	}

	for _, choice := range chunk.Choices {
		if !e.delta(choice.Delta) {
			return false
		}
		if choice.FinishReason != "" {
			e.stopReason = client.AnthropicStopReason(choice.FinishReason)
		}
	}
	return true
}

// start emits message_start (and headers) once.
func (e *anthropicEncoder) start() {
	e.started = true
	e.w.Header().Set("Content-Type", "text/event-stream")
	e.w.Header().Set("Cache-Control", "no-cache")
	e.w.Header().Set("Connection", "keep-alive")
	e.w.WriteHeader(http.StatusOK)
	e.openIdx = -1

	e.send("message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":      e.msgID,
			"type":    "message",
			"role":    "assistant",
			"model":   e.model,
			"content": []interface{}{},
			"usage":   map[string]int{"input_tokens": 0, "output_tokens": 0},
		},
	})
}

// delta emits the text/thinking/tool-call pieces of one streaming delta.
func (e *anthropicEncoder) delta(d client.ChatMessage) bool {
	if th := d.Reasoning(); th != "" {
		if !e.ensure("thinking", "", "") {
			return false
		}
		if !e.send("content_block_delta", map[string]interface{}{
			"type":  "content_block_delta",
			"index": e.openIdx,
			"delta": map[string]string{"type": "thinking_delta", "thinking": th},
		}) {
			return false
		}
	}
	if txt := chatMessageText(d); txt != "" {
		if !e.ensure("text", "", "") {
			return false
		}
		if !e.send("content_block_delta", map[string]interface{}{
			"type":  "content_block_delta",
			"index": e.openIdx,
			"delta": map[string]string{"type": "text_delta", "text": txt},
		}) {
			return false
		}
	}
	for _, tc := range d.ToolCalls {
		if !e.toolCall(tc) {
			return false
		}
	}
	return true
}

// toolCall opens (when new) and streams one tool call.
func (e *anthropicEncoder) toolCall(tc client.ToolCall) bool {
	id := tc.ID
	if id == "" {
		// OpenAI-style argument-only deltas: attribute them to the last
		// tool call seen.
		id = e.lastToolID
	}
	if id == "" {
		id = fmt.Sprintf("toolu_%d", time.Now().UnixNano())
	}
	if !e.ensure("tool_use", id, tc.Function.Name) {
		return false
	}

	if tc.Function.Arguments != "" {
		idx, ok := e.toolBlocks[id]
		if !ok {
			return true
		}
		return e.send("content_block_delta", map[string]interface{}{
			"type":  "content_block_delta",
			"index": idx,
			"delta": map[string]string{"type": "input_json_delta", "partial_json": tc.Function.Arguments},
		})
	}
	return true
}

// ensure makes a block of the given kind the open one, closing the previous
// block and emitting content_block_start as needed.
func (e *anthropicEncoder) ensure(kind, toolID, toolName string) bool {
	if e.openKind == kind && (kind != "tool_use" || e.openTool == toolID) {
		return true
	}
	e.closeBlock()

	idx := e.nextIndex
	e.nextIndex++
	e.openIdx = idx
	e.openKind = kind

	block := map[string]interface{}{"type": kind}
	switch kind {
	case "tool_use":
		if toolID == "" {
			toolID = fmt.Sprintf("toolu_%d", time.Now().UnixNano())
		}
		if e.toolBlocks == nil {
			e.toolBlocks = map[string]int{}
		}
		e.toolBlocks[toolID] = idx
		e.openTool = toolID
		e.lastToolID = toolID
		block["id"] = toolID
		block["name"] = toolName
		block["input"] = map[string]interface{}{}
	default:
		e.openTool = ""
	}

	return e.send("content_block_start", map[string]interface{}{
		"type":          "content_block_start",
		"index":         idx,
		"content_block": block,
	})
}

// closeBlock ends the currently open content block.
func (e *anthropicEncoder) closeBlock() {
	if e.openKind == "" {
		return
	}
	e.send("content_block_stop", map[string]interface{}{ //nolint:errcheck
		"type":  "content_block_stop",
		"index": e.openIdx,
	})
	e.openKind = ""
	e.openTool = ""
	e.openIdx = -1
}

// finish closes the stream: message_delta with the stop reason and usage,
// then message_stop.
func (e *anthropicEncoder) finish() {
	if e.openKind == "" && e.nextIndex == 0 {
		// No content ever arrived; still emit one empty text block so the
		// message shape matches Anthropic's.
		e.ensure("text", "", "") //nolint:errcheck
	}
	e.closeBlock()

	stop := e.stopReason
	if stop == "" {
		stop = "end_turn"
	}
	e.send("message_delta", map[string]interface{}{ //nolint:errcheck
		"type": "message_delta",
		"delta": map[string]interface{}{
			"stop_reason":   stop,
			"stop_sequence": nil,
		},
		"usage": map[string]int{"output_tokens": e.usage.OutputTokens},
	})
	e.send("message_stop", map[string]interface{}{"type": "message_stop"}) //nolint:errcheck
}

// send writes one SSE event and flushes; false on write failure (client gone).
func (e *anthropicEncoder) send(event string, payload interface{}) bool {
	b, err := json.Marshal(payload)
	if err != nil {
		return true // skip malformed event, keep streaming
	}
	if _, err := fmt.Fprintf(e.w, "event: %s\ndata: %s\n\n", event, b); err != nil {
		return false
	}
	e.flusher.Flush()
	return true
}
