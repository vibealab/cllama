package server

// Faked Ollama-native endpoints beyond the proxy core, so the ollama CLI can
// complete its preflight and reach /api/chat. `ollama run <model>` calls
// POST /api/show and POST /api/pull before chatting; without these the CLI
// aborts with a 404 long before any request is routed to a backend.
//
// All responses are static; the requested model is only validated against
// this server's -name model list.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"cllama/client"
)

// fakeOllamaVersion is reported by GET /api/version. Recent CLIs gate some
// features on the reported server version, so it should look current.
const fakeOllamaVersion = "0.12.0"

// handleVersionOllama serves GET /api/version.
func (s *Server) handleVersionOllama(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"version": fakeOllamaVersion})
}

// handleShowOllama fakes POST /api/show: reports proxy metadata for a known
// model so the CLI believes the model is available locally.
func (s *Server) handleShowOllama(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	// The CLI sends {"name": ...}; accept {"model": ...} too for API users.
	var body struct {
		Model string `json:"model"`
		Name  string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		log.Printf("[ollama] /api/show rejected: invalid JSON from %s", r.RemoteAddr)
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	model := body.Model
	if model == "" {
		model = body.Name
	}
	if model == "" {
		log.Printf("[ollama] /api/show rejected: missing model from %s", r.RemoteAddr)
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "model is required"})
		return
	}
	if !s.router.KnownModel(model) {
		log.Printf("[ollama] /api/show: unknown model %q from %s", model, r.RemoteAddr)
		respondJSON(w, http.StatusNotFound, map[string]string{"error": fmt.Sprintf("model %q not found", model)})
		return
	}

	caps := s.router.CapabilitiesForModel(model)
	log.Printf("[ollama] /api/show %q: served fake model metadata capabilities=%v", model, caps)
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"model": model,
		// Shapes must mirror ollama's api.ShowResponse: parameters/template
		// are strings (KV text format), details is a ModelDetails object.
		// Capabilities matter: the CLI consults them before enabling
		// streaming, thinking, vision, tools, or embedding for a model.
		"parameters": "",
		"template":   "",
		"details": map[string]interface{}{
			"parent_model":       "",
			"format":             "cllama",
			"family":             "cllama",
			"families":           []string{"cllama"},
			"parameter_size":     "0B",
			"quantization_level": "Q0_0",
		},
		"model_info": map[string]interface{}{
			"general.architecture":   "cllama",
			"general.name":           model,
			"general.family":         "cllama",
			"general.context_length": 131072,
			"general.embedding_length": 4096,
			"general.file_type":      0,
			"general.quantization_version": 1,
		},
		"capabilities": caps,
		"modified_at":  s.startedAt.UTC().Format(time.RFC3339),
	})
}

// handlePullOllama fakes POST /api/pull: known models "pull" instantly with a
// success status so the CLI continues straight to /api/chat.
func (s *Server) handlePullOllama(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	var body struct {
		Model  string `json:"model"`
		Name   string `json:"name"`
		Stream *bool  `json:"stream"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		log.Printf("[ollama] /api/pull rejected: invalid JSON from %s", r.RemoteAddr)
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	model := body.Model
	if model == "" {
		model = body.Name
	}
	if model == "" {
		log.Printf("[ollama] /api/pull rejected: missing model from %s", r.RemoteAddr)
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "model is required"})
		return
	}
	if !s.router.KnownModel(model) {
		log.Printf("[ollama] /api/pull: unknown model %q from %s", model, r.RemoteAddr)
		respondJSON(w, http.StatusNotFound, map[string]string{"error": fmt.Sprintf("model %q not found", model)})
		return
	}

	log.Printf("[ollama] /api/pull %q: model is served by this proxy, nothing to pull", model)

	stream := true // ollama's pull endpoint streams unless stream=false
	if body.Stream != nil {
		stream = *body.Stream
	}
	if !stream {
		respondJSON(w, http.StatusOK, map[string]string{"status": "success"})
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	enc.Encode(map[string]string{"status": "cllama proxy: model is served directly, nothing to pull"})
	enc.Encode(map[string]string{"status": "success"})
}

// ── /api/generate ───────────────────────────────────────────────────────────────

// ollamaGenerateResponse is one NDJSON line of ollama's /api/generate
// response (also the single-shot body when stream=false).
type ollamaGenerateResponse struct {
	Model              string `json:"model"`
	CreatedAt          string `json:"created_at"`
	Response           string `json:"response"`
	Thinking           string `json:"thinking,omitempty"`
	Done               bool   `json:"done"`
	DoneReason         string `json:"done_reason,omitempty"`
	PromptEvalCount    int    `json:"prompt_eval_count,omitempty"`
	PromptEvalDuration int64  `json:"prompt_eval_duration,omitempty"`
	EvalCount          int    `json:"eval_count,omitempty"`
	EvalDuration       int64  `json:"eval_duration,omitempty"`
	TotalDuration      int64  `json:"total_duration,omitempty"`
}

// handleGenerateOllama serves ollama's native POST /api/generate by mapping
// prompt/system/images onto the chat pipeline. The `context` field (raw
// token carry-over between turns) is not supported; use /api/chat for
// stateful multi-turn conversations.
func (s *Server) handleGenerateOllama(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	var raw struct {
		Model    string                 `json:"model"`
		Prompt   string                 `json:"prompt"`
		System   string                 `json:"system"`
		Images   []string               `json:"images"`
		Stream   *bool                  `json:"stream"`
		Think    interface{}            `json:"think"`
		Options  map[string]interface{} `json:"options"`
		Messages []client.ChatMessage   `json:"messages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		log.Printf("[ollama] /api/generate rejected: invalid JSON from %s", r.RemoteAddr)
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if raw.Model == "" {
		log.Printf("[ollama] /api/generate rejected: missing model from %s", r.RemoteAddr)
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "model is required"})
		return
	}
	if !s.router.KnownModel(raw.Model) {
		log.Printf("[ollama] /api/generate: unknown model %q from %s", raw.Model, r.RemoteAddr)
		respondJSON(w, http.StatusNotFound, map[string]string{"error": fmt.Sprintf("model %q not found", raw.Model)})
		return
	}
	if raw.Prompt == "" && len(raw.Messages) == 0 {
		// `ollama run` opens its interactive session with an empty-prompt
		// generate call before reading any user input; answer with an empty
		// completion so the CLI drops into its prompt instead of failing.
		log.Printf("[ollama] /api/generate %q: empty prompt (session open), replying empty completion", raw.Model)
		open := ollamaGenerateResponse{
			Model:      raw.Model,
			CreatedAt:  time.Now().UTC().Format(time.RFC3339),
			Done:       true,
			DoneReason: "stop",
		}
		if raw.Stream != nil && !*raw.Stream {
			respondJSON(w, http.StatusOK, open)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(open)
		return
	}

	req := &client.ChatRequest{Model: raw.Model, Messages: raw.Messages}

	// Thinking control: ollama's generate endpoint toggles it via
	// options.thinking / options.reasoning_effort; accept the chat-style
	// top-level "think" field too.
	req.Think = raw.Think
	if raw.Options != nil {
		if req.Think == nil {
			if v, ok := raw.Options["thinking"]; ok {
				req.Think = v
			}
		}
		if lvl, ok := raw.Options["reasoning_effort"].(string); ok && lvl != "" {
			req.ReasoningEffort = lvl
		}
	}
	if lvl := req.ThinkingEffort(); lvl != "" {
		req.ReasoningEffort = lvl
	}

	if len(req.Messages) == 0 {
		if raw.System != "" {
			req.Messages = append(req.Messages, client.NewTextMessage("system", raw.System))
		}
		if len(raw.Images) > 0 {
			req.Messages = append(req.Messages, ollamaImagesMessage(raw.Prompt, raw.Images))
		} else {
			req.Messages = append(req.Messages, client.NewTextMessage("user", raw.Prompt))
		}
	}

	stream := true // ollama's generate endpoint streams unless stream=false
	if raw.Stream != nil {
		stream = *raw.Stream
	}

	if !stream {
		resp, err := s.execChat(r.Context(), req)
		if err != nil {
			respondRouteError(w, raw.Model, err)
			return
		}
		text, thinking := "", ""
		if len(resp.Choices) > 0 {
			text = chatMessageText(resp.Choices[0].Message)
			thinking = resp.Choices[0].Message.Reasoning()
		}
		respondJSON(w, http.StatusOK, ollamaGenerateResponse{
			Model:           raw.Model,
			CreatedAt:       time.Now().UTC().Format(time.RFC3339),
			Response:        text,
			Thinking:        thinking,
			Done:            true,
			DoneReason:      "stop",
			PromptEvalCount: resp.Usage.PromptTokens,
			EvalCount:       resp.Usage.CompletionTokens,
		})
		return
	}

	s.streamGenerate(r, w, req, raw.Model)
}

// streamGenerate emits ollama /api/generate NDJSON lines: text chunks with
// done=false, then a final done=true line carrying token counts.
func (s *Server) streamGenerate(r *http.Request, w http.ResponseWriter, req *client.ChatRequest, mockModel string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming not supported"})
		return
	}

	headerSent := false
	var usage client.ChatUsage

	send := func(gr ollamaGenerateResponse) bool {
		if !headerSent {
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(http.StatusOK)
			headerSent = true
		}
		if err := json.NewEncoder(w).Encode(gr); err != nil {
			log.Printf("[ollama] /api/generate stream write failed for model %q: %v", mockModel, err)
			return false
		}
		flusher.Flush()
		return true
	}

	now := func() string { return time.Now().UTC().Format(time.RFC3339) }

	err := s.execChatStream(r.Context(), req, func(evt string) bool {
		data := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(evt), "data:"))
		if data == "" {
			return true
		}
		if data == "[DONE]" {
			return send(ollamaGenerateResponse{
				Model:           mockModel,
				CreatedAt:       now(),
				Done:            true,
				DoneReason:      "stop",
				PromptEvalCount: usage.PromptTokens,
				EvalCount:       usage.CompletionTokens,
			})
		}

		var chunk client.StreamingChatResponse
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return true // relay-resilient: skip unparseable events
		}
		if chunk.Usage.PromptTokens > 0 || chunk.Usage.CompletionTokens > 0 {
			usage = chunk.Usage
		}
		text, thinking := "", ""
		if len(chunk.Choices) > 0 {
			text = chatMessageText(chunk.Choices[0].Delta)
			thinking = chunk.Choices[0].Delta.Reasoning()
		}
		if text == "" && thinking == "" {
			return true // finish_reason/role-only chunk; ollama emits text only
		}
		return send(ollamaGenerateResponse{
			Model:     mockModel,
			CreatedAt: now(),
			Response:  text,
			Thinking:  thinking,
		})
	})

	switch {
	case err != nil && !headerSent:
		respondRouteError(w, mockModel, err)
	case err != nil:
		// mid-stream failure: report an ollama-style error line, end stream
		log.Printf("[ollama] /api/generate stream for model %q aborted: %v", mockModel, err)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		flusher.Flush()
	}
}

// ollamaImagesMessage turns ollama's prompt + bare-base64 images[] into an
// OpenAI-format multi-part message.
func ollamaImagesMessage(prompt string, images []string) client.ChatMessage {
	parts := make([]client.MessagePart, 0, 1+len(images))
	if prompt != "" {
		parts = append(parts, client.MessagePart{Type: "text", Text: prompt})
	}
	for _, img := range images {
		parts = append(parts, client.MessagePart{
			Type:     "image_url",
			ImageURL: &client.ImageURL{URL: "data:" + sniffImageMIME(img) + ";base64," + img},
		})
	}
	return client.ChatMessage{Role: "user", Content: parts}
}

// sniffImageMIME guesses the media type of a bare base64-encoded image;
// ollama's images[] carries no media type. Ollama upstreams ignore the type,
// but OpenAI-compatible upstreams validate the data URL prefix.
func sniffImageMIME(b64 string) string {
	if raw, err := base64.StdEncoding.DecodeString(b64); err == nil {
		if ct := http.DetectContentType(raw); strings.HasPrefix(ct, "image/") {
			return ct
		}
	}
	return "image/png"
}

// chatMessageText extracts the text body of a message or delta.
func chatMessageText(m client.ChatMessage) string {
	switch c := m.Content.(type) {
	case string:
		return c
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", c)
	}
}
