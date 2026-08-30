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

// execChat routes and executes a non-streaming chat request.
// On success req.Model is rewritten to the upstream model name.
func (s *Server) execChat(ctx context.Context, req *client.ChatRequest) (*client.ChatResponse, error) {
	mockModel := req.Model
	be, upstreamModel, rq, err := s.router.Acquire(ctx, mockModel)
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
// aborts the stream.
func (s *Server) execChatStream(ctx context.Context, req *client.ChatRequest, emit func(event string) bool) error {
	mockModel := req.Model
	be, upstreamModel, rq, err := s.router.Acquire(ctx, mockModel)
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
				rq.Done()
				return nil
			}
		case <-ctx.Done():
			log.Printf("[route] chat stream %q backend %s aborted: %v", mockModel, be.ID, ctx.Err())
			rq.Fail(ctx.Err())
			return ctx.Err()
		}
	}
}

// execEmbedding routes and executes an embedding request.
func (s *Server) execEmbedding(ctx context.Context, model string, input interface{}) ([][]float64, error) {
	be, upstreamModel, rq, err := s.router.Acquire(ctx, model)
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
		fmt.Fprint(w, evt)
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
	case err != nil:
		log.Printf("[http] chat stream model %q aborted after headers were sent: %v", mockModel, err)
		if ollamaStyle {
			// ollama clients surface an {"error": ...} NDJSON line to the caller.
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			flusher.Flush()
		}
	}
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
