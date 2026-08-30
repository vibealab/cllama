package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"cllama/ui"
)

// HandleFunc registers all HTTP routes on the given mux.
func (s *Server) HandleFunc(mux *http.ServeMux) {
	// Root / health
	mux.HandleFunc("/", s.handleRoot)

	// API-surface toggles (config.Enable{Ollama,OpenAI,Anthropic}API,
	// switchable at runtime from the web UI / PUT /admin/config): while a
	// surface is disabled its routes answer 404 as if unregistered.
	ollamaAPI := func() bool { return s.cfg.Get().EnableOllamaAPI }
	openaiAPI := func() bool { return s.cfg.Get().EnableOpenAIAPI }

	// Model list APIs (mock model names from -name)
	mux.HandleFunc("/api/tags", s.requireAPIEnabled(ollamaAPI, s.requireAPIToken(s.handleModelsOllama)))
	mux.HandleFunc("/api/openai/v1/models", s.requireAPIEnabled(openaiAPI, s.requireAPIToken(s.handleModelsOpenAI)))

	// Faked ollama-native endpoints so the ollama CLI's `run` preflight
	// (/api/version, /api/show, /api/pull) succeeds and reaches /api/chat.
	mux.HandleFunc("/api/version", s.requireAPIEnabled(ollamaAPI, s.requireAPIToken(s.handleVersionOllama)))
	mux.HandleFunc("/api/show", s.requireAPIEnabled(ollamaAPI, s.requireAPIToken(s.handleShowOllama)))
	mux.HandleFunc("/api/pull", s.requireAPIEnabled(ollamaAPI, s.requireAPIToken(s.handlePullOllama)))
	mux.HandleFunc("/api/generate", s.requireAPIEnabled(ollamaAPI, s.requireAPIToken(s.handleGenerateOllama)))

	// LLM API routes (Ollama style)
	mux.HandleFunc("/api/chat", s.requireAPIEnabled(ollamaAPI, s.requireAPIToken(s.handleChat)))
	mux.HandleFunc("/api/embed", s.requireAPIEnabled(ollamaAPI, s.requireAPIToken(s.handleEmbedding)))
	mux.HandleFunc("/api/embeddings", s.requireAPIEnabled(ollamaAPI, s.requireAPIToken(s.handleEmbedding)))

	// LLM API routes (OpenAI style)
	mux.HandleFunc("/api/openai/v1/chat/completions", s.requireAPIEnabled(openaiAPI, s.requireAPIToken(s.handleChat)))
	mux.HandleFunc("/api/openai/v1/embeddings", s.requireAPIEnabled(openaiAPI, s.requireAPIToken(s.handleEmbedding)))

	// LLM API routes (Anthropic Messages style; consumed by Claude Code and
	// other Anthropic-native clients — see client/anthropic.go)
	mux.HandleFunc("/api/anthropic/v1/messages", s.requireAnthropicEnabled(s.requireAPITokenAnthropic(s.handleAnthropicMessages)))
	mux.HandleFunc("/api/anthropic/v1/messages/count_tokens", s.requireAnthropicEnabled(s.requireAPITokenAnthropic(s.handleAnthropicCountTokens)))

	// Admin routes
	mux.HandleFunc("/admin/backends", s.handleAdminBackends)
	mux.HandleFunc("/admin/backends/", s.handleAdminBackendDetail)
	mux.HandleFunc("/admin/models", s.handleAdminModels)
	mux.HandleFunc("/admin/config", s.handleAdminConfig)
	mux.HandleFunc("/admin/config/secret/", s.handleAdminConfigSecret)
	mux.HandleFunc("/admin/queue", s.handleAdminQueue)
	mux.HandleFunc("/admin/queue/", s.handleAdminQueueDetail)
	mux.HandleFunc("/admin/events", s.handleAdminEvents)

	// Web UI: embedded static assets by default, or the -debug-ui directory
	// when one was configured via Server.UseUI.
	mux.Handle("/ui/", http.StripPrefix("/ui", ui.Serve(s.uiFS)))

	// Parent side: child cllama servers connect here (reverse tunnel)
	mux.HandleFunc("/admin/parent/stream", s.handleTunnelStream)
	mux.HandleFunc("/admin/parent/response/", s.handleTunnelResponse)

	// Child side: manage outbound parent connections (-parent)
	mux.HandleFunc("/admin/parents", s.handleAdminParents)
	mux.HandleFunc("/admin/parents/", s.handleAdminParentDetail)
}

// requireAPIEnabled guards one of cllama's own API surfaces with its config
// toggle (enable_ollama_api / enable_openai_api / enable_anthropic_api).
// While disabled, the surface's routes answer 404 — as if they had never
// been registered — so clients see the same shape as any unknown path.
func (s *Server) requireAPIEnabled(enabled func() bool, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !enabled() {
			respondJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		next(w, r)
	}
}

// requireAPIToken guards LLM API endpoints with the configured API token
// (config.APIToken, seeded from -token; empty means unauthenticated).
func (s *Server) requireAPIToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		want := s.cfg.Get().APIToken
		if want == "" {
			next(w, r)
			return
		}

		token := bearerToken(r)
		if token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(want)) == 1 {
			next(w, r)
			return
		}
		respondJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid or missing API token"})
	}
}

// requireAnthropicEnabled guards the Anthropic-format surface with its
// config toggle (enable_anthropic_api). Like requireAPIEnabled a disabled
// surface answers 404, but in the Anthropic error envelope so Anthropic
// clients can parse the response.
func (s *Server) requireAnthropicEnabled(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.cfg.Get().EnableAnthropicAPI {
			respondAnthropicErrorObj(w, http.StatusNotFound, "not_found_error", "anthropic api is disabled")
			return
		}
		next(w, r)
	}
}

// bearerToken extracts the token from an "Authorization: Bearer <token>" header.
func bearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return ""
}

// requireAPITokenAnthropic is requireAPIToken for the Anthropic endpoints:
// Anthropic clients authenticate with "x-api-key" (or a bearer token when
// ANTHROPIC_AUTH_TOKEN is used) instead of the OpenAI-style header.
func (s *Server) requireAPITokenAnthropic(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		want := s.cfg.Get().APIToken
		if want == "" {
			next(w, r)
			return
		}

		token := bearerToken(r)
		if token == "" {
			token = r.Header.Get("x-api-key")
		}
		if token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(want)) == 1 {
			next(w, r)
			return
		}
		// Anthropic-format error envelope so Anthropic clients can parse it.
		respondAnthropicErrorObj(w, http.StatusUnauthorized, "authentication_error", "invalid or missing API token")
	}
}

// handleRoot reports service status and the exposed mock model list.
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		log.Printf("[http] no route matches %s %s (404)", r.Method, r.URL.Path)
		respondJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"service": "cllama",
		"status":  "running",
		"models":  s.router.Models(),
	})
}

// handleModelsOpenAI lists the mock models in OpenAI's /v1/models format.
func (s *Server) handleModelsOpenAI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	data := make([]map[string]interface{}, 0)
	for _, name := range s.router.Models() {
		data = append(data, map[string]interface{}{
			"id":       name,
			"object":   "model",
			"owned_by": "cllama",
		})
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"object": "list",
		"data":   data,
	})
}

// handleModelsOllama lists the mock models in Ollama's /api/tags format.
func (s *Server) handleModelsOllama(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	models := make([]map[string]interface{}, 0)
	for _, name := range s.router.Models() {
		// The ollama CLI slices digest with m.Digest[:12] when listing
		// models, so every entry must carry a real-length digest or the
		// CLI panics with a slice-out-of-range error. Use a pseudo digest
		// derived deterministically from the model name.
		sum := sha256.Sum256([]byte(name))
		digest := "sha256:" + hex.EncodeToString(sum[:])

		models = append(models, map[string]interface{}{
			"name":        name,
			"model":       name,
			"digest":      digest,
			"size":        0,
			"modified_at": s.startedAt.UTC().Format(time.RFC3339),
			"details": map[string]string{
				"family":              "cllama",
				"parameter_size":      "0B",
				"quantization_level":  "Q0_0",
			},
		})
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"models": models,
	})
}

// respondJSON writes body as JSON with the given status code.
func respondJSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}
