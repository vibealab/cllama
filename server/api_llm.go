package server

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

// This file handles LLM API requests under /api (Ollama style) and /v1
// (OpenAI style). Requests are routed to backends bound to the requested
// mock model, using round-robin load balancing. The model name in the
// forwarded request is rewritten to the backend's actual upstream model.
//
// The exec* functions below are the transport-agnostic core of the proxy;
// they are used both by the HTTP handlers here and by the child-side parent
// tunnel (parent.go), which serves requests pushed by a parent cllama.

// ── Core pipeline ────────────────────────────────────────────────────────────

// errClientGone aborts a stream whose consumer (downstream client that
// clicked stop, or a parent tunnel that went away) disconnected mid-stream.
// Failing the queued request with it cancels the request's call context, so
// the upstream connection is closed and the backend stops generating instead
// of running to completion for a vanished client.
var errClientGone = errors.New("cancelled: client disconnected")

// execChat routes and executes a non-streaming chat request.
// On success req.Model is rewritten to the upstream model name.
func (s *Server) execChat(ctx context.Context, req *client.ChatRequest) (*client.ChatResponse, error) {
	mockModel := req.Model
	be, upstreamModel, rq, err := s.router.Acquire(ctx, mockModel, req)
	if errors.Is(err, ErrManualResolved) {
		// An admin answered this request by hand (web UI takeaway flow):
		// return the authored answer to the client instead of a backend.
		resp := manualChatResponse(mockModel, rq.ManualAnswer())
		rq.Done()
		return resp, nil
	}
	if err != nil {
		log.Printf("[route] chat %q: no backend: %v", mockModel, err)
		return nil, err
	}
	log.Printf("[route] chat %q -> backend %s (%s %s) as upstream model %q stream=false",
		mockModel, be.ID, be.Type, be.Endpoint, upstreamModel)
	req.Model = upstreamModel
	// Use the handle's call context so removing the request from the queue
	// (admin UI / API) aborts the upstream generation and ends this response.
	ctx = rq.Context()

	rq.MarkProcessing()
	resp, err := be.client.Chat(ctx, req)
	if err != nil {
		log.Printf("[route] chat %q backend %s (%s %s) failed: %v", mockModel, be.ID, be.Type, be.Endpoint, err)
		rq.Fail(err)
		return resp, err
	}
	rq.Done()
	return resp, err
}

// execChatStream routes and executes a streaming chat request. emit receives
// complete OpenAI SSE events ("data: {...}\n\n"); returning false from emit
// aborts the stream, fails the queued request with errClientGone and cancels
// the upstream generation.
func (s *Server) execChatStream(ctx context.Context, req *client.ChatRequest, emit func(event string) bool) error {
	mockModel := req.Model
	be, upstreamModel, rq, err := s.router.Acquire(ctx, mockModel, req)
	if errors.Is(err, ErrManualResolved) {
		// Hand-written admin answer: relay it to the client as a minimal
		// OpenAI-format SSE stream, exactly like a backend would.
		if !emitManualAnswerStream(mockModel, rq.ManualAnswer(), emit) {
			rq.Fail(errClientGone)
			return errClientGone
		}
		rq.Done()
		return nil
	}
	if err != nil {
		log.Printf("[route] chat stream %q: no backend: %v", mockModel, err)
		return err
	}
	log.Printf("[route] chat %q -> backend %s (%s %s) as upstream model %q stream=true",
		mockModel, be.ID, be.Type, be.Endpoint, upstreamModel)
	req.Model = upstreamModel
	// Use the handle's call context so removing the request from the queue
	// (admin UI / API) aborts the upstream generation and ends this stream.
	ctx = rq.Context()

	rq.MarkProcessing()
	upstream, err := be.client.ChatStream(ctx, req)
	if err != nil {
		log.Printf("[route] chat stream %q backend %s (%s %s) failed: %v", mockModel, be.ID, be.Type, be.Endpoint, err)
		rq.Fail(err)
		return err
	}
	defer upstream.Close()

	ch := make(chan string, 16)
	sseErr := make(chan error, 1)
	go func() { sseErr <- be.client.StreamToSSE(ctx, upstream, req.Model, ch) }()

	for {
		select {
		case evt, ok := <-ch:
			if !ok {
				// StreamToSSE closes ch only after its result is queued, so
				// this receive never blocks. A truncated upstream stream is
				// an error, not a silent 200.
				if err := <-sseErr; err != nil {
					rq.Fail(err)
					return err
				}
				rq.Done()
				return nil
			}
			if !emit(evt) {
				// The consumer vanished (client clicked stop). Fail the queued
				// request: this cancels its call context, and the deferred
				// upstream.Close tears down the backend connection, which makes
				// the upstream server abort generation right away.
				log.Printf("[route] chat stream %q backend %s: client disconnected, aborting upstream generation", mockModel, be.ID)
				rq.Fail(errClientGone)
				return errClientGone
			}
		case <-ctx.Done():
			log.Printf("[route] chat stream %q backend %s aborted: %v", mockModel, be.ID, ctx.Err())
			rq.Fail(fmt.Errorf("request cancelled: %w", ctx.Err()))
			return ctx.Err()
		}
	}
}

// execEmbedding routes and executes an embedding request.
func (s *Server) execEmbedding(ctx context.Context, model string, input interface{}) ([][]float64, error) {
	be, upstreamModel, rq, err := s.router.Acquire(ctx, model, nil)
	if errors.Is(err, ErrManualResolved) {
		// A text answer cannot satisfy an embedding request; surface the
		// situation to the client and mark the queue entry as failed.
		manErr := fmt.Errorf("request was manually answered by an admin; manual answers are not supported for embedding requests")
		log.Printf("[route] embed %q: %v", model, manErr)
		rq.Fail(manErr)
		return nil, manErr
	}
	if err != nil {
		log.Printf("[route] embed %q: no backend: %v", model, err)
		return nil, err
	}
	log.Printf("[route] embed %q -> backend %s (%s %s) as upstream model %q",
		model, be.ID, be.Type, be.Endpoint, upstreamModel)
	// Use the handle's call context so removing the request from the queue
	// (admin UI / API) aborts the upstream call.
	ctx = rq.Context()

	rq.MarkProcessing()
	embeddings, err := be.client.Embeddings(ctx, upstreamModel, input)
	if err != nil {
		log.Printf("[route] embed %q backend %s (%s %s) failed: %v", model, be.ID, be.Type, be.Endpoint, err)
		rq.Fail(err)
		return embeddings, err
	}
	rq.Done()
	return embeddings, err
}

// ── Chat ─────────────────────────────────────────────────────────────────────

// handleChat proxies chat requests (with or without streaming, tool use and
// image content parts supported).
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to read body"})
		return
	}
	defer r.Body.Close()

	var req client.ChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if req.Model == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "model is required"})
		return
	}
	mockModel := req.Model
	ollamaStyle := isOllamaRoute(r.URL.Path)

	if req.Stream {
		s.streamChatResponse(w, r, &req, mockModel, ollamaStyle)
		return
	}

	resp, err := s.execChat(r.Context(), &req)
	if err != nil {
		respondRouteError(w, mockModel, err)
		return
	}
	if ollamaStyle {
		// /api/chat callers (the ollama CLI) cannot parse OpenAI-format
		// responses: they would read an empty message and later resend it
		// with a blank role, breaking chat templates upstream.
		respondJSON(w, http.StatusOK, toOllamaChatResponse(resp, mockModel))
		return
	}
	respondJSON(w, http.StatusOK, resp)
}

func (s *Server) streamChatResponse(w http.ResponseWriter, r *http.Request, req *client.ChatRequest, mockModel string, ollamaStyle bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming not supported"})
		return
	}

	headerSent := false
	var usage client.ChatUsage

	// emit relays one internal OpenAI-format SSE event to the client in the
	// wire format the endpoint speaks.
	emit := func(evt string) bool {
		if !headerSent {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			w.WriteHeader(http.StatusOK)
			headerSent = true
		}
		// A failed write means the client went away (e.g. Zed's stop button).
		// While a handler streams, Go's HTTP/1 server surfaces disconnects only
		// through write errors, so this check is what aborts the relay; returning
		// false cancels the upstream generation via execChatStream.
		if _, err := fmt.Fprint(w, evt); err != nil {
			log.Printf("[http] chat stream model %q client write failed: %v", mockModel, err)
			return false
		}
		flusher.Flush()
		return true
	}
	if ollamaStyle {
		emit = func(evt string) bool {
			data := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(evt), "data:"))
			if data == "" {
				return true
			}
			if data == "[DONE]" {
				return s.writeOllamaLine(w, flusher, &headerSent, ollamaChatResponse{
					Model:           mockModel,
					CreatedAt:       ollamaNow(),
					Message:         ollamaChatMessage{Role: "assistant"},
					Done:            true,
					DoneReason:      "stop",
					PromptEvalCount: usage.PromptTokens,
					EvalCount:       usage.CompletionTokens,
				})
			}

			var chunk client.StreamingChatResponse
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				return true // skip unparseable events
			}
			if chunk.Usage.PromptTokens > 0 || chunk.Usage.CompletionTokens > 0 {
				usage = chunk.Usage
			}
			msg := ollamaChatMessage{Role: "assistant"}
			hasContent := false
			if len(chunk.Choices) > 0 {
				if t := chatMessageText(chunk.Choices[0].Delta); t != "" {
					msg.Content = t
					hasContent = true
				}
				if th := chunk.Choices[0].Delta.Reasoning(); th != "" {
					msg.Thinking = th
					hasContent = true
				}
				if tcs := toOllamaToolCalls(chunk.Choices[0].Delta.ToolCalls); len(tcs) > 0 {
					msg.ToolCalls = tcs
					hasContent = true
				}
			}
			if !hasContent {
				return true // role/finish-only chunk; ollama sends none
			}
			return s.writeOllamaLine(w, flusher, &headerSent, ollamaChatResponse{
				Model:     mockModel,
				CreatedAt: ollamaNow(),
				Message:   msg,
			})
		}
	}

	err := s.execChatStream(r.Context(), req, emit)
	switch {
	case err != nil && !headerSent:
		respondRouteError(w, mockModel, err)
	case errors.Is(err, errClientGone):
		// The client is already gone; there is nobody left to read an error.
		log.Printf("[http] chat stream model %q cancelled: client disconnected", mockModel)
	case err != nil:
		log.Printf("[http] chat stream model %q aborted after headers were sent: %v", mockModel, err)
		if ollamaStyle {
			// ollama clients surface an {"error": ...} NDJSON line to the caller.
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			flusher.Flush()
		}
	}
}

// ── Manual answers (admin takeaway flow) ────────────────────────────────────

// manualChatResponse wraps a hand-written admin answer in an OpenAI-format
// non-streaming chat response; the route handlers translate it to whatever
// wire format the client speaks.
func manualChatResponse(model, content string) *client.ChatResponse {
	return &client.ChatResponse{
		ID:      "chatcmpl-cllama-manual",
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []client.ChatChoice{{
			Index:        0,
			Message:      client.ChatMessage{Role: "assistant", Content: content},
			FinishReason: "stop",
		}},
	}
}

// emitManualAnswerStream relays a hand-written answer as OpenAI-format SSE
// events (content chunk, finish chunk, [DONE]) through emit, mirroring what
// a real backend stream produces. It reports false when the consumer
// vanished (emit returned false), which the caller fails as errClientGone.
func emitManualAnswerStream(model, content string, emit func(event string) bool) bool {
	now := time.Now().Unix()
	base := client.StreamingChatResponse{
		ID:      "chatcmpl-cllama-manual",
		Object:  "chat.completion.chunk",
		Created: now,
		Model:   model,
	}
	contentChunk := base
	contentChunk.Choices = []client.StreamingChatChoice{{
		Index: 0,
		Delta: client.ChatMessage{Role: "assistant", Content: content},
	}}
	finishChunk := base
	finishChunk.Choices = []client.StreamingChatChoice{{Index: 0, FinishReason: "stop"}}

	events := make([]string, 0, 3)
	for _, chunk := range []client.StreamingChatResponse{contentChunk, finishChunk} {
		b, err := json.Marshal(chunk)
		if err != nil {
			return false // unreachable for these types
		}
		events = append(events, "data: "+string(b)+"\n\n")
	}
	events = append(events, "data: [DONE]\n\n")
	for _, evt := range events {
		if !emit(evt) {
			return false
		}
	}
	return true
}

// isOllamaRoute reports whether path speaks ollama's wire format. Note that
// /api/openai/* endpoints are OpenAI-format despite living under /api/.
func isOllamaRoute(path string) bool {
	return (strings.HasPrefix(path, "/api/") &&
		!strings.HasPrefix(path, "/api/openai/") &&
		!strings.HasPrefix(path, "/api/anthropic/"))
}

// ── Ollama response wire types (for /api/chat) ──────────────────────────────

type ollamaChatResponse struct {
	Model           string            `json:"model"`
	CreatedAt       string            `json:"created_at"`
	Message         ollamaChatMessage `json:"message"`
	Done            bool              `json:"done"`
	DoneReason      string            `json:"done_reason,omitempty"`
	PromptEvalCount int               `json:"prompt_eval_count,omitempty"`
	EvalCount       int               `json:"eval_count,omitempty"`
}

type ollamaChatMessage struct {
	Role      string               `json:"role"`
	Content   string               `json:"content"`
	Thinking  string               `json:"thinking,omitempty"`
	ToolCalls []ollamaToolCallWire `json:"tool_calls,omitempty"`
}

// ollamaToolCallWire mirrors ollama's tool call, where arguments is a JSON
// object (OpenAI carries the same data as a JSON string).
type ollamaToolCallWire struct {
	Function ollamaFunctionWire `json:"function"`
}

type ollamaFunctionWire struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func ollamaNow() string { return time.Now().UTC().Format(time.RFC3339) }

// toOllamaChatResponse converts an internal OpenAI-format response to the
// ollama /api/chat wire format. model is the requested (mock) model name.
func toOllamaChatResponse(resp *client.ChatResponse, model string) ollamaChatResponse {
	out := ollamaChatResponse{
		Model:           model,
		CreatedAt:       ollamaNow(),
		Message:         ollamaChatMessage{Role: "assistant"},
		Done:            true,
		DoneReason:      "stop",
		PromptEvalCount: resp.Usage.PromptTokens,
		EvalCount:       resp.Usage.CompletionTokens,
	}
	if len(resp.Choices) > 0 {
		m := resp.Choices[0].Message
		if m.Role != "" {
			out.Message.Role = m.Role
		}
		out.Message.Content = chatMessageText(m)
		out.Message.Thinking = m.Reasoning()
		out.Message.ToolCalls = toOllamaToolCalls(m.ToolCalls)
	}
	return out
}

func toOllamaToolCalls(tcs []client.ToolCall) []ollamaToolCallWire {
	if len(tcs) == 0 {
		return nil
	}
	out := make([]ollamaToolCallWire, len(tcs))
	for i, tc := range tcs {
		args := json.RawMessage(tc.Function.Arguments)
		if !json.Valid(args) {
			args = json.RawMessage("{}")
		}
		out[i] = ollamaToolCallWire{
			Function: ollamaFunctionWire{Name: tc.Function.Name, Arguments: args},
		}
	}
	return out
}

// writeOllamaLine emits one NDJSON line of an ollama streaming response.
func (s *Server) writeOllamaLine(w http.ResponseWriter, flusher http.Flusher, headerSent *bool, payload ollamaChatResponse) bool {
	if !*headerSent {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		*headerSent = true
	}
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		log.Printf("[http] ollama stream write failed: %v", err)
		return false
	}
	flusher.Flush()
	return true
}

// ── Embedding ────────────────────────────────────────────────────────────────

// handleEmbedding proxies text-embedding requests.
func (s *Server) handleEmbedding(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to read body"})
		return
	}
	defer r.Body.Close()

	var raw struct {
		Model  string      `json:"model"`
		Input  interface{} `json:"input"`
		Prompt string      `json:"prompt"` // legacy /api/embeddings field
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if raw.Input == nil && raw.Prompt != "" {
		raw.Input = raw.Prompt
	}
	if raw.Model == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "model is required"})
		return
	}
	if raw.Input == nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "input is required"})
		return
	}

	embeddings, err := s.execEmbedding(r.Context(), raw.Model, raw.Input)
	if err != nil {
		respondRouteError(w, raw.Model, err)
		return
	}

	// Ollama-style response for /api/embed*, OpenAI-style for /api/openai/*.
	if isOllamaRoute(r.URL.Path) {
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"model":      raw.Model,
			"embeddings": embeddings,
		})
		return
	}

	data := make([]client.EmbeddingData, len(embeddings))
	for i, e := range embeddings {
		data[i] = client.EmbeddingData{Index: i, Object: "embedding", Embedding: e}
	}
	respondJSON(w, http.StatusOK, client.EmbeddingResponse{
		ID:      fmt.Sprintf("embed-%d", time.Now().UnixNano()),
		Object:  "list",
		Created: time.Now().Unix(),
		Model:   raw.Model,
		Data:    data,
	})
}

// ── Helpers ──────────────────────────────────────────────────────────────────

// respondRouteError maps routing errors to HTTP responses.
func respondRouteError(w http.ResponseWriter, model string, err error) {
	switch {
	case errors.Is(err, ErrQueueFull):
		log.Printf("[http] 503 no backend for model %q and queue full", model)
		respondJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": fmt.Sprintf("no backend registered for model %q and request queue is full", model),
		})
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// client went away while queued; nothing to reply
		log.Printf("[http] request for model %q cancelled before routing: %v", model, err)
	default:
		log.Printf("[http] 502 model %q: %v", model, err)
		respondJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
	}
}
