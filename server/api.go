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
)

// HandleFunc registers all HTTP routes on the given mux.
func (s *Server) HandleFunc(mux *http.ServeMux) {
	// Root / health
	mux.HandleFunc("/", s.handleRoot)

	// Model list APIs (mock model names from -name)
	mux.HandleFunc("/api/tags", s.requireAPIToken(s.handleModelsOllama))
	mux.HandleFunc("/api/openai/v1/models", s.requireAPIToken(s.handleModelsOpenAI))

	// Faked ollama-native endpoints so the ollama CLI's `run` preflight
	// (/api/version, /api/show, /api/pull) succeeds and reaches /api/chat.
	mux.HandleFunc("/api/version", s.requireAPIToken(s.handleVersionOllama))
	mux.HandleFunc("/api/show", s.requireAPIToken(s.handleShowOllama))
	mux.HandleFunc("/api/pull", s.requireAPIToken(s.handlePullOllama))
	mux.HandleFunc("/api/generate", s.requireAPIToken(s.handleGenerateOllama))

	// LLM API routes (Ollama style)
	mux.HandleFunc("/api/chat", s.requireAPIToken(s.handleChat))
	mux.HandleFunc("/api/embed", s.requireAPIToken(s.handleEmbedding))
	mux.HandleFunc("/api/embeddings", s.requireAPIToken(s.handleEmbedding))

	// LLM API routes (OpenAI style)
	mux.HandleFunc("/api/openai/v1/chat/completions", s.requireAPIToken(s.handleChat))
	mux.HandleFunc("/api/openai/v1/embeddings", s.requireAPIToken(s.handleEmbedding))

	// Admin routes
	mux.HandleFunc("/admin/backends", s.handleAdminBackends)
	mux.HandleFunc("/admin/backends/", s.handleAdminBackendDetail)
	mux.HandleFunc("/admin/models", s.handleAdminModels)

	// Parent side: child cllama servers connect here (reverse tunnel)
	mux.HandleFunc("/admin/parent/stream", s.handleTunnelStream)
	mux.HandleFunc("/admin/parent/response/", s.handleTunnelResponse)

	// Child side: manage outbound parent connections (-parent)
	mux.HandleFunc("/admin/parents", s.handleAdminParents)
	mux.HandleFunc("/admin/parents/", s.handleAdminParentDetail)
}

// requireAPIToken guards LLM API endpoints with the -token bearer token.
// When -token is unset, requests pass through unauthenticated.
func (s *Server) requireAPIToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.apiToken == "" {
			next(w, r)
			return
		}

		token := bearerToken(r)
		if token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(s.apiToken)) == 1 {
			next(w, r)
			return
		}
		respondJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid or missing API token"})
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
